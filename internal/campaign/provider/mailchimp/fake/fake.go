// Package fake is an in-memory mailchimp.Client for tests. It keeps audiences,
// members, tags and webhooks in maps, records every call, and can be told to fail
// a method on its next call.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// Request is one recorded call.
type Request struct {
	Method string
	Input  any
}

// Client is the fake. Exported fields may be set before use; after the first call
// they are guarded by the mutex and should be read through the methods.
type Client struct {
	mu sync.Mutex

	// Fail queues errors per method name ("UpsertMember"); each call pops one.
	Fail map[string][]error
	// Account is what Ping returns.
	Account mailchimp.Account
	// Audiences is what ListAudiences returns and GetAudience searches.
	Audiences []mailchimp.Audience
	// Members is listID → subscriber hash → member.
	Members map[string]map[string]mailchimp.Member
	// Tags is listID → subscriber hash → tags added through AddTags.
	Tags map[string]map[string][]mailchimp.Tag
	// ComplianceEmails lists lower-cased addresses whose upsert is refused with
	// provider.ErrComplianceState, as Mailchimp does for unsubscribed or bounced
	// members.
	ComplianceEmails map[string]bool
	// AutoConfirmPending stores a member upserted as pending as subscribed, as if
	// the person clicked the confirmation email.
	AutoConfirmPending bool
	// Webhooks is listID → webhooks.
	Webhooks map[string][]mailchimp.Webhook
	// SigningSecret is stamped on every created webhook.
	SigningSecret string
	// Now is the clock for LastChanged.
	Now func() time.Time

	requests []Request
	seq      int64
}

// New builds an empty fake.
func New() *Client {
	return &Client{
		Fail:             make(map[string][]error),
		Account:          mailchimp.Account{AccountName: "Fake Account", Email: "owner@example.com", DC: "us1"},
		Members:          make(map[string]map[string]mailchimp.Member),
		Tags:             make(map[string]map[string][]mailchimp.Tag),
		ComplianceEmails: make(map[string]bool),
		Webhooks:         make(map[string][]mailchimp.Webhook),
		SigningSecret:    "fake-signing-secret",
		Now:              time.Now,
	}
}

// Requests returns a copy of every recorded call, in order.
func (c *Client) Requests() []Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Request, len(c.requests))
	copy(out, c.requests)
	return out
}

// Reset forgets recorded calls and queued failures but keeps the data.
func (c *Client) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = nil
	c.Fail = make(map[string][]error)
}

// begin records the call and pops a queued failure. Callers hold the mutex.
func (c *Client) begin(method string, input any) error {
	c.requests = append(c.requests, Request{Method: method, Input: input})
	queue := c.Fail[method]
	if len(queue) == 0 {
		return nil
	}
	err := queue[0]
	c.Fail[method] = queue[1:]
	return err
}

func (c *Client) next() int64 {
	c.seq++
	return c.seq
}

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// Ping implements mailchimp.Client.
func (c *Client) Ping(context.Context) (mailchimp.Account, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("Ping", nil); err != nil {
		return mailchimp.Account{}, err
	}
	return c.Account, nil
}

// ListAudiences implements mailchimp.Client.
func (c *Client) ListAudiences(context.Context) ([]mailchimp.Audience, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListAudiences", nil); err != nil {
		return nil, err
	}
	out := make([]mailchimp.Audience, len(c.Audiences))
	copy(out, c.Audiences)
	return out, nil
}

// GetAudience implements mailchimp.Client.
func (c *Client) GetAudience(_ context.Context, listID string) (mailchimp.Audience, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetAudience", listID); err != nil {
		return mailchimp.Audience{}, err
	}
	for _, a := range c.Audiences {
		if a.ID == listID {
			return a, nil
		}
	}
	return mailchimp.Audience{}, fmt.Errorf("fake mailchimp: list %q: %w", listID, provider.ErrNotFound)
}

// UpsertMember implements mailchimp.Client.
func (c *Client) UpsertMember(_ context.Context, listID string, in mailchimp.MemberInput) (mailchimp.Member, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("UpsertMember", in); err != nil {
		return mailchimp.Member{}, err
	}
	email := strings.ToLower(strings.TrimSpace(in.EmailAddress))
	if c.ComplianceEmails[email] {
		return mailchimp.Member{}, fmt.Errorf("fake mailchimp: %s: %w", email, provider.ErrComplianceState)
	}

	hash := mailchimp.SubscriberHash(in.EmailAddress)
	list := c.members(listID)
	now := c.now()

	m, exists := list[hash]
	if exists {
		if in.Status != "" {
			m.Status = in.Status
		}
	} else {
		n := c.next()
		status := in.StatusIfNew
		if status == "" {
			status = in.Status
		}
		if status == "" {
			status = mailchimp.StatusSubscribed
		}
		m = mailchimp.Member{
			ID:            hash,
			EmailAddress:  in.EmailAddress,
			UniqueEmailID: fmt.Sprintf("ue_%d", n),
			ContactID:     fmt.Sprintf("c_%d", n),
			WebID:         n,
			Status:        status,
			Source:        "API - Generic",
			ListID:        listID,
		}
		if in.TimestampOpt != "" {
			if at, err := time.Parse(time.RFC3339, in.TimestampOpt); err == nil {
				m.TimestampOpt = &at
			}
		}
	}
	if c.AutoConfirmPending && m.Status == mailchimp.StatusPending {
		m.Status = mailchimp.StatusSubscribed
	}
	m.LastChanged = &now
	list[hash] = m
	return m, nil
}

