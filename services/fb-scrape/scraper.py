"""Extract the Intro/Details box from one Facebook page (async Playwright)."""
import re

from playwright.async_api import Page
from playwright.async_api import TimeoutError as PWTimeout

# Headings Facebook uses for the details box (profiles say "Intro", some layouts say "Details").
SECTION_HEADINGS = ["Intro", "Details", "About"]
# Neighbouring sections; the box we want must not contain these.
STOP_HEADINGS = ["Photos", "Friends", "Posts", "Featured", "Reels", "Videos", "Mentions"]
# UI text to drop from the output.
JUNK_LINES = {"See all", "See more", "Edit details", "Add Featured", "Edit bio", "·"}

FIND_SECTION_JS = """
([heads, stops]) => {
  const txt = e => e.textContent.trim();
  let stopEls = null;
  const find = sel => {
    const candidates = [...document.querySelectorAll(sel)].filter(e => heads.includes(txt(e)));
    if (!candidates.length) return null;
    // textContent doesn't force layout like innerText does; only the final match uses innerText.
    stopEls ??= [...document.querySelectorAll('h2, h3, span')].filter(e => stops.includes(txt(e)));
    for (const h of candidates) {
      const headLen = txt(h).length;
      let best = null, n = h;
      for (let i = 0; i < 12 && n.parentElement; i++) {
        n = n.parentElement;
        if (stopEls.some(s => n.contains(s))) break;   // grew into the next section
        if (txt(n).length > headLen) best = n;
      }
      if (best) return best.innerText;
    }
    return null;
  };
  // Real headings are few; only fall back to scanning every span if they miss.
  return find('h2, h3') || find('span');
}
"""

EMAIL_RE = re.compile(r"[\w.+-]+@[\w-]+\.[\w.-]+")
PHONE_RE = re.compile(r"^\+?[\d\s().-]{7,}$")
URL_RE = re.compile(r"^(https?://)?([\w-]+\.)+[a-z]{2,}(/\S*)?$", re.I)


class Blocked(Exception):
    """Facebook sent us to a login/checkpoint page: this IP is likely flagged."""


def normalize_url(page: str) -> str:
    """Accept 'FreshThymeMarket', 'facebook.com/x' or a full URL."""
    page = page.strip()
    if page.startswith(("http://", "https://")):
        return page
    if "facebook.com/" in page:
        return f"https://{page.lstrip('/')}"
    return f"https://www.facebook.com/{page.strip('/')}"


def parse_lines(lines: list[str]) -> dict:
    """Pull out the common fields; everything else stays in 'lines'."""
    out = {"category": None, "email": None, "phone": None, "website": None}
    for line in lines:
        if not out["email"] and EMAIL_RE.search(line):
            out["email"] = EMAIL_RE.search(line).group(0)
        elif not out["phone"] and PHONE_RE.match(line):
            out["phone"] = line
        elif not out["website"] and URL_RE.match(line):
            out["website"] = line
        elif not out["category"] and line.startswith(("Page ·", "Page·")):
            out["category"] = line.split("·", 1)[1].strip()
    return out


async def dismiss_popups(page: Page) -> None:
    """Close the cookie banner and the 'log in to see more' popup when logged out."""
    for loc in (
        page.locator('div[role="dialog"] div[aria-label="Close"]'),
        page.get_by_text("Decline optional cookies"),
        page.get_by_text("Allow all cookies"),
    ):
        for el in await loc.all():
            try:
                await el.click(timeout=1000)
            except Exception:
                pass


async def scrape_page(page: Page, url: str) -> dict:
    """Raises on network/proxy errors and Blocked; returns a result (maybe with 'error') otherwise."""
    await page.goto(url, wait_until="domcontentloaded", timeout=30_000)
    if any(k in page.url for k in ("/login", "/checkpoint")):
        raise Blocked(f"redirected to {page.url}")
    try:
        await page.wait_for_selector("h1", timeout=10_000)
    except PWTimeout:
        pass  # login wall or odd layout; the section lookup below reports it
    await dismiss_popups(page)

    raw = None
    for _ in range(4):  # the box sometimes loads a moment later / below the fold
        try:
            handle = await page.wait_for_function(
                FIND_SECTION_JS, arg=[SECTION_HEADINGS, STOP_HEADINGS], timeout=2000, polling=500
            )
            raw = await handle.json_value()
            break
        except PWTimeout:
            await page.mouse.wheel(0, 600)
            await dismiss_popups(page)

    h1 = page.locator("h1").first
    result = {"url": url, "name": (await h1.inner_text()).strip() if await h1.count() else None}

    if not raw:
        result["error"] = "Details section not found (not public, login wall, or layout changed)"
        return result

    lines = [l.strip() for l in raw.split("\n") if l.strip()]
    lines = [l for l in lines if l not in JUNK_LINES and l not in SECTION_HEADINGS]
    result.update(parse_lines(lines))
    result["lines"] = lines
    return result
