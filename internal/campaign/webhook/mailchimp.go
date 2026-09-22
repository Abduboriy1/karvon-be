package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MailchimpSignatureHeader carries "t=<unix seconds>,v1=<hex hmac-sha256>".
const MailchimpSignatureHeader = "X-Mailchimp-Signature"

// MailchimpEvent is one normalised Mailchimp webhook payload.
type MailchimpEvent struct {
	Type      string
	FiredAt   time.Time
	ListID    string
	Email     string
	ID        string
	Reason    string
	EmailType string
	OldEmail  string
	NewEmail  string
	WebID     string
	Merges    map[string]string
	Data      map[string]string
	Raw       map[string]any
}

// dataKeyRE matches data[x] and data[x][y].
var dataKeyRE = regexp.MustCompile(`^data\[([^\]]+)\](?:\[([^\]]+)\])?$`)

// ParseMailchimp reads a decoded Mailchimp form. Type is required; Email is
// lower-cased; Data holds every scalar data[x] field and Merges every
// data[merges][X] field. Raw keeps the whole form.
func ParseMailchimp(form url.Values) (MailchimpEvent, error) {
	ev := MailchimpEvent{
		Type:   strings.ToLower(strings.TrimSpace(form.Get("type"))),
		Merges: map[string]string{},
		Data:   map[string]string{},
		Raw:    make(map[string]any, len(form)),
	}
	if ev.Type == "" {
		return MailchimpEvent{}, ErrMissingEventType
	}
	ev.FiredAt = parseMailchimpTime(form.Get("fired_at"))

	for key, values := range form {
		if len(values) == 1 {
			ev.Raw[key] = values[0]
		} else {
			ev.Raw[key] = values
		}
		m := dataKeyRE.FindStringSubmatch(key)
		if m == nil || len(values) == 0 {
			continue
		}
		value := values[0]
		switch {
		case m[2] == "":
			ev.Data[m[1]] = value
		case m[1] == "merges":
			ev.Merges[m[2]] = value
		}
	}

	ev.ID = strings.TrimSpace(ev.Data["id"])
	ev.ListID = strings.TrimSpace(ev.Data["list_id"])
	ev.Email = strings.ToLower(strings.TrimSpace(ev.Data["email"]))
	ev.EmailType = strings.TrimSpace(ev.Data["email_type"])
	ev.Reason = strings.TrimSpace(ev.Data["reason"])
	ev.OldEmail = strings.ToLower(strings.TrimSpace(ev.Data["old_email"]))
	ev.NewEmail = strings.ToLower(strings.TrimSpace(ev.Data["new_email"]))
	ev.WebID = strings.TrimSpace(ev.Data["web_id"])
	if ev.Email == "" && ev.NewEmail != "" {
		ev.Email = ev.NewEmail
	}
	return ev, nil
}

var mailchimpTimeLayouts = []string{
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
}

func parseMailchimpTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range mailchimpTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// MailchimpDedupeKey is the hex SHA-256 of the fields that identify one event.
func MailchimpDedupeKey(ev MailchimpEvent) string {
	fired := ""
	if !ev.FiredAt.IsZero() {
		fired = ev.FiredAt.UTC().Format(time.RFC3339)
	}
	parts := []string{"mailchimp", ev.Type, fired, ev.ListID, ev.Email, ev.ID}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

// VerifyMailchimpSignature checks the X-Mailchimp-Signature header against the raw
// body. It returns ErrMissingSignature when the header is absent or malformed,
// ErrStaleSignature when the timestamp is further than tolerance from now, and
// ErrBadSignature when the HMAC does not match.
func VerifyMailchimpSignature(header string, body []byte, secret string, now time.Time, tolerance time.Duration) error {
	ts, v1, ok := parseSignatureHeader(header)
	if !ok {
		return ErrMissingSignature
	}
	if secret == "" {
		return ErrBadSignature
	}
	at := time.Unix(ts, 0)
	if tolerance < 0 {
		tolerance = -tolerance
	}
	if diff := now.Sub(at); diff > tolerance || diff < -tolerance {
		return ErrStaleSignature
	}
	want := mailchimpMAC(body, secret, ts)
	if !hmac.Equal([]byte(strings.ToLower(v1)), []byte(want)) {
		return ErrBadSignature
	}
	return nil
}

// SignMailchimp builds a valid signature header for tests and fakes.
func SignMailchimp(body []byte, secret string, at time.Time) string {
	ts := at.Unix()
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + mailchimpMAC(body, secret, ts)
}

func mailchimpMAC(body []byte, secret string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func parseSignatureHeader(header string) (ts int64, v1 string, ok bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, "", false
	}
	var haveT bool
	for _, part := range strings.Split(header, ",") {
		k, v, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, "", false
			}
			ts, haveT = n, true
		case "v1":
			v1 = v
		}
	}
	if !haveT || v1 == "" {
		return 0, "", false
	}
	if _, err := hex.DecodeString(v1); err != nil || len(v1) != sha256.Size*2 {
		return 0, "", false
	}
	return ts, v1, true
}
