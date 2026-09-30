package cloudflare

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DNS record types the workspace module publishes.
const (
	RecordTXT = "TXT"
	RecordMX  = "MX"
)

// codeIdenticalRecord is Cloudflare's error code for a record that already exists
// with the same type, name and content.
const codeIdenticalRecord = "81058"

// maxRecordsPerPage is the largest page the DNS records list serves.
const maxRecordsPerPage = 100

// Zone is a DNS zone in the account. A domain registered through Cloudflare gets one
// automatically.
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// DNSRecord is one record in a zone. TTL 1 means "automatic".
type DNSRecord struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Priority *int   `json:"priority,omitempty"`
	TTL      int    `json:"ttl,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

// FindZone returns the account's zone for a domain, or ErrNotFound.
func (c *Client) FindZone(ctx context.Context, name string) (Zone, error) {
	query := url.Values{"name": {name}, "account.id": {c.account}, "per_page": {"5"}}
	var out []Zone
	if _, err := c.do(ctx, http.MethodGet, "/zones", query, nil, true, &out); err != nil {
		return Zone{}, err
	}
	for _, zone := range out {
		if strings.EqualFold(zone.Name, name) {
			return zone, nil
		}
	}
	return Zone{}, &APIError{Status: http.StatusNotFound, sentinel: ErrNotFound,
		Messages: []Message{{Message: "no zone for " + name + " in this account"}}}
}

// ListDNSRecords returns the zone's records of one type at exactly one name.
func (c *Client) ListDNSRecords(ctx context.Context, zoneID, recordType, name string) ([]DNSRecord, error) {
	query := url.Values{"type": {recordType}, "name": {name}, "per_page": {strconv.Itoa(maxRecordsPerPage)}}
	var out []DNSRecord
	if _, err := c.do(ctx, http.MethodGet, zonePath(zoneID, "/dns_records"), query, nil, true, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateDNSRecord adds a record. Cloudflare refuses an identical second copy, so a
// repeated call is harmless; see IsIdenticalRecord.
func (c *Client) CreateDNSRecord(ctx context.Context, zoneID string, rec DNSRecord) (DNSRecord, error) {
	if rec.TTL == 0 {
		rec.TTL = 1
	}
	var out DNSRecord
	_, err := c.do(ctx, http.MethodPost, zonePath(zoneID, "/dns_records"), nil, rec, false, &out)
	return out, err
}

// UpdateDNSRecordContent replaces one record's content.
func (c *Client) UpdateDNSRecordContent(ctx context.Context, zoneID, recordID, content string) (DNSRecord, error) {
	body := map[string]string{"content": content}
	var out DNSRecord
	_, err := c.do(ctx, http.MethodPatch, zonePath(zoneID, "/dns_records/"+url.PathEscape(recordID)), nil, body, true, &out)
	return out, err
}

// IsIdenticalRecord reports whether a create failed only because the record exists.
func IsIdenticalRecord(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, m := range apiErr.Messages {
		if m.Code.String() == codeIdenticalRecord {
			return true
		}
	}
	return false
}

// TXTValue is a TXT record's content as one string: `"v=DKIM1; k=rsa; " "p=MIIB..."`
// becomes `v=DKIM1; k=rsa; p=MIIB...`, the quoted chunks joined back together.
// Unquoted content is returned as it is.
func TXTValue(content string) string {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, `"`) {
		return content
	}
	var b strings.Builder
	for i, part := range strings.Split(content, `"`) {
		if i%2 == 1 { // inside quotes
			b.WriteString(part)
		}
	}
	return b.String()
}

func zonePath(zoneID, suffix string) string {
	return "/zones/" + url.PathEscape(zoneID) + suffix
}
