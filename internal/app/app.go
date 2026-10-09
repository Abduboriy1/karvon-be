// Package app wires every component together: configuration, database pool,
// migrations, the River queue, the event listener and the HTTP server. cmd/api is a
// thin shell around it, and integration tests build the same graph.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"golang.org/x/sync/errgroup"

	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/campaign"
	campaignjobs "github.com/bory/karvon-be/internal/campaign/jobs"
	"github.com/bory/karvon-be/internal/category"
	"github.com/bory/karvon-be/internal/config"
	"github.com/bory/karvon-be/internal/crypto"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/exclusion"
	httpapi "github.com/bory/karvon-be/internal/http"
	"github.com/bory/karvon-be/internal/queue"
	"github.com/bory/karvon-be/internal/registrar"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/scraper/crawler"
	"github.com/bory/karvon-be/internal/scraper/fbscrape"
	"github.com/bory/karvon-be/internal/scraper/jobs"
	"github.com/bory/karvon-be/internal/scraper/provider/apify"
	"github.com/bory/karvon-be/internal/source"
	"github.com/bory/karvon-be/internal/stats"
	"github.com/bory/karvon-be/internal/verify"
	verifyjobs "github.com/bory/karvon-be/internal/verify/jobs"
	"github.com/bory/karvon-be/internal/verify/provider"
	"github.com/bory/karvon-be/internal/verify/provider/mailchecker"
	"github.com/bory/karvon-be/internal/verify/provider/reacher"
	"github.com/bory/karvon-be/internal/workspace"
)

// App is a fully wired service instance.
type App struct {
	cfg      config.Config
	log      *slog.Logger
	pool     *pgxpool.Pool
	store    *db.Store
	river    *river.Client[pgx.Tx]
	listener *events.Listener
	handler  http.Handler
	server   *http.Server
	opts     options
}

// New builds the application graph. It runs migrations when KARVON_MIGRATE_ON_BOOT is
// set, which is the default in development and in the compose stack.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, opts ...Option) (*App, error) {
	var resolved options
	for _, opt := range opts {
		opt(&resolved)
	}

	pool, err := db.NewPool(ctx, db.PoolConfig{URL: cfg.DatabaseURL, MaxConns: cfg.DBMaxConns})
	if err != nil {
		return nil, err
	}

	app := &App{cfg: cfg, log: log, pool: pool, opts: resolved}
	if err := app.build(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return app, nil
}

