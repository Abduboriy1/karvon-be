package httpapi

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/apperr"

	"github.com/bory/karvon-be/internal/events"
)

// DefaultPerPage and MaxPerPage bound every paginated endpoint.
const (
	DefaultPerPage = 50
	MaxPerPage     = 200
)

// HealthChecker reports dependency health for GET /healthz.
type HealthChecker interface {
	CheckDB(ctx context.Context) error
	CheckQueue(ctx context.Context) error
}

// Config holds transport-level settings.
type Config struct {
	Version         string
	SSEPingInterval time.Duration
	// WebhookMaxBodyBytes caps an inbound provider delivery.
	WebhookMaxBodyBytes int64
	// MailchimpWebhookTolerance is how much clock skew a Mailchimp signature may carry.
	MailchimpWebhookTolerance time.Duration
}

// Server implements gen.ServerInterface over the service layer.
type Server struct {
	jobs         JobService
	businesses   BusinessService
	sources      SourceService
	categories   CategoryService
	exclusions   ExclusionService
	verification VerificationService
	campaigns    CampaignService
	stats        StatsService
	store        EventStore
	listener     *events.Listener
	health       HealthChecker
	log          *slog.Logger
	cfg          Config
}

// Deps bundles everything the HTTP layer needs.
type Deps struct {
	Jobs         JobService
	Businesses   BusinessService
	Sources      SourceService
	Categories   CategoryService
	Exclusions   ExclusionService
	Verification VerificationService
	Campaigns    CampaignService
	Stats        StatsService
	Store        EventStore
	Listener     *events.Listener
	Health       HealthChecker
	Log          *slog.Logger
	Config       Config
}

// NewServer builds the handler set.
func NewServer(deps Deps) *Server {
	if deps.Config.SSEPingInterval <= 0 {
		deps.Config.SSEPingInterval = 15 * time.Second
	}
	if deps.Config.WebhookMaxBodyBytes <= 0 {
		deps.Config.WebhookMaxBodyBytes = 1 << 20
	}
	if deps.Config.MailchimpWebhookTolerance <= 0 {
		deps.Config.MailchimpWebhookTolerance = 5 * time.Minute
	}
	return &Server{
		jobs:         deps.Jobs,
		businesses:   deps.Businesses,
		sources:      deps.Sources,
		categories:   deps.Categories,
		exclusions:   deps.Exclusions,
		verification: deps.Verification,
		campaigns:    deps.Campaigns,
		stats:        deps.Stats,
		store:        deps.Store,
		listener:     deps.Listener,
		health:       deps.Health,
		log:          deps.Log,
		cfg:          deps.Config,
	}
}

// paginate resolves page and per_page, applying defaults and the hard maximum.
func paginate(page, perPage *int) (int, int) {
	resolvedPage := 1
	if page != nil && *page > 0 {
		resolvedPage = *page
	}
	resolvedPerPage := DefaultPerPage
	if perPage != nil && *perPage > 0 {
		resolvedPerPage = *perPage
	}
	if resolvedPerPage > MaxPerPage {
		resolvedPerPage = MaxPerPage
	}
	return resolvedPage, resolvedPerPage
}

// resolveSort validates a client-supplied sort value against the whitelist the data
// layer accepts, so an unknown value is a clear 422 rather than a silent fallback.
func resolveSort(requested *string, allowed []string, fallback string) (string, error) {
	if requested == nil || *requested == "" {
		return fallback, nil
	}
	if slices.Contains(allowed, *requested) {
		return *requested, nil
	}
	return "", apperr.Validation("request parameters are invalid", apperr.FieldError{
		Field:   "sort",
		Message: "must be one of: " + strings.Join(allowed, ", "),
	})
}

// resolveSortParam adapts a generated enum parameter to resolveSort.
func resolveSortParam[T ~string](requested *T, allowed []string) (string, error) {
	if requested == nil {
		return defaultSort, nil
	}
	value := string(*requested)
	return resolveSort(&value, allowed, defaultSort)
}

// defaultSort is the ordering used when a client sends no sort parameter.
const defaultSort = "created_at:desc"
