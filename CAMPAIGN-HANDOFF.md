# Campaign module — state of the work

Written at the end of the first implementation session. The backend is complete and
green; the frontend has its foundation and needs its pages. Read this, then
`README.md` → "Campaigns" for the domain itself.

---

## Where things stand

| Area | State |
| --- | --- |
| Research (Instantly v2, Mailchimp v3, OpenAI) | Done. Findings in `/Users/bory/.claude/plans/i-want-you-to-sharded-kay.md` Part 1, decisions in `README.md` → Campaigns and ADRs 17–21. |
| Database (`migrations/00007_campaigns.sql`) | Done. 22 tables, 3 guard triggers. |
| sqlc queries + generated code | Done, committed and diff-clean under `make gen`. |
| Domain packages (`internal/campaign/…`) | Done, with unit tests. |
| Provider clients (Instantly, Mailchimp, AI) + fakes | Done, with unit tests against `httptest`. |
| River jobs (13 workers, 6 periodic) | Done. |
| HTTP API (`api/openapi.yaml`, 78 new operations) + handlers | Done. |
| Backend tests | **Green**: `go build ./...`, `golangci-lint run` (0 issues), `go test ./internal/...`, and `KARVON_INTEGRATION=1 go test ./tests/` (~9 min). |
| Docs | `README.md` Campaigns section, env table, ADRs 17–21. `.env.example` updated. |
| Frontend foundation | Done: types, `api.ts`, `queryKeys.ts`, composables, `lib/`, `theme.ts`, `nav.ts`, routes, MSW mocks, 40 unit tests. `pnpm typecheck`, `pnpm lint`, `pnpm test` all pass. |
| **Frontend pages and components** | **Not started. This is the remaining work.** |

---

## What to do next

### 1. Frontend pages (the only outstanding deliverable)

`src/modules/campaigns/routes.ts` already points at nine page files that do not exist
yet, so `pnpm build` fails on exactly those imports and nothing else. Create them
under `src/modules/campaigns/pages/` plus their components under
`src/modules/campaigns/components/`:

| Route | Page | Contents |
| --- | --- | --- |
| `/campaigns` | `CampaignsOverviewPage.vue` | `StatCard` row (sends, reply rate, positive reply rate, bounce rate, newsletter conversion), funnel bars, recent activity |
| `/campaigns/all` | `CampaignsListPage.vue` | table + filters + create dialog |
| `/campaigns/all/:id/:tab?` | `CampaignDetailPage.vue` | local `Tabs`: overview, leads, content, variants, accounts, analytics, activity, newsletter, settings; launch button gated by `useCampaignChecklist` |
| `/campaigns/contacts` | `ContactsPage.vue` | table, `ContactSheet`, `ConsentDialog`, `SuppressDialog` |
| `/campaigns/content` | `ContentLibraryPage.vue` | Components / Variants tabs, editor sheet, variant builder with live preview |
| `/campaigns/ai` | `AIGeneratorPage.vue` | brief form → prompt with Copy → paste → parsed preview → import |
| `/campaigns/accounts` | `SendingAccountsPage.vue` | mirror table with health and bounce rate |
| `/campaigns/newsletter` | `NewsletterPage.vue` | Audiences / Eligible / Subscriptions tabs |
| `/campaigns/integrations` | `IntegrationsPage.vue` | provider cards, webhook status, event log, sync runs |

Everything they need already exists: the composable names are listed in
`src/modules/campaigns/composables/`, colours come from `src/config/theme.ts`
(never inline), and the MSW mocks in `tests/mocks/` serve every endpoint, so
`VITE_USE_MOCKS=true pnpm dev` shows real-looking data. Follow
`src/modules/verify/pages/VerificationPage.vue` for the page skeleton
(PageHeader → filters Card → TableSkeleton / ErrorState / EmptyState → table →
DataTablePagination → Sheet) and the density rules in `docs/PLAN.md`.

Finish with `pnpm typecheck && pnpm lint && pnpm build && pnpm test`, and add
`tests/e2e/campaigns.spec.ts`.

### 2. Validation that needs real credentials

Nothing below blocks the code; all of it is "confirm the provider behaves as
documented". Each has a fallback already in place.

