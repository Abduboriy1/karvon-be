// Package service is the campaign module's application layer: the operations the
// HTTP handlers and the River jobs share, on top of the pure domain in package
// campaign and the provider clients.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/ai"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp"
	"github.com/bory/karvon-be/internal/crypto"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/queue"
)

// Config bounds what the service does.
type Config struct {
	// PublicBaseURL is where providers can reach this API; empty means webhooks
	// cannot be registered and reconciliation carries the load.
	PublicBaseURL string
	MaxImport     int
	LeadBatch     int
}

// Page is one page of a list.
type Page[T any] struct {
	Rows  []T
	Total int64
}

// Service is the campaign module's application layer: everything the HTTP handlers
// and the jobs share. Provider calls go through the two factories so tests never
// reach the network.
type Service struct {
	store     *db.Store
	cipher    *crypto.Cipher
	instantly instantly.Factory
	mailchimp mailchimp.Factory
	ai        ai.Provider
	queue     queue.Enqueuer
	log       *slog.Logger
	cfg       Config
	now       func() time.Time
}

// NewService wires the service. queue may be nil at construction and set later with
// SetQueue, because the River client needs the workers and the workers need it.
func NewService(store *db.Store, cipher *crypto.Cipher, instantlyFactory instantly.Factory,
	mailchimpFactory mailchimp.Factory, aiProvider ai.Provider, q queue.Enqueuer, log *slog.Logger, cfg Config,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxImport <= 0 {
		cfg.MaxImport = 50_000
	}
	if cfg.LeadBatch <= 0 {
		cfg.LeadBatch = 100
	}
	return &Service{
		store: store, cipher: cipher, instantly: instantlyFactory, mailchimp: mailchimpFactory,
		ai: aiProvider, queue: q, log: log, cfg: cfg, now: func() time.Time { return time.Now().UTC() },
	}
}

// SetQueue closes the River reference once the client exists.
func (s *Service) SetQueue(q queue.Enqueuer) { s.queue = q }

// SetClock replaces the clock, for tests.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Config exposes the service configuration.
func (s *Service) Config() Config { return s.cfg }

// AI exposes the configured generator.
func (s *Service) AI() ai.Provider { return s.ai }

/* ------------------------------------------------------------ providers */

// instantlySource loads the Instantly source row.
func (s *Service) instantlySource(ctx context.Context) (dbgen.Source, error) {
	return s.store.GetSource(ctx, uuid.MustParse(campaign.InstantlySourceID))
}

// mailchimpSource loads the Mailchimp source row.
func (s *Service) mailchimpSource(ctx context.Context) (dbgen.Source, error) {
	return s.store.GetSource(ctx, uuid.MustParse(campaign.MailchimpSourceID))
}

// Instantly resolves a ready Instantly client, or an apperr explaining why not.
func (s *Service) Instantly(ctx context.Context) (instantly.Client, error) {
	source, err := s.instantlySource(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.Conflict("the Instantly source row is missing; run the migrations")
	}
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return s.instantly.For(ctx, source)
}

// Mailchimp resolves a ready Mailchimp client.
func (s *Service) Mailchimp(ctx context.Context) (mailchimp.Client, error) {
	source, err := s.mailchimpSource(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.Conflict("the Mailchimp source row is missing; run the migrations")
	}
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return s.mailchimp.For(ctx, source)
}

/* ---------------------------------------------------------------- queue */

func (s *Service) enqueue(ctx context.Context, args river.JobArgs) error {
	if s.queue == nil {
		return apperr.Internal(errors.New("campaign: queue is not wired"))
	}
	if _, err := s.queue.Insert(ctx, args, nil); err != nil {
		return apperr.Internal(fmt.Errorf("campaign: enqueue %s: %w", args.Kind(), err))
	}
	return nil
}

func (s *Service) enqueueTx(ctx context.Context, tx pgx.Tx, args river.JobArgs) error {
	if s.queue == nil {
		return errors.New("campaign: queue is not wired")
	}
	if _, err := s.queue.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("campaign: enqueue %s: %w", args.Kind(), err)
	}
	return nil
}

// cancelJobs cancels every queued or scheduled job tagged with a campaign.
func (s *Service) cancelJobs(ctx context.Context, campaignID uuid.UUID) {
	if s.queue == nil {
		return
	}
	params := river.NewJobListParams().Metadata(campaign.MetadataFilter(campaignID)).
		States(rivertype.JobStateAvailable, rivertype.JobStateScheduled, rivertype.JobStateRetryable).First(1000)
	list, err := s.queue.JobList(ctx, params)
	if err != nil {
		s.log.Warn("could not list campaign jobs", "campaign_id", campaignID, "error", err)
		return
	}
	for _, job := range list.Jobs {
		if _, err := s.queue.JobCancel(ctx, job.ID); err != nil {
			s.log.Warn("could not cancel campaign job", "job_id", job.ID, "error", err)
		}
	}
}

/* ------------------------------------------------------------- helpers */

func notFound(resource string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound(resource)
	}
	return apperr.Internal(err)
}

func (s *Service) contact(ctx context.Context, id uuid.UUID) (dbgen.Contact, error) {
	row, err := s.store.GetContact(ctx, id)
	if err != nil {
		return dbgen.Contact{}, notFound("contact", err)
	}
	return row, nil
}

func (s *Service) campaign(ctx context.Context, id uuid.UUID) (dbgen.Campaign, error) {
	row, err := s.store.GetCampaign(ctx, id)
	if err != nil {
		return dbgen.Campaign{}, notFound("campaign", err)
	}
	return row, nil
}

// EncodeJSON marshals a map for a jsonb column, never failing.
func EncodeJSON(m map[string]any) []byte {
	if m == nil {
		m = map[string]any{}
	}
	raw, _ := json.Marshal(m)
	return raw
}
