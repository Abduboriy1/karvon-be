package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDotenvLine(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantKey   string
		wantValue string
		wantOK    bool
	}{
		{name: "plain", line: "KARVON_ADDR=:8080", wantKey: "KARVON_ADDR", wantValue: ":8080", wantOK: true},
		{name: "export prefix", line: "export KARVON_ENV=prod", wantKey: "KARVON_ENV", wantValue: "prod", wantOK: true},
		{name: "double quoted", line: `KARVON_UA="Mozilla/5.0 (X)"`, wantKey: "KARVON_UA", wantValue: "Mozilla/5.0 (X)", wantOK: true},
		{name: "single quoted", line: "KARVON_UA='a b'", wantKey: "KARVON_UA", wantValue: "a b", wantOK: true},
		{name: "value with equals", line: "KARVON_DATABASE_URL=postgres://u:p@h/db?a=b", wantKey: "KARVON_DATABASE_URL", wantValue: "postgres://u:p@h/db?a=b", wantOK: true},
		{name: "comment", line: "# nothing here", wantOK: false},
		{name: "blank", line: "   ", wantOK: false},
		{name: "no separator", line: "KARVON_ADDR", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, value, ok := parseDotenvLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if key != tt.wantKey || value != tt.wantValue {
				t.Fatalf("got (%q, %q), want (%q, %q)", key, value, tt.wantKey, tt.wantValue)
			}
		})
	}
}

func TestLoadDotenvDoesNotOverrideRealEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	contents := "KARVON_TEST_EXISTING=from-file\nKARVON_TEST_NEW=from-file\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KARVON_TEST_EXISTING", "from-environment")

	if err := loadDotenv(path); err != nil {
		t.Fatal(err)
	}

	if got := os.Getenv("KARVON_TEST_EXISTING"); got != "from-environment" {
		t.Errorf("existing variable was overwritten: %q", got)
	}
	if got := os.Getenv("KARVON_TEST_NEW"); got != "from-file" {
		t.Errorf("new variable = %q, want from-file", got)
	}
	t.Cleanup(func() { _ = os.Unsetenv("KARVON_TEST_NEW") })
}

func TestLoadDotenvMissingFileIsNotAnError(t *testing.T) {
	if err := loadDotenv(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Fatalf("missing .env should be ignored, got %v", err)
	}
}

func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KARVON_DATABASE_URL", "postgres://karvon:karvon@localhost:5432/karvon")
	t.Setenv("KARVON_API_KEY", "dev-key")
	t.Setenv("KARVON_SECRET_KEY", strings.Repeat("a", 64))
}

func TestLoadAppliesDocumentedDefaults(t *testing.T) {
	setMinimalEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.CrawlConcurrency != 8 {
		t.Errorf("CrawlConcurrency = %d, want 8", cfg.CrawlConcurrency)
	}
	if cfg.CrawlTimeout != 10*time.Second {
		t.Errorf("CrawlTimeout = %s, want 10s", cfg.CrawlTimeout)
	}
	if cfg.MaxQueriesPerJob != 500 {
		t.Errorf("MaxQueriesPerJob = %d, want 500", cfg.MaxQueriesPerJob)
	}
	if !cfg.Workers {
		t.Error("Workers should default to true")
	}
	if cfg.IsProduction() {
		t.Error("development env should not report production")
	}
}

func TestValidateRejectsMissingSecrets(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{name: "no database url", mutate: func(c *Config) { c.DatabaseURL = "" }, wantSub: "DATABASE_URL"},
		{name: "no api key", mutate: func(c *Config) { c.APIKey = "" }, wantSub: "API_KEY"},
		{name: "no secret key", mutate: func(c *Config) { c.SecretKey = "" }, wantSub: "SECRET_KEY"},
		{
			name:    "short api key in production",
			mutate:  func(c *Config) { c.Env = "production"; c.APIKey = "short" },
			wantSub: "at least 16 characters",
		},
		{name: "crawl concurrency too high", mutate: func(c *Config) { c.CrawlConcurrency = 999 }, wantSub: "CRAWL_CONCURRENCY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.DatabaseURL = "postgres://localhost/karvon"
			cfg.APIKey = "dev-key"
			cfg.SecretKey = strings.Repeat("a", 64)
			tt.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

func TestValidateRejectsThePlaceholderSecretInProduction(t *testing.T) {
	cfg := Defaults()
	cfg.Env = "production"
	cfg.DatabaseURL = "postgres://localhost/karvon"
	cfg.APIKey = strings.Repeat("k", 32)
	cfg.SecretKey = strings.Repeat("0", 64)

	err := cfg.Validate()
	if err == nil {
		t.Fatal("the example key must be rejected in production")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("error = %q, want it to explain the problem", err)
	}

	// The same key is fine in development, where compose ships with it.
	cfg.Env = "development"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("development should accept the placeholder: %v", err)
	}
}

// Defaults must be a valid configuration once the three required secrets are set;
// otherwise a fresh deployment cannot start.
func TestDefaultsAreValid(t *testing.T) {
	cfg := Defaults()
	cfg.DatabaseURL = "postgres://localhost/karvon"
	cfg.APIKey = "dev-key"
	cfg.SecretKey = strings.Repeat("a", 64)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the shipped defaults do not validate: %v", err)
	}
	if cfg.VerifyPass2MinScore != 50 {
		t.Errorf("the third-party gate defaults to %d, want 50", cfg.VerifyPass2MinScore)
	}
	if cfg.VerifyRoleSoftMode != "hard" {
		t.Errorf("the role mode defaults to %q, want \"hard\"", cfg.VerifyRoleSoftMode)
	}
}
