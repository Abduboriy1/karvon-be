package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// settingsTTL is how long a loaded settings row is reused.
//
// Short enough that an operator saving the settings page sees the new policy take
// effect on the next run without a restart, long enough that a bulk run of fifty
// thousand addresses does not read the same row fifty thousand times.
const settingsTTL = 15 * time.Second

// SettingsSource is the read side every component that needs policy depends on. The
// HTTP layer, the service and the workers all go through it, so there is exactly one
// place where "what are the current weights" is answered.
type SettingsSource interface {
	// Settings returns the current policy. It never fails: a database that cannot
	// be read yields the configured defaults, because refusing to verify is a
	// worse outcome than verifying with the defaults an operator already approved.
	Settings(ctx context.Context) Settings
}

// SettingsStore loads, caches and persists the single settings row.
type SettingsStore struct {
	store    *db.Store
	defaults Settings
	log      *slog.Logger

	mu       sync.RWMutex
	cached   Settings
	cachedAt time.Time
}

// NewSettingsStore builds the store. defaults come from the KARVON_* environment and
// are what an unwritten row is seeded with, so an existing deployment keeps the
// behaviour it had before this table existed.
func NewSettingsStore(store *db.Store, defaults Settings, log *slog.Logger) *SettingsStore {
	if log == nil {
		log = slog.Default()
	}
	return &SettingsStore{store: store, defaults: defaults.Normalize(), log: log}
}

// Settings implements SettingsSource.
func (s *SettingsStore) Settings(ctx context.Context) Settings {
	s.mu.RLock()
	if !s.cachedAt.IsZero() && time.Since(s.cachedAt) < settingsTTL {
		defer s.mu.RUnlock()
		return s.cached.Clone()
	}
	s.mu.RUnlock()

	loaded, err := s.load(ctx)
	if err != nil {
		s.log.Warn("could not read the verification settings; using the configured defaults",
			"error", err)
		return s.defaults.Clone()
	}
	return loaded
}

// Load reads the row, reporting an error rather than silently falling back. The API
// uses it so a broken settings row is visible rather than invisible.
func (s *SettingsStore) Load(ctx context.Context) (Settings, error) {
	return s.load(ctx)
}

func (s *SettingsStore) load(ctx context.Context) (Settings, error) {
	row, err := s.store.GetVerificationSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		// The migration inserts the row, so this only happens if it was deleted.
		return s.defaults.Clone(), nil
	}
	if err != nil {
		return Settings{}, apperr.Internal(fmt.Errorf("verify: load settings: %w", err))
	}

	settings, err := settingsFromRow(row, s.defaults)
	if err != nil {
		return Settings{}, apperr.Internal(err)
	}
	s.cache(settings)
	return settings.Clone(), nil
}

// Save validates and persists the settings, and refreshes the cache so the next run
// picks them up immediately rather than after the TTL.
func (s *SettingsStore) Save(ctx context.Context, settings Settings) (Settings, error) {
	settings = settings.Normalize()
	if err := settings.Validate(); err != nil {
		return Settings{}, err
	}

	weights, err := json.Marshal(settings.Weights)
	if err != nil {
		return Settings{}, apperr.Internal(fmt.Errorf("verify: encode weights: %w", err))
	}
	enabled, err := json.Marshal(settings.Enabled)
	if err != nil {
		return Settings{}, apperr.Internal(fmt.Errorf("verify: encode provider flags: %w", err))
	}

	row, err := s.store.UpsertVerificationSettings(ctx, dbgen.UpsertVerificationSettingsParams{
		Weights:        weights,
		Enabled:        enabled,
		PaidEnabled:    settings.PaidEnabled,
		PaidThreshold:  int32(settings.PaidThreshold), //nolint:gosec // G115: validated 0-100
		PaidMinScore:   int32(settings.PaidMinScore),  //nolint:gosec // G115: validated 0-100
		AutoSelfVerify: settings.AutoSelfVerify,
	})
	if err != nil {
		return Settings{}, apperr.Internal(fmt.Errorf("verify: save settings: %w", err))
	}

	saved, err := settingsFromRow(row, s.defaults)
	if err != nil {
		return Settings{}, apperr.Internal(err)
	}
	s.cache(saved)
	s.log.Info("verification settings updated",
		"weights", saved.Weights, "paid_enabled", saved.PaidEnabled,
		"paid_min_score", saved.PaidMinScore, "paid_threshold", saved.PaidThreshold,
		"auto_self_verify", saved.AutoSelfVerify)
	return saved.Clone(), nil
}

// UpdatedAt reports when the row was last written, for the API's benefit.
func (s *SettingsStore) UpdatedAt(ctx context.Context) *time.Time {
	row, err := s.store.GetVerificationSettings(ctx)
	if err != nil {
		return nil
	}
	return &row.UpdatedAt
}

func (s *SettingsStore) cache(settings Settings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached, s.cachedAt = settings, time.Now()
}

// settingsFromRow decodes the stored JSONB, falling back to the defaults for any key
// the row does not carry. A row written before a provider existed is therefore still
// readable, which is the point of storing weights as an object rather than columns.
func settingsFromRow(row dbgen.VerificationSetting, defaults Settings) (Settings, error) {
	out := Settings{
		Weights:        map[Key]int{},
		Enabled:        map[Key]bool{},
		PaidEnabled:    row.PaidEnabled,
		PaidThreshold:  int(row.PaidThreshold),
		PaidMinScore:   int(row.PaidMinScore),
		AutoSelfVerify: row.AutoSelfVerify,
	}
	if len(row.Weights) > 0 {
		if err := json.Unmarshal(row.Weights, &out.Weights); err != nil {
			return Settings{}, fmt.Errorf("verify: decode stored weights: %w", err)
		}
	}
	if len(row.Enabled) > 0 {
		if err := json.Unmarshal(row.Enabled, &out.Enabled); err != nil {
			return Settings{}, fmt.Errorf("verify: decode stored provider flags: %w", err)
		}
	}

	// An untouched row carries no weights at all; seeding it from the environment
	// defaults is what makes the first read of a freshly migrated database behave
	// exactly like the deployment did before the table existed.
	if len(out.Weights) == 0 {
		out.Weights = defaults.Clone().Weights
	}
	if len(out.Enabled) == 0 {
		out.Enabled = defaults.Clone().Enabled
	}
	return out.Normalize(), nil
}

// StaticSettings is a SettingsSource backed by a fixed value, for tests and for the
// few code paths that must not touch the database.
type StaticSettings Settings

// Settings implements SettingsSource.
func (s StaticSettings) Settings(context.Context) Settings { return Settings(s).Normalize() }

var _ SettingsSource = (*SettingsStore)(nil)
