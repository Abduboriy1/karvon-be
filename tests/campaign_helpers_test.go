package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/webhook"
)

/* ---------------------------------------------------------------- payloads */

type campaignPayload struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	Status              string  `json:"status"`
	Steps               int     `json:"steps"`
	WeightsVersion      int     `json:"weights_version"`
	InstantlyCampaignID *string `json:"instantly_campaign_id"`
	LeadsTotal          int     `json:"leads_total"`
	LeadsPushed         int     `json:"leads_pushed"`
	Contacted           int64   `json:"contacted"`
	Replied             int64   `json:"replied"`
	Interested          int64   `json:"interested"`
	Bounced             int64   `json:"bounced"`
	Unsubscribed        int64   `json:"unsubscribed"`
	Eligible            int64   `json:"eligible"`
	Subscribed          int64   `json:"subscribed"`
	Error               *string `json:"error"`
}

type checklistPayload struct {
	Ready bool `json:"ready"`
	Items []struct {
		Key      string `json:"key"`
		OK       bool   `json:"ok"`
		Blocking bool   `json:"blocking"`
		Message  string `json:"message"`
	} `json:"items"`
}

type campaignLeadPayload struct {
	ID              string   `json:"id"`
	ContactID       string   `json:"contact_id"`
	Email           string   `json:"email"`
	Status          string   `json:"status"`
	LifecycleStage  string   `json:"lifecycle_stage"`
	InstantlyLeadID *string  `json:"instantly_lead_id"`
	OpenCount       int      `json:"open_count"`
	ReplyCount      int      `json:"reply_count"`
	PushAttempts    int      `json:"push_attempts"`
	LastPushError   *string  `json:"last_push_error"`
	VariantNames    []string `json:"variant_names"`
}

type campaignLeadListPayload struct {
	Data []campaignLeadPayload `json:"data"`
	Meta struct {
		Total int64 `json:"total"`
	} `json:"meta"`
}

type assignmentPayload struct {
	ID              string  `json:"id"`
	Step            int     `json:"step"`
	VariantID       string  `json:"variant_id"`
	WeightsVersion  int     `json:"weights_version"`
	RenderedSubject string  `json:"rendered_subject"`
	RenderedBody    string  `json:"rendered_body"`
	LockedAt        *string `json:"locked_at"`
}

type sendPayload struct {
	ID                  string  `json:"id"`
	Step                int     `json:"step"`
	VariantID           *string `json:"variant_id"`
	AssignmentID        *string `json:"assignment_id"`
	Source              string  `json:"source"`
	SentAt              string  `json:"sent_at"`
	FirstOpenedAt       *string `json:"first_opened_at"`
	RepliedAt           *string `json:"replied_at"`
	BouncedAt           *string `json:"bounced_at"`
	ReplyClassification *string `json:"reply_classification"`
}

type contactEventPayload struct {
	ID         int64   `json:"id"`
	Type       string  `json:"type"`
	OccurredAt string  `json:"occurred_at"`
	Source     string  `json:"source"`
	StageAfter *string `json:"stage_after"`
	Step       *int    `json:"step"`
	VariantID  *string `json:"variant_id"`
}

type leadDetailPayload struct {
	campaignLeadPayload
	Assignments []assignmentPayload   `json:"assignments"`
	Sends       []sendPayload         `json:"sends"`
	Events      []contactEventPayload `json:"events"`
}

type contactPayload struct {
	ID                string  `json:"id"`
	Email             string  `json:"email"`
	LifecycleStage    string  `json:"lifecycle_stage"`
	SuppressedAt      *string `json:"suppressed_at"`
	SuppressionReason *string `json:"suppression_reason"`
	HasConsent        *bool   `json:"has_consent"`
}

type contactListPayload struct {
	Data []contactPayload `json:"data"`
	Meta struct {
		Total int64 `json:"total"`
	} `json:"meta"`
}

