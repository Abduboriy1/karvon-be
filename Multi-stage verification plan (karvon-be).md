# Multi-stage verification plan (`karvon-be`)

> **Status: implemented.** This document is the design record for the four-stage
> verification pipeline. Where the build refined the design, the text below matches
> the code. The user-facing summary lives in the README under "Email verification".

Extends the existing two-pass system (local Pass 1 → paid Pass 2) into a pipeline of
**three free providers combined by weight**, followed by the **paid provider as a
gated fallback**. Nothing that already worked is removed: the existing local pipeline
becomes provider #1 of the free stage and the paid pass keeps its execution path, its
cache and its spend guards.

---

## 0. Decisions taken

| # | Question | Decision |
| --- | --- | --- |
| 1 | A weighted free score could reach 90+ and tag an address "Verified" with no paid check. | **Free score is capped at 89.** Only the paid provider can award green. `FreeMaxScore = 89` replaces the old `Pass1MaxScore = 85` as the invariant that protects the meaning of "Verified". |
| 2 | The brief's "run paid below 75%" conflicts with the existing "never pay below 50". | **Both bounds apply.** Paid runs when `paid_min_score ≤ free_score < paid_threshold` (defaults 50 and 75). The floor is the existing behaviour, untouched; the ceiling is new and only ever *reduces* spend. |
| 3 | Weights have to be editable from a settings page. | **`verification_settings` table + `GET`/`PUT /api/v1/verification/settings`.** `KARVON_*` env vars seed the row on first read, so an existing deployment keeps its current behaviour. |
| 4 | Reacher is AGPL-3.0 or commercial. | **Disabled by default** (`KARVON_VERIFY_REACHER_ENABLED=false`) and shipped behind a Compose profile, so nothing depends on it until the licence question is settled. See §7. |
| 5 | MailChecker as a service or a library. | **Library.** It is MIT, pure Go, and does syntax + disposable-domain lookups against an embedded list. A network hop would add latency and a failure mode for zero benefit. |

---

## 1. Shape of the pipeline

```
                       free stage (one River job per address, queue verify_self)
   ┌──────────────────────────────────────────────────────────────────────┐
   │  existing   →  mailchecker  →  reacher                               │
   │  (local)       (library)       (HTTP, may be down)                   │
   │      ↓              ↓               ↓                                │
   │   0–100          0 or 100         0–100        each a ProviderResult │
   │      └──────────────┴───────────────┘                                │
   │                     ↓                                                │
   │            weighted combination  → free_score (0–89)                 │
   └──────────────────────────────────────────────────────────────────────┘
                                 ↓
        gate:  paid_min_score ≤ free_score < paid_threshold
                                 ↓
                       paid stage (queue verify_third)
                        Emailable → 0 / 70 / 100
                                 ↓
        final_score = paid score when conclusive, else free_score
```

The two stages stay separate River runs (`pass = self` / `pass = third_party`), as
today. The operator still triggers a paid run explicitly; the gate decides which
addresses inside that run are actually billed.

## 2. Packages

```
internal/verify/
  provider/                provider.go     Result, Status, Provider, Key constants
  provider/mailchecker/    mailchecker.go  MIT library adapter
  provider/reacher/        reacher.go      HTTP client, breaker, normalizer
  existing.go              adapter over the current *Pipeline
  scoring.go               weighted combination, redistribution, caps
  settings.go              Settings, validation, env defaults
  registry.go              the provider list the scorer and the API iterate
```

`provider` holds only types and has no dependency on `verify`, so `verify` can
implement the interface for the existing pipeline without an import cycle. The paid
vendors keep their own `verify/verifier` package; their verdicts are folded into the
same `ProviderResult` shape for storage and for the API breakdown, but they keep a
separate execution path because they cost money.

```go
type Result struct {
    Provider string            // "existing" | "mailchecker" | "reacher" | "paid"
    Score    int               // 0–100, meaningful only when Status is StatusScored
    Status   Status            // scored | inconclusive | skipped | unavailable | error
    Reason   string            // short, human-readable, shown in the UI
    Metadata map[string]any    // small and structured; never a raw vendor payload
    Err      error             // set with StatusError; not serialised, Reason carries it
    Duration time.Duration
}
```

