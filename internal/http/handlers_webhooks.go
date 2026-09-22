package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/campaign/webhook"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/http/middleware"
)

// These three endpoints are the only ones the API key does not protect: they
// authenticate themselves from the path token and the provider's own signature. They
// are also the only ones a third party retries on our behalf, which shapes the status
// codes below: once a delivery is stored we answer 2xx whatever the rest of the
// pipeline later makes of it, so a slow or failing application never talks a provider
// into disabling the webhook. Nothing here logs a secret, a signature or a body.

// ReceiveInstantlyWebhook implements POST /webhooks/instantly/{token}.
func (s *Server) ReceiveInstantlyWebhook(w http.ResponseWriter, r *http.Request, token gen.WebhookTokenPath) {
	body, err := s.readWebhookBody(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	if err := s.campaigns.AuthenticateInstantlyWebhook(r.Context(), token, r.Header.Get(webhook.SecretHeader)); err != nil {
		writeWebhookAuthError(w, r, campaign.ProviderInstantly, len(token), err)
		return
	}

	// The body is stored verbatim and applied asynchronously. A payload that does not
	// parse is still stored, and a repeat delivery is recognised by its dedupe key, so
	// the only thing that can fail here is storage itself.
	result, err := s.campaigns.IngestInstantly(r.Context(), body, campaign.ProviderEventWebhook)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// 202: accepted for processing, not processed.
	writeJSON(w, r, http.StatusAccepted, gen.WebhookAck{
		Duplicate: result.Duplicate,
		EventId:   result.EventID,
		Stored:    result.Stored,
	})
}

// VerifyMailchimpWebhook implements GET /webhooks/mailchimp/{token}.
//
// Mailchimp probes the URL when the webhook is created and refuses to create it unless
// the probe answers 200 with an empty body. That happens before any secret exists, so
// there is nothing to authenticate and the token is deliberately not looked at: a
// lookup here would only turn the probe into an oracle for guessed tokens.
func (s *Server) VerifyMailchimpWebhook(w http.ResponseWriter, _ *http.Request, _ gen.WebhookTokenPath) {
	w.WriteHeader(http.StatusOK)
}

// ReceiveMailchimpWebhook implements POST /webhooks/mailchimp/{token}.
func (s *Server) ReceiveMailchimpWebhook(w http.ResponseWriter, r *http.Request, token gen.WebhookTokenPath) {
	// Read before parsing: the signature covers these exact bytes, and re-encoding a
	// parsed form would not reproduce them.
	body, err := s.readWebhookBody(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	audience, err := s.campaigns.AuthenticateMailchimpWebhook(r.Context(), token,
		r.Header.Get(webhook.MailchimpSignatureHeader), body, s.cfg.MailchimpWebhookTolerance)
	if err != nil {
		writeWebhookAuthError(w, r, campaign.ProviderMailchimp, len(token), err)
		return
	}

	// Parsed here rather than with r.ParseForm, which would find the body already
	// consumed by the read above and quietly hand back an empty form.
	form, err := url.ParseQuery(string(body))
	if err != nil {
		WriteError(w, r, apperr.BadRequest("the request body is not valid form data"))
		return
	}

	result, err := s.campaigns.IngestMailchimp(r.Context(), audience, form, campaign.ProviderEventWebhook)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, gen.WebhookAck{
		Duplicate: result.Duplicate,
		EventId:   result.EventID,
		Stored:    result.Stored,
	})
}

// readWebhookBody reads a provider delivery under the configured cap. The body is
// needed whole and unparsed because the signature is computed over exactly these bytes.
func (s *Server) readWebhookBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, apperr.BadRequest("a request body is required")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.WebhookMaxBodyBytes))
	if err != nil {
		return nil, apperr.BadRequest("the request body could not be read")
	}
	return body, nil
}

// writeWebhookAuthError maps the two authentication sentinels onto their responses and
// lets everything else through unchanged.
//
// An unknown token is a 404 rather than a 401 so it is indistinguishable from a path
// that was never a webhook: a prober learns nothing from the difference. Only the
// token's length is logged, never the token, the secret or the signature.
func writeWebhookAuthError(w http.ResponseWriter, r *http.Request, provider string, tokenLen int, err error) {
	log := middleware.LoggerFrom(r.Context())
	switch {
	case errors.Is(err, campaignsvc.ErrUnknownWebhookToken):
		log.Warn("webhook token not recognised", "provider", provider, "token_len", tokenLen)
		WriteError(w, r, apperr.NotFound("webhook"))
	case errors.Is(err, campaignsvc.ErrBadWebhookSecret):
		log.Warn("webhook signature rejected", "provider", provider, "token_len", tokenLen)
		WriteError(w, r, apperr.Unauthorized("%s", "the delivery could not be authenticated"))
	default:
		WriteError(w, r, err)
	}
}
