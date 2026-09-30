"""Load the proxy list from the Webshare API (https://apidocs.webshare.io/proxy-list/list)."""
import asyncio
import json
import logging
import urllib.request
from urllib.parse import quote

log = logging.getLogger("fb-scrape")

API = "https://proxy.webshare.io/api/v2"
BACKBONE_HOST = "p.webshare.io"  # backbone mode entries have no proxy_address

# WEBSHARE_PLAN names -> plan proxy_subtype values in the Webshare API.
PLAN_SUBTYPES = {
    "static_residential": {"isp"},
    "proxy_server": {"default", "premium", "datacenter_and_isp"},
}


def _get(url: str, api_key: str) -> dict:
    req = urllib.request.Request(url, headers={"Authorization": f"Token {api_key}"})
    with urllib.request.urlopen(req, timeout=20) as resp:
        return json.load(resp)


def _paginate(url: str, api_key: str) -> list[dict]:
    items = []
    while url:
        data = _get(url, api_key)
        items += data["results"]
        url = data.get("next")
    return items


def _resolve_plan_id(api_key: str, plan: str) -> str | None:
    """'' -> account default plan; digits -> that plan id; a PLAN_SUBTYPES name -> matching active plan."""
    if not plan or plan.isdigit():
        return plan or None
    if plan not in PLAN_SUBTYPES:
        raise ValueError(f"WEBSHARE_PLAN must be a plan id or one of {list(PLAN_SUBTYPES)}, got {plan!r}")
    plans = _paginate(f"{API}/subscription/plan/", api_key)
    matches = [p for p in plans
               if p.get("status") == "active" and p.get("proxy_subtype") in PLAN_SUBTYPES[plan]]
    if not matches:
        found = [(p.get("id"), p.get("proxy_type"), p.get("proxy_subtype"), p.get("status")) for p in plans]
        raise RuntimeError(f"no active {plan} plan on this Webshare account; plans found "
                           f"(id, type, subtype, status): {found}. Set WEBSHARE_PLAN to an id instead.")
    if len(matches) > 1:
        log.warning("several %s plans %s; using %s (set WEBSHARE_PLAN=<id> to pick)",
                    plan, [p["id"] for p in matches], matches[0]["id"])
    return str(matches[0]["id"])


def _fetch(api_key: str, mode: str, plan: str) -> list[str]:
    plan_id = _resolve_plan_id(api_key, plan)
    url = f"{API}/proxy/list/?mode={mode}&page=1&page_size=100"
    if plan_id:
        url += f"&plan_id={plan_id}"
    urls = []
    for p in _paginate(url, api_key):
        if mode == "direct" and not p.get("valid", True):
            continue
        host = p.get("proxy_address") or BACKBONE_HOST
        user, pw = quote(p["username"], safe=""), quote(p["password"], safe="")
        urls.append(f"http://{user}:{pw}@{host}:{p['port']}")
    log.info("loaded %d proxies from Webshare (plan %s, %s mode)", len(urls), plan_id or "default", mode)
    return urls


async def fetch_webshare_proxies(api_key: str, mode: str = "direct", plan: str = "") -> list[str]:
    return await asyncio.to_thread(_fetch, api_key, mode, plan)
