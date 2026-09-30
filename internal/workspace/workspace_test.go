package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/registrar/cloudflare"
)

func TestNormalizeLocalPart(t *testing.T) {
	good := map[string]string{"Jane": "jane", " j.doe ": "j.doe", "sales-1": "sales-1", "a_b": "a_b", "x": "x"}
	for in, want := range good {
		if got, err := NormalizeLocalPart(in); err != nil || got != want {
			t.Errorf("NormalizeLocalPart(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".jane", "jane.", "ja..ne", "jane@x", "ja ne", "jané", strings.Repeat("a", 65)} {
		if got, err := NormalizeLocalPart(in); err == nil {
			t.Errorf("NormalizeLocalPart(%q) = %q, want an error", in, got)
		}
	}
}

func TestNormalizeDKIM(t *testing.T) {
	want := "v=DKIM1; k=rsa; p=MIIBIjANBgkq"
	for _, in := range []string{
		"v=DKIM1; k=rsa; p=MIIBIjANBgkq",
		`"v=DKIM1; k=rsa; " "p=MIIBIjANBgkq"`,
		"v=DKIM1;k=rsa;p=MIIBIj\nANBgkq",
		"  v=DKIM1; k=rsa; p=MIIB IjAN Bgkq  ",
	} {
		if got, err := NormalizeDKIM(in); err != nil || got != want {
			t.Errorf("NormalizeDKIM(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "k=rsa; p=abc", "v=DKIM1; k=rsa", "v=spf1 -all"} {
		if _, err := NormalizeDKIM(in); err == nil {
			t.Errorf("NormalizeDKIM(%q) was accepted", in)
		}
	}
}

func TestNormalizeSelectorAndAdmin(t *testing.T) {
	if got, _ := NormalizeSelector(""); got != DefaultDKIMSelector {
		t.Errorf("empty selector = %q", got)
	}
	if _, err := NormalizeSelector("google._domainkey"); err == nil {
		t.Error("a dotted selector was accepted")
	}
	if got, err := NormalizeAdminEmail(" Admin@Karvon.IO "); err != nil || got != "admin@karvon.io" {
		t.Errorf("NormalizeAdminEmail = %q, %v", got, err)
	}
	for _, in := range []string{"admin", "admin@", "@karvon.io", "admin@karvon", "a b@karvon.io"} {
		if _, err := NormalizeAdminEmail(in); err == nil {
			t.Errorf("NormalizeAdminEmail(%q) was accepted", in)
		}
	}
}

func fieldsOf(t *testing.T, err error) map[string]string {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Code != apperr.CodeValidationFailed {
		t.Fatalf("err = %v, want a validation error", err)
	}
	out := map[string]string{}
	for _, f := range appErr.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func TestValidateSetup(t *testing.T) {
	jane := MailboxInput{LocalPart: "Jane", GivenName: " Jane ", FamilyName: "Doe"}
	valid, err := validateSetup(SetupInput{Domain: "https://Shop.COM/", Mailboxes: []MailboxInput{jane}, Confirm: true})
	if err != nil || valid.domain != "shop.com" || valid.mailboxes[0].LocalPart != "jane" || valid.mailboxes[0].GivenName != "Jane" {
		t.Fatalf("validateSetup = %+v, %v", valid, err)
	}

	six := make([]MailboxInput, MaxMailboxesPerDomain+1)
	for i := range six {
		six[i] = MailboxInput{LocalPart: fmt.Sprintf("m%d", i), GivenName: "A", FamilyName: "B"}
	}
	fields := fieldsOf(t, func() error { _, err := validateSetup(SetupInput{Domain: "shop.com", Mailboxes: six}); return err }())
	if fields["confirm"] == "" || fields["mailboxes"] == "" {
		t.Fatalf("fields = %v", fields)
	}

	fields = fieldsOf(t, func() error {
		_, err := validateSetup(SetupInput{Domain: "com", Confirm: true, Mailboxes: []MailboxInput{
			jane, {LocalPart: "JANE", GivenName: "x", FamilyName: "y"}, {LocalPart: "ok", GivenName: "", FamilyName: "<b>"},
		}})
		return err
	}())
	for _, field := range []string{"domain", "mailboxes[1].local_part", "mailboxes[2].given_name", "mailboxes[2].family_name"} {
		if fields[field] == "" {
			t.Errorf("no error for %s in %v", field, fields)
		}
	}
}

// fakeZone is one Cloudflare zone's DNS records.
type fakeZone struct {
	mu      sync.Mutex
	records []cloudflare.DNSRecord
	next    int
}

func (z *fakeZone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	z.mu.Lock()
	defer z.mu.Unlock()
	reply := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
	}
	switch {
	case r.URL.Path == "/zones":
		reply([]map[string]string{{"id": "zone-1", "name": r.URL.Query().Get("name"), "status": "active"}})
	case r.Method == http.MethodGet:
		out := []cloudflare.DNSRecord{}
		for _, rec := range z.records {
			if rec.Type == r.URL.Query().Get("type") && rec.Name == r.URL.Query().Get("name") {
				out = append(out, rec)
			}
		}
		reply(out)
	case r.Method == http.MethodPost:
		var rec cloudflare.DNSRecord
		_ = json.NewDecoder(r.Body).Decode(&rec)
		z.next++
		rec.ID = fmt.Sprintf("rec-%d", z.next)
		z.records = append(z.records, rec)
		reply(rec)
	case r.Method == http.MethodPatch:
		var body struct{ Content string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		for i := range z.records {
			if z.records[i].ID == id {
				z.records[i].Content = body.Content
				reply(z.records[i])
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

func (z *fakeZone) find(recordType, name string) []string {
	var out []string
	for _, rec := range z.records {
		if rec.Type == recordType && rec.Name == name {
			out = append(out, rec.Content)
		}
	}
	return out
}

func newZone(t *testing.T, records ...cloudflare.DNSRecord) (*fakeZone, *cloudflare.Client) {
	t.Helper()
	zone := &fakeZone{}
	for i, rec := range records {
		rec.ID = fmt.Sprintf("old-%d", i)
		zone.records = append(zone.records, rec)
	}
	srv := httptest.NewServer(zone)
	t.Cleanup(srv.Close)
	return zone, cloudflare.New(cloudflare.Config{BaseURL: srv.URL, AccountID: "acct", Token: "tok"})
}

func TestPublishRecordsOnAnEmptyZone(t *testing.T) {
	zone, client := newZone(t)
	token := "google-site-verification=abc"
	for range 2 { // the second pass finds everything in place
		if err := publishRecords(context.Background(), client, "shop.com", &token); err != nil {
			t.Fatal(err)
		}
	}
	if got := zone.find("MX", "shop.com"); len(got) != 1 || got[0] != MXHost {
		t.Errorf("MX = %v", got)
	}
	if got := zone.find("TXT", "shop.com"); len(got) != 2 || got[0] != SPFRecord || got[1] != token {
		t.Errorf("apex TXT = %v", got)
	}
	if got := zone.find("TXT", "_dmarc.shop.com"); len(got) != 1 || got[0] != DMARCRecord {
		t.Errorf("DMARC = %v", got)
	}
}

func TestPublishRecordsReplacesNoMailRecordsOnly(t *testing.T) {
	zone, client := newZone(t,
		cloudflare.DNSRecord{Type: "MX", Name: "shop.com", Content: "."},
		cloudflare.DNSRecord{Type: "TXT", Name: "shop.com", Content: `"v=spf1 -all"`},
		cloudflare.DNSRecord{Type: "TXT", Name: "_dmarc.shop.com", Content: "v=DMARC1; p=reject"},
	)
	if err := publishRecords(context.Background(), client, "shop.com", nil); err != nil {
		t.Fatal(err)
	}
	if got := zone.find("MX", "shop.com"); len(got) != 1 || got[0] != MXHost {
		t.Errorf("MX = %v", got)
	}
	if got := zone.find("TXT", "shop.com"); len(got) != 1 || got[0] != SPFRecord {
		t.Errorf("SPF = %v", got)
	}
	if got := zone.find("TXT", "_dmarc.shop.com"); len(got) != 1 || got[0] != "v=DMARC1; p=reject" {
		t.Errorf("an existing DMARC policy was changed: %v", got)
	}
}

func TestPublishRecordsRefusesAnotherMailService(t *testing.T) {
	cases := map[string]cloudflare.DNSRecord{
		"mx":  {Type: "MX", Name: "shop.com", Content: "route1.mx.cloudflare.net"},
		"spf": {Type: "TXT", Name: "shop.com", Content: "v=spf1 include:spf.protection.outlook.com -all"},
	}
	for name, rec := range cases {
		zone, client := newZone(t, rec)
		err := publishRecords(context.Background(), client, "shop.com", nil)
		var conflict *dnsConflict
		if !errors.As(err, &conflict) {
			t.Errorf("%s: err = %v, want a conflict", name, err)
		}
		if got := zone.find(rec.Type, rec.Name); len(got) != 1 || got[0] != rec.Content {
			t.Errorf("%s: the existing record was touched: %v", name, got)
		}
	}
	// An SPF record that already includes Google is kept as it is.
	spf := "v=spf1 include:_spf.google.com include:mailgun.org ~all"
	zone, client := newZone(t, cloudflare.DNSRecord{Type: "TXT", Name: "shop.com", Content: spf})
	if err := publishRecords(context.Background(), client, "shop.com", nil); err != nil {
		t.Fatal(err)
	}
	if got := zone.find("TXT", "shop.com"); len(got) != 1 || got[0] != spf {
		t.Errorf("SPF = %v", got)
	}
}

func TestUpsertDKIMReplacesAnOlderKey(t *testing.T) {
	zone, client := newZone(t, cloudflare.DNSRecord{Type: "TXT", Name: "google._domainkey.shop.com", Content: "v=DKIM1; k=rsa; p=OLD"})
	if err := upsertDKIM(context.Background(), client, "shop.com", "google", "v=DKIM1; k=rsa; p=NEW"); err != nil {
		t.Fatal(err)
	}
	if got := zone.find("TXT", "google._domainkey.shop.com"); len(got) != 1 || got[0] != "v=DKIM1; k=rsa; p=NEW" {
		t.Errorf("DKIM = %v", got)
	}
}

func TestWithLoginHint(t *testing.T) {
	got := withLoginHint("https://accounts.google.com/o/oauth2/v2/auth?client_id=x&state=api_session:1", "jane@shop.com")
	if !strings.Contains(got, "login_hint=jane%40shop.com") || !strings.Contains(got, "state=api_session%3A1") {
		t.Errorf("withLoginHint = %q", got)
	}
	other := "https://instantly.ai/connect?session=1"
	if got := withLoginHint(other, "jane@shop.com"); got != other {
		t.Errorf("a non-Google URL was changed: %q", got)
	}
}