type contactDetailPayload struct {
	contactPayload
	Consents      []map[string]any `json:"consents"`
	Suppressions  []map[string]any `json:"suppressions"`
	Subscriptions []map[string]any `json:"subscriptions"`
	Leads         []map[string]any `json:"leads"`
}

type eventListPayload struct {
	Data []contactEventPayload `json:"data"`
	Meta struct {
		Total int64 `json:"total"`
	} `json:"meta"`
}

type componentPayload struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Body   string `json:"body"`
	Status string `json:"status"`
}

type variantPayload struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Step            int    `json:"step"`
	Status          string `json:"status"`
	SubjectTemplate string `json:"subject_template"`
	BodyTemplate    string `json:"body_template"`
}

// webhookAckPayload mirrors the endpoint's answer: the delivery was stored (or
// recognised as one we already hold), and processing happens afterwards.
type webhookAckPayload struct {
	Stored    bool   `json:"stored"`
	Duplicate bool   `json:"duplicate"`
	EventID   string `json:"event_id"`
}

// accepted reports whether the provider's delivery was taken.
func (a webhookAckPayload) accepted() bool { return a.Stored || a.Duplicate }

type pushResultPayload struct {
	Queued   []string `json:"queued"`
	Rejected []struct {
		ContactID string `json:"contact_id"`
		Reason    string `json:"reason"`
	} `json:"rejected"`
}

type subscriptionPayload struct {
	ID         string  `json:"id"`
	ContactID  string  `json:"contact_id"`
	Email      string  `json:"email"`
	Status     string  `json:"status"`
	SyncStatus string  `json:"sync_status"`
	LastError  *string `json:"last_error"`
}

type subscriptionListPayload struct {
	Data []subscriptionPayload `json:"data"`
	Meta struct {
		Total int64 `json:"total"`
	} `json:"meta"`
}

/* ----------------------------------------------------------------- setup */

// configureInstantly stores a key on the Instantly source and enables it.
func (h *harness) configureInstantly() {
	h.t.Helper()
	h.mustRequest(http.MethodPut, "/api/v1/sources/"+instantlySourceID,
		`{"api_key":"integration-instantly-key","enabled":true}`, http.StatusOK)
}

// configureMailchimp stores a key on the Mailchimp source and enables it.
func (h *harness) configureMailchimp() {
	h.t.Helper()
	h.mustRequest(http.MethodPut, "/api/v1/sources/"+mailchimpSourceID,
		`{"api_key":"integration-mailchimp-key-us1","enabled":true}`, http.StatusOK)
}

