// Package config loads and validates every setting the service needs from the
// environment. Nothing else in the codebase reads os.Getenv directly.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// EnvPrefix is the prefix every Karvon environment variable carries.
const EnvPrefix = "KARVON_"

// Config is the fully resolved runtime configuration.
type Config struct {
	Env      string `env:"ENV" envDefault:"development"`
	Addr     string `env:"ADDR" envDefault:":8080"`
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`
	Version  string `env:"VERSION" envDefault:"dev"`

	DatabaseURL      string        `env:"DATABASE_URL"`
	DBMaxConns       int32         `env:"DB_MAX_CONNS" envDefault:"10"`
	DBConnectTimeout time.Duration `env:"DB_CONNECT_TIMEOUT" envDefault:"10s"`
	MigrateOnBoot    bool          `env:"MIGRATE_ON_BOOT" envDefault:"true"`

	// APIKey is the single static bearer token the dashboard authenticates with.
	APIKey string `env:"API_KEY"`
	// SecretKey is a 32-byte AES key, hex or base64 encoded, used to encrypt
	// provider API keys at rest.
	SecretKey  string `env:"SECRET_KEY"`
	CORSOrigin string `env:"CORS_ORIGIN" envDefault:"http://localhost:5173"`

	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"30s"`

	// Workers controls whether this process runs River workers in addition to the API.
	Workers          bool `env:"WORKERS" envDefault:"true"`
	QueryConcurrency int  `env:"QUERY_CONCURRENCY" envDefault:"4"`

	CrawlConcurrency int           `env:"CRAWL_CONCURRENCY" envDefault:"8"`
	CrawlTimeout     time.Duration `env:"CRAWL_TIMEOUT" envDefault:"10s"`
	// CrawlUserAgent is sent on every crawl request. It is browser-shaped on purpose:
	// shared hosting firewalls answer 403 to anything that announces itself as
	// "(compatible; SomethingBot)", while the trailing KarvonBot token still lets a
	// robots.txt target the crawler by name.
	CrawlUserAgent string `env:"CRAWL_USER_AGENT" envDefault:"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 KarvonBot/1.0 (+https://karvon.local/bot)"`
	// CrawlMaxBodyBytes caps how much of a page body is read before extraction.
	CrawlMaxBodyBytes int64 `env:"CRAWL_MAX_BODY_BYTES" envDefault:"2097152"`
	// CrawlRecrawlAfterDays is how long a domain's crawl result stays fresh.
	CrawlRecrawlAfterDays int `env:"CRAWL_RECRAWL_AFTER_DAYS" envDefault:"30"`
	// CrawlMaxExtraPages caps how many contact-like pages are fetched after the homepage.
	CrawlMaxExtraPages int `env:"CRAWL_MAX_EXTRA_PAGES" envDefault:"5"`
	// CrawlContactWords are extra link words (comma-separated) appended to the
	// crawler's built-in list, e.g. "kontakt,contacto".
	CrawlContactWords []string `env:"CRAWL_CONTACT_WORDS" envSeparator:","`
	// CrawlContactPaths are extra fallback paths (comma-separated) appended to the
	// crawler's built-in list, e.g. "/reach-us,/pages/contact-us".
	CrawlContactPaths []string `env:"CRAWL_CONTACT_PATHS" envSeparator:","`

	ProviderTimeout   time.Duration `env:"PROVIDER_TIMEOUT" envDefault:"5m"`
	ApifyBaseURL      string        `env:"APIFY_BASE_URL" envDefault:"https://api.apify.com"`
	OutscraperBaseURL string        `env:"OUTSCRAPER_BASE_URL" envDefault:"https://api.outscraper.cloud"`

	// Long provider runs. A whole-state search takes hours, so it is started, polled
	// and drained rather than awaited, and these settings bound what it may spend
	// and how much of the vendor account it may occupy.
	//
	// ProviderMaxActiveRuns is the one to tune first. It caps how many runs a vendor
	// account has in flight, which queue concurrency cannot do: a worker polling a
	// run gives its slot back between polls. Six 8 GB runs fit inside a 64 GB Apify
	// account with headroom; raise it with the plan, not past it.
	ProviderMaxActiveRuns int           `env:"PROVIDER_MAX_ACTIVE_RUNS" envDefault:"6"`
	ProviderPollInterval  time.Duration `env:"PROVIDER_POLL_INTERVAL" envDefault:"1m"`
	ProviderMaxRunTime    time.Duration `env:"PROVIDER_MAX_RUN_TIME" envDefault:"12h"`
	ProviderPageSize      int           `env:"PROVIDER_PAGE_SIZE" envDefault:"1000"`

	// Apify run settings. Memory buys speed, not results: the bill is per place.
	ApifyRunMemoryMB int `env:"APIFY_RUN_MEMORY_MB" envDefault:"8192"`
	// ApifyMaxChargeUSD caps what a single run may spend. It is the only hard stop
	// on an uncapped run, which is what a whole-state search is.
	ApifyMaxChargeUSD float64 `env:"APIFY_MAX_CHARGE_USD" envDefault:"25"`
	// ApifyScrapeContacts adds website-contact enrichment, billed per place on top of
	// the place itself. The crawl stage already finds emails, so this is off by
	// default and worth measuring against a crawl before turning on.
	ApifyScrapeContacts bool `env:"APIFY_SCRAPE_CONTACTS" envDefault:"false"`
	// ApifySkipClosedPlaces drops closed businesses before they are billed.
	ApifySkipClosedPlaces bool   `env:"APIFY_SKIP_CLOSED_PLACES" envDefault:"true"`
	ApifyCountryCode      string `env:"APIFY_COUNTRY_CODE" envDefault:"us"`

	// Verification settings. Pass 1 is local and free; Pass 2 calls a paid API, so
	// everything that bounds spending lives here.
	VerifySelfConcurrency       int     `env:"VERIFY_SELF_CONCURRENCY" envDefault:"8"`
	VerifyThirdPartyConcurrency int     `env:"VERIFY_THIRD_PARTY_CONCURRENCY" envDefault:"4"`
	VerifyThirdPartyRPS         float64 `env:"VERIFY_THIRD_PARTY_RPS" envDefault:"5"`
	// VerifyPass2MinScore is the paid floor: an address whose free score is below it
	// is never sent to the paid provider, because a vendor would only confirm what
	// the free checks already established. It seeds paid_min_score in the settings
	// row; after that the operator owns it.
	VerifyPass2MinScore int `env:"VERIFY_PASS2_MIN_SCORE" envDefault:"50"`
	// VerifyPaidThreshold is the paid ceiling: an address whose free score reaches
	// it is already confident enough and is never billed. Seeds paid_threshold.
	//
	// The default of 90 is one above FreeMaxScore, so every address the free stage
	// scores at or above the floor is paid for. A free score is a statement about
	// the domain, not about the mailbox: only the paid provider can confirm that
	// mail is accepted, and only a paid result can reach the green band. Lower this
	// to trade confidence for credits.
	VerifyPaidThreshold int `env:"VERIFY_PAID_THRESHOLD" envDefault:"90"`
	// VerifyPaidEnabled seeds paid_enabled: whether the paid stage runs at all.
	VerifyPaidEnabled bool `env:"VERIFY_PAID_ENABLED" envDefault:"true"`

	// Weights of the free providers, as percentages that must total 100. They seed
	// the settings row; the settings page owns them afterwards.
	VerifyWeightExisting    int `env:"VERIFY_WEIGHT_EXISTING" envDefault:"30"`
	VerifyWeightMailChecker int `env:"VERIFY_WEIGHT_MAILCHECKER" envDefault:"20"`
	VerifyWeightReacher     int `env:"VERIFY_WEIGHT_REACHER" envDefault:"50"`

	// VerifyMailCheckerEnabled controls the compiled-in MailChecker provider. It is
	// MIT-licensed and needs no service, so it is on by default.
	VerifyMailCheckerEnabled bool `env:"VERIFY_MAILCHECKER_ENABLED" envDefault:"true"`

	// Reacher is the self-hosted SMTP probe. It is OFF by default, and deliberately
	// so: check-if-email-exists is dual-licensed AGPL-3.0 / commercial, and running
	// it behind a proprietary service is exactly the case the AGPL covers. Turn it
	// on once the licence question has been settled. See the verification plan.
	VerifyReacherEnabled bool   `env:"VERIFY_REACHER_ENABLED" envDefault:"false"`
	VerifyReacherURL     string `env:"VERIFY_REACHER_URL" envDefault:"http://localhost:8081"`
	// VerifyReacherSecret is sent as x-reacher-secret; empty when the backend has
	// no header_secret configured.
	VerifyReacherSecret string `env:"VERIFY_REACHER_SECRET"`
	// VerifyReacherTimeout bounds one verification including its retries. SMTP
	// conversations with a greylisting server are slow, so it is generous.
	VerifyReacherTimeout time.Duration `env:"VERIFY_REACHER_TIMEOUT" envDefault:"30s"`
	VerifyReacherRetries int           `env:"VERIFY_REACHER_RETRIES" envDefault:"2"`
	// VerifyReacherConcurrency caps in-flight requests so we never outrun the
	// backend's own throttle.
	VerifyReacherConcurrency int `env:"VERIFY_REACHER_CONCURRENCY" envDefault:"4"`
	// VerifyReacherRatePerMinute paces requests to Reacher. Keep it at or below
	// the backend's RCH__THROTTLE__MAX_REQUESTS_PER_MINUTE, or the backend answers
	// 429 and the breaker opens. 0 disables the limit.
	VerifyReacherRatePerMinute int `env:"VERIFY_REACHER_RATE_PER_MINUTE" envDefault:"100"`
	// VerifyReacherBreakerThreshold is how many consecutive failures pause calls to
	// Reacher, so a backend that is simply down costs one connection attempt per
	// cooldown rather than one per address. 0 disables the breaker.
	VerifyReacherBreakerThreshold int           `env:"VERIFY_REACHER_BREAKER_THRESHOLD" envDefault:"5"`
	VerifyReacherBreakerCooldown  time.Duration `env:"VERIFY_REACHER_BREAKER_COOLDOWN" envDefault:"1m"`
	// VerifyReacherHelloName and VerifyReacherFromEmail override the backend's own
	// SMTP identity per request. Leave empty to let the backend decide, which is
	// the norm: its reverse DNS is what mail servers actually check.
	VerifyReacherHelloName string `env:"VERIFY_REACHER_HELLO_NAME"`
	VerifyReacherFromEmail string `env:"VERIFY_REACHER_FROM_EMAIL"`
	// VerifyMaxRunEmails bounds one bulk run.
	VerifyMaxRunEmails int `env:"VERIFY_MAX_RUN_EMAILS" envDefault:"50000"`
	// VerifyRoleSoftMode decides what a shared mailbox such as info@ costs:
	// "hard" scores it 0, "penalty" caps it at 60 and keeps it eligible.
	VerifyRoleSoftMode string `env:"VERIFY_ROLE_SOFT_MODE" envDefault:"hard"`
	// VerifyListsDir overrides the embedded blocklists file by file.
	VerifyListsDir string `env:"VERIFY_LISTS_DIR"`

	VerifyDNSTimeout   time.Duration `env:"VERIFY_DNS_TIMEOUT" envDefault:"3s"`
	VerifyDNSCacheDays int           `env:"VERIFY_DNS_CACHE_DAYS" envDefault:"7"`

	// RDAP is the one Pass 1 lookup that is not DNS. It is worth 10 points and every
	// failure degrades to a skipped check; turning it off lowers the maximum to 75.
	VerifyRDAPEnabled   bool          `env:"VERIFY_RDAP_ENABLED" envDefault:"true"`
	VerifyRDAPBaseURL   string        `env:"VERIFY_RDAP_BASE_URL" envDefault:"https://rdap.org/domain/"`
	VerifyRDAPTimeout   time.Duration `env:"VERIFY_RDAP_TIMEOUT" envDefault:"5s"`
	VerifyRDAPCacheDays int           `env:"VERIFY_RDAP_CACHE_DAYS" envDefault:"30"`

	VerifierTimeout  time.Duration `env:"VERIFIER_TIMEOUT" envDefault:"30s"`
	EmailableBaseURL string        `env:"EMAILABLE_BASE_URL" envDefault:"https://api.emailable.com"`

	// Campaign module. Instantly sends the cold email, Mailchimp sends the
	// newsletter; both keys live encrypted in the sources table. Everything here is
	// wiring: endpoints, pacing, reconcile intervals and the OpenAI key.
	//
	// PublicBaseURL is where the two providers can reach this API; it is required
	// to register webhooks and is otherwise optional.
	PublicBaseURL string `env:"PUBLIC_BASE_URL"`

	InstantlyBaseURL      string        `env:"INSTANTLY_BASE_URL" envDefault:"https://api.instantly.ai/api/v2"`
	InstantlyTimeout      time.Duration `env:"INSTANTLY_TIMEOUT" envDefault:"30s"`
	InstantlyRPS          float64       `env:"INSTANTLY_RPS" envDefault:"5"`
	InstantlyLeadBatch    int           `env:"INSTANTLY_LEAD_BATCH" envDefault:"100"`
	InstantlyLeadBatchGap time.Duration `env:"INSTANTLY_LEAD_BATCH_GAP" envDefault:"2s"`

	MailchimpBaseURL          string        `env:"MAILCHIMP_BASE_URL" envDefault:"https://{dc}.api.mailchimp.com/3.0"`
	MailchimpTimeout          time.Duration `env:"MAILCHIMP_TIMEOUT" envDefault:"30s"`
	MailchimpConcurrency      int           `env:"MAILCHIMP_CONCURRENCY" envDefault:"4"`
	MailchimpWebhookTolerance time.Duration `env:"MAILCHIMP_WEBHOOK_TOLERANCE" envDefault:"5m"`

	// OpenAIAPIKey switches the AI generator from the manual copy/paste flow to
	// direct API calls. A ChatGPT subscription does not provide this key.
	OpenAIAPIKey  string        `env:"OPENAI_API_KEY"`
	OpenAIModel   string        `env:"OPENAI_MODEL" envDefault:"gpt-5.6-terra"`
	OpenAIBaseURL string        `env:"OPENAI_BASE_URL" envDefault:"https://api.openai.com/v1"`
	OpenAITimeout time.Duration `env:"OPENAI_TIMEOUT" envDefault:"120s"`

	CampaignSyncInterval          time.Duration `env:"CAMPAIGN_SYNC_INTERVAL" envDefault:"15m"`
	CampaignAccountsSyncInterval  time.Duration `env:"CAMPAIGN_ACCOUNTS_SYNC_INTERVAL" envDefault:"6h"`
	CampaignWebhookReplayInterval time.Duration `env:"CAMPAIGN_WEBHOOK_REPLAY_INTERVAL" envDefault:"30m"`
	CampaignLeadsFullSyncInterval time.Duration `env:"CAMPAIGN_LEADS_FULL_SYNC_INTERVAL" envDefault:"24h"`
	NewsletterSyncInterval        time.Duration `env:"NEWSLETTER_SYNC_INTERVAL" envDefault:"30m"`
	CampaignPushConcurrency       int           `env:"CAMPAIGN_PUSH_CONCURRENCY" envDefault:"2"`
	CampaignEventConcurrency      int           `env:"CAMPAIGN_EVENT_CONCURRENCY" envDefault:"4"`
	CampaignMaxImport             int           `env:"CAMPAIGN_MAX_IMPORT" envDefault:"50000"`
	WebhookMaxBodyBytes           int64         `env:"WEBHOOK_MAX_BODY_BYTES" envDefault:"1048576"`

	EventRetentionDays int           `env:"EVENT_RETENTION_DAYS" envDefault:"30"`
	SSEPingInterval    time.Duration `env:"SSE_PING_INTERVAL" envDefault:"15s"`

	MaxQueriesPerJob int `env:"MAX_QUERIES_PER_JOB" envDefault:"500"`
}

