# Verification plan (`karvon-be`)

> **Status: implemented.** The defaults in section 0 were taken. This document is
> kept as the design record; where the build refined the design, the text below has
> been updated to match the code rather than the original intent. The user-facing
> summary lives in the README under "Email verification".

Email verification for Karvon: a free, local **Pass 1** (syntax, blocklists, DNS, heuristics) that runs on every address, and a paid **Pass 2** (third-party API) that only sees addresses Pass 1 could not rule out. Both passes are River jobs in the existing binary; the UI reads scores from Postgres, so a provider outage can never break a page.

Stack: Go 1.26 + chi + oapi-codegen (spec-first) + sqlc + goose + River + Postgres 16. Zero new Go dependencies: RFC 5322 parsing is `net/mail`, DNS is `net.Resolver`, RDAP is `net/http`, Damerau-Levenshtein and the token bucket are ~40 lines each.

---

## 0. Decisions to confirm before code

These are the places where the brief and the existing product disagree, or where the brief leaves room. Each has a default; say "go" and the defaults ship.

| # | Question | Default in this plan | Why it matters |
| --- | --- | --- | --- |
| 1 | **Role accounts as hard fail.** The brief lists `info@`, `contact@`, `sales@`, `support@`, `billing@`, `admin@` as hard fails (score 0, RED). The scraper's primary-email ranking (`business.PickPrimary`) deliberately *prefers* `info@` > `hello@` > `contact@`, because for small businesses that is usually the only address on the site. | Ship per brief, but split the list into `role_hard.txt` (noreply, postmaster, abuse, mailer-daemon, bounce…) and `role_soft.txt` (info, contact, sales, support, billing, admin, hello, office) with `KARVON_VERIFY_ROLE_SOFT_MODE=hard\|penalty` (default `hard`). `penalty` mode caps the score at 60 (YELLOW) instead of zeroing it, so those addresses still reach Pass 2. | With the default, most scraped primary emails will turn RED and never be sent to Pass 2. |
| 2 | **Domain age via RDAP vs "fully offline except DNS".** RDAP is HTTP, not DNS. | Allow RDAP as the one other network call, cached per domain for 30 days, 5 s timeout, any failure = check `skip` (0 points). `KARVON_VERIFY_RDAP_ENABLED=false` turns it off; Pass 1 max then becomes 75. | Only affects +10 points; never blocks. |
| 3 | **"Not a catch-all-suspect free provider pattern" (+5).** Ambiguous. | Interpreted as: +5 when the domain is **not** a free/webmail provider (`free_providers.txt`: gmail, yahoo, hotmail, outlook, icloud, aol, proton…). Rationale: for those domains MX/SPF/DMARC/age say nothing about the mailbox, so a custom domain earns the small bonus. | Cosmetic; rename the check if you meant something else. |
| 4 | **Where verification lives.** The brief says "emails table (or extend existing)". | A new `email_verifications` table keyed by normalized email (one row per address), joined to `business_emails` by the citext email. `business_emails.verified_status` is dropped. | The same address appears under several businesses; one row per address is what makes the 90-day cache and "never re-bill" trivial. |
| 5 | **Pass 2 provider.** | **Emailable** (`GET /v1/verify`, states `deliverable / undeliverable / risky / unknown` map 1:1 onto the brief's scale; `GET /v1/account` gives credits; test-mode keys return deterministic results without billing). Alternatives that fit the same adapter: ZeroBounce, NeverBounce, Kickbox. | The adapter interface isolates this; the choice only touches one package. |
| 6 | **Verifier credentials.** | Reuse the `sources` table (encrypted key, cost, enabled, test button). Add `role` column (`maps` \| `verifier`) and kind `emailable`; the Sources page shows it as a second group. `POST /sources/{id}/test` on a verifier source calls `Balance()`. | No second key-management UI. |
| 7 | **Live progress for bulk runs.** | Polling (`GET /verification/runs/{id}` every 2 s while active). No SSE for runs in v1; the job_events/SSE machinery is keyed to scrape jobs. | Keeps the change out of the events schema. |
| 8 | **Wire value for the tag.** | Colour names as the enum: `green`, `light_green`, `yellow`, `orange`, `red`. The API also returns `tag_label`. The UI maps tag → colour token + label + icon in one place. | Matches the brief literally ("tag (color)"). |

---

## 1. Scoring: weights and boundaries (verified)

Pass 1 soft signals:

| Check | Points |
| --- | ---: |
| MX records present and resolve | 30 |
| Domain has valid A/AAAA | 10 |
| Domain age > 1 year (RDAP) | 10 |
| SPF record present | 5 |
| DMARC record present | 5 |
| Local part looks human | 10 |
| Not a free-provider domain | 5 |
| No typo against top-domain list | 5 |
| Length and structure sane | 5 |
| **Sum** | **85** |

Tag mapping (`final_score = pass2_score if Pass 2 succeeded else pass1_score`):

| Range | Tag | Label |
| --- | --- | --- |
| 90–100 | `green` | Verified |
| 70–89 | `light_green` | Likely valid |
| 50–69 | `yellow` | Uncertain |
| 30–49 | `orange` | Risky |
| 0–29 | `red` | Invalid |

Consequences, all intended:

- Pass 1 max 85 → best possible local result is `light_green`. Only Pass 2 (`deliverable` → 100) reaches `green`. ✔
- Typo detected → score capped at 40 → `orange`, below the Pass 2 gate (≥ 50), so a typo'd address is never billed. ✔
- Hard fail → 0 → `red`. ✔
- Pass 2 `risky` → 70 → `light_green`; `undeliverable` → 0 → `red`; `unknown` → keep Pass 1 score and flag for retry. ✔
- Pass 2 gate at 50 means an address needs at least MX (30) + A (10) + one more +10 signal, or MX + A + two +5 signals. A bare "domain resolves" address (40) stays local. ✔
- With RDAP disabled the max is 75, still `light_green`. ✔
- Boundary unit tests at 29/30, 49/50, 69/70, 89/90 plus 0 and 100.

---

## 2. Architecture

```
internal/verify/                 scoring + service (pure Go, no DB in the scorer)
  pass1.go                       ordered check pipeline, short-circuit on hard fail
  checks_*.go                    one file per check family
  score.go                       weights table, TagFor(score), typo cap
  lists.go                       blocklist loading (embedded defaults + override dir)
  dns.go                         Resolver interface + per-domain cache (memory + table)
  rdap.go                        registration date lookup, cached
  service.go                     estimate / runs / single actions / apply-typo / stats
  args.go                        River job args + kinds
  verifiers.go                   VerifierFactory (decrypts the source key, builds client)
  ratelimit.go                   token bucket for Pass 2 calls
  jobs/                          River workers: run (fan-out), self, third_party, finalize
  verifier/                      EmailVerifier interface + Result/Status/errors
  verifier/emailable/            the one concrete implementation
  verifier/fake/                 deterministic double for tests
config/verify/                   *.txt blocklists (embedded like migrations/, overridable)
```

Flow for a bulk run:

```
POST /verification/runs ──▶ verification_runs row + River VerifyRunArgs (same tx)
        VerifyRunWorker: expand filter → verification_run_items → InsertMany(per-item jobs)
                 ├── queue verify_self   : VerifySelfWorker  (DNS-bound, 8 workers)
                 └── queue verify_third  : VerifyThirdWorker (rate-limited, 4 workers)
        each item worker: write email_verifications row → mark item → RecomputeRunStats
                          → last item enqueues VerifyRunFinalizeArgs (unique by run id)
```

Single-email actions create a one-item run through the same path, so there is exactly one execution model.

---

## 3. Data model (`migrations/00003_email_verification.sql`)

```sql
CREATE TABLE email_verifications (
    id                 uuid PRIMARY KEY,
    email              citext NOT NULL UNIQUE,          -- already normalized at ingest (business.NormalizeEmail)
    domain             text   NOT NULL,                 -- for grouping / DNS cache join
    pass1_score        integer NOT NULL DEFAULT 0 CHECK (pass1_score BETWEEN 0 AND 85),
    pass1_checks       jsonb   NOT NULL DEFAULT '[]',   -- [{key,label,status:pass|fail|skip,points,max,detail}]
    pass1_hard_fail    text,                            -- which hard check killed it, null otherwise
    pass1_verified_at  timestamptz,
    pass2_score        integer CHECK (pass2_score IN (0, 70, 100)),
    pass2_status       text CHECK (pass2_status IN ('deliverable','risky','unknown','undeliverable','error')),
    pass2_raw          jsonb,
    pass2_source_id    uuid REFERENCES sources (id) ON DELETE SET NULL,
    pass2_credits      integer NOT NULL DEFAULT 0,
    pass2_verified_at  timestamptz,
    final_score        integer NOT NULL DEFAULT 0 CHECK (final_score BETWEEN 0 AND 100),
    verification_tag   text NOT NULL DEFAULT 'red'
                       CHECK (verification_tag IN ('green','light_green','yellow','orange','red')),
    typo_suggestion    text,                            -- full corrected address, e.g. a@gmail.com
    last_error         text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_verifications_tag_idx    ON email_verifications (verification_tag, final_score DESC);
CREATE INDEX email_verifications_score_idx  ON email_verifications (final_score DESC);
CREATE INDEX email_verifications_domain_idx ON email_verifications (domain);
CREATE INDEX email_verifications_email_trgm ON email_verifications USING gin ((email::text) gin_trgm_ops);
CREATE INDEX email_verifications_pass2_idx  ON email_verifications (pass2_verified_at) WHERE pass2_verified_at IS NOT NULL;

-- Per-domain DNS/RDAP cache shared by every worker replica.
CREATE TABLE verification_domains (
    domain          text PRIMARY KEY,
    has_mx          boolean NOT NULL,
    has_a           boolean NOT NULL,
    has_spf         boolean NOT NULL,
    has_dmarc       boolean NOT NULL,
    mx_hosts        text[]  NOT NULL DEFAULT '{}',
    registered_at   timestamptz,                        -- from RDAP, null when unknown
    rdap_checked_at timestamptz,
    dns_checked_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE verification_runs (
    id              uuid PRIMARY KEY,
    pass            text NOT NULL CHECK (pass IN ('self','third_party')),
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued','running','done','failed','cancelled')),
    filter          jsonb NOT NULL,                     -- the RunFilter that was submitted
    total           integer NOT NULL DEFAULT 0,
    done            integer NOT NULL DEFAULT 0,
    failed          integer NOT NULL DEFAULT 0,
    skipped         integer NOT NULL DEFAULT 0,         -- cached / no longer qualifying
    credits_used    integer NOT NULL DEFAULT 0,
    est_cost_cents  bigint  NOT NULL DEFAULT 0,
    source_id       uuid REFERENCES sources (id) ON DELETE RESTRICT,  -- third_party only
    error           text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz
);
CREATE INDEX verification_runs_created_idx ON verification_runs (created_at DESC);
CREATE INDEX verification_runs_status_idx  ON verification_runs (status);

CREATE TABLE verification_run_items (
    run_id          uuid NOT NULL REFERENCES verification_runs (id) ON DELETE CASCADE,
    verification_id uuid NOT NULL REFERENCES email_verifications (id) ON DELETE CASCADE,
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued','done','failed','skipped')),
    error           text,
    -- Credits are recorded per item so a run's total is a sum of what it actually
    -- spent, rather than of a lifetime counter on the address.
    credits         integer NOT NULL DEFAULT 0 CHECK (credits >= 0),
    finished_at     timestamptz,
    PRIMARY KEY (run_id, verification_id)
);
CREATE INDEX verification_run_items_status_idx ON verification_run_items (run_id, status);

-- Sources grow a role; the verifier is just another source.
ALTER TABLE sources ADD COLUMN role text NOT NULL DEFAULT 'maps' CHECK (role IN ('maps','verifier'));
ALTER TABLE sources DROP CONSTRAINT sources_kind_check;
ALTER TABLE sources ADD  CONSTRAINT sources_kind_check CHECK (kind IN ('apify','outscraper','emailable'));
INSERT INTO sources (id, kind, name, role, cost_per_1k_cents, enabled)
VALUES ('0192f000-0000-7000-8000-000000000003', 'emailable', 'Emailable · Email verification', 'verifier', 500, false)
ON CONFLICT (kind) DO NOTHING;

-- Verification is per address now, not per business/email pair.
ALTER TABLE business_emails DROP COLUMN verified_status;
```

Rows in `email_verifications` are created lazily: a run's expansion step upserts one row per distinct `business_emails.email` it touches (`INSERT … ON CONFLICT (email) DO NOTHING`). Listing `/verification/emails` therefore only shows addresses that have been enqueued at least once; the `needs_self` counter in stats reports how many `business_emails` addresses have no row yet.

`final_score` and `verification_tag` are computed in Go (`verify.Finalize`) and stored, so the DB and the scorer can never disagree on the tag; a schema test asserts the CHECK constraints match the Go constants.

---

## 4. Pass 1 pipeline

Order is cheapest-first; the brief's listing order is not an execution order.

| # | Check key | Kind | Source | Result |
| --- | --- | --- | --- | --- |
| 1 | `syntax` | hard | `net/mail.ParseAddress` + must parse to exactly the input (no display name, no comments) | fail → 0, stop |
| 2 | `role_account` | hard (or penalty, see decision 1) | `role_hard.txt` / `role_soft.txt`, local part with `+tag` stripped | fail → 0, stop |
| 3 | `disposable_domain` | hard | `disposable_domains.txt`, apex and sub-domain match | fail → 0, stop |
| 4 | `structure` | +5 | local ≤ 64, total ≤ 254, no leading/trailing/consecutive dots, no quoted local, labels alnum/hyphen ≤ 63, alphabetic TLD ≥ 2 | pass/fail |
| 5 | `typo` | +5 / cap 40 | Damerau-Levenshtein ≤ 1 (≤ 2 when the domain is ≥ 10 chars) against `top_domains.txt`, skipped when the domain is itself on the list or a known free provider | fail → cap 40, store `typo_suggestion` |
| 6 | `human_local` | +10 | not: digit ratio > 0.5, 12+ chars with no separator and either entropy > 3.5 bits/char or a vowel ratio under 0.2, keyboard-row run ≥ 5 (`qwert`, `asdfg`, `zxcvb`, `12345`) | pass/fail |
| 7 | `not_free_provider` | +5 | `free_providers.txt` | pass/fail |
| 8 | `dns_resolves` | hard | cached lookup: no MX **and** no A/AAAA | fail → 0, stop |
| 9 | `mx` | +30 | same cached lookup as #8 | pass/fail |
| 10 | `a_record` | +10 | same cached lookup as #8 | pass/fail |
| 11 | `spf` | +5 | TXT at apex containing `v=spf1` (cached) | pass/fail |
| 12 | `dmarc` | +5 | TXT at `_dmarc.<domain>` containing `v=DMARC1` (cached) | pass/fail |
| 13 | `domain_age` | +10 | RDAP `registration` event > 365 days ago (cached 30 d); disabled or failed → `skip` | pass/fail/skip |

Two refinements the build settled:

- **The one DNS-dependent hard check runs after the free local checks**, not before
  them. That keeps the order genuinely cheapest-first: an address killed by syntax,
  a role account or a disposable domain never costs a DNS query at all. A hard fail
  still zeroes the score and marks every remaining check `skip`, and on a hard fail
  the points of checks that had already run are zeroed too, so the breakdown the UI
  renders always adds up to the stored score.
- **The typo suggestion is computed before anything can short-circuit** and is kept
  even on a hard fail. Otherwise a misspelled domain that does not resolve would be
  a dead end: tagged red with no correction to offer, which is precisely the case
  the feature exists for.

Every check appends `{key, label, status, points, max, detail}` to `pass1_checks`; on a hard fail the remaining checks are recorded as `skip` so the UI breakdown is always the full list. Points are only ever read from one table in `score.go`; the test `TestWeightsSumTo85` guards it.

DNS: one `lookupDomain(ctx, domain)` per address that first checks an in-process TTL cache, then `verification_domains` (fresh if `dns_checked_at` < 7 days), then the resolver with a 3 s timeout. The four DNS-backed checks read from that single result. `Resolver` is an interface (`LookupMX`, `LookupIP`, `LookupTXT`) so unit tests use a map-backed fake; nothing in the scorer package touches the network in tests.

Blocklists: `config/verify/{disposable_domains,role_hard,role_soft,free_providers,top_domains}.txt`, one entry per line, `#` comments. Embedded via `config/verify/embed.go` (same pattern as `migrations/embed.go`) so the container works out of the box; `KARVON_VERIFY_LISTS_DIR` points at a directory whose files override the embedded ones file-by-file (docker-compose mounts `./config/verify`). Loaded once at boot; a reload needs a restart (documented).

No SMTP `RCPT TO` probe anywhere in Pass 1.

---

## 5. Pass 2 — third-party adapter

```go
// internal/verify/verifier/verifier.go
type Status string // deliverable | risky | unknown | undeliverable

type Result struct {
    Status      Status
    Reason      string          // provider reason code, kept for the UI
    Raw         json.RawMessage // untouched provider payload → pass2_raw
    CreditsUsed int
}

type Balance struct{ Credits int64 }

type Verifier interface {
    Verify(ctx context.Context, email string) (Result, error)
    Balance(ctx context.Context) (Balance, error)
    Name() string
}

var (
    ErrAuth                = errors.New("verifier: authentication failed")
    ErrRateLimited         = errors.New("verifier: rate limited")   // carries Retry-After when known
    ErrInsufficientCredits = errors.New("verifier: insufficient credits")
    ErrPending             = errors.New("verifier: result not ready") // Emailable 249
)
```

`verify.VerifierFactory` mirrors `scraper.ProviderFactory`: takes a `dbgen.Source` with `role = verifier`, decrypts the key with the existing `crypto.Cipher`, builds the client. Tests swap it via `app.WithVerifierFactory`.

Emailable mapping (`verifier/emailable`):

| Emailable `state` | Our status | Score |
| --- | --- | --- |
| `deliverable` | deliverable | 100 |
| `risky` (incl. `accept_all`) | risky | 70 |
| `unknown`, HTTP 249, timeout | unknown | keep Pass 1, `pass2_status = unknown`, retry later |
| `undeliverable` | undeliverable | 0 |
| 401/403 | `ErrAuth` | fail the run |
| 402 / "insufficient credits" | `ErrInsufficientCredits` | fail the run |
| 429 | `ErrRateLimited` | snooze the job |

Rules enforced by the worker, not the adapter:

- **Gate:** the item is skipped (status `skipped`, reason recorded) unless `pass1_verified_at IS NOT NULL AND pass1_score >= 50` at execution time. A self run is never implicitly triggered; the estimate endpoint tells the UI how many addresses still need Pass 1.
- **Cache 90 days:** skipped when `pass2_verified_at > now() - 90d` and `pass2_status IN (deliverable, risky, undeliverable)`. `unknown` and `error` are not cached. The estimate excludes cached rows, so the count and cost shown before confirmation are the count and cost that will be billed.
- **Rate limit:** a per-process token bucket (`KARVON_VERIFY_THIRD_PARTY_RPS`, default 5) in front of `Verify`, plus River's `MaxWorkers` on the `verify_third` queue. `ErrRateLimited` → `river.JobSnooze(retryAfter or 2^attempt s)`.
- **Backoff:** transport errors and 5xx return an error; River retries with its default exponential policy (`attempt^4` s + jitter), `MaxAttempts: 5`. After the last attempt the item is `failed`, `last_error` is set, Pass 1 score stays.
- **Outage isolation:** Pass 2 has its own queue and its own worker pool; Pass 1 jobs and every HTTP handler read only Postgres. A dead provider means Pass 2 items sit in `retryable`, nothing else changes.
- **Credits:** `Result.CreditsUsed` (1 per billed call, 0 on cache hit) → `email_verifications.pass2_credits`, summed into `verification_runs.credits_used`; `GET /verification/stats` returns all-time and 30-day totals plus the provider's live balance (`Balance()`, cached in-process for 60 s, `null` when the provider is unreachable).

---

## 6. Runs and River jobs

Queues (`internal/queue/queue.go`): `QueueVerifySelf = "verify_self"`, `QueueVerifyThird = "verify_third"`; `default` for run/finalize.

| Kind | Args | Queue | Unique by | MaxAttempts |
| --- | --- | --- | --- | --- |
| `verify_run` | `RunID` | default | args | 3 |
| `verify_self` | `RunID, VerificationID` | verify_self | args | 3 |
| `verify_third_party` | `RunID, VerificationID` | verify_third | args | 5 |
| `verify_run_finalize` | `RunID` | default | args | 5 |

`RunFilter` (stored as `verification_runs.filter`):

```json
{
  "scope": "all" | "selection",
  "ids": ["<verification id>"],          // selection
  "business_ids": ["<business id>"],     // selection, expands to their emails
  "job_id": "<scrape job id>",           // all emails of a job's businesses
  "tags": ["yellow","light_green"],
  "min_score": 50,
  "include_suppressed": false,
  "stale_after_days": 30                 // self runs: skip rows verified more recently
}
```

`VerifyRunWorker` expands the filter into `verification_run_items` in one transaction (upserting missing `email_verifications` rows from `business_emails` first), sets `total`, marks the run running, then `InsertMany` per-item jobs (capped by `KARVON_VERIFY_MAX_RUN_EMAILS`, default 50 000; above that the run fails with a clear error). A run with zero items finalizes immediately as `done`.

Item workers follow `crawl_job.go`: load run, stop quietly if the run is no longer active, do the work, write the row, mark the item, `RecomputeRunStats`, and enqueue finalize when `done + failed + skipped == total` (finalize is unique by run id, so concurrent last-item races collapse). Cancel = `POST /verification/runs/{id}/cancel`: flip the row, cancel pending River jobs by metadata (same `MetadataFilter` trick as scrape jobs), running workers notice on their next status check.

---

## 7. API (`api/openapi.yaml`, tag `verification`)

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/verification/stats` | counts per tag, self/third-party verified counts, `needs_self`, `qualifying_for_third_party` + est. cost, credits used (all time, 30 d), provider balance, active runs |
| GET | `/verification/emails` | list; filters `tag[]`, `min_score`, `max_score`, `pass2_status`, `needs_third_party`, `has_typo`, `q` (trigram on email), `business_id`, `job_id`; sort `final_score:desc\|asc`, `email:asc\|desc`, `pass1_verified_at:desc`, `pass2_verified_at:desc`; page/per_page |
| GET | `/verification/emails/{id}` | full row incl. `pass1_checks[]`, `pass2_raw`, the businesses using the address, active run item if any |
| POST | `/verification/emails/{id}/self` | 202 → one-item self run |
| POST | `/verification/emails/{id}/third-party` | 202 → one-item third-party run; 409 when the gate fails or the result is cached (message says until when) |
| POST | `/verification/emails/{id}/apply-typo` | rewrites every `business_emails` row carrying the address to `typo_suggestion` (merging into an existing corrected row when one exists), re-picks primaries, queues a self run for the corrected address; 409 when there is no suggestion |
| POST | `/verification/runs/estimate` | body `{pass, filter}` → `{emails, needs_self, cached, credits_needed, cost_per_1k_cents, est_cost_cents, balance_credits}` |
| POST | `/verification/runs` | body `{pass, filter, max_cost_cents?}`; for `third_party` the field is required and the server re-estimates: 409 if the fresh estimate exceeds it (the confirmation the user saw is the ceiling) |
| GET | `/verification/runs` | history, filters `pass`, `status`; page/per_page |
| GET | `/verification/runs/{id}` | counters for polling |
| POST | `/verification/runs/{id}/cancel` | cooperative cancel |

Existing endpoints change:

- `Business.primary_email_verified_status` now carries the tag enum; add `primary_email_verification_score`. `BusinessEmail.verified_status` → tag enum; add `verification_score`, `verification_id`, `typo_suggestion`.
- `GET /businesses` gains `verification_tag[]` filter and `verification_score:desc|asc` sorts (`businesses_query.go` joins `email_verifications` in the existing LATERAL).
- `Source` gains `role`; `SourceKind` gains `emailable`; `POST /sources/{id}/test` on a verifier returns `{ok, kind, credits}`.
- `POST /jobs` rejects a `verifier` source (`requireUsableSource`).

Key schema: `EmailVerification { id, email, domain, pass1_score, pass1_checks[], pass1_hard_fail, pass1_verified_at, pass2_score, pass2_status, pass2_verified_at, pass2_credits, final_score, verification_tag, tag_label, typo_suggestion, last_error, businesses[] (id, name) , updated_at }`; `VerificationCheck { key, label, status, points, max, detail }`; `VerificationRun { id, pass, status, filter, total, done, failed, skipped, credits_used, est_cost_cents, source_id, error, created_at, started_at, finished_at }`.

---

## 8. Configuration (`KARVON_VERIFY_*`)

| Variable | Default | Purpose |
| --- | --- | --- |
| `VERIFY_SELF_CONCURRENCY` | 8 | River workers on `verify_self` |
| `VERIFY_THIRD_PARTY_CONCURRENCY` | 4 | workers on `verify_third` |
| `VERIFY_THIRD_PARTY_RPS` | 5 | token bucket rate for provider calls |
| `VERIFY_PASS2_MIN_SCORE` | 50 | gate |
| `VERIFY_CACHE_DAYS` | 90 | Pass 2 cache window |
| `VERIFY_DNS_TIMEOUT` | 3s |  |
| `VERIFY_DNS_CACHE_DAYS` | 7 | `verification_domains` freshness |
| `VERIFY_RDAP_ENABLED` | true | decision 2 |
| `VERIFY_RDAP_TIMEOUT` | 5s |  |
| `VERIFY_RDAP_CACHE_DAYS` | 30 |  |
| `VERIFY_ROLE_SOFT_MODE` | hard | decision 1: `hard` \| `penalty` |
| `VERIFY_LISTS_DIR` | (empty → embedded) | blocklist override directory |
| `VERIFY_MAX_RUN_EMAILS` | 50000 | cap per bulk run |
| `EMAILABLE_BASE_URL` | https://api.emailable.com | swap for a mock in tests |

Validation in `config.Validate()` for the numeric ranges, same style as the crawl settings.

---

## 9. Files

**Create**

```
migrations/00003_email_verification.sql
queries/email_verifications.sql          upsert-from-business-emails, get, update pass1, update pass2, set error, apply typo helpers
queries/verification_runs.sql            create, get, mark running/terminal, recompute stats, list items, mark item
queries/verification_domains.sql         get, upsert dns, upsert rdap
queries/verification_stats.sql           counts per tag, credits, needs_self, qualifying
internal/db/verifications_query.go       dynamic list/count + sort whitelist (pattern: businesses_query.go)
config/verify/embed.go
config/verify/disposable_domains.txt     (seed from the public disposable-email-domains list)
config/verify/role_hard.txt
config/verify/role_soft.txt
config/verify/free_providers.txt
config/verify/top_domains.txt            (top-100 mail domains)
internal/verify/types.go                 Tag, Status, CheckResult, Pass1Result, RunFilter, constants
internal/verify/score.go                 weights, TagFor, Finalize(pass1, pass2) → final/tag
internal/verify/pass1.go                 Pipeline.Run(ctx, email) Pass1Result
internal/verify/checks_syntax.go
internal/verify/checks_local.go          role, structure, human heuristic
internal/verify/checks_domain.go         disposable, free provider, typo (+ Damerau-Levenshtein)
internal/verify/checks_dns.go            mx, a, spf, dmarc
internal/verify/checks_age.go            RDAP
internal/verify/dns.go                   Resolver interface, cached lookup, verification_domains persistence
internal/verify/rdap.go
internal/verify/lists.go
internal/verify/ratelimit.go
internal/verify/args.go                  River kinds + args + metadata helpers
internal/verify/verifiers.go             VerifierFactory
internal/verify/service.go               Service (estimate, runs, single actions, apply-typo, stats, cancel)
internal/verify/verifier/verifier.go
internal/verify/verifier/emailable/emailable.go
internal/verify/verifier/fake/fake.go
internal/verify/jobs/deps.go
internal/verify/jobs/run_job.go
internal/verify/jobs/self_job.go
internal/verify/jobs/third_party_job.go
internal/verify/jobs/finalize_job.go
internal/http/handlers_verification.go
internal/http/handlers_verification_test.go
tests/verification_test.go               integration: ingest → self run → third-party run with the fake → tags, cache, outage
internal/verify/score_test.go            weights sum, every boundary
internal/verify/pass1_test.go            every check pass/fail/skip, hard-fail short-circuit, typo cap + suggestion
internal/verify/checks_*_test.go
internal/verify/verifier/emailable/emailable_test.go   httptest server: every state, 249, 401, 402, 429
internal/verify/service_test.go          estimate math, gate, cache window, max_cost ceiling
```

**Modify**

```
api/openapi.yaml                         verification tag + schemas; Source.role/kind; BusinessEmail/Business fields; /businesses filter+sort
internal/http/gen/gen.go                 `make gen`
internal/db/dbgen/*                      `make gen`
internal/http/services.go                VerificationService interface
internal/http/server.go                  wire the service
internal/http/mapper.go                  verification fields on business/email payloads
internal/http/handlers_businesses.go     verification_tag filter, new sorts
internal/http/handlers_sources.go        role in payload; test dispatch
internal/http/stubs_test.go              stubVerification
internal/db/businesses_query.go          join email_verifications; filter/sort
queries/business_emails.sql              ListBusinessEmails joins verification (score, tag, typo)
internal/business/service.go             Detail carries verification per email
internal/source/service.go               Test() → Balance() for verifier role
internal/scraper/service.go              requireUsableSource rejects role != maps
internal/queue/queue.go                  two queue names
internal/app/app.go                      service, workers, queues, factory
internal/app/options.go                  WithVerifierFactory, WithResolver
internal/config/config.go (+_test)       KARVON_VERIFY_*; a new Defaults() constructor so a
                                         setting with an envDefault does not break every test literal
.env.example, docker-compose.yml         new vars; mount ./config/verify
README.md                                verification section
tests/harness_test.go                    fake verifier + fake resolver wired in
```

---

## 10. Tests

- **Scorer (unit, no network, no DB):** table test per check with pass/fail/skip inputs; hard-fail short-circuit records remaining checks as `skip`; `TestWeightsSumTo85`; typo → cap 40 + suggestion; `TagFor` at 0, 29, 30, 49, 50, 69, 70, 89, 90, 100; `Finalize` for every Pass 2 status.
- **DNS cache:** fake resolver counts calls; two addresses on one domain → one lookup; stale table row → refetch.
- **Emailable client:** `httptest.Server` per state and error status; `Raw` preserved; credits parsed; `ErrRateLimited` carries Retry-After.
- **Service:** estimate excludes cached and non-qualifying rows; `max_cost_cents` ceiling → 409; third-party on `pass1 < 50` → 409.
- **Handlers:** stub-based, like `handlers_businesses_test.go`.
- **Integration (`tests/`, testcontainers):** seeded scrape → self run over all → a clean personal address tags light green, `info@` tags red (per decision 1) and a typo'd free-provider domain tags red while still offering its correction; third-party run with `fake.Verifier` → green, credits counted, and the role account and the typo'd address are never sent; re-running within 90 days bills nothing and the single-address action answers 409 naming the cache; an underfunded or unconfirmed paid run is refused before any call; a rejected key fails the whole run at once; an unknown verdict is not cached and stays eligible; applying a typo rewrites the master list and re-picks the primary.
- **Provider outage:** the run is deliberately *not* waited out, because a transient failure is retried with exponential backoff and five attempts take minutes. The test asserts what the outage claim actually means: nothing is billed, the local score and tag stand, `/verification/stats`, `/verification/emails` and `/businesses` all keep answering, the stuck run can be cancelled, and a fresh self run still completes while the vendor is down.
- **Schema test:** CHECK constraints for tags/statuses equal the Go constants; the `verification_tag` index is used by the default list ordering.

Zero real network calls in the suite: resolver and verifier are injected doubles; RDAP is off in the harness.

---

## 11. Build order

1. Migration + sqlc queries + `make gen` (schema settles first; FE can generate types from the spec at step 5).
2. `internal/verify` scorer with unit tests (pure; fastest feedback).
3. Verifier interface + Emailable client + fake.
4. Service + River jobs + app wiring; integration test for a self run.
5. OpenAPI + handlers + mapper changes + existing-endpoint changes; `make gen`; handler tests.
6. Third-party run end to end with the fake; cache, ceiling, outage tests.
7. README, `.env.example`, compose, lint (`make check`).

Each step ends with `make check` green and a conventional commit.

---

## 12. Frontend handoff prompt (`karvon-fe`)

See the chat message that accompanied this plan; the same prompt is reproduced verbatim below so it can be pasted into Claude Code inside `karvon-fe` once the backend's OpenAPI spec exists (step 5 above), or earlier against MSW mocks.

```markdown
You are extending `karvon-fe` (Vue 3.5 + `<script setup lang="ts">` + Vite + Tailwind v4 +
shadcn-vue `new-york` over Reka UI, @tanstack/vue-query, vue-router 4, ofetch, MSW mocks).
Read README.md and docs/PLAN.md fully first — especially "UI theme & component rules" — and
follow the Scraper module (src/modules/scraper) as the reference for every convention:
module folder layout, `api.ts` wrappers, `queryKeys.ts`, composables, four page states
(loading skeleton / empty / error with retry / data), toasts on mutations, `ConfirmDialog`
for anything irreversible or paid.

## What you are building
The **Verify** module: a Verification page at `/verify` plus a "Verification" tab in the
business detail sheet. Every email carries a `final_score` (0–100) and a `verification_tag`
produced by two independent passes that must stay visually separate:
- **Self verification** (Pass 1): free, local checks — syntax, role/disposable blocklists,
  DNS (MX, A, SPF, DMARC), domain age, human-looking local part, typo detection. Max 85.
- **Third-party verification** (Pass 2): paid API, only for emails with Pass 1 score ≥ 50,
  results cached 90 days. Scores 100 / 70 / 0, or "unknown" (keeps Pass 1 score).

Tags (wire value → label, colour token, lucide icon):
| tag | label | token | icon |
| green | Verified | verify-green | ShieldCheck |
| light_green | Likely valid | verify-lime | BadgeCheck |
| yellow | Uncertain | verify-yellow | CircleHelp |
| orange | Risky | verify-orange | TriangleAlert |
| red | Invalid | verify-red | CircleX |
A tag is never shown as colour alone: always icon + label (+ score where there is room).

## Backend contract (karvon-be, `/api/v1`, bearer key as today)
Until `pnpm gen:api` works against the running backend, hand-write these in
src/types/api.ts exactly as the Scraper types were, then switch models.ts aliases to
api.gen.ts when the spec is live. Shapes:

EmailVerification { id, email, domain, pass1_score, pass1_hard_fail: string|null,
  pass1_verified_at: string|null, pass2_score: number|null,
  pass2_status: 'deliverable'|'risky'|'unknown'|'undeliverable'|'error'|null,
  pass2_verified_at: string|null, pass2_credits: number, final_score: number,
  verification_tag: VerificationTag, tag_label: string, typo_suggestion: string|null,
  last_error: string|null, business_count: number, updated_at }
// The list endpoint returns EmailVerification. The detail endpoint returns
// EmailVerificationDetail, which adds the three heavy fields:
EmailVerificationDetail = EmailVerification & { pass1_checks: VerificationCheck[],
  pass2_raw: Record<string, unknown>|null, businesses: {id, name}[] }
VerificationCheck { key, label, status: 'pass'|'fail'|'skip', points, max, detail: string|null }
VerificationRun { id, pass: 'self'|'third_party', status: JobStatus, filter: RunFilter,
  total, done, failed, skipped, credits_used, est_cost_cents, source_id: string|null,
  error: string|null, created_at, started_at: string|null, finished_at: string|null }
RunFilter { scope: 'all'|'selection', ids?: string[], business_ids?: string[], job_id?: string,
  tags?: VerificationTag[], min_score?: number, include_suppressed?: boolean,
  stale_after_days?: number }
RunEstimate { emails, needs_self, cached, credits_needed, cost_per_1k_cents, est_cost_cents,
  balance_credits: number|null }
VerificationStats { total, by_tag: Record<VerificationTag, number>, self_verified,
  third_party_verified, needs_self, qualifying_for_third_party, qualifying_est_cost_cents,
  credits_used_total, credits_used_30d, balance_credits: number|null, active_runs: number }

Endpoints:
GET  /verification/stats
GET  /verification/emails?tag=&tag=&min_score=&max_score=&pass2_status=&needs_third_party=
     &has_typo=&q=&business_id=&job_id=&include_suppressed=&sort=final_score:desc&page=&per_page=
     → Paginated<EmailVerification>
     (sort: final_score|email|pass1_verified_at|pass2_verified_at|updated_at, each :asc|:desc)
GET  /verification/emails/{id}   → EmailVerificationDetail
POST /verification/emails/{id}/self          → 202 VerificationRun
POST /verification/emails/{id}/third-party   → 202 VerificationRun | 409 (gate failed / cached)
POST /verification/emails/{id}/apply-typo    → 200 EmailVerificationDetail (the corrected address,
     which is a DIFFERENT row from the one you posted to; re-key any local state on its id)
POST /verification/runs/estimate  {pass, filter} → RunEstimate
POST /verification/runs           {pass, filter, max_cost_cents?} → 201 VerificationRun
     (max_cost_cents is REQUIRED for third_party: send the est_cost_cents you displayed; 409 if exceeded)
GET  /verification/runs?pass=&status=&page=   → Paginated<VerificationRun>
GET  /verification/runs/{id}
POST /verification/runs/{id}/cancel
Also changed: Business.primary_email_verified_status is now VerificationTag|null and
Business.primary_email_verification_score: number|null; BusinessEmail gains
verification_id, verification_score, verified_status: VerificationTag|null, typo_suggestion;
GET /businesses accepts verification_tag[] and sort=verification_score:desc|asc; Source gains
role: 'maps'|'verifier' and kind 'emailable'; POST /sources/{id}/test on the verifier returns
`credits: number|null` alongside `ok`. Errors use the existing envelope.
Progress for runs is polling, not SSE: refetch GET /verification/runs/{id} every 2 s while
status is queued/running, and invalidate ['verification'] keys when it turns terminal.

## Build in this order (conventional commit after each)
1. Theme: add `--verify-green`, `--verify-lime`, `--verify-yellow`, `--verify-orange`,
   `--verify-red` tokens (light + dark, contrast ≥ 4.5:1 as text on card) in
   src/assets/main.css, registered in `@theme inline` so `text-verify-*`, `bg-verify-*`,
   `border-verify-*` exist. Replace VERIFIED_STATUS in src/config/theme.ts with
   VERIFICATION_TAG: Record<VerificationTag, {label, icon, badgeClass, dotClass, variant}>.
   New `<VerificationTagBadge :tag :score?>` in src/components (Badge + icon + label).
2. Nav + routes: enable the Verify group in src/config/nav.ts with children
   Verification (`/verify`), Runs (`/verify/runs`); src/modules/verify/routes.ts merged in
   src/router/index.ts; breadcrumbs like the scraper routes.
3. src/modules/verify/api.ts, queryKeys.ts (`verificationKeys.stats/emails/email(id)/runs/run(id)`),
   composables: useVerificationStats, useVerifications(params), useVerification(id),
   useVerificationRuns, useVerificationRun(id, {poll}), useRunEstimate, useCreateRun,
   useCancelRun, useVerifyEmail(pass), useApplyTypo. Every mutation invalidates
   ['verification'] and ['businesses'] and toasts.
4. MSW: extend tests/mocks/db.ts with email_verifications + runs, tests/mocks/handlers.ts
   with every endpoint above, and tests/mocks/engine.ts with a run engine that advances
   done/failed/skipped every 300 ms and finishes; a deterministic scorer so gmial.com gets a
   typo suggestion, info@ is red, hi@ is light_green, and a third-party run turns ~70% green.
5. Page `/verify` (VerificationPage.vue):
   - Stat row (StatCard ×4): total scored, by-tag distribution (five mini counts with
     badges), needs self verification, qualifying for third-party (+ est. cost + balance).
     `qualifying_est_cost_cents` can legitimately be 0 when only a handful of addresses
     qualify, because the price is per 1000 — show the count as well as the cost.
   - Two clearly separated action cards side by side, titled **Self verification** and
     **Third-party verification**, each with its own description, last-run timestamp,
     active-run progress (Progress + counters) and its own bulk button:
     "Run self verification on all" and "Send qualifying to third-party (N emails, ~$X)".
     The third-party button calls /runs/estimate first, opens ConfirmDialog showing the
     count, cached count, cost and remaining credits, and only then POSTs with
     max_cost_cents = est_cost_cents. Disabled with a tooltip when the verifier source is
     not configured (Source.role === 'verifier' && !has_key) or credits are 0.
   - Filters card: tag multi-select (chips with icon+label), score range (min/max inputs),
     pass2 status, "needs third-party", "has typo", email search (debounced 300 ms).
   - DataTable (shadcn DataTable over @tanstack/vue-table, same as BusinessesTable):
     columns email (mono), tag badge, final score, pass1 score, pass2 status + verified-at,
     typo suggestion (inline "Did you mean x? [Apply]" one-click, ConfirmDialog not needed
     since it is reversible by re-verifying), businesses count, updated. Sortable by score,
     email, pass1/pass2 verified-at. Row selection → BulkActionsBar variant with
     "Self-verify selected" and "Send selected to third-party (N, ~$X)" (same estimate +
     confirm flow, scope 'selection').
   - Row click opens VerificationSheet (below).
6. VerificationSheet.vue (Sheet): header email + big tag badge + final score; then two
   sections separated by `Separator`, each a Card: **Self verification** (score/85, timestamp,
   "Run again" button, Collapsible "Show breakdown" listing every check: icon for
   pass/fail/skip, label, detail, `+points / max` right-aligned mono; hard fail highlighted
   with destructive text) and **Third-party verification** (score, status, provider reason,
   verified-at, credits used, "Verify with provider" button — disabled with tooltip when
   pass1 < 50 or cached, showing the cache expiry). Typo banner with Apply when present.
   Businesses list linking to the scraped-emails sheet.
7. Runs page `/verify/runs`: DataTable of runs (pass badge, status via StatusBadge, total/
   done/failed/skipped, credits, cost, started, duration), cancel action while active, row
   expands to show the filter. Polls every 5 s while any run is active.
8. Integrate with Scraper: BusinessSheet gets shadcn Tabs "Details" | "Verification"; the
   Verification tab lists each email with tag badge + score and both section summaries,
   with per-email Self / Third-party buttons and typo Apply. BusinessesTable's "Verified"
   column renders VerificationTagBadge from primary_email_verified_status; BusinessFilters
   gains a tag multi-select mapped to verification_tag[].
9. Sources page: group cards by role (Maps providers / Email verification); the verifier
   card's test button shows remaining credits.
10. Tests: Vitest for VerificationTagBadge (renders icon+label for all five tags, never
    colour only), the estimate → confirm → create flow (mocked), the breakdown list
    (points sum equals pass1_score), apply-typo; one Playwright smoke: open /verify, run
    self verification, see the run finish and the table update.

## Rules (non-negotiable, same as the Scraper module)
- shadcn-vue only; add components with `pnpm dlx shadcn-vue add <name>`; never edit
  src/components/ui by hand. Icons from @lucide/vue only, size-4, stroke-width 1.75.
- Tokens only (no hex/oklch literals in components, no inline styles); the five verify
  tokens live in main.css and are consumed through theme.ts.
- `<script setup lang="ts">`, no `any`, Pinia only for UI state, vue-query for server
  state, stable query keys, toasts + invalidation on every mutation.
- A paid action always shows count + cost and requires ConfirmDialog before the request.
- Accessibility: every icon-only button has aria-label + Tooltip; tag badges carry text.
- Stop and ask before adding a dependency. `pnpm lint`, `pnpm typecheck`, `pnpm test` must
  pass before every commit.
```
