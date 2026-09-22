package verify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// StoreCache backs the DNS and RDAP caches with the verification_domains table, so
// the lookups a bulk run performs are shared by every worker replica and survive a
// restart.
type StoreCache struct {
	store *db.Store
}

// NewStoreCache wraps a store as a cache for the prober and the RDAP client.
func NewStoreCache(store *db.Store) *StoreCache { return &StoreCache{store: store} }

// LoadDomainFacts implements DomainCache.
func (c *StoreCache) LoadDomainFacts(ctx context.Context, domain string) (DomainFacts, time.Time, bool, error) {
	row, err := c.store.GetVerificationDomain(ctx, domain)
	if errors.Is(err, pgx.ErrNoRows) {
		return DomainFacts{}, time.Time{}, false, nil
	}
	if err != nil {
		return DomainFacts{}, time.Time{}, false, fmt.Errorf("verify: load domain cache: %w", err)
	}
	return DomainFacts{
		HasMX:    row.HasMx,
		HasA:     row.HasA,
		HasSPF:   row.HasSpf,
		HasDMARC: row.HasDmarc,
		MXHosts:  row.MxHosts,
	}, row.DnsCheckedAt, true, nil
}

// SaveDomainFacts implements DomainCache.
func (c *StoreCache) SaveDomainFacts(ctx context.Context, domain string, facts DomainFacts) error {
	hosts := facts.MXHosts
	if hosts == nil {
		hosts = []string{}
	}
	_, err := c.store.UpsertVerificationDNS(ctx, dbgen.UpsertVerificationDNSParams{
		Domain:   domain,
		HasMx:    facts.HasMX,
		HasA:     facts.HasA,
		HasSpf:   facts.HasSPF,
		HasDmarc: facts.HasDMARC,
		MxHosts:  hosts,
	})
	if err != nil {
		return fmt.Errorf("verify: save domain cache: %w", err)
	}
	return nil
}

// LoadDomainAge implements AgeCache. A row that exists but has never been asked
// about registration reports "not cached", so the first RDAP call still happens.
func (c *StoreCache) LoadDomainAge(ctx context.Context, domain string) (*time.Time, time.Time, bool, error) {
	row, err := c.store.GetVerificationDomain(ctx, domain)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("verify: load domain age: %w", err)
	}
	if row.RdapCheckedAt == nil {
		return nil, time.Time{}, false, nil
	}
	return row.RegisteredAt, *row.RdapCheckedAt, true, nil
}

// SaveDomainAge implements AgeCache.
func (c *StoreCache) SaveDomainAge(ctx context.Context, domain string, registeredAt *time.Time) error {
	err := c.store.UpsertVerificationRDAP(ctx, dbgen.UpsertVerificationRDAPParams{
		Domain:       domain,
		RegisteredAt: registeredAt,
	})
	if err != nil {
		return fmt.Errorf("verify: save domain age: %w", err)
	}
	return nil
}

var (
	_ DomainCache = (*StoreCache)(nil)
	_ AgeCache    = (*StoreCache)(nil)
)
