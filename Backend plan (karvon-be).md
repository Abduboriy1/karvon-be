# Backend plan (`karvon-be`)

Go API + worker in one binary: REST for the dashboard, a Postgres-backed job queue for scrapes, SSE for live progress. Built so Verify and Send become new packages and new job kinds, not new services.

## BE stack & project setup

| Concern | Choice | Why |
| --- | --- | --- |
| Language | Go 1.23+, single module `github.com/<you>/karvon-be` | Static binary, easy concurrency for crawling |
| HTTP router | `chi` v5 + `net/http` | Small, middleware-friendly, stdlib handlers |
| OpenAPI | `oapi-codegen` from `api/openapi.yaml` (spec-first) | FE generates its types from the same file; served at `/openapi.json` |
| Database | Postgres 16 via `pgx` v5 | JSONB for job config, good full-text/trigram search on business names |
| Queries | `sqlc` (typed queries, no ORM) | Compile-time checked SQL |
| Migrations | `goose` (SQL files in `migrations/`) | Runs on startup in dev, as a CLI step in prod |
| Job queue | [River](https://riverqueue.com) (Postgres-backed) | No Redis; retries, cancellation, unique jobs built in; one binary runs API + workers |
| Google Maps providers | Apify API (`compass/crawler-google-places`), Outscraper API | Both behind a `Provider` interface |
| HTTP crawling | `net/http` + `golang.org/x/net/html` + `colly` optional | Keep the crawler dependency-light |
| Config | `caarlos0/env` from `KARVON_*` vars + `.env` for dev | 12-factor |
| Logging | `log/slog` JSON; request ID middleware |  |
| Validation | `go-playground/validator` on request DTOs |  |
| Tests | `testing` + `testcontainers-go` for Postgres; `httptest` for handlers |  |
| Lint | `golangci-lint` (default + `gosec`, `errcheck`) |  |
| Build/run | `Makefile` (`make dev`, `make gen`, `make migrate`, `make test`), `docker-compose.yml` (postgres + api), multi-stage `Dockerfile` (distroless) |  |

**Bootstrap commands:**

```bash
mkdir karvon-be && cd karvon-be && go mod init github.com/<you>/karvon-be
go get github.com/go-chi/chi/v5 github.com/jackc/pgx/v5 github.com/riverqueue/river \
  github.com/riverqueue/river/riverdriver/riverpgxv5 github.com/pressly/goose/v3 \
  github.com/caarlos0/env/v11 github.com/go-playground/validator/v10 github.com/google/uuid golang.org/x/net
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
```

**Env (`.env.example`):** `KARVON_ADDR=:8080`, `KARVON_DATABASE_URL=postgres://karvon:karvon@localhost:5432/karvon`, `KARVON_API_KEY=dev-key`, `KARVON_CORS_ORIGIN=http://localhost:5173`, `KARVON_CRAWL_CONCURRENCY=8`, `KARVON_CRAWL_TIMEOUT=10s`, `KARVON_CRAWL_USER_AGENT=...`. Provider API keys are stored in the `sources` table (entered from the Sources page), encrypted at rest with `KARVON_SECRET_KEY` (AES-GCM).

## Data model

Six tables. `businesses` is the long-lived master list (deduped across jobs); everything else is per job.

```mermaid
flowchart LR
  sources --> jobs
  jobs --> job_queries
  jobs --> job_events
  job_queries --> job_results
  job_results --> businesses
  businesses --> business_emails
```

| Table | Key columns | Notes |
| --- | --- | --- |
| `sources` | `id`, `kind` (`apify` / `outscraper`), `name`, `api_key_enc`, `cost_per_1k_cents`, `enabled`, `last_tested_at` | One row per provider |
| `jobs` | `id`, `name`, `status` (`queued`/`running`/`done`/`failed`/`cancelled`), `source_id`, `config` JSONB (terms\[\], locations\[\], max\_per\_query, crawl\_emails bool, concurrency), `stats` JSONB (queries\_total/done, listings\_found, sites\_total/crawled, emails\_found, cost\_cents), `error`, `created_at`, `started_at`, `finished_at` | `config` is immutable after create; re-run copies it to a new job |
| `job_queries` | `id`, `job_id`, `term`, `city`, `state`, `status`, `listings_found`, `provider_run_id` | One row per term × location; unit of provider work |
| `job_results` | `job_id`, `business_id`, `query_id`, PK(`job_id`,`business_id`) | Links a job to the businesses it found; drives the Results tab and per-job export |
| `businesses` | `id`, `place_id` UNIQUE (Google Maps id), `name`, `category`, `address`, `city`, `state`, `zip`, `phone`, `website`, `domain`, `rating`, `reviews`, `lat`, `lng`, `raw` JSONB, `first_job_id`, `suppressed` bool, `created_at`, `updated_at` | Upsert on `place_id`; `domain` normalized (no `www.`) with an index; trigram index on `name` |
| `business_emails` | `id`, `business_id`, `email` (citext), `source` (`mailto`/`regex`/`provider`), `page_url`, `is_primary`, `verified_status` (null for now), `found_at`, UNIQUE(`business_id`,`email`) | Primary = best-ranked (`info@` > `hello@` > `contact@` > first found) |
| `job_events` | `id` bigserial, `job_id`, `ts`, `type` (`progress`/`log`/`status`), `data` JSONB | Append-only; SSE replays from `?after=<id>`; pruned after 30 days |

**Indexes that matter:** `businesses(domain)`, `businesses(state, category)`, `business_emails(email)`, `job_events(job_id, id)`, `job_results(job_id)`, GIN trigram on `businesses(name)`.

**Dedupe rules:** same `place_id` → same business; no `place_id` (rare) → match on (`domain`) then (`phone`, `zip`). Emails are lowercased; role addresses to drop at insert: `noreply@`, `no-reply@`, `postmaster@`, `abuse@`, plus a denylist of platform domains (`wixpress.com`, `sentry.io`, `godaddy.com`, `example.com`).

## API endpoints (`/api/v1`)

All routes require `Authorization: Bearer <KARVON_API_KEY>` except `/healthz` and `/openapi.json`. Every endpoint is defined in `api/openapi.yaml` first; handlers are generated stubs.

| Method & path | Purpose | Request / response notes |
| --- | --- | --- |
| `GET /healthz` | Liveness + DB ping | `{ "ok": true, "db": true, "queue": true }` |
| `GET /openapi.json` | Spec for FE codegen |  |
| `GET /stats/scraper` | Dashboard cards + chart | `{ businesses, with_email, jobs_total, last_job, emails_per_job: [{job_id, name, emails}] }` |
| `GET /sources` | List providers | Never returns the key; returns `has_key: bool` |
| `PUT /sources/{id}` | Set key, cost, enabled | Key encrypted before insert |
| `POST /sources/{id}/test` | Validate key with a 1-result call | 200 or `{error.code: "provider_auth"}` |
| `POST /jobs/estimate` | Cost/size estimate before creating | Body = job config; returns `{ queries, est_listings, est_cost_cents }` |
| `POST /jobs` | Create + enqueue | Validates config (≤ 500 queries, terms 1–20, locations 0–300); a location may give a city, a state, or both, and an empty `locations` (or an entry with neither field) expands to one query per US state; returns 201 job |
| `GET /jobs` | History | Filters `status`, `source_id`, `from`, `to`, `q`; sorted `created_at desc` |
| `GET /jobs/{id}` | Detail incl. `stats` |  |
| `POST /jobs/{id}/cancel` | Cooperative cancel | Sets status, River cancels the running job; workers check `ctx.Done()` |
| `POST /jobs/{id}/rerun` | Clone config to a new job | Returns new job |
| `DELETE /jobs/{id}` | Delete job, queries, events, results | Businesses stay |
| `GET /jobs/{id}/events` | **SSE** stream | `text/event-stream`; supports `Last-Event-ID`; sends `: ping` every 15 s; closes after terminal status |
| `GET /jobs/{id}/export.csv` | Per-job CSV | Streams rows, `Content-Disposition: attachment` |
| `GET /businesses` | Master list | Filters `job_id`, `category`, `state`, `city`, `has_email`, `suppressed`, `q` (name/domain trigram); pagination; includes `primary_email` |
| `GET /businesses/{id}` | Detail with all emails |  |
| `PATCH /businesses/{id}` | `suppressed`, notes |  |
| `POST /businesses/bulk` | \`{ ids\[\], action: "suppress" | "unsuppress" }\` |
| `POST /businesses/export` | CSV for a filter set | Same filters as list; streams; > 50k rows → 202 + job-backed file (later) |

**SSE event shapes:**

```json
event: progress
id: 1042
data: {"queries_done":12,"queries_total":40,"sites_crawled":180,"sites_total":611,"emails_found":97}

event: log
id: 1043
data: {"level":"info","msg":"crawled ironworksgym.com: 2 emails","ts":"2026-09-19T15:04:05Z"}

event: status
id: 1044
data: {"status":"done","finished_at":"2026-09-19T15:09:12Z"}
```

**Middleware stack:** request ID → real IP → slog logger → recoverer → CORS (`KARVON_CORS_ORIGIN`) → API-key auth → timeout 30 s (skipped for SSE and CSV routes).

## Scrape pipeline & workers

A job runs as three River job kinds chained per stage, so a crash resumes at the stage that failed and the FE sees progress per stage.

```mermaid
flowchart LR
  A[ScrapeJob\nexpand terms × locations] --> B[QueryJob × N\nprovider search]
  B --> C[upsert businesses\n+ job_results]
  C --> D[CrawlJob × M\nfetch site, extract emails]
  D --> E[FinalizeJob\nstats, status=done]
```

**Stage 1 — `ScrapeJob` (one per job):** loads config, inserts `job_queries` rows, enqueues one `QueryJob` per row (River unique on `query_id`), sets status `running`, emits `progress`.

**Stage 2 — `QueryJob` (fan-out, concurrency 4):** calls the provider through a `Provider` interface:

```go
type Provider interface {
    Search(ctx context.Context, q SearchQuery) ([]Listing, error) // term, city, state, max
    Name() string
}
```

- `apify.Provider`: POST run-sync-get-dataset-items on `compass~crawler-google-places` with `searchStringsArray`, `locationQuery`, `maxCrawledPlacesPerSearch`; 5-minute timeout; parse `placeId`, `title`, `website`, `phone`, `address`, `categoryName`, `totalScore`, `reviewsCount`, `location`.
- `outscraper.Provider`: `GET /maps/search-v3` with `query`, `limit`, `async=false`; same mapping.
- Results upsert into `businesses` on `place_id`, insert `job_results`; update `job_queries.status`; emit `progress` + one `log` line per query. Provider errors retry 3× with backoff; auth errors fail the job immediately with `provider_auth`.
- When all queries for a job are terminal (checked with a single `SELECT count(*) ... WHERE status NOT IN (...)`), the last one enqueues `CrawlJob`s for every business in `job_results` with a `website` and no email yet (skip if `crawl_emails=false` → go straight to Finalize).

**Stage 3 — `CrawlJob` (fan-out, concurrency `KARVON_CRAWL_CONCURRENCY`, per-host limiter 1 req/s):**

1. Normalize URL, resolve domain, skip denylisted platforms and any domain crawled in the last 30 days that already has emails.
2. Fetch `/` with a 10 s timeout, 2 MB body cap, browser-like `User-Agent`, follow ≤ 3 redirects, respect `robots.txt` (cache per host).
3. Extract from `mailto:` links first, then regex `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}` over visible text and `href`s; discard image-like matches (`.png@`), role denylist, and emails whose domain is a known platform.
4. If none found, try up to 3 more paths from the same host in order: links whose text/href contains `contact`, `about`, `book`, then `/contact`, `/contact-us`, `/about`.
5. Insert `business_emails` (unique per business), pick `is_primary` by rank, emit a `log` line, bump `sites_crawled` / `emails_found` in `jobs.stats` with an atomic JSONB update.
6. Never raise on a single site's failure; record it as a `log` at `warn`.

**Stage 4 — `FinalizeJob`:** recomputes `stats` from tables (source of truth, not counters), sets `status=done` (or `failed` if > 50% of queries failed), `finished_at`, emits `status` event.

**Cancellation:** `POST /jobs/{id}/cancel` sets `status=cancelled` and calls `river.JobCancel` on every queued/running job with that `job_id` in args; workers check `ctx.Err()` between sites/queries and exit cleanly.

**Cost tracking:** each `QueryJob` adds `listings_found × cost_per_1k_cents / 1000` to `stats.cost_cents` so the Job detail shows real spend; the estimate endpoint uses `max_per_query × queries × cost_per_1k`.

**Event fan-out:** workers `INSERT INTO job_events` then `NOTIFY job_events, '<job_id>'`; the SSE handler `LISTEN`s and reads new rows `WHERE id > last_sent` — works across multiple API replicas without an in-process bus.

## BE structure, config & handoff

**Folder structure (standard Go layout, package per domain):**

```
karvon-be/
├─ cmd/
│  ├─ api/main.go          HTTP server + River workers in one process (flag --workers=0 to run API only)
│  └─ migrate/main.go      goose wrapper
├─ api/openapi.yaml        spec-first; oapi-codegen → internal/http/gen
├─ internal/
│  ├─ config/              env loading
│  ├─ db/                  pgx pool, sqlc output (queries/*.sql → db/*.go), tx helpers
│  ├─ http/                router.go, middleware/, handlers/{jobs,businesses,sources,stats,events}.go, sse.go, errors.go
│  ├─ scraper/
│  │  ├─ provider/         provider.go (interface), apify/, outscraper/, fake/ (for tests)
│  │  ├─ crawler/          fetch.go, extract.go, robots.go, denylist.go
│  │  ├─ jobs/             scrape_job.go, query_job.go, crawl_job.go, finalize_job.go (River workers)
│  │  └─ service.go        Create/Estimate/Cancel/Rerun orchestration
│  ├─ events/              publisher (INSERT + NOTIFY), listener (LISTEN → channel)
│  ├─ business/            dedupe, email ranking, export (CSV streaming)
│  └─ crypto/              AES-GCM for provider keys
├─ migrations/             0001_init.sql …
├─ queries/                sqlc SQL
├─ sqlc.yaml  Makefile  docker-compose.yml  Dockerfile  .env.example  .golangci.yml
└─ tests/                  integration (testcontainers), fixtures/ (recorded provider JSON)
```

**Docker:** `docker-compose.yml` = `postgres:16` + `api` (build from Dockerfile, `depends_on` healthy DB, runs migrations on boot in dev). Dockerfile: `golang:1.23` build → `gcr.io/distroless/static` runtime, non-root, `EXPOSE 8080`.

**Handoff prompt for a coding agent** (paste into Claude Code in the `karvon-be` repo after `go mod init`):

```markdown
You are building `karvon-be`, a Go 1.23 service: chi HTTP API + River (Postgres) job workers
in one binary. Read docs/PLAN.md (this tab) fully before writing code. Spec-first: write
api/openapi.yaml for every endpoint in the plan, generate handlers with oapi-codegen, and keep
the spec the single source of truth (the FE generates its types from /openapi.json).

Build in this order, committing after each step with a conventional-commit message:
1. Skeleton: config, pgx pool, goose migrations for all six tables + indexes, sqlc setup,
   /healthz, API-key middleware, CORS, slog, docker-compose with Postgres. `make dev` must boot.
2. openapi.yaml + generated server; sources endpoints with AES-GCM key storage and /test.
3. Jobs: estimate, create (validation), list/detail, rerun, delete; River client wired in;
   ScrapeJob + QueryJob using a `fake` provider that returns fixtures, then the Apify provider,
   then Outscraper. Record real provider responses into tests/fixtures once and replay them.
4. Crawler package with unit tests on extract.go (mailto, regex, denylist, ranking) and
   robots handling; CrawlJob + FinalizeJob; cancel endpoint.
5. job_events publisher/listener (INSERT + NOTIFY/LISTEN), SSE handler with Last-Event-ID
   replay and 15 s pings; businesses list/detail/patch/bulk/export (streamed CSV); stats.
6. Integration tests with testcontainers: create job with fake provider → events stream
   → businesses visible → export CSV. golangci-lint clean.

Rules: no ORM (sqlc only); every handler returns the error envelope
{ "error": { "code", "message" } }; contexts everywhere, no goroutines without ctx; per-host
rate limit and robots.txt are mandatory in the crawler; never log provider keys; stop and ask
before adding a dependency not listed in the plan.
```

## BE milestones & acceptance

About 6 working days; the OpenAPI spec (day 2) unblocks the FE, so do it early even with stub handlers.

| Day | Milestone | Done when |
| --- | --- | --- |
| 1 | Skeleton, migrations, config, healthz, auth middleware, compose | `make dev` boots; `curl /healthz` returns ok with db+queue true |
| 2 | `openapi.yaml` complete, generated server, sources CRUD + key encryption + test | FE can run `pnpm gen:api`; `POST /sources/1/test` distinguishes bad vs good key |
| 3 | Jobs endpoints, River wiring, ScrapeJob + QueryJob with fake and Apify providers | Creating a job with 2 terms × 2 cities produces 4 `job_queries` and upserts businesses |
| 4 | Crawler + CrawlJob + FinalizeJob + cancel | A 50-site job finds emails on ≥ 40% of sites with websites; cancel stops within 5 s |
| 5 | Events + SSE, businesses list/detail/bulk/export, stats | FE job detail updates live; export of 10k rows streams in < 3 s |
| 6 | Outscraper provider, integration tests, lint, Dockerfile, README | `make test` green in CI; image runs with only env vars |

**Acceptance checklist**

- [ ] Restarting the process mid-job resumes it (River re-leases stuck jobs) and the FE reconnects via `Last-Event-ID` with no lost events
- [ ] Two jobs finding the same place produce one `businesses` row and two `job_results` rows
- [ ] Crawler never exceeds 1 req/s per host and honors `robots.txt` `Disallow: /`
- [ ] Provider keys are encrypted in the DB and absent from all logs and API responses
- [ ] `GET /businesses?q=iron` uses the trigram index (verify with `EXPLAIN`) and returns under 100 ms at 50k rows
- [ ] `job_events` older than 30 days pruned by a periodic River job
- [ ] `golangci-lint run` and `go vet` clean; no `context.Background()` in request or worker paths