Adding a fourth free provider is: implement `Provider`, add a key to the registry,
add a weight key. The scorer, the storage column and the API are all keyed by
provider name and need no change.

## 3. Normalisation — what each provider actually tells us

Each provider is normalised from **the information it really returns**, not from an
assumed percentage.

**existing** — the current local pipeline returns 0…`MaxScore()`, which is 85, or 75
when RDAP is disabled. Normalised as `round(score × 100 / MaxScore())`, so a perfect
local result is 100 *on the provider's own scale*; the 89 cap is applied once, to the
combined score. A hard fail (bad syntax, disposable, automated mailbox, dead domain)
is 0. Metadata carries `hard_fail` and `typo_suggestion`.

**mailchecker** — the library exposes exactly two booleans, so its normalised score
is honestly binary:

| Observation | Score | Reason |
| --- | ---: | --- |
| `IsBlacklisted` | 0 | known disposable/throwaway provider |
| `!IsValid` (and not blacklisted) | 0 | fails RFC-shaped syntax |
| otherwise | 100 | valid syntax, not a known disposable domain |

**reacher** — `is_reachable` plus the SMTP detail:

| Observation | Score | Status |
| --- | ---: | --- |
| `misc.is_disposable` | 0 | scored |
| `is_reachable = invalid` | 0 | scored |
| `is_reachable = safe` | 100 | scored |
| `risky` + `smtp.has_full_inbox` | 50 | scored |
| `risky` + `smtp.is_catch_all` | 60 | scored |
| `risky` (anything else) | 70 | scored |
| `is_reachable = unknown` | — | **inconclusive** |
| transport error, 5xx, timeout, breaker open, disabled | — | **unavailable / error** |

Only `StatusScored` results enter the weighted sum. 70 for plain `risky` matches the
value the paid pass already uses for a risky verdict, so the two scales agree.

## 4. Weighted combination

Weights are stored per provider key and **must total exactly 100** across all
registered free providers, whether or not each is enabled. Validation rejects
anything else with a field error.

At scoring time the weight of every provider that did *not* return a score — disabled,
unavailable, errored, inconclusive — is **redistributed proportionally** over the
providers that did:

```
free_score = Σ (score_i × weight_i) / Σ weight_i     over scored providers only
```

Consequences, all covered by tests:

- A Reacher outage does not fail the pipeline and does not drag the score down. It
  falls back to existing + mailchecker at their relative weights.
- Disabling a provider in settings is equivalent to it being unavailable.
- If every scored provider happens to carry weight 0, the scorer falls back to an
  equal split over the scored providers rather than dividing by zero.
- The local provider never errors, so at least one score always exists.

The result is clamped to `[0, 89]`.

**Short-circuit.** When the existing pipeline hard-fails, Reacher is skipped:
there is no reason to open an SMTP conversation with a domain that has no MX record
or an address that is not valid syntax. The skipped result is recorded with
`StatusSkipped` and its weight is redistributed like any other.

## 5. Data model

Migration `00004_verification_providers.sql`.

`email_verifications` gains:

| Column | Purpose |
| --- | --- |
| `free_score` | the weighted 0–89 result of the free stage |
| `free_scored_at` | when the free stage last completed |
| `provider_results` | `jsonb` array: one entry per provider with `provider`, `score`, `status`, `reason`, `weight`, `weighted_points`, `metadata`, `error` |

`pass1_score`, `pass1_checks` and every Pass 2 column are untouched, so the existing
breakdown UI keeps working. Existing rows are backfilled with
`free_score = LEAST(89, ROUND(pass1_score × 100.0 / 85))` and
`free_scored_at = pass1_verified_at`, which keeps them eligible for exactly the paid
runs they were eligible for before.

`provider_results` is deliberately the *normalised* record — score, status, reason,
the weight that was applied and the points it contributed — and not the raw vendor
payload. Reacher's full JSON is a few kB per address and says nothing the normalised
result does not. The paid provider's raw payload keeps its existing `pass2_raw`
column, because a billed verdict can be disputed.