func (a *App) build(ctx context.Context) error {
	if a.cfg.MigrateOnBoot {
		if err := db.Migrate(ctx, a.pool); err != nil {
			return err
		}
		a.log.Info("migrations applied")
	} else {
		// River's schema is required even when application migrations are managed
		// externally, because the queue client fails without it.
		if err := db.MigrateQueue(ctx, a.pool); err != nil {
			return err
		}
	}

	cipher, err := crypto.New(a.cfg.SecretKey)
	if err != nil {
		return err
	}

	a.store = db.NewStore(a.pool)
	publisher := events.NewPublisher(a.store)
	a.listener = events.NewListener(a.pool, a.log)

	var providers scraper.ProviderFactory = scraper.NewProviderFactory(cipher, scraper.ProviderConfig{
		ApifyBaseURL:      a.cfg.ApifyBaseURL,
		OutscraperBaseURL: a.cfg.OutscraperBaseURL,
		Timeout:           a.cfg.ProviderTimeout,
		Apify: apify.Settings{
			MemoryMB:         a.cfg.ApifyRunMemoryMB,
			MaxChargeUSD:     a.cfg.ApifyMaxChargeUSD,
			ScrapeContacts:   a.cfg.ApifyScrapeContacts,
			SkipClosedPlaces: a.cfg.ApifySkipClosedPlaces,
			CountryCode:      a.cfg.ApifyCountryCode,
		},
	})
	if a.opts.providerFactory != nil {
		providers = a.opts.providerFactory
	}

	// The social media scrape reads Facebook Pages through the fb-scrape container.
	// Left nil when it is off, so the workers and the service both see it missing.
	var facebook jobs.FacebookScraper
	var socialNetworks []string
	if a.cfg.FBScrapeEnabled {
		facebook = fbscrape.New(fbscrape.Config{
			BaseURL: a.cfg.FBScrapeURL,
			APIKey:  a.cfg.FBScrapeAPIKey,
			Timeout: a.cfg.FBScrapeTimeout,
		})
		socialNetworks = append(socialNetworks, scraper.SocialNetworkFacebook)
		a.log.Info("Facebook scraper enabled", "url", a.cfg.FBScrapeURL)
	}

	workerDeps := jobs.NewDeps(jobs.Deps{
		Store:     a.store,
		Publisher: publisher,
		Ingestor:  business.NewIngestor(a.store),
		Crawler: crawler.New(crawler.Config{
			UserAgent:       a.cfg.CrawlUserAgent,
			Timeout:         a.cfg.CrawlTimeout,
			MaxBodyBytes:    a.cfg.CrawlMaxBodyBytes,
			PerHostInterval: time.Second,
			MaxExtraPages:   a.cfg.CrawlMaxExtraPages,
			ContactWords:    a.cfg.ContactWords(crawler.DefaultContactWords),
			ContactPaths:    a.cfg.ContactPaths(crawler.DefaultContactPaths),
			Transport:       a.opts.crawlTransport,
		}),
		Facebook:  facebook,
		Providers: providers,
		Log:       a.log,
		Config: jobs.Config{
			ProviderTimeout:    a.cfg.ProviderTimeout,
			RecrawlAfterDays:   a.cfg.CrawlRecrawlAfterDays,
			EventRetentionDays: a.cfg.EventRetentionDays,
			RunPollInterval:    a.cfg.ProviderPollInterval,
			MaxRunDuration:     a.cfg.ProviderMaxRunTime,
			RunPageSize:        a.cfg.ProviderPageSize,
			SocialPageTimeout:  a.cfg.FBScrapeTimeout,
		},
	})

	verifyDeps, verifyService, verifiers, err := a.buildVerification(cipher)
	if err != nil {
		return err
	}

	campaignDeps, campaignService := a.buildCampaign(cipher)
	exclusionService := exclusion.NewService(a.store)
	domainService := registrar.NewService(a.store, cipher, registrar.Config{
		BaseURL:      a.cfg.CloudflareBaseURL,
		Timeout:      a.cfg.CloudflareTimeout,
		PollInterval: a.cfg.DomainPollInterval,
		MaxWait:      a.cfg.DomainMaxWait,
	}, a.log)
	mailboxService := workspace.NewService(a.store, cipher, domainService, workspace.Config{
		TokenURL:              a.cfg.GoogleTokenURL,
		DirectoryURL:          a.cfg.GoogleDirectoryURL,
		SiteVerificationURL:   a.cfg.GoogleSiteVerificationURL,
		Timeout:               a.cfg.GoogleTimeout,
		PollInterval:          a.cfg.WorkspacePollInterval,
		VerifyMaxWait:         a.cfg.WorkspaceVerifyMaxWait,
		InstantlyPollInterval: a.cfg.WorkspaceInstantlyPollInterval,
	}, a.log)
	mailboxService.SetInstantly(campaignService)

	riverClient, err := a.newRiverClient(workerDeps, verifyDeps, campaignDeps, domainService, mailboxService)
	if err != nil {
		return err
	}
	a.river = riverClient
	// The workers need the client that owns them, so the reference is closed here.
	workerDeps.Queue = riverClient
	verifyDeps.Queue = riverClient
	verifyService.SetQueue(riverClient)
	campaignDeps.Queue = riverClient
	campaignService.SetQueue(riverClient)
	exclusionService.SetQueue(riverClient)
	domainService.SetQueue(riverClient)
	mailboxService.SetQueue(riverClient)

	jobService := scraper.NewService(a.store, riverClient, publisher, a.log,
		scraper.ServiceConfig{MaxQueriesPerJob: a.cfg.MaxQueriesPerJob, SocialNetworks: socialNetworks})

	sourceService := source.NewService(a.store, cipher, providers, verifiers, a.log)
	sourceService.SetRegistrar(domainService)
	sourceService.SetMailboxes(mailboxService)

	statsService := stats.NewService(a.store).WithRecurringCosts(stats.RecurringCosts{
		MailboxMonthlyCents: a.cfg.ReportMailboxMonthlyCents,
		FixedMonthlyCents:   a.cfg.ReportFixedMonthlyCents,
	})

	server := httpapi.NewServer(httpapi.Deps{
		Jobs:         jobService,
		Businesses:   business.NewService(a.store),
		Sources:      sourceService,
		Categories:   category.NewService(a.store),
		Exclusions:   exclusionService,
		Verification: verifyService,
		Campaigns:    campaignService,
		Domains:      domainService,
		Mailboxes:    mailboxService,
		Stats:        statsService,
		Store:        a.store,
		Listener:     a.listener,
		Health:       &healthChecker{pool: a.pool},
		Log:          a.log,
		Config: httpapi.Config{
			Version:                   a.cfg.Version,
			SSEPingInterval:           a.cfg.SSEPingInterval,
			WebhookMaxBodyBytes:       a.cfg.WebhookMaxBodyBytes,
			MailchimpWebhookTolerance: a.cfg.MailchimpWebhookTolerance,
			ChatGPTReturnURL:          a.cfg.ChatGPTLandingURL(),
		},
	})

	a.handler = httpapi.NewRouter(server, httpapi.RouterConfig{
		APIKey:         a.cfg.APIKey,
		CORSOrigins:    splitOrigins(a.cfg.CORSOrigin),
		RequestTimeout: a.cfg.RequestTimeout,
		Log:            a.log,
	})
	a.server = &http.Server{
		Addr:              a.cfg.Addr,
		Handler:           a.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: SSE streams and CSV exports are long-lived by design.
		IdleTimeout: 120 * time.Second,
	}
	return nil
}

