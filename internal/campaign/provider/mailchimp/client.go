// Package mailchimp is the Mailchimp Marketing API v3 client, limited to what the
// newsletter stage needs: audiences, members, tags and webhooks.
package mailchimp

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: Mailchimp defines subscriber_hash as MD5 of the lowercase address
	"encoding/hex"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/db/dbgen"
)

// Member statuses as Mailchimp names them.
const (
	StatusSubscribed    = "subscribed"
	StatusUnsubscribed  = "unsubscribed"
	StatusCleaned       = "cleaned"
	StatusPending       = "pending"
	StatusTransactional = "transactional"
	StatusArchived      = "archived"
)

// SubscriberHash is Mailchimp's member key: MD5 of the lower-cased address.
func SubscriberHash(email string) string {
	sum := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email)))) //nolint:gosec // G401: provider-defined key, not a credential
	return hex.EncodeToString(sum[:])
}

// Account is what Ping reports.
type Account struct {
	AccountName string `json:"account_name"`
	Email       string `json:"email"`
	DC          string `json:"-"`
}

// Audience is one list.
type Audience struct {
	ID          string `json:"id"`
	WebID       int64  `json:"web_id"`
	Name        string `json:"name"`
	DoubleOptin bool   `json:"double_optin"`
	Stats       struct {
		MemberCount      int `json:"member_count"`
		UnsubscribeCount int `json:"unsubscribe_count"`
		CleanedCount     int `json:"cleaned_count"`
	} `json:"stats"`
}

// Tag is one tag on a member.
type Tag struct {
	Name   string `json:"name"`
	Status string `json:"status"` // active | inactive
}

// MemberInput is the body of PUT /lists/{id}/members/{hash}.
type MemberInput struct {
	EmailAddress string            `json:"email_address"`
	StatusIfNew  string            `json:"status_if_new,omitempty"`
	Status       string            `json:"status,omitempty"`
	EmailType    string            `json:"email_type,omitempty"`
	MergeFields  map[string]string `json:"merge_fields,omitempty"`
	Tags         []string          `json:"tags,omitempty"`
	IPOpt        string            `json:"ip_opt,omitempty"`
	TimestampOpt string            `json:"timestamp_opt,omitempty"`
}

// Member is what Mailchimp returns.
type Member struct {
	ID                string     `json:"id"`
	EmailAddress      string     `json:"email_address"`
	UniqueEmailID     string     `json:"unique_email_id"`
	ContactID         string     `json:"contact_id"`
	WebID             int64      `json:"web_id"`
	Status            string     `json:"status"`
	UnsubscribeReason string     `json:"unsubscribe_reason"`
	LastChanged       *time.Time `json:"last_changed"`
	TimestampOpt      *time.Time `json:"timestamp_opt"`
	Source            string     `json:"source"`
	ListID            string     `json:"list_id"`
}

// ListMembersInput filters GET /lists/{id}/members.
type ListMembersInput struct {
	Count            int
	Offset           int
	SinceLastChanged *time.Time
	Status           string
}

// MemberPage is one page of members.
type MemberPage struct {
	Members    []Member `json:"members"`
	TotalItems int      `json:"total_items"`
}

// WebhookInput is the body of POST /lists/{id}/webhooks.
type WebhookInput struct {
	URL     string          `json:"url"`
	Events  map[string]bool `json:"events"`
	Sources map[string]bool `json:"sources"`
}

// Webhook is what Mailchimp returns; SigningSecret is only present on creation.
type Webhook struct {
	ID            string          `json:"id"`
	URL           string          `json:"url"`
	ListID        string          `json:"list_id"`
	Events        map[string]bool `json:"events"`
	Sources       map[string]bool `json:"sources"`
	SigningSecret string          `json:"signing_secret"`
}

// Client is everything the module asks Mailchimp for.
type Client interface {
	Ping(ctx context.Context) (Account, error)
	ListAudiences(ctx context.Context) ([]Audience, error)
	GetAudience(ctx context.Context, listID string) (Audience, error)
	UpsertMember(ctx context.Context, listID string, in MemberInput) (Member, error)
	GetMember(ctx context.Context, listID, hash string) (Member, error)
	ArchiveMember(ctx context.Context, listID, hash string) error
	ListMembers(ctx context.Context, listID string, in ListMembersInput) (MemberPage, error)
	AddTags(ctx context.Context, listID, hash string, tags []Tag) error
	CreateWebhook(ctx context.Context, listID string, in WebhookInput) (Webhook, error)
	DeleteWebhook(ctx context.Context, listID, webhookID string) error
	ListWebhooks(ctx context.Context, listID string) ([]Webhook, error)
}

// Factory builds a client from a stored source row, decrypting the key.
type Factory interface {
	For(ctx context.Context, source dbgen.Source) (Client, error)
}