New table:

```sql
CREATE TABLE verification_settings (
    id             smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    weights        jsonb NOT NULL,     -- {"existing":30,"mailchecker":20,"reacher":50}
    enabled        jsonb NOT NULL,     -- {"mailchecker":true,"reacher":false}
    paid_enabled   boolean NOT NULL DEFAULT true,
    paid_threshold integer NOT NULL DEFAULT 75 CHECK (paid_threshold BETWEEN 0 AND 100),
    paid_min_score integer NOT NULL DEFAULT 50 CHECK (paid_min_score BETWEEN 0 AND 100),
    updated_at     timestamptz NOT NULL
);
```

Weights and flags are JSONB keyed by provider, so a new provider is a code change and
a settings edit, never a migration.

## 6. API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/verification/settings` | current settings + the provider catalogue + live provider health |
| `PUT` | `/verification/settings` | update weights, flags, thresholds; 422 when weights ≠ 100 |

`EmailVerification` gains `free_score`; `EmailVerificationDetail` gains
`provider_results`. `VerificationStats` gains the settings-derived numbers the
dashboard needs. Everything stays spec-first: `api/openapi.yaml` then `make gen-api`.

## 7. Reacher licensing — read before enabling

`check-if-email-exists` is **dual-licensed**: AGPL-3.0, or a commercial licence sold
by Reacher. Karvon is proprietary and is served to users over a network, which is
precisely the case AGPL §13 covers, so the AGPL route would oblige us to publish the
corresponding source of the combined work. The practical positions are:

1. **Do not enable it** (the shipped default). No obligation.
2. **Buy the commercial licence** at <https://reacher.email/pricing>. Then the
   proprietary integration is covered, and nothing in this repository changes.
3. **Enable it under AGPL** only if legal accepts the source-availability obligation.

Because we talk to Reacher **only over HTTP, from a separate container, using its
documented public API**, no Reacher code is linked into, vendored by, or distributed
with the Karvon binary. That is the weakest possible form of coupling and it is the
reason this integration is a client package rather than a library dependency — but it
is not by itself a legal conclusion, and this document is not legal advice.

Two further operational notes:

- Reacher's backend can be configured with a **Commercial License Trial**
  (`RCH__COMMERCIAL_LICENSE_TRIAL__*`). When set, the backend **POSTs every
  verification result back to Reacher's servers**, which for us means customer email
  addresses leaving our infrastructure. We leave it unset, and the Compose file says
  so.
- MailChecker is **MIT** and vendored as an ordinary Go module. No obligations beyond
  retaining the licence text, which `go mod` does.

## 8. Failure handling for Reacher

| Condition | Behaviour |
| --- | --- |
| Timeout | per-request `context` deadline (`KARVON_VERIFY_REACHER_TIMEOUT`, 30 s) |
| 5xx / network error | retried with exponential backoff, `KARVON_VERIFY_REACHER_RETRIES` (2) |
| 429 | retried honouring backoff; exhausted → `unavailable` |
| Repeated failure | circuit breaker opens after N consecutive failures and short-circuits for a cooldown, so a dead Reacher costs one connection attempt per cooldown, not one per address |
| Concurrency | a semaphore caps in-flight requests (`KARVON_VERIFY_REACHER_CONCURRENCY`, 4) so we never exceed the backend's own throttle |
| Health | `GET /version` on the Reacher backend, surfaced through the settings endpoint |
| Any of the above | the address is still scored, from the remaining providers |

## 9. Build order

1. `provider` package, MailChecker adapter, Reacher client — unit tested in isolation.
2. `scoring.go` + `settings.go` — pure functions, unit tested.
3. Migration, `queries/verification_settings.sql`, `make gen-sql`.
4. `api/openapi.yaml`, `make gen-api`.
5. Service + handlers + mapper.
6. `app.go` wiring, `config.go` env vars.
7. Compose, README, `.env.example`.
8. Tests: unit, failure/timeout, handler, integration.
