package webhook

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
)

func TestInstantlyDedupeKeyIsStableAcrossFieldOrder(t *testing.T) {
	a := []byte(`{"timestamp":"2026-09-20T10:00:00.000Z","event_type":"email_sent","workspace":"ws",
		"campaign_id":"c1","campaign_name":"Launch","lead_email":"Ann@Acme.test","email_account":"me@karvon.test",
		"step":1,"variant":2,"is_first":true,"email_id":"e1","email_subject":"Hi"}`)
	b := []byte(`{"email_id":"e1","variant":"2","step":"1","email_account":"me@karvon.test",
		"lead_email":"ann@acme.test","campaign_id":"c1","event_type":"email_sent",
		"timestamp":"2026-09-20T10:00:00Z","is_first":"true","workspace":"ws","campaign_name":"Launch"}`)
	evA, err := ParseInstantly(a)
	if err != nil {
		t.Fatal(err)
	}
	evB, err := ParseInstantly(b)
	if err != nil {
		t.Fatal(err)
	}
	if InstantlyDedupeKey(evA) != InstantlyDedupeKey(evB) {
		t.Fatalf("keys differ: %s vs %s", InstantlyDedupeKey(evA), InstantlyDedupeKey(evB))
	}
	if len(InstantlyDedupeKey(evA)) != 64 {
		t.Fatalf("key length = %d", len(InstantlyDedupeKey(evA)))
	}
	if evA.LeadEmail != "ann@acme.test" || evA.IsFirst == nil || !*evA.IsFirst {
		t.Fatalf("unexpected event: %+v", evA)
	}
	if !evA.Timestamp.Equal(time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("timestamp = %s", evA.Timestamp)
	}

	evC := evA
	evC.EmailID = "e2"
	if InstantlyDedupeKey(evA) == InstantlyDedupeKey(evC) {
		t.Fatal("a different email id must change the key")
	}
}