// IsProduction reports whether the service runs with production defaults.
func (c Config) IsProduction() bool {
	return strings.EqualFold(c.Env, "production") || strings.EqualFold(c.Env, "prod")
}

// Load reads .env (when present), parses KARVON_* variables and validates the result.
func Load(dotenvPaths ...string) (Config, error) {
	for _, p := range dotenvPaths {
		if err := loadDotenv(p); err != nil {
			return Config{}, fmt.Errorf("load %s: %w", p, err)
		}
	}
	cfg, err := env.ParseAsWithOptions[Config](env.Options{Prefix: EnvPrefix})
	if err != nil {
		return Config{}, fmt.Errorf("parse environment: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Defaults returns the configuration with every envDefault applied and nothing read
// from the process environment. Production always goes through Load; this exists so
// tests can build on the real defaults and adding a setting does not break every
// literal in the suite.
func Defaults() Config {
	cfg, err := env.ParseAsWithOptions[Config](env.Options{
		Prefix:      EnvPrefix,
		Environment: map[string]string{},
	})
	if err != nil {
		// The struct tags are compiled in, so this cannot fail at run time.
		panic("config: defaults are not parseable: " + err.Error())
	}
	return cfg
}

// Validate returns an error describing every missing or nonsensical setting.
func (c Config) Validate() error {
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New(EnvPrefix+"DATABASE_URL is required"))
	}
	if c.APIKey == "" {
		errs = append(errs, errors.New(EnvPrefix+"API_KEY is required"))
	} else if c.IsProduction() && len(c.APIKey) < 16 {
		errs = append(errs, errors.New(EnvPrefix+"API_KEY must be at least 16 characters in production"))
	}
	switch {
	case c.SecretKey == "":
		errs = append(errs, errors.New(EnvPrefix+"SECRET_KEY is required (32 bytes, hex or base64)"))
	case c.IsProduction() && isPlaceholderSecret(c.SecretKey):
		errs = append(errs, errors.New(EnvPrefix+"SECRET_KEY is still the development placeholder; generate one with `make secret`"))
	}
	if c.CrawlConcurrency < 1 || c.CrawlConcurrency > 200 {
		errs = append(errs, errors.New(EnvPrefix+"CRAWL_CONCURRENCY must be between 1 and 200"))
	}
	if c.QueryConcurrency < 1 || c.QueryConcurrency > 64 {
		errs = append(errs, errors.New(EnvPrefix+"QUERY_CONCURRENCY must be between 1 and 64"))
	}
	if c.MaxQueriesPerJob < 1 {
		errs = append(errs, errors.New(EnvPrefix+"MAX_QUERIES_PER_JOB must be positive"))
	}
	if c.ProviderMaxActiveRuns < 1 || c.ProviderMaxActiveRuns > 64 {
		errs = append(errs, errors.New(EnvPrefix+"PROVIDER_MAX_ACTIVE_RUNS must be between 1 and 64"))
	}
	if c.ProviderPollInterval < time.Second {
		errs = append(errs, errors.New(EnvPrefix+"PROVIDER_POLL_INTERVAL must be at least 1s"))
	}
	if c.ProviderPageSize < 1 || c.ProviderPageSize > 10000 {
		errs = append(errs, errors.New(EnvPrefix+"PROVIDER_PAGE_SIZE must be between 1 and 10000"))
	}
	if c.ApifyRunMemoryMB < 256 || c.ApifyRunMemoryMB > 32768 {
		errs = append(errs, errors.New(EnvPrefix+"APIFY_RUN_MEMORY_MB must be between 256 and 32768"))
	}
	if c.ApifyMaxChargeUSD < 0 {
		errs = append(errs, errors.New(EnvPrefix+"APIFY_MAX_CHARGE_USD must not be negative"))
	}
	if c.CrawlMaxBodyBytes < 1024 {
		errs = append(errs, errors.New(EnvPrefix+"CRAWL_MAX_BODY_BYTES must be at least 1024"))
	}
	if c.CrawlMaxExtraPages < 0 || c.CrawlMaxExtraPages > 20 {
		errs = append(errs, errors.New(EnvPrefix+"CRAWL_MAX_EXTRA_PAGES must be between 0 and 20"))
	}
	if c.VerifySelfConcurrency < 1 || c.VerifySelfConcurrency > 64 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_SELF_CONCURRENCY must be between 1 and 64"))
	}
	if c.VerifyThirdPartyConcurrency < 1 || c.VerifyThirdPartyConcurrency > 64 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_THIRD_PARTY_CONCURRENCY must be between 1 and 64"))
	}
	if c.VerifyThirdPartyRPS <= 0 || c.VerifyThirdPartyRPS > 1000 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_THIRD_PARTY_RPS must be between 0 and 1000"))
	}
	if c.VerifyPass2MinScore < 0 || c.VerifyPass2MinScore > 100 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_PASS2_MIN_SCORE must be between 0 and 100"))
	}
	if c.VerifyPaidThreshold < 0 || c.VerifyPaidThreshold > 100 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_PAID_THRESHOLD must be between 0 and 100"))
	}
	if c.VerifyPass2MinScore > c.VerifyPaidThreshold {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_PASS2_MIN_SCORE must not exceed "+
			EnvPrefix+"VERIFY_PAID_THRESHOLD, or no address would ever qualify for a paid check"))
	}
	for _, weight := range []struct {
		name  string
		value int
	}{
		{"VERIFY_WEIGHT_EXISTING", c.VerifyWeightExisting},
		{"VERIFY_WEIGHT_MAILCHECKER", c.VerifyWeightMailChecker},
		{"VERIFY_WEIGHT_REACHER", c.VerifyWeightReacher},
	} {
		if weight.value < 0 || weight.value > 100 {
			errs = append(errs, errors.New(EnvPrefix+weight.name+" must be between 0 and 100"))
		}
	}
	// The weights seed the settings row, and the settings row is rejected unless its
	// weights total 100. Catching it here means a bad deployment fails at boot with a
	// clear message rather than at the first save.
	if total := c.VerifyWeightExisting + c.VerifyWeightMailChecker + c.VerifyWeightReacher; total != 100 {
		errs = append(errs, fmt.Errorf(
			"%[1]sVERIFY_WEIGHT_EXISTING + %[1]sVERIFY_WEIGHT_MAILCHECKER + %[1]sVERIFY_WEIGHT_REACHER must total 100, got %[2]d",
			EnvPrefix, total))
	}
	if c.VerifyReacherRetries < 0 || c.VerifyReacherRetries > 10 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_REACHER_RETRIES must be between 0 and 10"))
	}
	if c.VerifyReacherConcurrency < 1 || c.VerifyReacherConcurrency > 64 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_REACHER_CONCURRENCY must be between 1 and 64"))
	}
	if c.VerifyReacherEnabled && c.VerifyReacherURL == "" {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_REACHER_URL is required when "+
			EnvPrefix+"VERIFY_REACHER_ENABLED is true"))
	}
	if c.VerifyDNSCacheDays < 1 || c.VerifyDNSCacheDays > 365 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_DNS_CACHE_DAYS must be between 1 and 365"))
	}
	if c.VerifyRDAPCacheDays < 1 || c.VerifyRDAPCacheDays > 3650 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_RDAP_CACHE_DAYS must be between 1 and 3650"))
	}
	if c.VerifyMaxRunEmails < 1 {
		errs = append(errs, errors.New(EnvPrefix+"VERIFY_MAX_RUN_EMAILS must be positive"))
	}
	switch strings.ToLower(c.VerifyRoleSoftMode) {
	case "hard", "penalty":
	default:
		errs = append(errs, errors.New(EnvPrefix+`VERIFY_ROLE_SOFT_MODE must be "hard" or "penalty"`))
	}
	if c.PublicBaseURL != "" && !strings.HasPrefix(c.PublicBaseURL, "http://") && !strings.HasPrefix(c.PublicBaseURL, "https://") {
		errs = append(errs, errors.New(EnvPrefix+"PUBLIC_BASE_URL must start with http:// or https://"))
	}
	if c.InstantlyRPS <= 0 || c.InstantlyRPS > 100 {
		errs = append(errs, errors.New(EnvPrefix+"INSTANTLY_RPS must be between 0 and 100"))
	}
	if c.InstantlyLeadBatch < 1 || c.InstantlyLeadBatch > 1000 {
		errs = append(errs, errors.New(EnvPrefix+"INSTANTLY_LEAD_BATCH must be between 1 and 1000"))
	}
	if c.MailchimpConcurrency < 1 || c.MailchimpConcurrency > 10 {
		errs = append(errs, errors.New(EnvPrefix+"MAILCHIMP_CONCURRENCY must be between 1 and 10, Mailchimp allows 10 connections"))
	}
	if c.CampaignPushConcurrency < 1 || c.CampaignPushConcurrency > 16 {
		errs = append(errs, errors.New(EnvPrefix+"CAMPAIGN_PUSH_CONCURRENCY must be between 1 and 16"))
	}
	if c.CampaignEventConcurrency < 1 || c.CampaignEventConcurrency > 64 {
		errs = append(errs, errors.New(EnvPrefix+"CAMPAIGN_EVENT_CONCURRENCY must be between 1 and 64"))
	}
	if c.CampaignMaxImport < 1 {
		errs = append(errs, errors.New(EnvPrefix+"CAMPAIGN_MAX_IMPORT must be positive"))
	}
	if c.WebhookMaxBodyBytes < 1024 {
		errs = append(errs, errors.New(EnvPrefix+"WEBHOOK_MAX_BODY_BYTES must be at least 1024"))
	}
	if c.OpenAIAPIKey != "" && strings.TrimSpace(c.OpenAIModel) == "" {
		errs = append(errs, errors.New(EnvPrefix+"OPENAI_MODEL is required when "+EnvPrefix+"OPENAI_API_KEY is set"))
	}
	return errors.Join(errs...)
}

// isPlaceholderSecret catches the all-zero key shipped in .env.example and
// docker-compose.yml, which must never reach production.
func isPlaceholderSecret(key string) bool {
	return strings.Trim(key, "0") == ""
}

// ContactWords is the crawler's built-in contact-link word list followed by any
// KARVON_CRAWL_CONTACT_WORDS additions, blanks removed.
func (c Config) ContactWords(defaults []string) []string {
	return appendClean(defaults, c.CrawlContactWords)
}

// ContactPaths is the crawler's built-in fallback path list followed by any
// KARVON_CRAWL_CONTACT_PATHS additions, blanks removed.
func (c Config) ContactPaths(defaults []string) []string {
	return appendClean(defaults, c.CrawlContactPaths)
}

func appendClean(defaults, extra []string) []string {
	out := make([]string, 0, len(defaults)+len(extra))
	seen := make(map[string]struct{}, len(defaults)+len(extra))
	for _, v := range append(append([]string{}, defaults...), extra...) {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		key := strings.ToLower(v)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	return out
}