// buildVerification assembles the verification graph: the free stage (the local
// scorer with its DNS and RDAP caches, MailChecker, and optionally Reacher), the
// settings store that decides how their answers are weighted, the paid vendor
// factory, and the service the HTTP layer talks to.
func (a *App) buildVerification(cipher *crypto.Cipher) (*verifyjobs.Deps, *verify.Service, verify.VerifierFactory, error) {
	lists, err := verify.LoadLists(a.cfg.VerifyListsDir)
	if err != nil {
		return nil, nil, nil, err
	}

	cache := verify.NewStoreCache(a.store)

	prober := verify.NewProber(verify.ProberConfig{
		Resolver: a.opts.resolver,
		Cache:    cache,
		TTL:      time.Duration(a.cfg.VerifyDNSCacheDays) * 24 * time.Hour,
		Timeout:  a.cfg.VerifyDNSTimeout,
	})

	// RDAP is the one Pass 1 lookup that is not DNS. Turning it off lowers the
	// maximum local score from 85 to 75 and is a supported configuration.
	age := verify.DisabledAgeLookup()
	switch {
	case a.opts.ageLookup != nil:
		age = a.opts.ageLookup
	case a.cfg.VerifyRDAPEnabled:
		age = verify.NewRDAPLookup(verify.RDAPConfig{
			BaseURL: a.cfg.VerifyRDAPBaseURL,
			Cache:   cache,
			TTL:     time.Duration(a.cfg.VerifyRDAPCacheDays) * 24 * time.Hour,
			Timeout: a.cfg.VerifyRDAPTimeout,
		})
	}

	pipeline := verify.NewPipeline(verify.PipelineConfig{
		Lists:        lists,
		DNS:          prober,
		Age:          age,
		RoleSoftMode: verify.RoleMode(strings.ToLower(a.cfg.VerifyRoleSoftMode)),
	})

	// The free providers, cheapest first. A provider is constructed whether or not
	// it is currently enabled: enablement is a settings decision the operator can
	// change at run time, and a provider that is switched off simply reports a
	// skipped result.
	extras := []provider.Provider{mailchecker.New()}
	extras = append(extras, reacher.New(reacher.Config{
		BaseURL:          a.cfg.VerifyReacherURL,
		Secret:           a.cfg.VerifyReacherSecret,
		Timeout:          a.cfg.VerifyReacherTimeout,
		Retries:          a.cfg.VerifyReacherRetries,
		Concurrency:      a.cfg.VerifyReacherConcurrency,
		RatePerMinute:    a.cfg.VerifyReacherRatePerMinute,
		BreakerThreshold: a.cfg.VerifyReacherBreakerThreshold,
		BreakerCooldown:  a.cfg.VerifyReacherBreakerCooldown,
		HelloName:        a.cfg.VerifyReacherHelloName,
		FromEmail:        a.cfg.VerifyReacherFromEmail,
		HTTPClient:       a.opts.reacherClient,
		Log:              a.log,
	}))

	stage := verify.NewFreeStage(verify.FreeStageConfig{
		Pipeline: pipeline,
		Extra:    extras,
		Log:      a.log,
	})

	// The environment only seeds the settings row; once it exists the operator owns
	// the policy, so an existing deployment keeps behaving as it did.
	settingsStore := verify.NewSettingsStore(a.store, verify.Settings{
		Weights: map[verify.Key]int{
			provider.KeyExisting:    a.cfg.VerifyWeightExisting,
			provider.KeyMailChecker: a.cfg.VerifyWeightMailChecker,
			provider.KeyReacher:     a.cfg.VerifyWeightReacher,
		},
		Enabled: map[verify.Key]bool{
			provider.KeyMailChecker: a.cfg.VerifyMailCheckerEnabled,
			provider.KeyReacher:     a.cfg.VerifyReacherEnabled,
			provider.KeyPaid:        a.cfg.VerifyPaidEnabled,
		},
		PaidEnabled:    a.cfg.VerifyPaidEnabled,
		PaidThreshold:  a.cfg.VerifyPaidThreshold,
		PaidMinScore:   a.cfg.VerifyPass2MinScore,
		AutoSelfVerify: true,
	}, a.log)

	var verifiers verify.VerifierFactory = verify.NewVerifierFactory(cipher, verify.VerifierConfig{
		EmailableBaseURL: a.cfg.EmailableBaseURL,
		Timeout:          a.cfg.VerifierTimeout,
	})
	if a.opts.verifierFactory != nil {
		verifiers = a.opts.verifierFactory
	}

	serviceCfg := verify.ServiceConfig{
		MaxRunEmails: a.cfg.VerifyMaxRunEmails,
	}

	deps := verifyjobs.NewDeps(verifyjobs.Deps{
		Store:     a.store,
		Stage:     stage,
		Settings:  settingsStore,
		Verifiers: verifiers,
		Limiter:   verify.NewRateLimiter(a.cfg.VerifyThirdPartyRPS),
		Log:       a.log,
		Config:    serviceCfg,
	})

	service := verify.NewService(a.store, nil, verifiers,
		business.NewIngestor(a.store), settingsStore, stage, a.log, serviceCfg)

	a.log.Info("verification ready",
		"max_local_score", pipeline.MaxScore(),
		"free_max_score", verify.FreeMaxScore,
		"paid_band", fmt.Sprintf("%d-%d", a.cfg.VerifyPass2MinScore, a.cfg.VerifyPaidThreshold),
		"mailchecker", a.cfg.VerifyMailCheckerEnabled,
		"reacher", a.cfg.VerifyReacherEnabled,
		"third_party_sends", "once per address, permanent",
		"role_mode", a.cfg.VerifyRoleSoftMode)

	if a.cfg.VerifyReacherEnabled {
		// Loud on purpose. check-if-email-exists is dual-licensed AGPL-3.0 /
		// commercial, and this line is the last chance to notice that nobody
		// decided which of the two applies here.
		a.log.Warn("Reacher is enabled: confirm the AGPL-3.0 or commercial licence "+
			"position before running this in production",
			"url", a.cfg.VerifyReacherURL)
	}

	return deps, service, verifiers, nil
}