// seedSendingAccount puts one mailbox in the Instantly fake and mirrors it.
func (h *harness) seedSendingAccount(email string) string {
	h.t.Helper()
	h.instantly.Accounts = append(h.instantly.Accounts, instantlyAccount(email))
	h.mustRequest(http.MethodPost, "/api/v1/sending-accounts/sync", "", http.StatusAccepted)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/sending-accounts", "", http.StatusOK)
		var accounts []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil {
			h.t.Fatalf("could not decode the sending accounts: %v", err)
		}
		for _, a := range accounts {
			if strings.EqualFold(a.Email, email) {
				return a.ID
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("the sending account %s never appeared", email)
	return ""
}

// seedContent creates a subject, a hook and a CTA, assembles them into a variant
// and approves everything, which is what a campaign needs before it can launch.
func (h *harness) seedContent(name, subject string) string {
	h.t.Helper()
	component := func(componentType, componentName, body string) string {
		rec := h.mustRequest(http.MethodPost, "/api/v1/content/components",
			fmt.Sprintf(`{"type":%q,"name":%q,"body":%q}`, componentType, componentName, body), http.StatusCreated)
		id := decodeBody[componentPayload](h.t, rec).ID
		for _, status := range []string{campaign.ContentReviewed, campaign.ContentApproved} {
			h.mustRequest(http.MethodPost, "/api/v1/content/components/"+id+"/status",
				fmt.Sprintf(`{"status":%q}`, status), http.StatusOK)
		}
		return id
	}
	subjectID := component(campaign.ComponentSubject, name+" subject", subject)
	hookID := component(campaign.ComponentHook, name+" hook", "Hi {{first_name|there}}, noticed {{company|your gym}} runs busy evenings.")
	ctaID := component(campaign.ComponentCTA, name+" cta", "Worth a 15-minute call next week?")

	body := fmt.Sprintf(`{"name":%q,"step":1,"components":[
		{"slot":"subject","component_id":%q},
		{"slot":"hook","component_id":%q},
		{"slot":"cta","component_id":%q}]}`, name, subjectID, hookID, ctaID)
	rec := h.mustRequest(http.MethodPost, "/api/v1/content/variants", body, http.StatusCreated)
	variantID := decodeBody[variantPayload](h.t, rec).ID
	for _, status := range []string{campaign.ContentReviewed, campaign.ContentApproved} {
		h.mustRequest(http.MethodPost, "/api/v1/content/variants/"+variantID+"/status",
			fmt.Sprintf(`{"status":%q}`, status), http.StatusOK)
	}
	return variantID
}

// createCampaign creates a draft, attaches one sending account and one variant.
func (h *harness) createCampaign(name string, accountID string, variantIDs ...string) campaignPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodPost, "/api/v1/campaigns",
		fmt.Sprintf(`{"name":%q,"steps":1}`, name), http.StatusCreated)
	camp := decodeBody[campaignPayload](h.t, rec)

	h.mustRequest(http.MethodPut, "/api/v1/campaigns/"+camp.ID+"/sending-accounts",
		fmt.Sprintf(`{"sending_account_ids":[%q]}`, accountID), http.StatusOK)

	weight := 100 / len(variantIDs)
	items := make([]string, 0, len(variantIDs))
	for i, id := range variantIDs {
		w := weight
		if i == 0 {
			w = 100 - weight*(len(variantIDs)-1)
		}
		items = append(items, fmt.Sprintf(`{"variant_id":%q,"step":1,"weight":%d}`, id, w))
	}
	h.mustRequest(http.MethodPut, "/api/v1/campaigns/"+camp.ID+"/variants",
		`{"items":[`+strings.Join(items, ",")+`]}`, http.StatusOK)
	return camp
}

// importLeads brings the scraped businesses into a campaign.
func (h *harness) importLeads(campaignID string, filter string) map[string]any {
	h.t.Helper()
	if filter == "" {
		filter = `{"primary_only":true}`
	}
	rec := h.mustRequest(http.MethodPost, "/api/v1/campaigns/"+campaignID+"/leads/import",
		`{"filter":`+filter+`}`, http.StatusOK)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("could not decode the import result: %v", err)
	}
	return out
}

// estimateImport asks what the same import would do, without doing it.
func (h *harness) estimateImport(campaignID string, filter string) map[string]any {
	h.t.Helper()
	if filter == "" {
		filter = `{"primary_only":true}`
	}
	rec := h.mustRequest(http.MethodPost, "/api/v1/campaigns/"+campaignID+"/leads/import/estimate",
		`{"filter":`+filter+`}`, http.StatusOK)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("could not decode the estimate: %v", err)
	}
	return out
}

// launchCampaign launches and waits until the campaign settles.
func (h *harness) launchCampaign(id string) campaignPayload {
	h.t.Helper()
	h.mustRequest(http.MethodPost, "/api/v1/campaigns/"+id+"/launch", "", http.StatusAccepted)
	return h.waitForCampaign(id, campaign.CampaignActive, campaign.CampaignFailed)
}

