package fbscrape

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc, apiKey string) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return New(Config{BaseURL: server.URL + "/", APIKey: apiKey, Timeout: 2 * time.Second, HTTPClient: server.Client()})
}

func TestScrapeSendsOnePageAndDecodesTheResult(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/scrape" {
			t.Errorf("request = %s %s, want POST /scrape", r.Method, r.URL.Path)
		}
		if got := r.Header.Get(APIKeyHeader); got != "secret" {
			t.Errorf("%s = %q, want the configured key", APIKeyHeader, got)
		}
		var body struct {
			Pages []string `json:"pages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(body.Pages) != 1 || body.Pages[0] != "https://www.facebook.com/ironworksgym" {
			t.Errorf("pages = %v", body.Pages)
		}
		_, _ = io.WriteString(w, `{"results":[{"url":"https://www.facebook.com/ironworksgym","name":"Iron Works",
			"category":"Gym","email":"hello@ironworks.com","phone":"(512) 555-0100","website":null,"lines":["Gym"]}]}`)
	}, "secret")

	page, err := client.Scrape(context.Background(), "https://www.facebook.com/ironworksgym")
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if page.Email != "hello@ironworks.com" || page.Phone != "(512) 555-0100" || page.Website != "" || page.Error != "" {
		t.Errorf("page = %+v", page)
	}
}

func TestScrapeReturnsAnUnreadablePageAsData(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"url":"https://www.facebook.com/x","name":null,"error":"Details section not found"}]}`)
	}, "")

	page, err := client.Scrape(context.Background(), "https://www.facebook.com/x")
	if err != nil {
		t.Fatalf("an unreadable page must not be an error: %v", err)
	}
	if page.Error != "Details section not found" {
		t.Errorf("page.Error = %q", page.Error)
	}
}

func TestScrapeOmitsTheKeyHeaderWhenNoKeyIsSet(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if _, sent := r.Header[http.CanonicalHeaderKey(APIKeyHeader)]; sent {
			t.Errorf("%s was sent without a key configured", APIKeyHeader)
		}
		_, _ = io.WriteString(w, `{"results":[{"url":"u"}]}`)
	}, "")
	if _, err := client.Scrape(context.Background(), "u"); err != nil {
		t.Fatalf("Scrape: %v", err)
	}
}

func TestScrapeFailsWhenTheServiceDoes(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"rejected key", http.StatusUnauthorized, `{"detail":"invalid"}`, "API key"},
		{"server error", http.StatusInternalServerError, "<html>boom</html>", "answered 500"},
		{"not json", http.StatusOK, "hello", "decode response"},
		{"wrong result count", http.StatusOK, `{"results":[]}`, "expected 1 result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}, "")
			_, err := client.Scrape(context.Background(), "https://www.facebook.com/x")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestScrapeFailsWhenTheServiceIsDown(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()

	_, err := New(Config{BaseURL: url, Timeout: time.Second}).Scrape(context.Background(), "u")
	if err == nil {
		t.Fatal("expected an error from a service that is not listening")
	}
}