func TestInstantlyParserAcceptsLinkClickedAlias(t *testing.T) {
	cases := []struct{ in, want string }{
		{"link_clicked", campaign.InstantlyEmailLinkClicked},
		{" Link_Clicked ", campaign.InstantlyEmailLinkClicked},
		{"email_link_clicked", campaign.InstantlyEmailLinkClicked},
		{"Reply_Received", campaign.InstantlyReplyReceived},
		{"auto_reply_received", campaign.InstantlyAutoReplyReceived},
	}
	for _, tc := range cases {
		if got := NormalizeInstantlyType(tc.in); got != tc.want {
			t.Fatalf("NormalizeInstantlyType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	ev, err := ParseInstantly([]byte(`{"event_type":"link_clicked","lead_email":"a@b.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventType != campaign.InstantlyEmailLinkClicked {
		t.Fatalf("event type = %q", ev.EventType)
	}
	if !ev.Timestamp.IsZero() {
		t.Fatalf("missing timestamp should stay zero, got %s", ev.Timestamp)
	}
}

func TestInstantlyParserToleratesNumericStringsForStep(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		step, variant int
	}{
		{"numbers", `{"event_type":"email_sent","step":2,"variant":3}`, 2, 3},
		{"strings", `{"event_type":"email_sent","step":"2","variant":"3"}`, 2, 3},
		{"floats", `{"event_type":"email_sent","step":2.0,"variant":"3.0"}`, 2, 3},
		{"missing", `{"event_type":"email_sent"}`, 0, 0},
		{"garbage", `{"event_type":"email_sent","step":"two","variant":null}`, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := ParseInstantly([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if ev.Step != tc.step || ev.Variant != tc.variant {
				t.Fatalf("step/variant = %d/%d, want %d/%d", ev.Step, ev.Variant, tc.step, tc.variant)
			}
		})
	}
}

func TestInstantlyParserRejectsAMissingEventType(t *testing.T) {
	for _, body := range []string{`{}`, `{"event_type":""}`, `{"event_type":"  "}`, `{"lead_email":"a@b.test"}`, `null`} {
		if _, err := ParseInstantly([]byte(body)); !errors.Is(err, ErrMissingEventType) {
			t.Fatalf("body %s: err = %v, want ErrMissingEventType", body, err)
		}
	}
	if _, err := ParseInstantly([]byte(`not json`)); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

func TestMailchimpParserParsesBracketedFormFields(t *testing.T) {
	form := url.Values{}
	form.Set("type", "subscribe")
	form.Set("fired_at", "2009-03-26 21:35:57")
	form.Set("data[id]", "8a25ff1d98")
	form.Set("data[list_id]", "a6b5da1054")
	form.Set("data[email]", "API@MailChimp.com")
	form.Set("data[email_type]", "html")
	form.Set("data[ip_opt]", "10.20.10.30")
	form.Set("data[web_id]", "123")
	form.Set("data[merges][EMAIL]", "api@mailchimp.com")
	form.Set("data[merges][FNAME]", "Mailchimp")
	form.Set("data[merges][LNAME]", "API")
	form.Set("data[merges][INTERESTS]", "Group1,Group2")

	ev, err := ParseMailchimp(form)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != campaign.MailchimpSubscribe {
		t.Fatalf("type = %q", ev.Type)
	}
	if !ev.FiredAt.Equal(time.Date(2009, 3, 26, 21, 35, 57, 0, time.UTC)) {
		t.Fatalf("fired at = %s", ev.FiredAt)
	}
	if ev.ID != "8a25ff1d98" || ev.ListID != "a6b5da1054" || ev.Email != "api@mailchimp.com" ||
		ev.EmailType != "html" || ev.WebID != "123" {
		t.Fatalf("unexpected fields: %+v", ev)
	}
	if ev.Merges["FNAME"] != "Mailchimp" || ev.Merges["LNAME"] != "API" || ev.Merges["INTERESTS"] != "Group1,Group2" {
		t.Fatalf("merges = %v", ev.Merges)
	}
	if ev.Data["ip_opt"] != "10.20.10.30" || ev.Data["email"] != "API@MailChimp.com" {
		t.Fatalf("data = %v", ev.Data)
	}
	if _, ok := ev.Data["merges"]; ok {
		t.Fatal("nested merges should not appear as a scalar data field")
	}
	if len(MailchimpDedupeKey(ev)) != 64 {
		t.Fatal("dedupe key should be hex sha256")
	}

	rfc := url.Values{"type": {"upemail"}, "fired_at": {"2009-03-26T21:35:57Z"},
		"data[old_email]": {"Old@x.test"}, "data[new_email]": {"New@x.test"}, "data[list_id]": {"l"}}
	ev2, err := ParseMailchimp(rfc)
	if err != nil {
		t.Fatal(err)
	}
	if !ev2.FiredAt.Equal(ev.FiredAt) {
		t.Fatalf("rfc3339 fired at = %s", ev2.FiredAt)
	}
	if ev2.OldEmail != "old@x.test" || ev2.NewEmail != "new@x.test" {
		t.Fatalf("unexpected upemail fields: %+v", ev2)
	}

	if _, err := ParseMailchimp(url.Values{"data[email]": {"a@b.test"}}); !errors.Is(err, ErrMissingEventType) {
		t.Fatalf("missing type: err = %v", err)
	}
}

func TestMailchimpSignatureAcceptsAValidSignature(t *testing.T) {
	body := []byte("type=subscribe&data%5Bemail%5D=a%40b.test")
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	header := SignMailchimp(body, "s3cret", now.Add(-time.Minute))
	if err := VerifyMailchimpSignature(header, body, "s3cret", now, 5*time.Minute); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Whitespace around the parts and a future timestamp inside the tolerance are fine.
	spaced := " t=" + header[2:len(header)-len(",v1=")-64] + " , v1=" + header[len(header)-64:]
	if err := VerifyMailchimpSignature(spaced, body, "s3cret", now, 5*time.Minute); err != nil {
		t.Fatalf("spaced header rejected: %v", err)
	}
	future := SignMailchimp(body, "s3cret", now.Add(2*time.Minute))
	if err := VerifyMailchimpSignature(future, body, "s3cret", now, 5*time.Minute); err != nil {
		t.Fatalf("slightly future signature rejected: %v", err)
	}
}

func TestMailchimpSignatureRejectsAStaleTimestamp(t *testing.T) {
	body := []byte("type=subscribe")
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	old := SignMailchimp(body, "s3cret", now.Add(-6*time.Minute))
	if err := VerifyMailchimpSignature(old, body, "s3cret", now, 5*time.Minute); !errors.Is(err, ErrStaleSignature) {
		t.Fatalf("err = %v, want ErrStaleSignature", err)
	}
	future := SignMailchimp(body, "s3cret", now.Add(6*time.Minute))
	if err := VerifyMailchimpSignature(future, body, "s3cret", now, 5*time.Minute); !errors.Is(err, ErrStaleSignature) {
		t.Fatalf("future err = %v, want ErrStaleSignature", err)
	}
}

func TestMailchimpSignatureRejectsATamperedBody(t *testing.T) {
	body := []byte("type=subscribe&data%5Bemail%5D=a%40b.test")
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	header := SignMailchimp(body, "s3cret", now)

	tampered := []byte("type=subscribe&data%5Bemail%5D=evil%40b.test")
	if err := VerifyMailchimpSignature(header, tampered, "s3cret", now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body: err = %v, want ErrBadSignature", err)
	}
	if err := VerifyMailchimpSignature(header, body, "other", now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret: err = %v, want ErrBadSignature", err)
	}
	if err := VerifyMailchimpSignature(header, body, "", now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("empty secret: err = %v, want ErrBadSignature", err)
	}
	for _, h := range []string{"", "v1=abc", "t=123", "t=notanumber,v1=" + header[len(header)-64:], "t=1,v1=zz"} {
		if err := VerifyMailchimpSignature(h, body, "s3cret", now, 5*time.Minute); !errors.Is(err, ErrMissingSignature) {
			t.Fatalf("header %q: err = %v, want ErrMissingSignature", h, err)
		}
	}
}

func TestVerifySecretIsFalseForAnEmptyExpectedSecret(t *testing.T) {
	if VerifySecret("", "") {
		t.Fatal("empty/empty must be false")
	}
	if VerifySecret("anything", "") {
		t.Fatal("any presented secret must be false when none is configured")
	}
	if !VerifySecret("abc", "abc") {
		t.Fatal("matching secrets must be true")
	}
	if VerifySecret("abd", "abc") || VerifySecret("", "abc") {
		t.Fatal("mismatched secrets must be false")
	}
}