// waitForCampaign polls until the campaign reaches one of the wanted statuses.
func (h *harness) waitForCampaign(id string, wanted ...string) campaignPayload {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last campaignPayload
	for time.Now().Before(deadline) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/campaigns/"+id, "", http.StatusOK)
		last = decodeBody[campaignPayload](h.t, rec)
		for _, want := range wanted {
			if last.Status == want {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("campaign %s never reached %v, last state: %+v", id, wanted, last)
	return last
}

// leads lists a campaign's leads.
func (h *harness) leads(campaignID string) []campaignLeadPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/campaigns/"+campaignID+"/leads?per_page=200", "", http.StatusOK)
	return decodeBody[campaignLeadListPayload](h.t, rec).Data
}

// leadByEmail finds one lead of a campaign.
func (h *harness) leadByEmail(campaignID, email string) campaignLeadPayload {
	h.t.Helper()
	for _, lead := range h.leads(campaignID) {
		if strings.EqualFold(lead.Email, email) {
			return lead
		}
	}
	h.t.Fatalf("campaign %s has no lead for %s", campaignID, email)
	return campaignLeadPayload{}
}

// leadDetail loads one lead with its assignments, sends and timeline.
func (h *harness) leadDetail(campaignID, leadID string) leadDetailPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/campaigns/"+campaignID+"/leads/"+leadID, "", http.StatusOK)
	return decodeBody[leadDetailPayload](h.t, rec)
}

// contactByEmail finds a contact by address.
func (h *harness) contactByEmail(email string) contactPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/contacts?q="+url.QueryEscape(email), "", http.StatusOK)
	for _, c := range decodeBody[contactListPayload](h.t, rec).Data {
		if strings.EqualFold(c.Email, email) {
			return c
		}
	}
	h.t.Fatalf("no contact for %s", email)
	return contactPayload{}
}

// contactDetail loads one contact in full.
func (h *harness) contactDetail(id string) contactDetailPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/contacts/"+id, "", http.StatusOK)
	return decodeBody[contactDetailPayload](h.t, rec)
}

// timeline reads a contact's events, oldest first.
func (h *harness) timeline(contactID string) []contactEventPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/contacts/"+contactID+"/timeline?per_page=200", "", http.StatusOK)
	return decodeBody[eventListPayload](h.t, rec).Data
}

/* --------------------------------------------------------------- webhooks */

// registerInstantlyWebhook registers the workspace webhook and returns the path
// token and the secret Instantly was asked to send back.
func (h *harness) registerInstantlyWebhook() (token, secret string) {
	h.t.Helper()
	h.mustRequest(http.MethodPost, "/api/v1/integrations/instantly/webhook", `{}`, http.StatusOK)

	for _, req := range h.instantly.Requests() {
		if req.Method != "CreateWebhook" {
			continue
		}
		raw, err := json.Marshal(req.Input)
		if err != nil {
			h.t.Fatalf("could not re-encode the webhook request: %v", err)
		}
		var in struct {
			TargetHookURL string            `json:"target_hook_url"`
			Headers       map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			h.t.Fatalf("could not decode the webhook request: %v", err)
		}
		parts := strings.Split(strings.TrimSuffix(in.TargetHookURL, "/"), "/")
		token = parts[len(parts)-1]
		secret = in.Headers[webhook.SecretHeader]
	}
	if token == "" || secret == "" {
		h.t.Fatal("the Instantly webhook was not registered with a token and a secret")
	}
	return token, secret
}

// sendInstantlyWebhook posts one event to the Instantly webhook endpoint.
func (h *harness) sendInstantlyWebhook(token, secret string, payload map[string]any) webhookAckPayload {
	h.t.Helper()
	rec := h.postWebhook("/api/v1/webhooks/instantly/"+token, secret, payload, http.StatusAccepted)
	return decodeBody[webhookAckPayload](h.t, rec)
}

