package verify

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// Resolver is the slice of net.Resolver the checks use. *net.Resolver satisfies it;
// tests substitute a map-backed fake so no unit test touches the network.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// DomainFacts is everything the four DNS-backed checks need, gathered in one pass.
type DomainFacts struct {
	HasMX    bool     `json:"has_mx"`
	HasA     bool     `json:"has_a"`
	HasSPF   bool     `json:"has_spf"`
	HasDMARC bool     `json:"has_dmarc"`
	MXHosts  []string `json:"mx_hosts"`
}

// Resolves reports whether the domain can receive mail at all. A domain with neither
// MX nor address records is a hard fail.
func (f DomainFacts) Resolves() bool { return f.HasMX || f.HasA }

// DomainLookup resolves a domain's mail-related records.
type DomainLookup interface {
	Lookup(ctx context.Context, domain string) (DomainFacts, error)
}

// DomainCache persists facts between processes so a bulk run over 50k addresses on a
// few thousand domains does not repeat the same queries on every replica.
type DomainCache interface {
	LoadDomainFacts(ctx context.Context, domain string) (DomainFacts, time.Time, bool, error)
	SaveDomainFacts(ctx context.Context, domain string, facts DomainFacts) error
}

// ProberConfig configures the DNS layer.
type ProberConfig struct {
	Resolver Resolver
	Cache    DomainCache
	// TTL is how long a stored result stays fresh.
	TTL time.Duration
	// Timeout bounds one domain's lookups.
	Timeout time.Duration
}

// Prober answers DomainLookup from, in order: an in-process map, the shared cache
// table, and finally the resolver. Concurrent lookups of one domain collapse into a
// single query.
type Prober struct {
	resolver Resolver
	cache    DomainCache
	ttl      time.Duration
	timeout  time.Duration

	group singleflight.Group
	mu    sync.RWMutex
	memo  map[string]memoEntry

	now func() time.Time
}

type memoEntry struct {
	facts     DomainFacts
	checkedAt time.Time
}

// NewProber builds the DNS layer. A nil resolver falls back to the system resolver
// and a nil cache keeps results in this process only.
func NewProber(cfg ProberConfig) *Prober {
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 7 * 24 * time.Hour
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	return &Prober{
		resolver: cfg.Resolver,
		cache:    cfg.Cache,
		ttl:      cfg.TTL,
		timeout:  cfg.Timeout,
		memo:     make(map[string]memoEntry),
		now:      time.Now,
	}
}

// Lookup implements DomainLookup. It returns an error only when the domain's
// existence could not be determined at all; a domain that genuinely has no records
// comes back as zero-valued facts with a nil error.
func (p *Prober) Lookup(ctx context.Context, domain string) (DomainFacts, error) {
	domain = normalizeDomain(domain)
	if domain == "" {
		return DomainFacts{}, errors.New("verify: empty domain")
	}

	if facts, ok := p.fromMemo(domain); ok {
		return facts, nil
	}

	result, err, _ := p.group.Do(domain, func() (any, error) {
		return p.load(ctx, domain)
	})
	if err != nil {
		return DomainFacts{}, err
	}
	return result.(DomainFacts), nil //nolint:errcheck // the only value stored is DomainFacts
}

func (p *Prober) fromMemo(domain string) (DomainFacts, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, ok := p.memo[domain]
	if !ok || p.now().Sub(entry.checkedAt) > p.ttl {
		return DomainFacts{}, false
	}
	return entry.facts, true
}

func (p *Prober) remember(domain string, facts DomainFacts, checkedAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.memo[domain] = memoEntry{facts: facts, checkedAt: checkedAt}
}

func (p *Prober) load(ctx context.Context, domain string) (DomainFacts, error) {
	if p.cache != nil {
		facts, checkedAt, ok, err := p.cache.LoadDomainFacts(ctx, domain)
		if err == nil && ok && p.now().Sub(checkedAt) <= p.ttl {
			p.remember(domain, facts, checkedAt)
			return facts, nil
		}
	}

	facts, err := p.resolve(ctx, domain)
	if err != nil {
		return DomainFacts{}, err
	}

	p.remember(domain, facts, p.now())
	if p.cache != nil {
		// A cache write failing must not fail the check; the next run re-resolves.
		_ = p.cache.SaveDomainFacts(ctx, domain, facts)
	}
	return facts, nil
}

// resolve runs the four queries concurrently. "Not found" is an answer, not a
// failure; only a transport-level problem on both existence probes is an error.
//
// Each goroutine writes to its own variable and the facts are assembled afterwards,
// so there is nothing for the race detector to find.
func (p *Prober) resolve(ctx context.Context, domain string) (DomainFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var (
		mxHosts        []string
		hasAddr        bool
		hasSPF         bool
		hasDMARC       bool
		mxErr, addrErr error
	)

	group := &errgroup.Group{}
	group.Go(func() error {
		records, err := p.resolver.LookupMX(ctx, domain)
		if err != nil && !isDNSNotFound(err) {
			mxErr = err
			return nil
		}
		for _, record := range records {
			// A single "." target is an explicit null MX: the domain takes no mail.
			if host := normalizeDomain(record.Host); host != "" {
				mxHosts = append(mxHosts, host)
			}
		}
		return nil
	})
	group.Go(func() error {
		ips, err := p.resolver.LookupIP(ctx, "ip", domain)
		if err != nil && !isDNSNotFound(err) {
			addrErr = err
			return nil
		}
		hasAddr = len(ips) > 0
		return nil
	})
	group.Go(func() error {
		records, err := p.resolver.LookupTXT(ctx, domain)
		if err == nil {
			hasSPF = containsPrefix(records, "v=spf1")
		}
		return nil
	})
	group.Go(func() error {
		records, err := p.resolver.LookupTXT(ctx, "_dmarc."+domain)
		if err == nil {
			hasDMARC = containsPrefix(records, "v=dmarc1")
		}
		return nil
	})
	_ = group.Wait()

	// Neither existence probe answered, so the domain's status is unknown rather
	// than absent. The caller records the DNS checks as skipped.
	if mxErr != nil && addrErr != nil {
		return DomainFacts{}, mxErr
	}

	return DomainFacts{
		HasMX:    len(mxHosts) > 0,
		HasA:     hasAddr,
		HasSPF:   hasSPF,
		HasDMARC: hasDMARC,
		MXHosts:  mxHosts,
	}, nil
}

// containsPrefix reports whether any TXT record starts with the given marker,
// ignoring case and leading whitespace.
func containsPrefix(records []string, marker string) bool {
	for _, record := range records {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(record)), marker) {
			return true
		}
	}
	return false
}

// isDNSNotFound reports whether the resolver said the name does not exist, which is
// a definite answer rather than a failure.
func isDNSNotFound(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	return false
}

// normalizeDomain lower-cases a host and strips the trailing root dot.
func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}