func (a *App) newRiverClient(deps *jobs.Deps, verifyDeps *verifyjobs.Deps,
	campaignDeps *campaignjobs.Deps, domainService *registrar.Service, mailboxService *workspace.Service,
) (*river.Client[pgx.Tx], error) {
	cfg := &river.Config{Logger: a.log}

	if a.cfg.Workers {
		workers := river.NewWorkers()
		if err := errors.Join(
			river.AddWorkerSafely(workers, jobs.NewScrapeWorker(deps)),
			river.AddWorkerSafely(workers, jobs.NewQueryWorker(deps)),
			river.AddWorkerSafely(workers, jobs.NewCrawlWorker(deps)),
			river.AddWorkerSafely(workers, jobs.NewRecrawlWorker(deps)),
			river.AddWorkerSafely(workers, jobs.NewSocialScrapeWorker(deps)),
			river.AddWorkerSafely(workers, jobs.NewSocialPageWorker(deps)),
			river.AddWorkerSafely(workers, jobs.NewFinalizeWorker(deps)),
			river.AddWorkerSafely(workers, jobs.NewAbortRunsWorker(deps)),
			river.AddWorkerSafely(workers, jobs.NewPruneWorker(deps)),
			river.AddWorkerSafely(workers, verifyjobs.NewRunWorker(verifyDeps)),
			river.AddWorkerSafely(workers, verifyjobs.NewSelfWorker(verifyDeps)),
			river.AddWorkerSafely(workers, verifyjobs.NewThirdPartyWorker(verifyDeps)),
			river.AddWorkerSafely(workers, verifyjobs.NewFinalizeWorker(verifyDeps)),
			river.AddWorkerSafely(workers, verifyjobs.NewAutoSelfWorker(verifyDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewLaunchWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewPrepareLaunchWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewPushLeadsWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewActivateWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewRemoveLeadWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewExclusionSweepWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewProcessEventWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewSyncCampaignWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewSyncAllWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewSyncAccountsWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewReplayWebhookEventsWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewSyncLeadsFullWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewSyncInboxWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewInstantlyCleanupWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewInstantlyCleanupAutoWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewNewsletterPushWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewNewsletterSyncMembersWorker(campaignDeps)),
			river.AddWorkerSafely(workers, campaignjobs.NewNewsletterSyncAudiencesWorker(campaignDeps)),
			river.AddWorkerSafely(workers, registrar.NewPurchaseWorker(domainService)),
			river.AddWorkerSafely(workers, workspace.NewSetupWorker(mailboxService)),
			river.AddWorkerSafely(workers, workspace.NewInstantlyConnectWorker(mailboxService)),
		); err != nil {
			return nil, fmt.Errorf("app: register workers: %w", err)
		}

		cfg.Workers = workers
		cfg.Queues = map[string]river.QueueConfig{
			queue.QueueDefault:  {MaxWorkers: 4},
			queue.QueueQueries:  {MaxWorkers: a.cfg.QueryConcurrency},
			queue.QueueCrawl:    {MaxWorkers: a.cfg.CrawlConcurrency},
			queue.QueueSocial:   {MaxWorkers: a.cfg.FBScrapeConcurrency},
			queue.QueueFinalize: {MaxWorkers: 2},
			// The paid pass gets its own pool: a vendor outage parks its jobs
			// without starving the scrape pipeline or the free local pass.
			queue.QueueVerifySelf:  {MaxWorkers: a.cfg.VerifySelfConcurrency},
			queue.QueueVerifyThird: {MaxWorkers: a.cfg.VerifyThirdPartyConcurrency},
			// Pushing leads, applying provider events, reconciling and the
			// newsletter each get their own pool, so one provider being slow or
			// down never blocks the others.
			queue.QueueCampaignPush:   {MaxWorkers: a.cfg.CampaignPushConcurrency},
			queue.QueueCampaignEvents: {MaxWorkers: a.cfg.CampaignEventConcurrency},
			queue.QueueCampaignSync:   {MaxWorkers: 2},
			queue.QueueNewsletter:     {MaxWorkers: 2},
			// One worker: purchases spend money and run strictly one at a time.
			queue.QueueDomains:   {MaxWorkers: 1},
			queue.QueueWorkspace: {MaxWorkers: 2},
		}
		periodic := func(every time.Duration, args river.JobArgs, runOnStart bool) *river.PeriodicJob {
			return river.NewPeriodicJob(
				river.PeriodicInterval(every),
				func() (river.JobArgs, *river.InsertOpts) { return args, nil },
				&river.PeriodicJobOpts{RunOnStart: runOnStart},
			)
		}
		// Webhooks update the campaign state immediately; these passes catch
		// whatever a webhook missed, and are the only source of truth when no
		// public URL is configured.
		cfg.PeriodicJobs = []*river.PeriodicJob{
			periodic(24*time.Hour, scraper.PruneEventsArgs{}, true),
			// Verifies what scrapes find as they find it; the setting can switch it off.
			// Not on start: a fresh process has nothing new to score that the next
			// pass, one interval later, would not also find.
			periodic(a.cfg.VerifyAutoInterval, verify.AutoSelfArgs{}, false),
			periodic(a.cfg.CampaignSyncInterval, campaign.SyncAllArgs{}, false),
			periodic(a.cfg.CampaignAccountsSyncInterval, campaign.SyncAccountsArgs{}, true),
			periodic(a.cfg.CampaignWebhookReplayInterval, campaign.ReplayWebhookEventsArgs{}, false),
			periodic(a.cfg.CampaignLeadsFullSyncInterval, campaign.SyncLeadsFullArgs{}, false),
			periodic(a.cfg.InboxSyncInterval, campaign.SyncInboxArgs{}, true),
			// Does nothing unless automatic cleanup is switched on in the settings.
			periodic(a.cfg.InstantlyCleanupInterval, campaign.InstantlyCleanupAutoArgs{}, false),
			periodic(a.cfg.NewsletterSyncInterval, campaign.NewsletterSyncMembersArgs{}, false),
			periodic(a.cfg.CampaignAccountsSyncInterval, campaign.NewsletterSyncAudiencesArgs{}, true),
		}
	}

	client, err := river.NewClient(riverpgxv5.New(a.pool), cfg)
	if err != nil {
		return nil, fmt.Errorf("app: create queue client: %w", err)
	}
	return client, nil
}

// Handler exposes the HTTP handler, which integration tests drive with httptest.
func (a *App) Handler() http.Handler { return a.handler }

// Store exposes the data layer for tests.
func (a *App) Store() *db.Store { return a.store }

// Queue exposes the River client for tests.
func (a *App) Queue() *river.Client[pgx.Tx] { return a.river }

// Run starts the queue workers, the event listener and the HTTP server, and blocks
// until ctx is cancelled or a component fails.
func (a *App) Run(ctx context.Context) error {
	group, groupCtx := errgroup.WithContext(ctx)

	if a.cfg.Workers {
		if err := a.river.Start(groupCtx); err != nil {
			return fmt.Errorf("app: start queue: %w", err)
		}
		a.log.Info("queue workers started",
			"queries", a.cfg.QueryConcurrency, "crawl", a.cfg.CrawlConcurrency)
	}

	group.Go(func() error {
		if err := a.listener.Run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})

	group.Go(func() error {
		a.log.Info("http server listening", "addr", a.cfg.Addr, "env", a.cfg.Env)
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("app: http server: %w", err)
		}
		return nil
	})

	group.Go(func() error {
		<-groupCtx.Done()
		return a.shutdown()
	})

	return group.Wait()
}

