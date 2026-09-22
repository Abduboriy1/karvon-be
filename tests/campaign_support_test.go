package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp"
)

// httpRecorder is the recorder type the webhook helpers hand back.
type httpRecorder = httptest.ResponseRecorder

func newRecorder() *httpRecorder { return httptest.NewRecorder() }

// newRequest builds an unauthenticated request; the webhook routes carry their
// own credentials, so no API key is attached.
func newRequest(method, target, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	return httptest.NewRequest(method, target, reader)
}

// instantlyAccount is one mailbox as the Instantly fake reports it.
func instantlyAccount(email string) instantly.Account {
	code, status, warmup, limit := 2, instantly.AccountStatusActive, 1, 50
	return instantly.Account{
		Email: email, FirstName: "Sender", LastName: "One",
		ProviderCode: &code, Status: status, WarmupStatus: &warmup, DailyLimit: &limit,
		Raw: map[string]any{"email": email},
	}
}

// mailchimpAudience is one audience as the Mailchimp fake reports it.
func mailchimpAudience(listID, name string) mailchimp.Audience {
	a := mailchimp.Audience{ID: listID, WebID: 1, Name: name, DoubleOptin: true}
	a.Stats.MemberCount = 0
	return a
}

// instantlySentEmail is one sent email as the Instantly unibox reports it.
func instantlySentEmail(campaignID, leadEmail, account string, at time.Time) instantly.Email {
	return instantly.Email{
		ID: "ie_" + leadEmail, MessageID: "<" + leadEmail + ">", CampaignID: campaignID,
		LeadEmail: leadEmail, EAccount: account, Subject: "Quick question", Step: "1",
		UEType: instantly.EmailTypeSentFromCampaign, TimestampEmail: at.UTC(), TimestampCreated: at.UTC(),
	}
}

// jsonString encodes a value for a request body.
func jsonString(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
