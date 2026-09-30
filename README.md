# karvon-be

Backend for the **Karvon** scraper module: a Go service that turns search terms ×
locations into a deduplicated master list of businesses and contact email addresses.

One binary runs both halves of the system: a REST API for the dashboard
(`karvon-fe`) and a Postgres-backed job queue that executes scrapes. Live job
progress reaches the browser over Server-Sent Events.

- REST + JSON under `/api/v1`, SSE at `/api/v1/jobs/{id}/events`
- Spec-first: `api/openapi.yaml` is the contract, served at `/api/v1/openapi.json`
- IDs are UUIDv7 strings, timestamps are RFC 3339 **UTC**
- Errors are always `{ "error": { "code", "message", "details"? } }`
- Pagination is `?page=1&per_page=50` → `{ "data": [], "meta": { page, per_page, total } }`
- Single static API key in `Authorization: Bearer <key>`

---

## Contents

- [Architecture](#architecture)
- [Folder structure](#folder-structure)
- [Setup](#setup)
- [Environment variables](#environment-variables)
- [Database and migrations](#database-and-migrations)
- [Development commands](#development-commands)
- [Tests](#tests)
- [Build and run](#build-and-run)
- [API overview](#api-overview)
- [Scrape pipeline](#scrape-pipeline)
- [Email verification](#email-verification)
- [Campaigns](#campaigns)
- [Domains](#domains)
- [Mailboxes](#mailboxes)
- [Frontend coverage](#frontend-coverage)
- [Decisions and deviations from the plan](#decisions-and-deviations-from-the-plan)

---

## Architecture

```
                 ┌───────────────────────────── one process ─────────────────────────────┐
  karvon-fe ───▶ │  chi router → middleware → handlers → services → repositories → pgx   │
   (browser)     │        │                                    │                          │
       ▲         │        └── SSE handler ◀── LISTEN ──┐       └── River workers          │
       │         └───────────────────────────────────────────────────────────────────────┘
       │                                              │                  │
       └──────────── text/event-stream ───────────────┴── NOTIFY ────────┴──▶ Postgres 16
```

Responsibilities are separated by layer, and each layer only knows the one below it:

| Layer | Package | Responsibility |
| --- | --- | --- |
| Transport | `internal/http` | HTTP only: decode, validate shape, call a service, render the response. No business rules. |
| Middleware | `internal/http/middleware` | Request id, access log, panic recovery, CORS, API-key auth, request timeout. |
| Services | `internal/scraper`, `internal/business`, `internal/source`, `internal/stats` | Business rules, validation, orchestration, authorization decisions. |
| Data access | `internal/db` (+ generated `internal/db/dbgen`) | SQL, transactions, dynamic filter/sort queries, streaming reads. |
| Workers | `internal/scraper/jobs` | The four River job kinds that execute a scrape. |
| Integrations | `internal/scraper/provider`, `internal/scraper/crawler` | Apify / Outscraper clients and the website email crawler. |
| Wiring | `internal/app` | Builds the whole graph; `cmd/api` is a thin shell around it. |

The HTTP layer depends on **interfaces** (`internal/http/services.go`), not on the
concrete services, so handlers are tested with stubs and no database.

**Code generation.** Two generators produce code that is never hand-edited:

- `api/openapi.yaml` → `internal/http/gen` (oapi-codegen): request/response types,
  parameter binding and the chi route table.
- `queries/*.sql` + `migrations/*.sql` → `internal/db/dbgen` (sqlc): typed, compile-time
  checked queries.

Run both with `make gen`.

---

## Folder structure

```
karvon-be/
├─ api/
│  ├─ openapi.yaml            the API contract (source of truth)
│  └─ oapi-codegen.yaml       generator configuration
├─ cmd/
│  ├─ api/                    HTTP server + River workers (--workers=false for API only)
│  ├─ migrate/                goose + River migration CLI (up | down | status | queue)
│  └─ secret/                 prints a fresh KARVON_SECRET_KEY
├─ internal/
│  ├─ app/                    dependency wiring, startup, graceful shutdown
│  ├─ apperr/                 the error type that becomes the API error envelope
│  ├─ business/               dedupe, email rules and ranking, CSV export, ingestion
│  ├─ config/                 KARVON_* environment loading, .env support, validation
│  ├─ crypto/                 AES-256-GCM for provider API keys
│  ├─ db/                     pgx pool, transactions, dynamic queries, migrations
│  │  └─ dbgen/               sqlc output (generated)
│  ├─ events/                 job_events publisher (INSERT + NOTIFY) and LISTEN listener
│  ├─ http/                   router, middleware/, handlers, SSE, mappers, validation
│  │  └─ gen/                 oapi-codegen output (generated)
│  ├─ ids/                    UUIDv7 generation
│  ├─ logging/                slog setup
│  ├─ queue/                  the narrow River interface the app depends on
│  ├─ scraper/                job service, config/stats types, River job args, providers
│  │  ├─ crawler/             fetch, extract, robots.txt, per-host rate limit
│  │  ├─ jobs/                the four River workers + retention job
│  │  └─ provider/            Provider interface, apify/, outscraper/, fake/
│  ├─ source/                 provider credential management and connection test
│  ├─ stats/                  dashboard counters and chart data
│  └─ verify/                 the verification pipeline: local checks, scoring, settings
│     ├─ jobs/                the four River workers (run, free stage, paid, finalize)
│     ├─ provider/            Provider interface and the free providers
│     │  ├─ mailchecker/      the MIT library adapter (compiled in)
│     │  └─ reacher/          the Reacher HTTP client: retries, breaker, normalisation
│     └─ verifier/            the paid vendors: Verifier interface, emailable/, fake/
├─ migrations/                goose SQL migrations (embedded in the binary)
├─ queries/                   sqlc query definitions
├─ tests/                     integration tests (testcontainers) and provider fixtures
├─ Makefile  docker-compose.yml  Dockerfile  sqlc.yaml  .golangci.yml  .env.example
```

---

## Setup

Requirements: **Go 1.26+**, Docker (for Postgres and the integration tests).

```bash
git clone <this repo> karvon-be && cd karvon-be
cp .env.example .env
make secret            # paste the value into KARVON_SECRET_KEY in .env
docker compose up -d postgres
make migrate
make run               # http://localhost:8080
```

Check it is alive:

```bash
curl -s localhost:8080/healthz
```

Then configure a provider from the dashboard's Sources page, or directly:

```bash
curl -s -X PUT localhost:8080/api/v1/sources/0192f000-0000-7000-8000-000000000001 \
  -H 'Authorization: Bearer dev-key' -H 'Content-Type: application/json' \
  -d '{"api_key":"<your apify token>","enabled":true,"cost_per_1k_cents":400}'
```

Install the code generators once if you intend to change the spec or the SQL:

```bash
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

---

## Environment variables

Every variable is read with the `KARVON_` prefix. In development they may come from
`.env` (which never overrides a real environment variable); in production pass them
as real environment variables. **`.env` is gitignored and must never be committed.**

### Required

| Variable | Description |
| --- | --- |
| `KARVON_DATABASE_URL` | Postgres connection string, e.g. `postgres://karvon:karvon@localhost:5432/karvon?sslmode=disable` |
| `KARVON_API_KEY` | The static bearer token the dashboard sends. Must be ≥ 16 characters when `KARVON_ENV=production`. |
| `KARVON_SECRET_KEY` | 32-byte AES key (hex or base64) encrypting provider API keys at rest. Generate with `make secret`. The all-zero placeholder in `.env.example` is rejected when `KARVON_ENV=production`. Rotating it makes every stored provider key undecryptable. |

### Optional

| Variable | Default | Description |
| --- | --- | --- |
| `KARVON_ENV` | `development` | `production` switches logs to JSON and tightens key validation. |
| `KARVON_ADDR` | `:8080` | Listen address. |
| `KARVON_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `KARVON_VERSION` | `dev` | Reported by `/healthz`. |
| `KARVON_DB_MAX_CONNS` | `10` | pgx pool size. |
| `KARVON_MIGRATE_ON_BOOT` | `true` | Run migrations at startup. Set `false` in production and use `make migrate`. |
| `KARVON_CORS_ORIGIN` | `http://localhost:5173` | Comma-separated list of allowed browser origins. |
| `KARVON_REQUEST_TIMEOUT` | `30s` | Applied to every route except SSE and CSV exports. |
| `KARVON_WORKERS` | `true` | Run the queue workers here. `false` (or `--workers=false`) gives an API-only replica. |
| `KARVON_QUERY_CONCURRENCY` | `4` | Concurrent provider searches. |
| `KARVON_CRAWL_CONCURRENCY` | `8` | Concurrent website crawls. |
| `KARVON_CRAWL_TIMEOUT` | `10s` | Per-page fetch timeout. |
| `KARVON_CRAWL_USER_AGENT` | `Mozilla/5.0 (X11; Linux x86_64) … Safari/537.36 KarvonBot/1.0 (+…)` | Sent on every crawl request and matched against `robots.txt`. Browser-shaped on purpose: shared-hosting firewalls answer 403 to `(compatible; SomethingBot)` agents; the trailing `KarvonBot` token still lets a `robots.txt` name the crawler. |
| `KARVON_CRAWL_MAX_BODY_BYTES` | `2097152` | Body cap per page. |
| `KARVON_CRAWL_RECRAWL_AFTER_DAYS` | `30` | A domain crawled within this window is reused, not refetched. |
| `KARVON_CRAWL_MAX_EXTRA_PAGES` | `5` | Contact-like pages fetched after the homepage. |
| `KARVON_CRAWL_CONTACT_WORDS` | _(empty)_ | Extra words, comma-separated, appended to the built-in contact-link list (`contact`, `get-in-touch`, `about`, `team`, `kontakt`, …). A homepage link whose href or text contains a word is followed; earlier words rank higher. |
| `KARVON_CRAWL_CONTACT_PATHS` | _(empty)_ | Extra paths, comma-separated, appended to the built-in fallback list (`/contact`, `/contact-us`, `/contact.html`, `/pages/contact`, `/about`, …) tried when the homepage links nowhere useful. |
| `KARVON_PROVIDER_TIMEOUT` | `5m` | Budget for one provider search. |
| `KARVON_APIFY_BASE_URL` | `https://api.apify.com` | Override for tests or a proxy. |
| `KARVON_OUTSCRAPER_BASE_URL` | `https://api.outscraper.cloud` | Override for tests or a proxy. |
| `KARVON_PROVIDER_MAX_ACTIVE_RUNS` | `6` | How many vendor runs may be in flight at once. Queue concurrency cannot cap this: a worker waiting on a run gives its slot back between polls. Six 8 GB runs fit a 64 GB Apify account; match it to the plan. |
| `KARVON_PROVIDER_POLL_INTERVAL` | `1m` | How often a worker asks whether a long run has finished, and how often a query waiting for a free slot looks again. |
| `KARVON_PROVIDER_MAX_RUN_TIME` | `12h` | A run older than this is aborted so it stops spending, and whatever it produced is kept. |
| `KARVON_PROVIDER_PAGE_SIZE` | `1000` | Dataset rows read per fetch while draining a finished run. |
| `KARVON_APIFY_RUN_MEMORY_MB` | `8192` | Memory per run. It buys speed and a bigger share of the account memory limit, not more results. |
| `KARVON_APIFY_MAX_CHARGE_USD` | `25` | Cap on what one run may spend. This is the only hard stop on an uncapped (`max_per_query: -1`) run. |
| `KARVON_APIFY_SCRAPE_CONTACTS` | `false` | Website-contact enrichment, billed per place on top of the place itself. The crawl stage already finds emails, so measure the two against each other before turning it on. |
| `KARVON_APIFY_SKIP_CLOSED_PLACES` | `true` | Drop closed businesses before they are billed. |
| `KARVON_APIFY_COUNTRY_CODE` | `us` | Country the structured location fields resolve inside. |
| `KARVON_VERIFY_WEIGHT_EXISTING` | `30` | Weight of the local checks in the free score. Seeds the settings row. |
| `KARVON_VERIFY_WEIGHT_MAILCHECKER` | `20` | Weight of MailChecker. Seeds the settings row. |
| `KARVON_VERIFY_WEIGHT_REACHER` | `50` | Weight of Reacher. The three must total 100. |
| `KARVON_VERIFY_PASS2_MIN_SCORE` | `50` | Paid floor, inclusive. Below it an address is never billed. |
| `KARVON_VERIFY_PAID_THRESHOLD` | `90` | Paid ceiling, exclusive. The default is one above the free ceiling, so the band is open at the top and every address above the floor is paid for once. |
| `KARVON_VERIFY_PAID_ENABLED` | `true` | Whether the paid stage runs at all. |
| `KARVON_VERIFY_MAILCHECKER_ENABLED` | `true` | The compiled-in MailChecker provider. |
| `KARVON_VERIFY_REACHER_ENABLED` | `false` | The Reacher SMTP probe. Off by default; read the licensing note below first. |
| `KARVON_VERIFY_REACHER_URL` | `http://localhost:8081` | Reacher backend root. `http://reacher:8080` inside Compose. |
| `KARVON_VERIFY_REACHER_SECRET` | _(empty)_ | Sent as `x-reacher-secret`; must match the backend's `RCH__HEADER_SECRET`. |
| `KARVON_VERIFY_REACHER_TIMEOUT` | `30s` | Bounds one verification including its retries. |
| `KARVON_VERIFY_REACHER_RETRIES` | `2` | Retries of a failed attempt. 5xx and 429 are retried; other 4xx are not. |
| `KARVON_VERIFY_REACHER_CONCURRENCY` | `4` | In-flight requests, so we never outrun the backend's own throttle. |
| `KARVON_VERIFY_REACHER_BREAKER_THRESHOLD` | `5` | Consecutive failures that pause calls. `0` disables the breaker. |
| `KARVON_VERIFY_REACHER_BREAKER_COOLDOWN` | `1m` | How long calls stay paused. |
| `KARVON_VERIFY_REACHER_HELLO_NAME` | _(empty)_ | Per-request EHLO name. Empty leaves the backend's own configuration in charge. |
| `KARVON_VERIFY_REACHER_FROM_EMAIL` | _(empty)_ | Per-request MAIL FROM address. |
| `KARVON_EVENT_RETENTION_DAYS` | `30` | `job_events` older than this are pruned daily. |
| `KARVON_SSE_PING_INTERVAL` | `15s` | Keep-alive comment interval on the event stream. |
| `KARVON_MAX_QUERIES_PER_JOB` | `500` | Hard ceiling on terms × locations. |
| `KARVON_PUBLIC_BASE_URL` | _(empty)_ | Public URL of this API, used to register the Instantly and Mailchimp webhooks. Empty is supported: reconciliation keeps campaign data correct without webhooks. |
| `KARVON_INSTANTLY_BASE_URL` | `https://api.instantly.ai/api/v2` | Override for tests or a proxy. |
| `KARVON_INSTANTLY_TIMEOUT` | `30s` | Budget for one Instantly call including retries. |
| `KARVON_INSTANTLY_RPS` | `5` | Outbound Instantly calls per second. The workspace limit is 100/s shared across every key. |
| `KARVON_INSTANTLY_LEAD_BATCH` | `100` | Leads per `POST /leads/add`. Instantly's own advice; its hard cap is 1000. |
| `KARVON_INSTANTLY_LEAD_BATCH_GAP` | `2s` | Pause between lead batches, scheduled rather than slept so no worker blocks. |
| `KARVON_MAILCHIMP_BASE_URL` | `https://{dc}.api.mailchimp.com/3.0` | `{dc}` is replaced with the data-centre suffix of the stored key. |
| `KARVON_MAILCHIMP_TIMEOUT` | `30s` | Budget for one Mailchimp call. |
| `KARVON_MAILCHIMP_CONCURRENCY` | `4` | In-flight Mailchimp requests. The account limit is 10 simultaneous connections. |
| `KARVON_MAILCHIMP_WEBHOOK_TOLERANCE` | `5m` | Clock skew allowed on an inbound `X-Mailchimp-Signature`. |
| `KARVON_OPENAI_API_KEY` | _(empty)_ | Switches the AI generator from the manual ChatGPT flow to the API. A ChatGPT subscription does not provide this key. |
| `KARVON_OPENAI_MODEL` | `gpt-5.6-terra` | Model id for the API generator. |
| `KARVON_OPENAI_BASE_URL` | `https://api.openai.com/v1` | Override for tests or a proxy. |
| `KARVON_OPENAI_TIMEOUT` | `120s` | Budget for one generation. |
| `KARVON_CAMPAIGN_SYNC_INTERVAL` | `15m` | How often every live campaign is reconciled with Instantly. |
| `KARVON_CAMPAIGN_ACCOUNTS_SYNC_INTERVAL` | `6h` | How often the sending-account mirror and the Mailchimp audiences refresh. |
| `KARVON_CAMPAIGN_WEBHOOK_REPLAY_INTERVAL` | `30m` | How often failed Instantly deliveries are replayed. |
| `KARVON_CAMPAIGN_LEADS_FULL_SYNC_INTERVAL` | `24h` | How often every campaign gets a full lead diff. |
| `KARVON_NEWSLETTER_SYNC_INTERVAL` | `30m` | How often Mailchimp member status is mirrored. |
| `KARVON_CAMPAIGN_PUSH_CONCURRENCY` | `2` | Workers pushing leads to Instantly. |
| `KARVON_CAMPAIGN_EVENT_CONCURRENCY` | `4` | Workers applying provider events. |
| `KARVON_CAMPAIGN_MAX_IMPORT` | `50000` | Ceiling on one lead import. |
| `KARVON_WEBHOOK_MAX_BODY_BYTES` | `1048576` | Cap on an inbound provider delivery. |
| `KARVON_CLOUDFLARE_BASE_URL` | `https://api.cloudflare.com/client/v4` | Cloudflare API root. |
| `KARVON_CLOUDFLARE_TIMEOUT` | `45s` | One Cloudflare call; registration alone can wait 10 s. At least 15 s. |
| `KARVON_DOMAIN_POLL_INTERVAL` | `15s` | How often a registration Cloudflare is still working on is asked about again. |
| `KARVON_DOMAIN_MAX_WAIT` | `30m` | When a purchase stops waiting and hands unfinished registrations to a person. |
| `KARVON_GOOGLE_TOKEN_URL` | `https://oauth2.googleapis.com/token` | Google OAuth token endpoint (tests only change it). |
| `KARVON_GOOGLE_DIRECTORY_URL` | `https://admin.googleapis.com/admin/directory/v1` | Admin SDK Directory API root. |
| `KARVON_GOOGLE_SITE_VERIFICATION_URL` | `https://www.googleapis.com/siteVerification/v1` | Site Verification API root. |
| `KARVON_GOOGLE_TIMEOUT` | `30s` | One Google call. |
| `KARVON_WORKSPACE_POLL_INTERVAL` | `1m` | How often Google is asked again to verify a domain it cannot see the record of yet. |
| `KARVON_WORKSPACE_VERIFY_MAX_WAIT` | `2h` | When a setup stops waiting for verification and hands the domain to a person. |
| `KARVON_WORKSPACE_INSTANTLY_POLL_INTERVAL` | `3s` | How often an open Instantly sign-in session is checked. |

Provider API keys are **not** environment variables: they are entered on the Sources
page, encrypted with `KARVON_SECRET_KEY` and stored in `sources.api_key_enc`. They are
never returned by the API and never written to a log. That covers the Instantly and
Mailchimp keys too. The OpenAI key is the one exception — it is a single global
setting with no per-row lifecycle, so it stays in the environment.

---

## Database and migrations

Postgres 16 with the `citext` and `pg_trgm` extensions (created by the first migration).

Seven tables for the scraper: `sources`, `jobs`, `job_queries`, `job_results`,
`businesses`, `business_emails`, `job_events`. Verification adds four more, and the
campaign module adds eighteen (`contacts`, `contact_consents`, `contact_suppressions`,
`campaigns`, `campaign_leads`, `campaign_sending_accounts`, `campaign_variants`,
`sending_accounts`, `email_components`, `email_variants`, `email_variant_components`,
`variant_assignments`, `email_sends`, `provider_events`, `contact_events`,
`newsletter_audiences`, `newsletter_subscriptions`, `ai_generations`, plus
`campaign_settings`, `campaign_analytics_snapshots`, `sending_account_stats_daily`
and `sync_runs`). River owns its own tables for the queue.

```
sources ──< jobs ──< job_queries ──< job_results >── businesses ──< business_emails
                └──< job_events
```

Constraints that matter:

- `businesses.place_id` is unique — the same Google place is one row forever.
- `job_results` has a composite primary key `(job_id, business_id)`, so a job lists a
  business once however many queries found it.
- `business_emails` is unique on `(business_id, email)` with `email` as `citext`, and a
  partial unique index guarantees **at most one primary address per business**.
- Deleting a job cascades to its queries, events and result links; businesses survive.
- `jobs.source_id` is `ON DELETE RESTRICT`: a provider in use cannot vanish.

Indexes chosen for the queries the dashboard actually runs: `businesses(domain)`,
`businesses(state, category)`, `businesses(city)`, `businesses(created_at DESC)`, GIN
trigram indexes on `businesses(name)` and `businesses(domain)` (these back the `?q=`
search), `business_emails(email)`, `job_events(job_id, id)`, `job_results(business_id)`,
`job_queries(job_id, status)`, and a partial `businesses(phone, zip)` for listings that
arrive without a place id.

```bash
make migrate          # apply application + queue migrations
make migrate-status   # show what is applied
make migrate-down     # roll back the most recent application migration
```

Migrations are embedded in the binary, so `./bin/karvon-migrate up` works in a
container with no source tree. Nothing creates tables by hand.

---

## Development commands

```bash
make help             # list every target
make dev              # Postgres + every enabled connector, migrate, run the API
make run              # run the API against an existing database
make compose-reacher  # start just the Reacher container (honours .env)
make verify-settings-sync  # point the stored Reacher flag at .env
make gen              # regenerate the OpenAPI server and the sqlc queries
make fmt vet lint     # formatting, vet, golangci-lint
make check            # fmt + vet + lint + test
make secret           # print a fresh KARVON_SECRET_KEY
make compose-up       # Postgres + API in Docker
make compose-reset    # stop everything and delete the database volume
```

---

## Tests

```bash
make test              # unit tests with the race detector
make test-integration  # adds the testcontainers suite (needs Docker)
make cover             # coverage summary
go test ./... -short   # skip everything that needs Docker
```

What is covered:

- **Domain logic** — email acceptance and ranking, domain normalisation, job config
  normalisation and validation, cost arithmetic, CSV rendering.
- **Crawler** — mailto vs regex extraction, script/style skipping, denylists, contact
  link discovery, `robots.txt` parsing and enforcement, per-host rate limiting,
  redirect and body caps, context cancellation.
- **Providers** — parsing recorded Apify and Outscraper payloads from
  `tests/fixtures/`, error classification (auth vs transient), and the assertion that
  an API key never reaches a query string.
- **Verification providers** — MailChecker's disposable/syntax split, and Reacher's
  full verdict mapping plus every failure mode it has to survive: 5xx, 429, malformed
  bodies, a refused connection, a backend that never answers, the retry policy, the
  circuit breaker and the concurrency limiter.
- **Weighted scoring** — the worked example, the 89 cap, redistribution when a
  provider is down, switched off or undecided, effective weights totalling 100 under
  rounding, the zero-weight fallback, and the paid band's boundaries.
- **HTTP** — authentication on every route, the error envelope, validation messages,
  pagination defaults and caps, sort whitelisting, `null` vs omitted fields, CSV
  headers, and the SSE contract (replay, `Last-Event-ID`, pings, terminal close).
- **Integration** (`tests/`, real Postgres via testcontainers) — the whole pipeline from
  `POST /jobs` to a finished job with crawled addresses, cross-job dedupe, `robots.txt`
  enforcement, provider auth failure, cancellation of a running job, cascade deletes,
  filtering/sorting/pagination, suppression, exports and event retention. For
  verification specifically: the stored per-provider breakdown adding up to the free
  score, a Reacher outage leaving the run healthy, settings round-tripping and
  rejecting invalid weights, weights actually changing the next score, and the paid
  band skipping addresses the free providers already agree on.

The integration suite replaces the scrape provider, the crawler's transport, the paid
verifier and the Reacher backend with deterministic doubles: **no test touches the
network, opens an SMTP connection or spends a credit.** If Docker is unavailable the
suite skips, unless `KARVON_INTEGRATION=1` forces it to fail instead.

---

## Build and run

```bash
make build            # ./bin/karvon-api and ./bin/karvon-migrate
docker build -t karvon-be .
docker compose up --build
```

The image is a multi-stage build onto `distroless/static`, runs as a non-root user and
takes all configuration from the environment. In production run migrations as a
separate step and start the API with `KARVON_MIGRATE_ON_BOOT=false`.

To scale the API separately from the workers, run some replicas with
`KARVON_WORKERS=false`; they serve HTTP and enqueue jobs, and the SSE stream still
works because fan-out goes through Postgres `LISTEN`/`NOTIFY` rather than an
in-process bus.

---

## API overview

Base path `/api/v1`. Everything except `/healthz` and `/openapi.json` requires
`Authorization: Bearer <KARVON_API_KEY>`.

| Method & path | Purpose |
| --- | --- |
| `GET /healthz` | Liveness with a database and queue check; 503 when one is down. |
| `GET /openapi.json` | The contract, for `pnpm gen:api` on the frontend. |
| `GET /stats/scraper` | Dashboard counters, the last job, and emails per job for the chart. |
| `GET /sources` | Providers. Returns `has_key`, never the key. |
| `GET /sources/{id}` | One provider. |
| `PUT /sources/{id}` | Set key / cost / enabled / name. The key is encrypted before it is stored. |
| `POST /sources/{id}/test` | One-result probe call; records `last_tested_at` and `last_test_ok`. |
| `POST /jobs/estimate` | `{ queries, est_listings, est_cost_cents, cost_per_1k_cents }`. |
| `POST /jobs` | Validate, store and enqueue. Returns 201 with the job. |
| `GET /jobs` | History. Filters `status` (repeatable), `source_id`, `from`, `to`, `q`; `sort`; pagination. |
| `GET /jobs/{id}` | Detail including live `stats`. |
| `POST /jobs/{id}/cancel` | Cooperative cancel; 409 if the job already finished. |
| `POST /jobs/{id}/rerun` | Clone the immutable config into a new job (201). |
| `POST /jobs/{id}/recrawl` | Re-crawl a finished job's websites (201). Copies the job's businesses into a new job that starts at the crawl stage: no provider search, no spend. Optional body `{"targets":["emails","socials"]}` (default `["emails"]`): a business is revisited when it lacks any target. 409 while the job is still running or when it has no businesses. |
| `POST /businesses/recrawl` | Re-crawl selected (`ids`) or filtered businesses from the master list for `targets` `emails` and/or `socials` (201, a new job). Same filter fields as `/businesses/export`; at most 20000 businesses. |
| `POST /businesses/social-scrape` | Read the social profiles of selected (`ids`) or filtered businesses (201, a new job). Body `{"networks":["facebook"],"missing_email_only":true}` plus the `/businesses/export` filter fields. Each business with a Facebook profile has its public page read through `services/fb-scrape`; the email it shows is stored with source `facebook`, its phone number only when the business has none. 409 when nothing matches or `KARVON_FB_SCRAPE_ENABLED` is off. |
| `DELETE /jobs/{id}` | Delete the job, its queries, events and result links. 204. |
| `GET /jobs/{id}/events` | **SSE**: `progress`, `log`, `status`. Supports `Last-Event-ID` / `?after=`. |
| `GET /jobs/{id}/export.csv` | Streamed per-job CSV attachment. |
| `GET /businesses` | Master list. Filters `job_id`, `category` (repeatable, any-of), `state`, `city`, `has_email`, `suppressed`, `email_source`, `q`; `sort`; pagination. Includes `primary_email`. |
| `GET /businesses/{id}` | Detail with every address and the raw provider payload. |
| `PATCH /businesses/{id}` | `suppressed` and `notes`. `"notes": null` clears the note. |
| `POST /businesses/bulk` | `{ ids[], action: "suppress" \| "unsuppress" }` → `{ updated }`. |
| `POST /businesses/export` | Streamed CSV for a filter set (or an explicit `ids` list). |
| `GET /verification/settings` | Provider weights, toggles, the paid band, and each provider's live readiness. |
| `PUT /verification/settings` | Replace them. 422 when the weights do not total 100% or the floor is above the ceiling. |
| `GET /verification/stats` | Tag distribution, pass coverage, credits used, and what a paid run would cost today. |
| `GET /verification/emails` | Scored addresses. Filters `tag` (repeatable), `min_score`, `max_score`, `pass2_status`, `needs_third_party`, `has_typo`, `q`, `business_id`, `job_id`; `sort`; pagination. |
| `GET /verification/emails/{id}` | One address with the local check breakdown, the per-provider weighted breakdown and the raw paid payload. |
| `POST /verification/emails/{id}/self` | Queue the free local pass for one address (202). |
| `POST /verification/emails/{id}/third-party` | Queue the paid check (202). 409 when the address has already been sent to a third party, is outside the paid band, or the paid stage is off. |
| `POST /verification/emails/{id}/apply-typo` | Rewrite every business holding the misspelled address to the suggestion. |
| `POST /verification/runs/estimate` | `{ emails, needs_self, cached, credits_needed, est_cost_cents, balance_credits }`. `cached` counts addresses already sent once. |
| `POST /verification/runs` | Start a bulk run. A `third_party` run must carry `max_cost_cents`. |
| `GET /verification/runs` | Run history. Filters `pass`, `status`; pagination. |
| `GET /verification/runs/{id}` | Run progress, polled while active. |
| `POST /verification/runs/{id}/cancel` | Cooperative cancel. |
| `GET /domains/settings` | Cloudflare connection (`account_id`, `has_token`, `enabled`, `ready`) and the limits. |
| `PUT /domains/settings` | Set `account_id`, `api_token` (encrypted), `enabled`. Omit to keep, `null` to clear. |
| `POST /domains/settings/test` | One free read proving the token and account. |
| `GET /domains/search` | Keyword suggestions with prices (`q`, `limit`, repeatable `extensions`). Cached, for discovery. |
| `POST /domains/check` | Authoritative availability and price for up to 20 names. |
| `POST /domains/purchases` | Buy up to 10 domains (`confirm: true`, each with `expected_cost_cents`). 202; 409 if one moved or another purchase runs. |
| `GET /domains/purchases` | Purchase history with each domain's outcome; pagination. |
| `GET /domains/purchases/{id}` | One purchase, polled until it settles. |
| `GET /domains/registrations` | Every domain the Cloudflare account owns, live. |
| `GET /domains/registrations/{domain}` | One owned domain. |
| `PATCH /domains/registrations/{domain}` | `{ "auto_renew": bool }`, the one setting Cloudflare's API can change. |
| `GET /workspace/settings` | Google Workspace connection (`admin_email`, `service_account_client_id`, `scopes`, `ready`). |
| `PUT /workspace/settings` | Set `admin_email`, `service_account_key` (the JSON file, encrypted), `enabled`. Omit to keep, `null` to clear. |
| `POST /workspace/settings/test` | One free read proving the key, delegation and admin; returns the primary domain. |
| `POST /workspace/domains` | Set up Workspace mail on a domain with 1-5 mailboxes (`confirm: true`; each is a paid licence). 202. |
| `GET /workspace/domains` | Setups with their mailboxes; pagination. |
| `GET /workspace/domains/{domain}` | One setup, polled until it leaves `provisioning`; `next_step` says what a person does next. |
| `POST /workspace/domains/{domain}/retry` | Resume a `failed` setup where it stopped. 202. |
| `PUT /workspace/domains/{domain}/dkim` | `{ "value", "selector"? }` from the Admin console; publishes DKIM, domain becomes `active`. |
| `GET /workspace/mailboxes/{id}/credentials` | A created mailbox's address and initial password (`Cache-Control: no-store`). |
| `POST /workspace/domains/{domain}/mailboxes` | Add mailboxes to a set-up domain (`confirm: true`; at most 5 per domain). 202. |
| `DELETE /workspace/mailboxes/{id}` | Remove a `failed` mailbox record, freeing its slot. 204; 409 for a created one. |
| `POST /workspace/mailboxes/{id}/instantly` | `{ "warmup"? }` → `auth_url` to sign in as the mailbox; Karvon follows the session. |

### Conventions

**Errors.** Every failure is the same envelope; `details` appears on validation errors.

```json
{
  "error": {
    "code": "validation_failed",
    "message": "job configuration is invalid",
    "details": [{ "field": "config.terms", "message": "must have at least 1" }]
  }
}
```

| Code | Status | When |
| --- | --- | --- |
| `bad_request` | 400 | Body is not JSON, or is not a single object. |
| `unauthorized` | 401 | Missing or wrong API key. |
| `not_found` | 404 | Unknown resource or route. |
| `conflict` | 409 | Action is impossible in the current state (cancelling a finished job). |
| `validation_failed` | 422 | Body or query parsed but broke a rule. |
| `provider_auth` | 502 | The vendor rejected the stored API key. |
| `provider_error` | 502 | The vendor failed for another reason. |
| `internal` | 500 | Anything unexpected. The cause is logged, never returned. |

`/healthz` answers 503 with its own payload rather than the error envelope, so a probe
can read `db` and `queue` independently.

**SSE.** Events carry a monotonic `id`; reconnect with `Last-Event-ID` (or `?after=`) to
replay exactly what was missed. A client that connects without a position receives the
last 200 events. `: ping` arrives every `KARVON_SSE_PING_INTERVAL`. The stream closes
after a terminal `status` event. Because `EventSource` cannot set headers, this route
also accepts the key as `?api_key=`.

```
event: progress
id: 1042
data: {"queries_done":12,"queries_total":40,"sites_crawled":180,"sites_total":611,"emails_found":97,"cost_cents":1220}

event: status
id: 1044
data: {"status":"done","finished_at":"2026-09-19T15:09:12Z"}
```

---

## Scrape pipeline

A job moves through four River job kinds, so a crash resumes at the stage that failed
and the dashboard sees progress per stage. A fifth kind, `scrape_abort_runs`, is not a
stage: it is the cleanup that stops vendor runs still spending after a job ends.

1. **`scrape`** — load the config, insert one `job_queries` row **per location**, each
   carrying every search term, set the job `running`, fan out.

   The grouping is a billing decision. Vendors charge per place returned and dedupe
   only inside a single run, so a gym answering to both `gym` and `fitness center` is
   paid for once when the terms share a run and twice when they do not. Locations stay
   apart because their areas must not overlap: state polygons are disjoint, so runs for
   different states can go at the same time without ever paying for the same place
   twice. Mixing granularities inside one job — a state *and* a city inside it — is the
   one way to reintroduce the overlap.
2. **`scrape_query`** (worker pool `KARVON_QUERY_CONCURRENCY`; for a vendor with long
   runs the cap that matters is `KARVON_PROVIDER_MAX_ACTIVE_RUNS`, because a worker
   waiting on a run gives its slot back between polls) — one provider search per
   location. Listings upsert into `businesses` (by `place_id`, else by `domain`,
   else by `phone`+`zip`) and link to the job via `job_results`. Transient failures
   retry three times with backoff; an authentication failure fails the whole job at
   once with `provider_auth` instead of burning retries on a bad key.

   A vendor with long runs (Apify) takes the asynchronous path: start the run, write
   its id down, poll, then drain the dataset page by page, resuming from the last
   committed offset. Each step is a short worker invocation, so a run lasting hours
   holds nothing open. The run id is the money: a row leaves `run_state = 'none'`
   exactly once, so a retry resumes the run that already exists instead of paying for
   a second one, and a worker that died between starting a run and recording it adopts
   the orphan rather than replacing it. Cancelling or failing a job queues
   `scrape_abort_runs`, which stops any run still spending. Spend is the vendor's own
   figure for the run when it reports one, otherwise
   `listings × cost_per_1k_cents / 1000`.
3. **`scrape_crawl`** (worker pool `KARVON_CRAWL_CONCURRENCY`, and within that the
   job's own `config.concurrency`) — fetch the homepage, then up to
   `KARVON_CRAWL_MAX_EXTRA_PAGES` contact-like pages: first the homepage's own links
   ranked by the contact-word list (`contact` beats `about`), then the conventional
   paths (`/contact`, `/contact.html`, `/pages/contact`, …). A homepage that cannot be
   fetched still gets the conventional paths tried. Rules that are not optional: `robots.txt` is honoured,
   at most one request per second per host, a 10 s timeout, a 2 MB body cap, at most
   three redirects. A website (same page: scheme, `www.`, query string and trailing
   slash ignored, path kept) crawled in the last `KARVON_CRAWL_RECRAWL_AFTER_DAYS` days
   is reused rather than refetched; re-crawls always fetch fresh. A site counts as
   failed — a `warn` log line, never a job failure — only when no page of it could be
   fetched; guessed contact paths that 404 are expected and not reported. Every fetched page is also scanned for social profile links (Facebook,
   Instagram, TikTok, YouTube, LinkedIn, X, Threads, Pinterest, Yelp, Linktree — in
   anchors, JSON-LD `sameAs` and inline scripts). Share buttons, pixels and individual
   posts are dropped, each profile is stored once in canonical form in
   `business_socials`, and `GET /businesses/{id}` returns them as `socials`. A business
   whose website *is* a social profile is not fetched but keeps that profile. Social
   pages themselves are never fetched.
4. **`scrape_finalize`** — recompute the statistics from the tables (the incremental
   counters are only for live progress), then set `done`, or `failed` when more than
   half the queries failed. `stats.duplicates` is listings minus unique businesses:
   every listing was paid for, so a number climbing above a few percent on areas that
   should not overlap is the signal that two runs are covering the same ground.

### Scraping every state

One job, `locations: []` (which expands to all 51 states), every term in `terms`, and
`max_per_query: -1` for full coverage. That is 51 runs, `KARVON_PROVIDER_MAX_ACTIVE_RUNS`
of them at a time, each bounded by `KARVON_APIFY_MAX_CHARGE_USD`. Pilot it on two or
three small states first and read `stats.duplicates` and the real cost per place from
the vendor console before committing to the other 48. Re-scraping a state always
re-bills every place in it — there is no incremental mode — so refreshes are scheduled
per state, never overlapping a run of the same state.

A **re-crawl** (`POST /jobs/{id}/recrawl`) creates a new job whose `job_results` are
copied from the source job and enqueues `scrape_recrawl` instead of `scrape`: it marks
the job running and hands straight over to stage 3, so the provider is never called.
The new job's `config.recrawl_of` names the original search.

A **social media scrape** (`POST /businesses/social-scrape`) is shaped like a re-crawl
of hand-picked businesses, but `scrape_social` queues one `scrape_social_page` per
business with a profile on a requested network (only Facebook today) instead of
website crawls. Those run on their own `scrape_social` queue, sized by
`KARVON_FB_SCRAPE_CONCURRENCY` to match the fb-scrape service, which owns the proxy
pool and per-IP limits. The pages are counted in `sites_total` / `sites_crawled`, so
the progress bar and finalize stage work unchanged. When the service itself fails a
page is retried with backoff; a page that shows no details is just logged.

A sixth kind, `prune_job_events`, runs daily and enforces the retention window.

**Cancellation** sets the job to `cancelled`, cancels its pending queue entries by
metadata, and marks every queued query cancelled. Workers re-check the job status
between units and exit cleanly, so a cancel takes effect within seconds even for work
already in flight.

**Event fan-out** is `INSERT INTO job_events` followed by `pg_notify` in the same
transaction — a subscriber is never woken for a row it cannot yet read.

---

## Email verification

Every address carries a `final_score` from 0 to 100 and a `verification_tag` derived
from it. The score is produced by a **free stage** that combines several providers by
weight, and a **paid stage** that only ever sees the addresses the free stage could
not settle. Both are stored separately, so the UI can always show which provider
contributed what.

```
   free stage (River queue verify_self, one job per address)
   ┌───────────────────────────────────────────────────────────┐
   │  local checks  →  MailChecker  →  Reacher                 │
   │   (0–100)          (0 or 100)      (0–100, may be down)   │
   │        └───────────────┴──────────────┘                   │
   │                        ↓                                  │
   │            weighted average → free_score (0–89)           │
   └───────────────────────────────────────────────────────────┘
                            ↓
       paid band:  paid_min_score ≤ free_score < paid_threshold
                            ↓
              paid stage (verify_third) → 0 / 70 / 100
                            ↓
     final_score = the paid verdict when there is one, else free_score
```

### The free providers

**Karvon's local checks** are free, run on every address and never touch anything but
cached DNS. Hard failures stop immediately and score 0: invalid syntax, an automated
or shared mailbox, a disposable domain, or a domain with neither MX nor address
records. Everything else is a soft signal:

| Check | Points |
| --- | ---: |
| MX records present | 30 |
| A/AAAA records present | 10 |
| Domain older than one year (RDAP) | 10 |
| Local part looks human | 10 |
| SPF record present | 5 |
| DMARC record present | 5 |
| Not a free mail provider | 5 |
| No typo in the domain | 5 |
| Length and structure sane | 5 |
| **Maximum** | **85** |

That 85 (or 75 with RDAP disabled) is the pipeline's own scale; it is rescaled to
0–100 before being weighted, so a perfect local result reports 100 *as a provider*.
A detected typo caps the local score at 40 and the suggestion is stored so the UI can
offer a one-click correction; the suggestion survives a hard failure, so a misspelled
domain that does not resolve is still actionable.

**MailChecker** is the MIT-licensed `FGRibreau/mailchecker`, compiled into the binary
rather than run as a service: it is pure Go and its ~56 000-entry disposable-domain
list is a compiled-in map, so a network hop would buy nothing. It exposes exactly two
booleans, so its normalised score is honestly binary — 0 for a disposable domain or
unparseable syntax, 100 for anything it has nothing against. It never confirms a
mailbox exists, which is why the weighting layer exists at all.

**Reacher** (`check-if-email-exists`) is the only provider that opens an SMTP
conversation with the recipient's mail server. It runs as its own container and is
reached through `internal/verify/provider/reacher`, the single place in the codebase
that knows its HTTP API. Its verdict plus the SMTP detail maps onto our scale:

| Observation | Score |
| --- | ---: |
| `is_reachable = safe` | 100 |
| `risky`, no further detail | 70 |
| `risky` + catch-all domain | 60 |
| `risky` + full inbox | 50 |
| `is_reachable = invalid` | 0 |
| a disposable domain, whatever SMTP said | 0, and disqualifying |
| `is_reachable = unknown` | *no score* — the provider declined to answer |

### Weighting, and what happens when a provider is down

The weights are percentages that must total 100 across all three providers, whether
or not each is switched on. At scoring time the weight of every provider that did
**not** produce a score — switched off, unreachable, errored, or undecided — is
redistributed proportionally over the providers that did:

```
free_score = Σ (score × weight) / Σ weight     over scoring providers only
```

This is the whole point of the design. Scoring a missing provider as 0 would mean a
Reacher outage silently downgraded every address in the database; instead the address
is scored from what is left, and the row records why the missing provider is missing.
The local checks can never fail, so there is always at least one contributor.

`provider_results` on the row stores, per provider, the normalised score, the status,
the reason, the configured weight, the effective weight after redistribution and the
points contributed. The contributions add up to `free_score`, so the UI can always
explain a number without recomputing it against settings that may have changed since.

**Some findings are not votes.** A provider can mark a result *disqualifying*: the
address is not valid syntax, its domain is a known throwaway, its mailbox is
automated, or the domain accepts no mail at all. Those zero the free score outright,
whatever the other providers said, because a provider that never checked for such a
thing has no opinion to weigh against it — MailChecker reporting "nothing against it"
about a `noreply@` mailbox means only that MailChecker does not look at mailbox
names. The local hard failures, MailChecker's two negatives and a disposable domain
found by Reacher are disqualifying; a rejected recipient is not, because "invalid" is
the verdict most prone to false negatives from greylisting and blocked ports.

**The free score is capped at 89.** A weighted score can never reach the Verified
band: only a provider we pay to stand behind its answer may do that.

| Score | Tag | Label |
| --- | --- | --- |
| 90–100 | `green` | Verified |
| 70–89 | `light_green` | Likely valid |
| 50–69 | `yellow` | Uncertain |
| 30–49 | `orange` | Risky |
| 0–29 | `red` | Invalid |

The API always sends `tag_label` alongside `verification_tag`, because a result must
never be conveyed by colour alone.

### When the paid provider runs

The paid stage is gated by a band with two bounds, both configurable:

- **`paid_min_score`** (default 50) is the floor. Nobody should ever pay to be told
  that `gmial.com` is a typo.
- **`paid_threshold`** (default 90) is the ceiling. It ships one above `FreeMaxScore`,
  which leaves the band open at the top: every address the floor lets through is paid
  for once. Lower it to trade confidence for credits.

Both are re-checked in the worker immediately before spending anything, rather than
trusted from when the run was created. The paid verdict still replaces the free score
outright rather than being averaged with it.

> **Why the ceiling ships open.** Every free check reads the *domain* — MX, A, SPF,
> DMARC, domain age, the shape of the local part, the typo table. None of them asks
> whether a mailbox accepts mail, so a high free score is exactly where false
> confidence collects: a catch-all domain and a departed employee at a well-run
> domain both score near the top. The free stage also caps at `FreeMaxScore`, below
> `MinScoreGreen`, so an address that never sees the paid provider can never be
> tagged Verified at all. The saving lives at the floor, where a typo caps at
> `TypoScoreCap` and is never billed, and the one-send rule means a given address
> costs at most one credit ever, not one per run.
>
> Lower `paid_threshold` when credits are scarce and the output is bulk enrichment
> nobody will mail; spend them on the uncertain middle instead. Keep it open when the
> addresses are going into a send, where one bounce costs more reputation than a
> credit costs money.

### Running Reacher locally

Reacher is disabled by default. Set this in `.env`:

```
KARVON_VERIFY_REACHER_ENABLED=true
KARVON_VERIFY_REACHER_URL=http://localhost:8081   # http://reacher:8080 inside Compose
KARVON_VERIFY_REACHER_SECRET=reacher-dev-secret   # must match RCH__HEADER_SECRET
```

and then the usual one command brings up the whole stack, Reacher included:

```bash
make dev
```

`make dev` reads that one variable and does the three things it implies: starts the
container behind the `reacher` Compose profile and waits for its healthcheck, applies
migrations, and points `verification_settings.enabled.reacher` at it. The wait matters:
an API that starts calling Reacher while it is still booting collects five failures,
opens the client's circuit breaker and then sits out a full cooldown reporting "the
Reacher backend is unavailable". A backend that never becomes healthy only warns — the
API still starts, because the pipeline is built to survive Reacher being absent. That third step is the one that is easy
to miss by hand — **the environment only seeds the settings row the first time it is
read, and after that the row decides**, so an existing database keeps saying "off"
however the environment is set. `make verify-settings-sync` is that step on its own,
and it is idempotent. Set the variable back to `false` and the same command turns the
flag off again; `make compose-reacher-down` stops the container.

By hand it is `docker compose --profile reacher up -d` plus a `PUT
/api/v1/verification/settings` carrying the whole settings object.

Check it is up with `curl localhost:8081/version`, which is also the health probe the
settings endpoint uses. `GET /api/v1/verification/settings` reports each provider's
readiness, so the settings page can show that Reacher is down without anyone reading
a log.

Three things regularly go wrong:

- **Outbound port 25 is blocked.** Most cloud providers block it by default, and
  without it every verification comes back `unknown` — which the scoring layer treats
  as a provider that declined to answer, so scores quietly fall back to the other two.
  Reacher supports SOCKS5 proxies (`RCH__PROXY__*`) for this.
- **The image is amd64-only.** On an Apple Silicon machine Docker will emulate it,
  which works but is slow enough to notice.
- **Reverse DNS matters.** `RCH__HELLO_NAME` and `RCH__FROM_EMAIL` should match the
  reverse DNS of the egress IP in production, or mail servers will treat the probe as
  suspicious and answer `unknown`.

A Reacher outage never fails a run: the client bounds each call with a timeout,
retries 5xx and 429 a couple of times, and opens a circuit breaker after five
consecutive failures so a backend that is simply down costs one connection attempt
per cooldown rather than one per address.

### Licensing

**MailChecker is MIT.** It is an ordinary Go module dependency with no obligations
beyond retaining the licence text, which `go mod` does.

**Reacher is dual-licensed: AGPL-3.0, or a commercial licence** sold at
<https://reacher.email/pricing>. This matters here. Karvon is proprietary and is
served to users over a network, which is the case AGPL section 13 covers, so the AGPL
route would oblige us to publish the corresponding source of the combined work. The
positions available are:

1. **Leave it disabled** — the shipped default. No obligation.
2. **Buy the commercial licence.** Nothing in this repository changes.
3. **Enable it under AGPL** only if legal accepts the source-availability obligation.

Karvon talks to Reacher only over HTTP, from a separate container, through its
documented public API; no Reacher code is linked into, vendored by, or distributed
with this binary. That is the weakest coupling available and it is why the
integration is a client package rather than a library dependency — but it is not by
itself a legal conclusion, and none of this is legal advice.

One more thing worth knowing: Reacher's backend can be configured with a
**Commercial License Trial** (`RCH__COMMERCIAL_LICENSE_TRIAL__*`). When it is set,
the backend POSTs every verification result back to Reacher's own servers, which for
us means customer email addresses leaving our infrastructure. The Compose file leaves
it unset and says so.

### Settings

Weights, toggles and the paid band live in the `verification_settings` table and are
edited through `GET`/`PUT /api/v1/verification/settings`. The `KARVON_*` variables
above seed the row the first time it is read, so an existing deployment keeps the
behaviour it had; after that the settings own the policy and the variables have no
further effect. Weights are stored as JSONB keyed by provider, so adding a fourth
provider later is a code change and a settings edit rather than a migration.

Validation refuses weights that do not total 100 and a floor above the ceiling, both
with field errors the settings form can point at.

### One send per address, for ever

An address is handed to a third-party verifier **at most once in its life**. This is
not a cache window and there is nothing to wait out: `email_verifications.third_party_sent_at`
is claimed immediately before the outbound call and, once held, the address can never
qualify for a paid run, a bulk run or the single-address action again.

The record is keyed by the address, not by the business, so the same address held by
ten businesses is sent once. Three layers enforce it, in the order they are reached:

1. **Selection.** Every paid path — the estimate, the run selector, the
   `needs_third_party` list filter and `checkGate` — filters on
   `third_party_sent_at IS NULL`.
2. **The claim.** The worker's last act before calling out is
   `ClaimThirdPartySend`, an `UPDATE ... WHERE third_party_sent_at IS NULL`. It is a
   compare-and-set, so two runs racing over the same address produce exactly one
   call; the loser gets no row back and skips.
3. **The database.** A `BEFORE UPDATE` trigger refuses to move a held lock, to
   release one that already has a verdict, or to write a second `pass2_verified_at`
   over a first. No code path, migration or hand-run statement gets past it.

The lock is released in exactly one case: the call never reached the provider — a
rejected API key, a throttle, a dropped connection. Our own infrastructure failing
must not spend an address's single send. Anything the provider does answer, including
`unknown` and "accepted, no verdict yet", keeps the lock: those are answers, and
re-asking would be a second send.

`POST /verification/runs/estimate` already excludes sent and non-qualifying
addresses, so the count and cost shown in the confirmation dialog are the count and
cost that will be billed. A `third_party` run must send `max_cost_cents`; the server
re-estimates and returns 409 if the real cost has grown past what was approved.

### Role accounts

The default `KARVON_VERIFY_ROLE_SOFT_MODE=hard` follows the specification: `info@`,
`sales@`, `support@` and friends score 0 and never reach the paid pass. Note that
this collides with how the scraper picks a primary address, which deliberately
prefers `info@` over everything else, so most scraped primaries will tag red. Set
the mode to `penalty` to cap those addresses at 60 instead, which keeps them
eligible. Automated and abuse mailboxes (`noreply@`, `postmaster@`) are a hard
failure in both modes.

These gates, like the paid band's `paid_min_score` floor, apply to bulk runs. The
single-address action (`POST /verification/emails/{id}/third-party`) skips the
floor: an address picked by hand is sent however low the free checks scored it.
It still needs the free checks to have run, still respects the ceiling, and is still
never sent twice.

### Blocklists

The disposable-domain, role-account, free-provider and typo-reference lists live in
`config/verify/*.txt` and are embedded in the binary. Point
`KARVON_VERIFY_LISTS_DIR` at a directory to replace any of them file by file without
a rebuild; the compose stack mounts `./config/verify` for exactly this. Lists are
read once at boot, so an update needs a restart. MailChecker's own list is separate
and is updated by bumping the module.

### Runs

Both stages are River jobs on their own queues, so a vendor outage parks paid jobs
without starving the scrape pipeline or the free stage. A run expands its filter into
`verification_run_items`, fans out one job per address, and finalizes when the last
one lands. A single-address action creates a one-item run, so there is exactly one
execution path. Progress is polled through `GET /verification/runs/{id}`; the SSE
stream remains scrape-only.

Within one address the free providers run in sequence, cheapest first, and the SMTP
probe is skipped entirely when the local checks already hard-failed: there is no
point opening a conversation with a mail server about an address that is not valid
syntax or whose domain has no MX record.

---

## Campaigns

The campaign module is the outreach layer. It sends nothing itself: Instantly
delivers the cold email, Mailchimp delivers the newsletter, and this service owns
everything in between.

| Owned here | Owned by the provider |
| --- | --- |
| Campaign ↔ provider mapping, lead lifecycle, consent and suppression | Delivery, mailbox health and warmup, sending schedule |
| Content components, variants, per-lead variant assignment and the rendered snapshot | Open and click tracking, bounce protection, unibox |
| The normalised event log, per-component and per-variant analytics | The provider's own campaign analytics (kept beside ours, never merged) |
| Newsletter eligibility and the consent record behind it | Confirmation emails, unsubscribe pages, newsletter sends |

### The two stages

Stage one is cold B2B outreach through Instantly. Stage two is permission-based
marketing through Mailchimp, and a contact only reaches it by an explicit act of
consent. **Interest is not consent.** A lead who replies "sounds interesting" has
told us they want a conversation, not a newsletter; the two are separated by a
consent record that names its source, its timestamp, who captured it and the
evidence, and by the `contacts_guard_stage` trigger, which refuses any newsletter
stage without one.

### Lifecycle

A contact carries one `lifecycle_stage` across every campaign. It moves forward
only, a terminal stage beats any funnel stage, and `unsubscribed` beats everything
and can never be undone.

```
cold → queued_for_instantly → contacted → engaged → replied → interested
     → permission_requested → permission_captured → newsletter_eligible
     → mailchimp_pending → mailchimp_subscribed
```

Terminal states are `not_interested`, `wrong_person`, `do_not_contact`,
`invalid_email`, `bounced` and `unsubscribed`. The first three can be lifted with a
note, which returns the contact to `contacted` — never higher, because consent has
to be captured again. The last three are permanent.

Suppression always goes through `internal/campaign/suppression.Apply`, whatever
raised it: the contact moves to its terminal stage, every live campaign lead stops,
pushed leads are deleted at Instantly, and a live newsletter subscription is
unsubscribed. Two guards make that stick: the claim that picks leads for a push only
ever returns leads whose contact is not suppressed, and the database trigger refuses
a suppressed contact in a non-terminal stage.

A campaign lead has its own status (`pending`, `pushing`, `active`, `paused`,
`completed`, `replied`, `bounced`, `unsubscribed`, `skipped`, `suppressed`,
`failed`) which mirrors Instantly's lead status plus our own pre-push states. The
contact stage is the funnel; the lead status is the delivery.

### Content, variants and attribution

Emails are assembled from reusable components — subject, hook, problem, value
proposition, proof, CTA, closing, PS — each with a stable id and a review status
(`draft` → `reviewed` → `approved` → `active` → `archived`, with AI output entering
at `ai_generated`). A variant is an ordered set of components, stored with the
assembled subject and body template so it stays reproducible after a component is
archived.

A campaign attaches variants per step with weights that total 100. When a lead is
pushed, `internal/campaign/assign` picks its variant from a seed derived from
`(campaign, contact, step, weights_version)` — deterministic, so a retry picks the
same variant — and the choice is written to `variant_assignments` together with the
**rendered subject and body**, the component ids, and the weights version in force.
The moment Instantly acknowledges the lead the row is locked, and a trigger refuses
any later edit or delete. Changing a campaign's weights bumps `weights_version`, so
new leads follow the new distribution while every existing assignment — and every
event attributed to it — stays exactly as it was sent.

Instantly never sees the variants. Each step of the Instantly campaign holds a
single variant whose subject and body are the custom variables `{{k_subject_N}}` and
`{{k_body_N}}`, and the push fills those per lead. That is what makes weighted
distribution and per-component analytics possible at all: Instantly's own variant
rotation cannot be weighted and cannot be attributed back to a component.

### Synchronisation

Webhooks update immediately; scheduled reconciliation catches whatever they missed
and is the only source of truth when no public URL is configured.

| Job | Interval | What it reconciles |
| --- | --- | --- |
| `campaign_sync_all` → `campaign_sync_campaign` | `KARVON_CAMPAIGN_SYNC_INTERVAL` (15m) | Campaign status and sending status, the Instantly analytics snapshot, every lead's status and counters, and sends the webhook never delivered |
| `campaign_sync_accounts` | `KARVON_CAMPAIGN_ACCOUNTS_SYNC_INTERVAL` (6h) | The sending-account mirror and its daily analytics |
| `campaign_replay_webhook_events` | `KARVON_CAMPAIGN_WEBHOOK_REPLAY_INTERVAL` (30m) | Instantly deliveries that failed and will not be retried, re-ingested through the same path |
| `campaign_sync_leads_full` | `KARVON_CAMPAIGN_LEADS_FULL_SYNC_INTERVAL` (24h) | A full lead diff for every launched campaign |
| `newsletter_sync_members` | `KARVON_NEWSLETTER_SYNC_INTERVAL` (30m) | Mailchimp member status for everything we pushed |
| `newsletter_sync_audiences` | 6h | The audience mirror |

Every inbound delivery is stored in `provider_events` under a `dedupe_key` — a
SHA-256 of the payload's identifying fields, because neither provider sends an event
id — and a `UNIQUE` violation makes a redelivery a no-op. Applying an event is one
transaction, and a timeline entry is unique per `(provider_event_id, type)`, so the
same event arriving twice, or a job retried after a crash, changes nothing the
second time.

### Webhooks

`POST /api/v1/webhooks/instantly/{token}` and `POST /api/v1/webhooks/mailchimp/{token}`
are the only routes that do not take the API key: neither provider can send one.
They authenticate themselves instead.

- **Instantly** does not sign its webhooks. Registration therefore generates an
  unguessable path token and a shared secret, and asks Instantly to send the secret
  back in the `X-Karvon-Webhook-Secret` header (the `headers` field on the webhook).
  Both are compared in constant time, and the secret is stored encrypted.
- **Mailchimp** returns a `signing_secret` when the webhook is created. Deliveries
  carry `X-Mailchimp-Signature: t=…,v1=…`, an HMAC-SHA256 over `"{t}.{raw body}"`,
  verified within `KARVON_MAILCHIMP_WEBHOOK_TOLERANCE`. An audience with no stored
  secret rejects everything.

Both endpoints answer 2xx as soon as the delivery is stored, before any of it is
applied, so a slow application never causes the provider to disable the webhook.

Registering either webhook needs `KARVON_PUBLIC_BASE_URL`. Without it the module
works — reconciliation keeps the data correct — and the integrations page says so.

### Required Instantly scopes

Create the API v2 key with the narrowest set that works:

```
campaigns:read campaigns:create campaigns:update
leads:read leads:create leads:update leads:delete
accounts:read emails:read
webhooks:all webhook_events:read
background-jobs:read account_campaign_mappings:read
```

Never `all:all`. Instantly's limits are 100 requests per second and 6 000 per minute
for the whole workspace, shared across every key; `KARVON_INSTANTLY_RPS` paces our
own calls well below that, and leads are pushed in batches of
`KARVON_INSTANTLY_LEAD_BATCH` with `KARVON_INSTANTLY_LEAD_BATCH_GAP` between them.

Mailchimp allows ten simultaneous connections per account, which
`KARVON_MAILCHIMP_CONCURRENCY` stays under.

### The AI generator

The generator turns a campaign brief into components and variants. Both providers
implement one interface, and the UI calls the whole thing "ChatGPT".

**A ChatGPT subscription does not include API access.** OpenAI bills the API
separately, per token, against a key from platform.openai.com, and "Sign in with
ChatGPT" only authorises OpenAI's own Codex surfaces. So the default is a manual
round trip: the brief builds a prompt, the operator pastes it into ChatGPT, pastes
the JSON reply back, and the parsed output is imported. Set `KARVON_OPENAI_API_KEY`
and the same brief runs through the Responses API with a strict JSON schema instead
— identical prompt, identical parsing, identical review flow.

Generated content never goes live on its own. It arrives as `ai_generated` and a
human has to review and approve it before a campaign can use it.

### Analytics

Every number on the dashboard is computed from `email_sends` and `contact_events`,
which we own, keyed to the locked variant assignment. That is what makes questions
like "which subject earns the most positive replies" answerable at all.

Instantly's own analytics are stored per day in `campaign_analytics_snapshots` with
`source = 'instantly'` and shown beside ours, labelled. They are never merged: the
two count differently (Instantly counts unique opens per lead per step, we count
sends with a first open), and where they disagree by more than the greater of two or
five percent the campaign analytics response lists the metric in `mismatch`.

Open rate is reported but never used as a headline: privacy proxies pre-fetch
images, so a high open rate can mean nothing. Reply rate, positive reply rate,
bounce rate and newsletter conversion are the numbers that carry weight.


## Domains

Search, buy and manage domain names through the [Cloudflare Registrar
API](https://developers.cloudflare.com/registrar/registrar-api/) (beta). Cloudflare is
the registrar and the source of truth for what the account owns; Karvon stores only
what it asked for, at what price, and how each registration ended
(`domain_purchases`, `domain_purchase_items`).

**Connecting.** Settings → Domains takes the 32-character account ID and an API token
with the account's *Registrar* edit permission. The token lives encrypted in the
`sources` row of kind `cloudflare` (role `registrar`), like every other provider key;
the account ID lives in `registrar_settings`. Cloudflare charges the account's default
payment method and registers against its default registrant contact, so both must be
set up in the Cloudflare dashboard first.

**Search, check, buy.** Search is fast and cached — for discovery. Check asks the
registries directly and is what the pick list and the confirm dialog should show. A
purchase is the confirm button: it carries `confirm: true` and, for every domain, the
price the operator saw (`expected_cost_cents`). Before anything is queued every
domain is checked again; if any is gone, premium, or dearer than confirmed, nothing is
bought and the 409 lists which (`details[].field` is `domains[i]`). Each domain is
checked once more right before it is registered and skipped if its price rose. Cheaper
is fine. Registrations are billed on success and are **not refundable**.

**Ten at a time.** A purchase holds at most ten domains and only one purchase runs at
a time, so no more than ten registrations are ever in flight. Both limits are enforced
by the schema as well as the service: item positions are `0-9` and unique per
purchase, and a partial unique index allows one `queued`/`processing` purchase.

**Never paying twice.** Purchases run on the `domains` River queue with one worker,
registering one domain after another. A domain's row is marked `registering` before
Cloudflare is called, and the registration call is never retried by the client. If the
answer is lost (timeout, 5xx, a crash) the next pass asks Cloudflare — registration
status, then whether the account owns the domain — instead of sending again. Only
when Cloudflare has no trace of the call after two minutes is it resent, which is safe
because a domain can be registered only once. Anything still unconfirmed when the
purchase gives up (`KARVON_DOMAIN_MAX_WAIT`, or the worker's last attempt) becomes
`action_required` for a person to check in the Cloudflare dashboard, rather than a
guess either way. Switching the connection off stops the domains of a running
purchase that have not been sent yet.

**Managing.** `GET /domains/registrations` lists the account's domains live, including
ones bought outside Karvon. Automatic renewal is the only setting the API can change
today; turning it on authorises Cloudflare to charge the renewal up to 30 days before
expiry. Transfers, renewals and contact updates are not in Cloudflare's beta API yet,
and premium domains cannot be bought through it.

## Mailboxes

Google Workspace mail on domains whose DNS is in the Cloudflare account — every domain
bought under [Domains](#domains) — and the mailboxes the sending tool connects to.
Karvon drives the [Admin SDK Directory
API](https://developers.google.com/admin-sdk/directory) and the [Site Verification
API](https://developers.google.com/site-verification) as a service account with
domain-wide delegation, and publishes DNS through the Cloudflare connection.

**One Workspace account, many domains.** Every domain is added to one Workspace
account as a secondary domain (up to 600 per account). Make that a Workspace account
used only for outreach, never the one the business runs on: if Google suspends it,
every mailbox in it stops. Its super admin should be a user that never sends.
Business Starter is billed per user, not per domain, and Business plans cap an account
at 300 users; the 14-day trial caps it at 10.

**Connecting.**

1. In the Google Cloud console create a project, enable the *Admin SDK API* and the
   *Site Verification API*, create a service account and download a JSON key.
2. In the Workspace Admin console open Security → Access and data control → API
   controls → Domain-wide delegation, add the service account's client ID (shown under
   Settings → Mailboxes once the key is saved) with the scopes Settings → Mailboxes
   lists: `admin.directory.domain`, `admin.directory.user`, `siteverification`.
3. Under Settings → Mailboxes paste the key and a super admin's address, test, enable.
4. The Cloudflare API token needs **Zone → Zone → Read** and **Zone → DNS → Edit** on
   the domains as well as the Registrar permission.

The key lives encrypted in the `sources` row of kind `google_workspace` (role
`mailboxes`); the admin email and the service account's identity live in
`workspace_settings`.

**Setting a domain up.** `POST /workspace/domains` takes the domain, one to five
mailboxes and `confirm: true`. Before anything is queued Karvon checks both
connections and that the domain has a zone in the Cloudflare account. A worker on the
`workspace` River queue then adds the domain to Workspace, publishes the Google
verification TXT, MX (`smtp.google.com`), SPF (`v=spf1 include:_spf.google.com ~all`)
and DMARC (`v=DMARC1; p=none`), and asks Google to verify the domain — snoozing every
`KARVON_WORKSPACE_POLL_INTERVAL` while Google cannot see the record yet, up to
`KARVON_WORKSPACE_VERIFY_MAX_WAIT`. Then it creates the mailboxes with generated
20-character passwords, stored encrypted and readable through
`GET /workspace/mailboxes/{id}/credentials` for connecting them to the sending tool.

**DNS is never overwritten.** Records already right are left alone. Records that say
the domain has no mail — a null MX, `v=spf1 -all` — are replaced. Any other MX or SPF
record fails the setup with `dns_conflict` and says what to remove; an existing DMARC
policy is kept as chosen.

**Never paying twice.** Each mailbox is a paid licence. A creation is never repeated
by the client; the attempt is counted before each call, and Google refuses a second
user with the same address. So "already exists" on a mailbox's first attempt means
the address belongs to someone else (`address_taken`, left alone), and on a later
attempt that an earlier call landed without its answer arriving (recorded as
created). A throttled or unauthorised call does not count as an attempt.

**DKIM is the manual step.** Google has no API for DKIM keys. When a setup reaches
`dkim_required`, open Apps → Google Workspace → Gmail → Authenticate email in the
Admin console, pick the domain, generate a record and send its value to
`PUT /workspace/domains/{domain}/dkim`. Karvon publishes it at
`google._domainkey.<domain>` (or the chosen selector) and the domain becomes `active`;
then press *Start authentication* in the Admin console. Sending the value again
replaces the key.

**When it stops.** A setup that needs a person — refused credentials, a DNS conflict,
a domain Google refuses or that belongs to another Workspace account, verification
that never came through — becomes `failed` with `error_code` and `error_message`.
`POST /workspace/domains/{domain}/retry` resumes it: finished steps are skipped,
refused mailboxes are sent again, taken addresses stay failed. Mailboxes and domains
are never deleted by Karvon; remove them in the Admin console to stop their billing.

**More mailboxes later.** `POST /workspace/domains/{domain}/mailboxes` adds mailboxes
to a domain in `dkim_required` or `active`. The domain goes back to `provisioning`
while they are created — every finished step is skipped — and returns to where it
was. Five per domain counts failed ones too; `DELETE /workspace/mailboxes/{id}`
removes a failed one (it exists nowhere but in Karvon). A created mailbox is a
Workspace user and is only ever deleted in the Admin console.

**Connecting to Instantly.** Instantly has no way to take a Google Workspace mailbox
with a password alone — Google no longer allows password sign-in for IMAP/SMTP, and
app passwords need a person per mailbox — so Karvon uses Instantly's [Google OAuth
API](https://developer.instantly.ai/oauth-connection-flow).
`POST /workspace/mailboxes/{id}/instantly` starts a session and returns `auth_url`,
Google's sign-in page pre-filled with the mailbox; a person opens it, signs in with
the password from `/credentials` (first sign-in also asks to accept Google's terms)
and allows Instantly. The session lives ten minutes. A worker follows it every
`KARVON_WORKSPACE_INSTANTLY_POLL_INTERVAL`: on success it records the account, turns
warmup on when `warmup: true` was sent, and queues the sending-accounts sync so the
mailbox can be put on a campaign; a refusal (`account_exists`, a different account
signed in) or an expired session is recorded in `instantly_status` / `instantly_error`
and connecting again starts a new session. The Instantly key needs the
`accounts:create`, `accounts:read` and `accounts:update` scopes. If Google says the
app is blocked, allow Instantly under Security → API controls → Manage third-party
app access in the Admin console.

## Frontend coverage

Every screen in the frontend plan, mapped to what serves it:

| Frontend page | Needs | Endpoint(s) |
| --- | --- | --- |
| Dashboard `/scraper` | 4 stat cards, emails-per-job chart, recent jobs | `GET /stats/scraper` (`businesses`, `with_email`, `jobs_total`, `last_job`, `emails_per_job`), `GET /jobs?per_page=10` |
| Create scrape `/scraper/new` | source picker, estimate card, submit | `GET /sources`, `POST /jobs/estimate`, `POST /jobs` → redirect to the returned `id` |
| Scrape history `/scraper/jobs` | table of name/status/source/queries/listings/emails/started/duration, filters, row actions | `GET /jobs` (each row carries `source_name`, `source_kind`, `stats`, `started_at`, `finished_at`), `POST /jobs/{id}/rerun`, `POST /jobs/{id}/cancel`, `DELETE /jobs/{id}` |
| Job detail `/scraper/jobs/:id` | header + progress, Overview / Results / Log tabs, export | `GET /jobs/{id}`, `GET /jobs/{id}/events` (SSE), `GET /businesses?job_id=`, `GET /jobs/{id}/export.csv` |
| Scraped emails `/scraper/emails` | master table, filters, bulk actions, detail sheet | `GET /businesses` (`primary_email`, `primary_email_source`, `primary_email_verified_status`, `first_job_id`, `first_job_name`), `GET /businesses/{id}`, `PATCH /businesses/{id}`, `POST /businesses/bulk`, `POST /businesses/export` |
| Sources `/scraper/sources` | masked key field, test button, cost, enabled toggle | `GET /sources`, `PUT /sources/{id}`, `POST /sources/{id}/test` |
| ⌘K palette | recent jobs | `GET /jobs?per_page=10` |
| SSE fallback polling | job status every 3 s | `GET /jobs/{id}` |

Three fields were added beyond the backend plan's table because a frontend column
needs them and nothing else could supply them:

- `Business.first_job_name` — the "first seen job" column needs a name, not only an id.
  It is `null` once that job is deleted.
- `Business.primary_email_verified_status` — the placeholder column for the Verify
  module; always `null` today.
- `Job.source_name` / `Job.source_kind` — the history table shows the provider.

---

## Decisions and deviations from the plan

Recorded where the implementation departs from the backend plan, or where the two
plans disagree.

1. **`POST /businesses/export` streams the CSV; it does not return a file URL.** The
   frontend plan says that above 5 000 rows it will "call `POST /businesses/export` and
   download the returned file URL", while the backend plan specifies a streamed
   response with a job-backed file deferred to later. The backend plan wins: the
   endpoint streams `text/csv` with a `Content-Disposition` attachment in constant
   memory. The frontend can save the response body directly; when the deferred
   job-backed variant lands it will be a `202` with a location, which is an additive
   change.

2. **Source ids are UUIDs.** The backend plan's prose shows `POST /sources/1/test`, but
   the shared conventions say "IDs are UUIDv7 strings". Consistency won: the two
   providers are seeded with fixed UUIDs in `migrations/00002_seed_sources.sql`, so the
   frontend can hardcode them in development if it wants to.

3. **List endpoints use hand-written SQL, everything else uses sqlc.** `GET /jobs` and
   `GET /businesses` combine many optional filters with a client-chosen sort. Expressing
   that in sqlc requires `ORDER BY CASE`, which defeats the `created_at` and trigram
   indexes — exactly the indexes the "under 100 ms at 50 k rows" requirement depends on.
   Those four queries are assembled in `internal/db/*_query.go` from **positional
   placeholders only**; the sort is resolved against a whitelist and an unknown value is
   a 422. No user input is ever concatenated into SQL. Every other query is sqlc.

4. **The HTTP layer is one package, not `http/handlers/`.** The plan's tree puts
   handlers in a subpackage while `errors.go` stays in `http/`. Since handlers must use
   the error envelope, that split creates an import cycle. `internal/http` is therefore
   a single package with one file per resource; `middleware/` remains a subpackage.

5. **The provider API key travels in a header, never the URL.** Apify's documented
   endpoint accepts `?token=`, which would put the secret into every proxy and access
   log; the equivalent `Authorization: Bearer` header is used instead, and a test
   asserts the key never appears in a query string.

6. **A domain crawled recently has its addresses copied, not just skipped.** The plan
   says to skip a domain already crawled within 30 days. Skipping alone would leave the
   new job's results without an address, so the existing addresses are copied onto the
   new business row and a log line records the reuse.

7. **Go 1.26.** The plan asks for "Go 1.23+". The current River and goose releases
   require 1.26, so that is the floor in `go.mod` and in the Dockerfile.

8. **`business_emails.verified_status` stays `null`.** It is written by the Verify
   module, which is out of scope; the column and the API field exist so that module is
   additive.

9. **Job configs are normalised before storage.** Terms and locations are trimmed,
   de-duplicated case-insensitively and blanks dropped, so `queries_total` reflects the
   work that will actually run rather than what was typed.

10. **`chi`'s `RealIP` middleware is not used.** It rewrites `RemoteAddr` from
    `X-Forwarded-For` whether or not a trusted proxy set it, which lets any client forge
    the address in the access log. The log records the real peer instead. Reintroduce it
    only behind a proxy you control.

11. **A job's `config.concurrency` is a per-process cap.** It limits how many sites one
    job crawls at once inside a worker process, on top of the `KARVON_CRAWL_CONCURRENCY`
    pool. Several worker replicas multiply it; the guarantee that a single site is never
    hit more than once per second comes from the per-host limiter, which is independent
    of this setting.

12. **Reacher ships disabled.** `check-if-email-exists` is dual-licensed AGPL-3.0 /
    commercial and Karvon is proprietary and network-served, so enabling it is a
    licensing decision rather than a configuration one. The client, the Compose
    service and the settings are all in place; `KARVON_VERIFY_REACHER_ENABLED`
    defaults to `false` and the service logs a warning when it is turned on. See
    "Licensing" above.

13. **A provider that does not answer is left out of the average, not scored 0.**
    Weighting a missing provider as zero would mean a Reacher outage silently
    downgraded every address in the database. Its weight is redistributed over the
    providers that did answer, and the row records why it is missing. The corollary
    is that a free score computed during an outage is a genuine score from fewer
    opinions, not a degraded one — `provider_results` is what tells them apart.

14. **Disqualifying findings are not weighted.** Bad syntax, a disposable domain, an
    automated mailbox and a domain with no mail server zero the free score outright.
    Weighting combines *opinions* about whether a mailbox will accept mail; it has no
    business averaging away a fact, and a provider that never checked for that fact
    has nothing to weigh against it. This is also what preserves the pre-existing
    behaviour of the local hard fails exactly.

15. **The paid gate is a band, not a floor.** The existing rule (never pay below 50)
    is kept as the floor, and a ceiling sits above it for operators who want one.
    Both bounds are in the settings and both are re-checked in the worker immediately
    before spending. The ceiling ships at 90 — one above `FreeMaxScore`, so it binds
    on nothing until it is lowered: a free score describes the domain, never the
    mailbox, so skipping the paid check on high scorers buys confidence that was not
    measured. See "When the paid provider runs" for when lowering it is the right
    trade.

16. **`free_score` is a new column rather than a reinterpretation of `pass1_score`.**
    `pass1_score` still means what it always did — the local pipeline's own raw score
    on its own 0–85 scale — and the Pass 1 breakdown is untouched. Existing rows were
    backfilled by rescaling, so they stayed eligible for exactly the paid runs they
    were eligible for before the migration.

17. **Instantly sees one variant per step, and we render the email.** Instantly's
    API has no per-variant weight: a step's variants are rotated or auto-selected by
    Instantly, and its step analytics report a variant index that nothing ties back
    to a component. Weighted distribution and per-component analytics were both
    requirements, so the campaign we create at Instantly has exactly one variant per
    step, `{{k_subject_N}}` / `{{k_body_N}}`, and every lead carries its own rendered
    subject and body as custom variables. The trade is that Instantly's own A/B
    screen shows one variant; ours shows the real experiment. `campaigns.settings`
    keeps a `variant_mode` key so a future native-variant mode is additive.

18. **Provider credentials reuse the `sources` table.** Instantly and Mailchimp are
    stored as two more rows with the existing encrypted-key column, the existing
    Sources page and the existing connection test, rather than a new `integrations`
    table. `role` gained `outreach` and `newsletter`; `kind` gained `instantly` and
    `mailchimp`. The OpenAI key is the exception: it is environment-only, because it
    is a single global setting with no per-row lifecycle.

19. **The Instantly webhook is authenticated by a secret we generate.** Instantly
    does not sign webhooks and sends no event id. Registration therefore generates a
    path token and a shared secret and asks Instantly to echo the secret in a header;
    the dedupe key is a hash of the payload's identifying fields. Mailchimp does sign,
    and that signature is verified.

20. **Mailchimp defaults to `pending`, not `subscribed`.** A contact is pushed as
    `pending` — double opt-in, Mailchimp asks them to confirm — unless an active
    consent record exists *and* the audience has `allow_single_opt_in` switched on,
    which is off by default. A previously unsubscribed or bounced address comes back
    as "compliance blocked" and is left alone: only the contact can re-subscribe
    themselves. The consent gate is evaluated twice, once when queueing and again
    inside the job immediately before the call.

21. **Campaign progress is polled, not streamed.** The SSE stream is bound to
    `job_events.job_id`, which references `jobs`. Campaigns follow the verification
    module's precedent and are polled through `GET /campaigns/{id}` and
    `GET /campaigns/{id}/activity`.