// GetMember implements mailchimp.Client.
func (c *Client) GetMember(_ context.Context, listID, hash string) (mailchimp.Member, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetMember", hash); err != nil {
		return mailchimp.Member{}, err
	}
	m, ok := c.Members[listID][hash]
	if !ok {
		return mailchimp.Member{}, fmt.Errorf("fake mailchimp: member %q: %w", hash, provider.ErrNotFound)
	}
	return m, nil
}

// ArchiveMember implements mailchimp.Client.
func (c *Client) ArchiveMember(_ context.Context, listID, hash string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ArchiveMember", hash); err != nil {
		return err
	}
	m, ok := c.Members[listID][hash]
	if !ok {
		return fmt.Errorf("fake mailchimp: member %q: %w", hash, provider.ErrNotFound)
	}
	now := c.now()
	m.Status = mailchimp.StatusArchived
	m.LastChanged = &now
	c.Members[listID][hash] = m
	return nil
}

// ListMembers implements mailchimp.Client. Members are ordered by hash so pages
// are stable.
func (c *Client) ListMembers(_ context.Context, listID string, in mailchimp.ListMembersInput) (mailchimp.MemberPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListMembers", in); err != nil {
		return mailchimp.MemberPage{}, err
	}
	var all []mailchimp.Member
	for _, m := range c.Members[listID] {
		if in.Status != "" && m.Status != in.Status {
			continue
		}
		if in.SinceLastChanged != nil && (m.LastChanged == nil || m.LastChanged.Before(*in.SinceLastChanged)) {
			continue
		}
		all = append(all, m)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	page := mailchimp.MemberPage{TotalItems: len(all), Members: []mailchimp.Member{}}
	start := in.Offset
	if start < 0 {
		start = 0
	}
	if start > len(all) {
		start = len(all)
	}
	end := len(all)
	if in.Count > 0 && start+in.Count < end {
		end = start + in.Count
	}
	page.Members = append(page.Members, all[start:end]...)
	return page, nil
}

// AddTags implements mailchimp.Client.
func (c *Client) AddTags(_ context.Context, listID, hash string, tags []mailchimp.Tag) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("AddTags", tags); err != nil {
		return err
	}
	if _, ok := c.Members[listID][hash]; !ok {
		return fmt.Errorf("fake mailchimp: member %q: %w", hash, provider.ErrNotFound)
	}
	if c.Tags[listID] == nil {
		c.Tags[listID] = make(map[string][]mailchimp.Tag)
	}
	c.Tags[listID][hash] = append(c.Tags[listID][hash], tags...)
	return nil
}

// CreateWebhook implements mailchimp.Client.
func (c *Client) CreateWebhook(_ context.Context, listID string, in mailchimp.WebhookInput) (mailchimp.Webhook, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("CreateWebhook", in); err != nil {
		return mailchimp.Webhook{}, err
	}
	w := mailchimp.Webhook{
		ID:            fmt.Sprintf("wh_%d", c.next()),
		URL:           in.URL,
		ListID:        listID,
		Events:        in.Events,
		Sources:       in.Sources,
		SigningSecret: c.SigningSecret,
	}
	c.Webhooks[listID] = append(c.Webhooks[listID], w)
	return w, nil
}

// DeleteWebhook implements mailchimp.Client.
func (c *Client) DeleteWebhook(_ context.Context, listID, webhookID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("DeleteWebhook", webhookID); err != nil {
		return err
	}
	hooks := c.Webhooks[listID]
	for i, w := range hooks {
		if w.ID == webhookID {
			c.Webhooks[listID] = append(hooks[:i:i], hooks[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("fake mailchimp: webhook %q: %w", webhookID, provider.ErrNotFound)
}

// ListWebhooks implements mailchimp.Client. As with Mailchimp, the signing secret
// is only reported on creation.
func (c *Client) ListWebhooks(_ context.Context, listID string) ([]mailchimp.Webhook, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListWebhooks", listID); err != nil {
		return nil, err
	}
	out := make([]mailchimp.Webhook, 0, len(c.Webhooks[listID]))
	for _, w := range c.Webhooks[listID] {
		w.SigningSecret = ""
		out = append(out, w)
	}
	return out, nil
}

// members returns the map for a list, creating it. Callers hold the mutex.
func (c *Client) members(listID string) map[string]mailchimp.Member {
	list := c.Members[listID]
	if list == nil {
		list = make(map[string]mailchimp.Member)
		c.Members[listID] = list
	}
	return list
}

// StaticFactory hands out one client (or one error) whatever the source row says.
type StaticFactory struct {
	Client mailchimp.Client
	Err    error
}

// For implements mailchimp.Factory.
func (f StaticFactory) For(context.Context, dbgen.Source) (mailchimp.Client, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Client, nil
}

// compile-time proof that the fakes satisfy the interfaces.
var (
	_ mailchimp.Client  = (*Client)(nil)
	_ mailchimp.Factory = StaticFactory{}
)
