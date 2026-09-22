package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AgeLookup resolves when a domain was registered. A nil time with a nil error means
// "the registry did not say", which records the check as skipped rather than failed.
type AgeLookup interface {
	RegisteredAt(ctx context.Context, domain string) (*time.Time, error)
}

// AgeCache persists registration dates, which change once in a domain's lifetime and
// are therefore worth caching for far longer than DNS.
type AgeCache interface {
	LoadDomainAge(ctx context.Context, domain string) (*time.Time, time.Time, bool, error)
	SaveDomainAge(ctx context.Context, domain string, registeredAt *time.Time) error
}

// DomainAgeThreshold is how old a domain must be to earn its points.
const DomainAgeThreshold = 365 * 24 * time.Hour

// RDAPConfig configures the registration-date lookup.
type RDAPConfig struct {
	// BaseURL is the RDAP bootstrap service. The domain is appended to it.
	BaseURL string
	Client  *http.Client
	Cache   AgeCache
	TTL     time.Duration
	Timeout time.Duration
}

// DefaultRDAPBaseURL is IANA's bootstrap endpoint, which redirects to the registry
// that actually holds the domain.
const DefaultRDAPBaseURL = "https://rdap.org/domain/"

// RDAPLookup implements AgeLookup over the RDAP protocol.
//
// RDAP is HTTP rather than DNS, so it is the one network call in Pass 1 that is not
// a name lookup. It is worth only 10 points and every failure degrades to a skipped
// check, so it can never block or fail a verification. Set KARVON_VERIFY_RDAP_ENABLED
// to false to remove it entirely, which lowers the Pass 1 maximum to 75.
type RDAPLookup struct {
	baseURL string
	client  *http.Client
	cache   AgeCache
	ttl     time.Duration

	mu   sync.RWMutex
	memo map[string]ageEntry

	now func() time.Time
}

type ageEntry struct {
	registeredAt *time.Time
	checkedAt    time.Time
}

// NewRDAPLookup builds the registration-date client.
func NewRDAPLookup(cfg RDAPConfig) *RDAPLookup {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultRDAPBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * 24 * time.Hour
	}
	return &RDAPLookup{
		baseURL: cfg.BaseURL,
		client:  cfg.Client,
		cache:   cfg.Cache,
		ttl:     cfg.TTL,
		memo:    make(map[string]ageEntry),
		now:     time.Now,
	}
}

// rdapResponse is the sliver of the RDAP domain object we need.
type rdapResponse struct {
	Events []struct {
		Action string `json:"eventAction"`
		Date   string `json:"eventDate"`
	} `json:"events"`
}

// RegisteredAt implements AgeLookup.
func (r *RDAPLookup) RegisteredAt(ctx context.Context, domain string) (*time.Time, error) {
	domain = normalizeDomain(domain)
	if domain == "" {
		return nil, errors.New("verify: empty domain")
	}

	r.mu.RLock()
	entry, ok := r.memo[domain]
	r.mu.RUnlock()
	if ok && r.now().Sub(entry.checkedAt) <= r.ttl {
		return entry.registeredAt, nil
	}

	if r.cache != nil {
		registeredAt, checkedAt, found, err := r.cache.LoadDomainAge(ctx, domain)
		if err == nil && found && r.now().Sub(checkedAt) <= r.ttl {
			r.store(domain, registeredAt)
			return registeredAt, nil
		}
	}

	registeredAt, err := r.fetch(ctx, domain)
	if err != nil {
		return nil, err
	}

	r.store(domain, registeredAt)
	if r.cache != nil {
		_ = r.cache.SaveDomainAge(ctx, domain, registeredAt)
	}
	return registeredAt, nil
}

func (r *RDAPLookup) store(domain string, registeredAt *time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.memo[domain] = ageEntry{registeredAt: registeredAt, checkedAt: r.now()}
}

func (r *RDAPLookup) fetch(ctx context.Context, domain string) (*time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+domain, nil)
	if err != nil {
		return nil, fmt.Errorf("verify: build rdap request: %w", err)
	}
	req.Header.Set("Accept", "application/rdap+json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("verify: rdap request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A registry that does not know the domain is a definite "no date", which is
	// cached so the same miss is not re-fetched for every address on the domain.
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("verify: rdap returned status %d", resp.StatusCode)
	}

	var payload rdapResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("verify: decode rdap response: %w", err)
	}

	for _, event := range payload.Events {
		if !strings.EqualFold(event.Action, "registration") {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, event.Date)
		if err != nil {
			return nil, nil
		}
		utc := parsed.UTC()
		return &utc, nil
	}
	return nil, nil
}

// disabledAgeLookup is used when RDAP is switched off: every domain reports an
// unknown registration date, so the check is always skipped.
type disabledAgeLookup struct{}

// DisabledAgeLookup returns an AgeLookup that never calls out.
func DisabledAgeLookup() AgeLookup { return disabledAgeLookup{} }

func (disabledAgeLookup) RegisteredAt(context.Context, string) (*time.Time, error) {
	return nil, errAgeDisabled
}

// errAgeDisabled marks the domain-age check as skipped rather than failed.
var errAgeDisabled = errors.New("verify: domain age lookup is disabled")
