package verify

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeResolver is a map-backed stand-in for *net.Resolver.
type fakeResolver struct {
	mu      sync.Mutex
	mx      map[string][]*net.MX
	ips     map[string][]net.IP
	txt     map[string][]string
	errs    map[string]error
	mxCalls int
}

func (f *fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	f.mu.Lock()
	f.mxCalls++
	f.mu.Unlock()
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	records, ok := f.mx[name]
	if !ok {
		return nil, notFoundError(name)
	}
	return records, nil
}

func (f *fakeResolver) LookupIP(_ context.Context, _, host string) ([]net.IP, error) {
	if err, ok := f.errs[host]; ok {
		return nil, err
	}
	ips, ok := f.ips[host]
	if !ok {
		return nil, notFoundError(host)
	}
	return ips, nil
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	records, ok := f.txt[name]
	if !ok {
		return nil, notFoundError(name)
	}
	return records, nil
}

func notFoundError(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func TestProberGathersEveryRecord(t *testing.T) {
	resolver := &fakeResolver{
		mx:  map[string][]*net.MX{goodDomain: {{Host: "aspmx.l.google.com.", Pref: 10}}},
		ips: map[string][]net.IP{goodDomain: {net.ParseIP("93.184.216.34")}},
		txt: map[string][]string{
			goodDomain:             {"some-verification=abc", "v=spf1 include:_spf.google.com ~all"},
			"_dmarc." + goodDomain: {"v=DMARC1; p=quarantine"},
		},
	}
	prober := NewProber(ProberConfig{Resolver: resolver})

	facts, err := prober.Lookup(context.Background(), "IronWorksGym.com")
	if err != nil {
		t.Fatalf("Lookup returned %v", err)
	}
	if !facts.HasMX || !facts.HasA || !facts.HasSPF || !facts.HasDMARC {
		t.Fatalf("facts = %+v, want every record found", facts)
	}
	if !facts.Resolves() {
		t.Fatal("a domain with MX and A records must resolve")
	}
	// The trailing root dot is stripped so the host is comparable.
	if len(facts.MXHosts) != 1 || facts.MXHosts[0] != "aspmx.l.google.com" {
		t.Fatalf("mx hosts = %v", facts.MXHosts)
	}
}

// Two addresses on one domain must cost one lookup: a bulk run over tens of
// thousands of addresses touches only a few thousand domains.
func TestProberCachesPerDomain(t *testing.T) {
	resolver := &fakeResolver{
		mx:  map[string][]*net.MX{goodDomain: {{Host: "mx.example.com"}}},
		ips: map[string][]net.IP{goodDomain: {net.ParseIP("1.2.3.4")}},
	}
	prober := NewProber(ProberConfig{Resolver: resolver})

	for range 5 {
		if _, err := prober.Lookup(context.Background(), goodDomain); err != nil {
			t.Fatalf("Lookup returned %v", err)
		}
	}
	if resolver.mxCalls != 1 {
		t.Fatalf("the resolver was called %d times for one domain, want 1", resolver.mxCalls)
	}
}

func TestProberExpiresItsCache(t *testing.T) {
	resolver := &fakeResolver{
		mx:  map[string][]*net.MX{goodDomain: {{Host: "mx.example.com"}}},
		ips: map[string][]net.IP{goodDomain: {net.ParseIP("1.2.3.4")}},
	}
	prober := NewProber(ProberConfig{Resolver: resolver, TTL: time.Hour})

	now := time.Now()
	prober.now = func() time.Time { return now }
	if _, err := prober.Lookup(context.Background(), goodDomain); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Hour)
	if _, err := prober.Lookup(context.Background(), goodDomain); err != nil {
		t.Fatal(err)
	}
	if resolver.mxCalls != 2 {
		t.Fatalf("resolver calls = %d, want the stale entry to be refetched", resolver.mxCalls)
	}
}

// A domain nobody has heard of is an answer, not an error.
func TestProberTreatsNotFoundAsNoRecords(t *testing.T) {
	prober := NewProber(ProberConfig{Resolver: &fakeResolver{}})

	facts, err := prober.Lookup(context.Background(), "nowhere.example")
	if err != nil {
		t.Fatalf("Lookup returned %v, want a clean empty answer", err)
	}
	if facts.Resolves() {
		t.Fatalf("facts = %+v, want nothing found", facts)
	}
}

// A transport failure on both existence probes is an error, so the caller skips the
// DNS checks instead of declaring the domain dead.
func TestProberReportsAnOutage(t *testing.T) {
	outage := errors.New("i/o timeout")
	resolver := &fakeResolver{errs: map[string]error{goodDomain: outage}}
	prober := NewProber(ProberConfig{Resolver: &fakeResolver{
		errs: resolver.errs,
	}})

	if _, err := prober.Lookup(context.Background(), goodDomain); err == nil {
		t.Fatal("Lookup succeeded during a total resolver outage")
	}
}

// An MX lookup that fails while the address lookup answers still yields a usable
// result: the domain demonstrably exists.
func TestProberToleratesOnePartialFailure(t *testing.T) {
	resolver := &fakeResolver{
		errs: map[string]error{},
		ips:  map[string][]net.IP{goodDomain: {net.ParseIP("1.2.3.4")}},
	}
	// Only the MX query fails, and LookupIP is keyed by host so it still answers.
	resolver.mx = nil
	prober := NewProber(ProberConfig{Resolver: resolver})

	facts, err := prober.Lookup(context.Background(), goodDomain)
	if err != nil {
		t.Fatalf("Lookup returned %v", err)
	}
	if facts.HasMX {
		t.Error("HasMX = true with no MX records")
	}
	if !facts.Resolves() {
		t.Error("a domain with address records must still resolve")
	}
}

func TestNullMXIsNotAMailHost(t *testing.T) {
	resolver := &fakeResolver{
		mx:  map[string][]*net.MX{goodDomain: {{Host: ".", Pref: 0}}},
		ips: map[string][]net.IP{goodDomain: {net.ParseIP("1.2.3.4")}},
	}
	prober := NewProber(ProberConfig{Resolver: resolver})

	facts, err := prober.Lookup(context.Background(), goodDomain)
	if err != nil {
		t.Fatal(err)
	}
	if facts.HasMX {
		t.Fatal("a null MX record was counted as a mail host")
	}
}