func (h *harness) postWebhook(path, secret string, payload map[string]any, wantStatus int) *httpRecorder {
	h.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("could not encode the webhook payload: %v", err)
	}
	req := newRequest(http.MethodPost, path, string(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set(webhook.SecretHeader, secret)
	}
	rec := newRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		h.t.Fatalf("POST %s = %d, want %d: %s", path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec
}

// instantlyEvent builds a webhook payload in Instantly's documented shape.
func instantlyEvent(eventType, instantlyCampaignID, leadEmail string, at time.Time, extra map[string]any) map[string]any {
	payload := map[string]any{
		"event_type":    eventType,
		"timestamp":     at.UTC().Format(time.RFC3339Nano),
		"campaign_id":   instantlyCampaignID,
		"campaign_name": "Integration campaign",
		"lead_email":    leadEmail,
		"email_account": "sender@karvon.test",
		"step":          1,
		"variant":       1,
	}
	for k, v := range extra {
		payload[k] = v
	}
	return payload
}

// campaignVariants lists the variant ids attached to a campaign.
func (h *harness) campaignVariants(t *testing.T, campaignID string) []string {
	t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/campaigns/"+campaignID+"/variants", "", http.StatusOK)
	var rows []struct {
		VariantID string `json:"variant_id"`
	}
	decodeInto(t, rec, &rows)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.VariantID)
	}
	return out
}

// seedAudience puts one audience in the Mailchimp fake and mirrors it.
func (h *harness) seedAudience(t *testing.T) string {
	t.Helper()
	h.mailchimp.Audiences = append(h.mailchimp.Audiences, mailchimpAudience("list-1", "Karvon newsletter"))
	h.mustRequest(http.MethodPost, "/api/v1/newsletter/audiences/sync", "", http.StatusAccepted)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/newsletter/audiences", "", http.StatusOK)
		var audiences []struct {
			ID              string `json:"id"`
			MailchimpListID string `json:"mailchimp_list_id"`
		}
		decodeInto(t, rec, &audiences)
		for _, a := range audiences {
			if a.MailchimpListID == "list-1" {
				return a.ID
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the audience never appeared")
	return ""
}

// mailchimpWebhookCredentials reads back the token and signing secret of the
// webhook we registered with the Mailchimp fake.
func (h *harness) mailchimpWebhookCredentials(t *testing.T, audienceID string) (token, secret string) {
	t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/newsletter/audiences", "", http.StatusOK)
	var audiences []struct {
		ID         string  `json:"id"`
		WebhookURL *string `json:"webhook_url"`
	}
	decodeInto(t, rec, &audiences)
	for _, a := range audiences {
		if a.ID == audienceID && a.WebhookURL != nil {
			parts := strings.Split(strings.TrimSuffix(*a.WebhookURL, "/"), "/")
			token = parts[len(parts)-1]
		}
	}
	if token == "" {
		t.Fatal("the Mailchimp webhook has no URL")
	}
	return token, h.mailchimp.SigningSecret
}

// decodeInto unmarshals a recorder body into dst.
func decodeInto(t *testing.T, rec *httpRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("could not decode the response: %v (body: %s)", err, rec.Body.String())
	}
}

// checklist reads a campaign's launch gate.
func (h *harness) checklist(t *testing.T, campaignID string) checklistPayload {
	t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/campaigns/"+campaignID+"/checklist", "", http.StatusOK)
	return decodeBody[checklistPayload](t, rec)
}

// draftVariant creates a variant whose components were never approved.
func (h *harness) draftVariant(t *testing.T, name string) string {
	t.Helper()
	rec := h.mustRequest(http.MethodPost, "/api/v1/content/components",
		fmt.Sprintf(`{"type":"subject","name":%q,"body":"Draft subject"}`, name), http.StatusCreated)
	subjectID := decodeBody[componentPayload](t, rec).ID

	rec = h.mustRequest(http.MethodPost, "/api/v1/content/variants",
		fmt.Sprintf(`{"name":%q,"step":1,"components":[{"slot":"subject","component_id":%q}]}`, name, subjectID),
		http.StatusCreated)
	return decodeBody[variantPayload](t, rec).ID
}