- **Instantly custom variables.** The whole local-rendering design assumes
  `{{k_subject_1}}` in a step's subject and `{{k_body_1}}` in its body expand per
  lead, and that the body's HTML is not escaped. Launch a one-lead campaign and
  read the delivered email. If it does not hold, `campaigns.settings.variant_mode`
  is reserved for a native-variant fallback (one Instantly variant per local
  variant, losing weights).
- **Instantly `email_sent` payload.** The parser treats every field as optional, so
  a missing `step` or `email_id` degrades rather than breaks; capture a few real
  deliveries from `provider_events` and tighten `InstantlyDedupeKey` if they carry
  something better than a content hash.
- **`POST /leads/add` partial success.** The push resolves an address Instantly did
  not create by asking `POST /leads/list`; confirm the real counters agree.
- **Mailchimp compliance state.** The sentinel matches on the substring
  `compliance`; confirm the real `title`/`detail`, and confirm that a `pending`
  push to a previously unsubscribed address really does re-send the confirmation.
- **Mailchimp signing secret.** Confirm `POST /lists/{id}/webhooks` returns
  `signing_secret` on the account in use. Without one, that audience's webhook
  rejects everything (by design) and reconciliation carries the load.
- **`KARVON_PUBLIC_BASE_URL`** must be set before either webhook can be registered.

### 3. Smaller follow-ups

- **CSV lead import.** `contacts.source` already accepts `csv`; only the endpoint
  and the UI are missing.
- **Reply classification.** Positive comes from Instantly's interest status today.
  `email_sends.reply_classification` is ready for a local classifier.
- **Subsequences, per-step content beyond step 1.** The schema supports five steps
  and the UI assumes one; the variant builder just needs a step picker.
- **`Instantly.AddLeadsResult.InvalidEmails`** is not in the documented response, so
  the push falls back to a lookup; drop the fallback if the field turns out to exist.

---

## Things worth knowing before you touch this code

1. **Two clocks.** The campaign service and its workers read an injected clock
   (`service.SetClock`, `jobs.Deps.Now`), which the integration harness fixes so a
   timeline can be asserted in order. Any new event must be stamped from that clock,
   never `time.Now()`, or it will sort wrongly in tests and drift in production.

2. **River unique-by-args bites fan-out jobs.** Args with no distinguishing field
   collapse into one job, so an operator pressing "sync now" was silently swallowed
   by the periodic pass. Every manually triggerable job therefore carries a
   `RequestID` that is zero for the periodic run and fresh for an explicit one. If
   you add a job of that shape, do the same.

3. **The assignment is the unit of attribution.** `variant_assignments` is written
   before the push and locked the moment Instantly acknowledges the lead; a trigger
   refuses any later edit. Re-weighting a campaign bumps `weights_version` and
   affects only leads assigned afterwards. Never "fix up" an assignment.

4. **Suppression has exactly one entry point**, `internal/campaign/suppression.Apply`.
   It decides removal from Instantly by "did this lead ever reach the provider",
   not by the rows it just stopped — a bounce marks the lead terminal first, and an
   earlier version therefore left bounced leads sitting in Instantly.

5. **Consent is checked three times**: when the contact is queued, again inside the
   push job immediately before the call, and by the `contacts_guard_stage` trigger.
   Interest never implies consent, at any of the three.

6. **Webhooks answer 2xx as soon as the delivery is stored.** Applying it is a
   separate job. Do not move work into the handler: a slow apply would make the
   provider disable the webhook.

7. **`KARVON_TEST_LOG=1`** turns on the service's own logs in the integration suite.
   That is how you find a 500 that the API reports as `internal server error`.

---

## Command reference

```bash
make gen                                   # sqlc + oapi-codegen; CI fails if the output differs
make lint && make test                     # unit
KARVON_INTEGRATION=1 go test ./tests/ -count=1 -timeout 40m   # full integration, ~9 min
KARVON_INTEGRATION=1 KARVON_TEST_LOG=1 go test ./tests/ -run TestX -v   # with service logs
```

```bash
cd ../karvon-fe
pnpm typecheck && pnpm lint && pnpm test
VITE_USE_MOCKS=true pnpm dev               # the whole Campaigns area against MSW
```