// shutdown stops accepting requests, then drains in-flight queue jobs.
func (a *App) shutdown() error {
	// Detached from the (already cancelled) run context so draining has real time.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a.log.Info("shutting down")
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		a.log.Warn("http shutdown returned an error", "error", err)
	}
	if a.cfg.Workers && a.river != nil {
		if err := a.river.Stop(shutdownCtx); err != nil {
			a.log.Warn("queue shutdown returned an error", "error", err)
		}
	}
	return nil
}

// Close releases the database pool.
func (a *App) Close() {
	if a.pool != nil {
		a.pool.Close()
	}
}

// healthChecker answers GET /healthz.
type healthChecker struct {
	pool *pgxpool.Pool
}

func (h *healthChecker) CheckDB(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return h.pool.Ping(ctx)
}

// CheckQueue verifies the River schema is present and readable.
func (h *healthChecker) CheckQueue(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	err := h.pool.QueryRow(ctx, "SELECT 1 FROM river_job LIMIT 1").Scan(&one)
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		// An empty queue table is a healthy queue.
		return nil
	}
	return err
}

// splitOrigins turns a comma-separated CORS setting into a list of origins.
func splitOrigins(raw string) []string {
	var out []string
	for _, origin := range strings.Split(raw, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			out = append(out, origin)
		}
	}
	return out
}
