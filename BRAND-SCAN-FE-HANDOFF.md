# Scan for major businesses: frontend handoff (karvon-fe)

The backend is done and tested. This document covers everything karvon-fe needs to
build a **Scan for major businesses** view inside the Exclusions tab.

## What it does for the user

Karvon targets small businesses, but the scraped data is full of chains: Anytime
Fitness, Planet Fitness, GNC, Walgreens, HOTWORX and so on. Finding them one listing
at a time is slow. The brand scan groups every stored business and lists the groups
with many locations, largest first. For each group the operator can:

- **Exclude it**: the backend suggests the right rule for each group. The operator
  can exclude one group, or tick many and exclude them all at once.
- **Dismiss it** ("this is a small local chain, keep it"): the group stops showing up
  in later scans. Dismissing excludes nothing.

The operator keeps going until the list holds only businesses they want to contact.
Excluded groups and dismissed groups drop out of the default view, so the list gets
shorter as they work.

Real data (25k businesses) gives about 630 domain groups with 3+ locations. Each
scan takes about 350 ms.

### The three groupings: a tab or segmented control on the page

| `group_by` | Groups by | Catches | Suggested rule |
|---|---|---|---|
| `domain` (default) | registrable website domain | brands whose locations have different names: "HOTWORX - Alvin, TX" and "HOTWORX - Waco" both link to hotworx.net | `domain` / `exact` |
| `name` | normalised name | identical names: 150 × "Planet Fitness" | `company` / `exact` |
| `name_prefix` | the first two words of the name | "Crunch Fitness - Amarillo", "Crunch Fitness Lubbock" | `company` / `prefix` |

The domain grouping skips shared platforms such as facebook.com, wixsite.com,
square.site, linktr.ee, booking sites and free-mail domains. On those, many unrelated
small businesses share one domain. A brand hosted on one of them still shows up in
the name groupings.

---

## API contract

Start by regenerating the types with karvon-be running: run `pnpm gen:api`, then
re-export the new types from `src/types/models.ts`: `BrandCandidate`,
`BrandCandidateList`, `BrandScanGroupBy`, `BrandSampleBusiness`,
`BrandScanDismissal`, `BrandScanDismissalRef`, `BrandScanDismissalCreate`,
`BrandScanDismissalList`, `ExclusionBulkCreate`, `ExclusionBulkResult` and
`ExclusionBulkItemResult`.

### `GET /exclusions/brand-scan`

| Param | Type | Default | Notes |
|---|---|---|---|
| `group_by` | `domain \| name \| name_prefix` | `domain` | |
| `min_locations` | int 2–100000 | 3 | fewest listings a group needs |
| `min_cities` | int 1–100000 | 1 | distinct city+state pairs |
| `min_states` | int 0–100000 | 0 | `2` = multi-state brands only |
| `q` | string ≤200 | | matches the key or the most common name |
| `category` | repeatable string | | only groups businesses with this exact category |
| `state` | repeatable string | | only groups businesses in these states (case-insensitive) |
| `include_excluded` | bool | false | also list groups that are 100% excluded already |
| `include_dismissed` | bool | false | also list dismissed groups |
| `sort` | `locations:desc` (default), `cities:desc`, `states:desc`, `reviews:desc`, `names:desc`, `key:asc` | | |
| `page`, `per_page` | | 1, 50 (max 200) | standard |

A bad value returns `422 validation_failed` with field details, for example
`min_locations=1`.

```jsonc
{
  "data": [{
    "group_by": "domain",
    "key": "hotworx.net",                 // stable group id: use as the row key and for dismissals
    "display_name": "HOTWORX - Alvin, TX", // the most common name in the group
    "top_domain": "hotworx.net",          // nullable
    "locations": 102,
    "cities": 80,
    "states": 5,
    "state_list": ["CA", "FL", "MO", "SD", "TX"],
    "distinct_names": 102,
    "distinct_domains": 1,
    "total_reviews": 8812,
    "avg_rating": 4.7,                    // nullable
    "excluded_locations": 0,              // > 0 and < locations means partly covered
    "existing_exclusion": null,           // ExclusionRef when an active rule with the suggested kind+key exists
    "dismissal": null,                    // { id, note } only appears with include_dismissed=true
    "suggested_rule": {                   // a ready GlobalExclusionCreate body
      "kind": "domain", "value": "hotworx.net", "match_mode": "exact",
      "reason": "brand scan: 102 locations in 80 cities", "source": "manual"
    },
    "sample_businesses": [                // ≤5, most-reviewed first
      { "id": "…", "name": "HOTWORX - Alvin, TX", "city": "Alvin", "state": "TX", "domain": "hotworx.net" }
    ]
  }],
  "meta": { "page": 1, "per_page": 50, "total": 629 }
}
```

