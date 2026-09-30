# fb-scrape service

Dockerized HTTP API that pulls the public **Intro / Details** box from Facebook Pages without logging in: name, category, email, phone and website as separate fields, plus the full text of the box (address, hours, etc.). Requests are spread across a pool of Webshare proxies with per-IP rate limits.

## Files

```
services/fb-scrape/
├── app.py               # FastAPI app: POST /scrape, GET /health
├── pool.py              # proxy pool, rate limiting, concurrent workers
├── scraper.py           # page extraction logic (async Playwright)
├── webshare.py          # loads proxies from the Webshare API at startup
├── Dockerfile
├── requirements.txt
└── .env.example         # copy to .env
```

The container is defined in the repository's root `docker-compose.yml`, behind the `fb-scrape` profile.

## Quick start

Requires Docker.

```bash
cp services/fb-scrape/.env.example services/fb-scrape/.env   # then set WEBSHARE_API_KEY
```

Then set `KARVON_FB_SCRAPE_ENABLED=true` in the root `.env`, and `make dev` builds and starts it alongside Postgres. Check that the proxies loaded:

```bash
docker compose logs fb-scrape | grep loaded
# ... loaded 20 proxies from Webshare (plan 13789984, direct mode)
```

Everyday commands, from the repository root:

| Task | Command |
|---|---|
| Start / apply `.env` or code changes | `make compose-fb-scrape` |
| Stop | `make compose-fb-scrape-down` |
| Logs | `docker compose logs -f fb-scrape` |
| Reload proxy list from Webshare | `docker compose restart fb-scrape` |

The container restarts automatically (`restart: unless-stopped`) until it is stopped.

## API

### `POST /scrape`

Pages can be a page name, `facebook.com/<name>`, or a full URL.

```bash
curl -X POST localhost:8000/scrape \
  -H 'Content-Type: application/json' \
  -d '{"pages": ["FreshThymeMarket", "https://www.facebook.com/EncompassHealth/"]}'
```

Results come back in the same order as the input:

```json
{
  "results": [
    {
      "url": "https://www.facebook.com/FreshThymeMarket",
      "name": "Fresh Thyme Market",
      "category": "Specialty Grocery Store",
      "email": "comments@freshthyme.com",
      "phone": null,
      "website": "http://freshthyme.com/our-stores",
      "lines": ["Fresh food. Friendly faces. ...", "Page · Specialty Grocery Store", "..."]
    },
    {
      "url": "https://www.facebook.com/somepage",
      "name": null,
      "error": "Details section not found (not public, login wall, or layout changed)"
    }
  ]
}
```

- `lines` holds every line of the details box, including fields without a dedicated key (address, hours, reviews).
- A page that fails has an `error` field instead of the data fields.
- The call stays open until every page is done. Large lists take minutes (see [Throughput](#throughput)), so set your HTTP client timeout accordingly.
- If `API_KEY` is set, send it as the `X-API-Key` header.

### `GET /health`

Queue size and the state of each proxy:

```json
{
  "ok": true,
  "queued": 0,
  "proxies": [
    {"proxy": "http://192.46.203.32:5998", "busy": false, "pages_in_window": 1, "cooldown_left": 0.0}
  ]
}
```

### Calling from the API

- API on the host (`make dev`): `http://localhost:8000/scrape`
- API in compose (`make compose-up`): `http://fb-scrape:8000/scrape`, since both services share the compose network.

## How proxy rotation works

1. On startup, the service loads all proxies from the Webshare plan you selected (skipping any Webshare marks invalid).
2. Each proxy loads up to `PAGES_PER_PROXY` (10) pages in a row, with a random `DELAY_MIN`–`DELAY_MAX` (1–2 s) pause between them.
3. It then rests for `COOLDOWN_SECONDS` (60 s) while the next free proxy takes over. The least recently used proxy is picked first.
4. Up to `MAX_CONCURRENCY` (20) proxies work in parallel, each in its own isolated browser context inside one shared Chromium.
5. If a page fails (dead proxy, timeout, or a redirect to Facebook's login/checkpoint), that proxy rests immediately and the page is retried on another proxy (`MAX_RETRIES`).
6. Proxy state is shared across API calls, so concurrent or back-to-back requests still respect the limits.

Images, media and fonts are blocked to save bandwidth and memory.

### Throughput

With 20 proxies: about 200 pages in the first ~35 s, then roughly 130 pages/minute sustained. 500 pages take about 4 minutes.

## Configuration (`.env`)

| Variable | Default | Description |
|---|---|---|
| `WEBSHARE_API_KEY` | — | Webshare API key (dashboard → API → Keys). |
| `WEBSHARE_PLAN` | `static_residential` | Which plan's proxies to load: `static_residential`, `proxy_server`, a numeric plan ID, or empty for the account default. |
| `WEBSHARE_MODE` | `direct` | `direct`, or `backbone` for rotating residential plans. |
| `PROXIES` | — | Extra proxies, comma-separated (`http://user:pass@host:port`). |
| `PROXIES_FILE` | — | Path to a file with one proxy per line. |
| `PAGES_PER_PROXY` | `10` | Pages one IP loads before resting. |
| `COOLDOWN_SECONDS` | `60` | Rest time after the quota, a block, or an error. |
| `DELAY_MIN` / `DELAY_MAX` | `1` / `2` | Random delay (seconds) between pages on the same IP. |
| `MAX_CONCURRENCY` | `20` | Proxies used in parallel. |
| `MAX_RETRIES` | `1` | Retries of a failed page on another proxy. |
| `MAX_PAGES_PER_REQUEST` | `500` | Upper limit on pages per `/scrape` call. |
| `API_KEY` | — | If set, clients must send it in the `X-API-Key` header. |
| `LOG_LEVEL` | `INFO` | Python log level. |

With no proxies configured, the service uses the container's own IP under the same limits.

If `WEBSHARE_PLAN=static_residential` finds no matching plan, the container exits with an error listing every plan on the account (ID, type, subtype); set `WEBSHARE_PLAN` to the right ID. To list plans yourself:

```bash
curl -s -H "Authorization: Token YOUR_KEY" https://proxy.webshare.io/api/v2/subscription/plan/ | python3 -m json.tool
```

## Operational notes

- **Memory:** each concurrent proxy uses about 100–150 MB. 20 in parallel needs 2–3 GB; give Docker at least 4 GB, or lower `MAX_CONCURRENCY`.
- **Single process:** proxy cooldown state lives in memory, so the service must run as one uvicorn worker (the Dockerfile does this). To scale out, run another container with a separate set of proxies.
- **Secrets:** `.env` contains your Webshare key. Don't commit it.
- **Image size:** about 1.3 GB (headless Chromium plus system libraries).
- **Formatting varies by proxy location:** e.g. the same phone may come back as `(314) 886-7706` or `+1 314-886-7706`.

## Caveats

- Facebook's terms prohibit automated scraping. Expect login walls, blocks or IP bans at volume, even with residential proxies.
- Only publicly visible, logged-out data is read. Profiles or Pages that hide their details box return an `error`.
- Extraction depends on Facebook's page layout (section headings like "Intro" / "Details"). If Facebook changes it, results will start returning `Details section not found`.
