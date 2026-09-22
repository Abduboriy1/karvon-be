package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
)

// InstantlyEvent is one normalised Instantly webhook payload.
type InstantlyEvent struct {
	EventType        string
	Timestamp        time.Time
	Workspace        string
	CampaignID       string
	CampaignName     string
	LeadEmail        string
	EmailAccount     string
	UniboxURL        string
	Step             int
	Variant          int
	IsFirst          *bool
	EmailID          string
	EmailSubject     string
	EmailText        string
	EmailHTML        string
	ReplySubject     string
	ReplyText        string
	ReplyHTML        string
	ReplyTextSnippet string
	Raw              map[string]any
}

// ParseInstantly decodes an Instantly webhook body. The caller has already
// enforced the size limit. A missing event_type is an error; a missing or
// unparseable timestamp leaves Timestamp zero so the caller can use the receive
// time. Numeric fields may arrive as JSON numbers or as strings. The event type
// is normalised with NormalizeInstantlyType and the lead email is lower-cased.
func ParseInstantly(body []byte) (InstantlyEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return InstantlyEvent{}, fmt.Errorf("webhook: decode instantly payload: %w", err)
	}
	if raw == nil {
		return InstantlyEvent{}, ErrMissingEventType
	}
	eventType := NormalizeInstantlyType(stringOf(raw["event_type"]))
	if eventType == "" {
		return InstantlyEvent{}, ErrMissingEventType
	}
	ev := InstantlyEvent{
		EventType:        eventType,
		Timestamp:        parseInstantlyTime(stringOf(raw["timestamp"])),
		Workspace:        stringOf(raw["workspace"]),
		CampaignID:       strings.TrimSpace(stringOf(raw["campaign_id"])),
		CampaignName:     stringOf(raw["campaign_name"]),
		LeadEmail:        strings.ToLower(strings.TrimSpace(stringOf(raw["lead_email"]))),
		EmailAccount:     strings.ToLower(strings.TrimSpace(stringOf(raw["email_account"]))),
		UniboxURL:        stringOf(raw["unibox_url"]),
		Step:             intOf(raw["step"]),
		Variant:          intOf(raw["variant"]),
		IsFirst:          boolOf(raw["is_first"]),
		EmailID:          strings.TrimSpace(stringOf(raw["email_id"])),
		EmailSubject:     stringOf(raw["email_subject"]),
		EmailText:        stringOf(raw["email_text"]),
		EmailHTML:        stringOf(raw["email_html"]),
		ReplySubject:     stringOf(raw["reply_subject"]),
		ReplyText:        stringOf(raw["reply_text"]),
		ReplyHTML:        stringOf(raw["reply_html"]),
		ReplyTextSnippet: stringOf(raw["reply_text_snippet"]),
		Raw:              raw,
	}
	return ev, nil
}

// NormalizeInstantlyType lower-cases and trims an event type and maps the guide's
// "link_clicked" spelling onto the enum's "email_link_clicked".
func NormalizeInstantlyType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == campaign.InstantlyLinkClicked {
		return campaign.InstantlyEmailLinkClicked
	}
	return t
}

// InstantlyDedupeKey is the hex SHA-256 of the fields that identify one delivery
// of one event, so a replayed webhook maps onto the same key.
func InstantlyDedupeKey(ev InstantlyEvent) string {
	ts := ""
	if !ev.Timestamp.IsZero() {
		ts = ev.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	parts := []string{"instantly", ev.EventType, ev.CampaignID, ev.LeadEmail, ev.EmailAccount,
		strconv.Itoa(ev.Step), strconv.Itoa(ev.Variant), ts, ev.EmailID}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

var instantlyTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
}

func parseInstantlyTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range instantlyTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	// A bare unix timestamp, in seconds or milliseconds.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		if n > 1e12 {
			return time.UnixMilli(n).UTC()
		}
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}

func stringOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

func intOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n)
		}
		if f, err := x.Float64(); err == nil {
			return int(f)
		}
	case string:
		s := strings.TrimSpace(x)
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(f)
		}
	}
	return 0
}

func boolOf(v any) *bool {
	switch x := v.(type) {
	case bool:
		return &x
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(x)); err == nil {
			return &b
		}
	case float64:
		b := x != 0
		return &b
	}
	return nil
}