### `POST /exclusions/bulk` (new): exclude many groups at once

Request: `{ "items": [GlobalExclusionCreate, …] }`, with 1 to 100 items. Send the
selected rows' `suggested_rule` objects as they are.

The response is always `200`. Each item is created independently:

```jsonc
{
  "created": 2,
  "results": [
    { "index": 0, "status": "created",   "exclusion": { /* GlobalExclusion */ } },
    { "index": 1, "status": "duplicate", "error": { "error": { "code": "conflict", "message": "…" } } },
    { "index": 2, "status": "invalid",   "error": { "error": { "code": "validation_failed", "message": "…", "details": [] } } }
  ]
}
```

`status` is one of `created | duplicate | invalid | failed`. Show a toast like
"Excluded 12 brands · 1 already excluded · 1 failed".

This endpoint is general, so other screens can use it too.

### Dismissals

- `POST /exclusions/brand-scan/dismissals` with body `{ group_by, key, note? }`
  returns `201 BrandScanDismissal`. A group that is already dismissed returns `409`.
  Send `key` exactly as the scan returned it.
- `DELETE /exclusions/brand-scan/dismissals/{id}` returns `204`, or `404`.
- `GET /exclusions/brand-scan/dismissals?group_by=&page=&per_page=` returns
  `{ data: BrandScanDismissal[], meta }`, newest first.

A dismissal applies to one grouping only. Dismissing the `name` group "joes pizza"
does not hide the `domain` group joespizza.com.

### Endpoints to reuse

- `POST /exclusions/preview` with `suggested_rule` as the body returns the exact
  coverage (businesses, emails, contacts, live leads), 10 sample businesses and a
  `warning`. Use it for the row's "Preview" action before a single exclude.
- `POST /exclusions` handles a single exclude.

---

## UI plan

1. **Entry point**: add a **Scan for major businesses** button or sub-tab on the
   Exclusions page. Use its own route, such as `/exclusions/brand-scan`, so the
   filters can live in the query string.
2. **Toolbar**:
   - a grouping segmented control (Domain · Name · Name prefix)
   - min locations (number input, default 3)
   - min states (0 / 2+ / 3+)
   - a search box (debounced, `q`)
   - category and state multi-selects
   - toggles for "Show already excluded" and "Show dismissed"
   - a sort select
3. **Table**, one row per group:
   - checkbox
   - display name, with the key under it in muted text
   - locations, cities, states (with a tooltip listing `state_list`)
   - distinct names / domains
   - total reviews and avg rating
   - status chip: `Excluded` when `existing_exclusion` is set or
     `excluded_locations == locations`; `Partly excluded (n/m)`; `Dismissed`
   - an expand chevron that shows `sample_businesses`
   - row actions: **Preview**, which opens the existing preview dialog with
     `suggested_rule`; **Exclude**; **Dismiss**, with an optional note;
     **Undismiss** for dismissed rows
4. **Bulk bar** (appears when rows are selected): **Exclude selected (n)** calls
   `POST /exclusions/bulk` with the selected `suggested_rule`s, then shows a result
   toast, then refetches. **Dismiss selected** loops `POST …/dismissals`.
5. **After an action**, refetch the current page. Excluded and dismissed rows drop out
   by default, and the pagination total shrinks.
6. **Signals that need care**:
   - A `name_prefix` row whose `distinct_names` is high and whose `distinct_domains`
     is also high is usually a generic phrase like "fitness center", not a brand.
     Highlight `distinct_domains > 1` in name groupings.
   - A `domain` row whose names are all different can be a marketplace or directory
     (bluepillow.com, vrbo.com). That is still worth excluding, but the operator
     decides.
7. **Dismissals manager**: add a small drawer or tab listing dismissals with an
   Undismiss button.
8. **MSW mocks**: add handlers for all five endpoints, plus fixtures for each
   grouping, including a partly excluded row and a dismissed row.
9. **Tests**:
   - Toolbar state syncs to the query.
   - Bulk exclude builds the body from `suggested_rule` and shows the toast counts.
   - Dismiss and undismiss work.
   - The status chip logic is correct.
