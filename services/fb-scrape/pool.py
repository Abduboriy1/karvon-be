"""Proxy pool + worker dispatcher.

Each proxy loads up to PAGES_PER_PROXY pages in a row (random DELAY_MIN..DELAY_MAX seconds
between them), then rests for COOLDOWN_SECONDS while the next free proxy takes over.
Different proxies run in parallel, up to MAX_CONCURRENCY at once.
"""
import asyncio
import logging
import os
import random
import time
from dataclasses import dataclass, field
from urllib.parse import unquote, urlsplit

from playwright.async_api import Browser, async_playwright
from playwright.async_api import Error as PWError

from scraper import Blocked, scrape_page
from webshare import fetch_webshare_proxies

log = logging.getLogger("fb-scrape")

# Resource types not needed to read text; blocking them saves bandwidth and memory.
BLOCKED_RESOURCES = {"image", "media", "font"}


@dataclass
class Settings:
    proxies: list[str]
    pages_per_proxy: int = 10
    cooldown: float = 60
    delay_min: float = 1
    delay_max: float = 2
    max_concurrency: int = 20
    max_retries: int = 1
    webshare_api_key: str | None = None
    webshare_mode: str = "direct"
    webshare_plan: str = "static_residential"

    @classmethod
    def from_env(cls) -> "Settings":
        proxies = [p.strip() for p in os.getenv("PROXIES", "").split(",") if p.strip()]
        if path := os.getenv("PROXIES_FILE"):
            with open(path) as f:
                proxies += [l.strip() for l in f if l.strip() and not l.startswith("#")]
        return cls(
            proxies=proxies,
            pages_per_proxy=int(os.getenv("PAGES_PER_PROXY", 10)),
            cooldown=float(os.getenv("COOLDOWN_SECONDS", 60)),
            delay_min=float(os.getenv("DELAY_MIN", 1)),
            delay_max=float(os.getenv("DELAY_MAX", 2)),
            max_concurrency=int(os.getenv("MAX_CONCURRENCY", 20)),
            max_retries=int(os.getenv("MAX_RETRIES", 1)),
            webshare_api_key=os.getenv("WEBSHARE_API_KEY") or None,
            webshare_mode=os.getenv("WEBSHARE_MODE", "direct"),
            webshare_plan=os.getenv("WEBSHARE_PLAN", "static_residential").strip(),
        )


def parse_proxy(proxy: str) -> dict:
    """'http://user:pass@host:port' -> Playwright proxy dict (credentials go in separate fields)."""
    parts = urlsplit(proxy if "://" in proxy else f"http://{proxy}")
    out = {"server": f"{parts.scheme}://{parts.hostname}:{parts.port}"}
    if parts.username:
        out["username"] = unquote(parts.username)
        out["password"] = unquote(parts.password or "")
    return out


@dataclass
class Proxy:
    config: dict | None  # None = direct connection (no proxies configured)
    label: str
    used: int = 0
    busy: bool = False
    cooldown_until: float = 0
    last_used: float = 0


class ProxyPool:
    def __init__(self, proxies: list[str], pages_per_proxy: int, cooldown: float):
        self.proxies = [Proxy(parse_proxy(p), parse_proxy(p)["server"]) for p in proxies] \
            or [Proxy(None, "direct")]
        self.pages_per_proxy = pages_per_proxy
        self.cooldown = cooldown
        self._cond = asyncio.Condition()

    async def acquire(self) -> Proxy:
        """Wait for a proxy that is neither busy nor cooling down; least recently used first."""
        async with self._cond:
            while True:
                now = time.monotonic()
                free = [p for p in self.proxies if not p.busy and p.cooldown_until <= now]
                if free:
                    p = min(free, key=lambda p: p.last_used)
                    if now - p.last_used >= self.cooldown:
                        p.used = 0  # idle long enough to count as rested
                    p.busy = True
                    return p
                resting = [p.cooldown_until for p in self.proxies if not p.busy]
                timeout = max(0.0, min(resting) - now) if resting else None
                try:
                    await asyncio.wait_for(self._cond.wait(), timeout)
                except asyncio.TimeoutError:
                    pass

    async def release(self, p: Proxy, bench: bool = False) -> None:
        """Hand the proxy back; it rests if it hit its page quota or got blocked/failed."""
        async with self._cond:
            now = time.monotonic()
            p.busy = False
            p.last_used = now
            if bench or p.used >= self.pages_per_proxy:
                p.cooldown_until = now + self.cooldown
                p.used = 0
            self._cond.notify_all()

    def status(self) -> list[dict]:
        now = time.monotonic()
        return [{
            "proxy": p.label,
            "busy": p.busy,
            "pages_in_window": p.used,
            "cooldown_left": round(max(0.0, p.cooldown_until - now), 1),
        } for p in self.proxies]


@dataclass
class Job:
    url: str
    future: asyncio.Future
    attempts: int = field(default=0)


class Dispatcher:
    def __init__(self, settings: Settings):
        self.s = settings
        self.pool: ProxyPool | None = None  # built in start(), once the proxy list is loaded
        self.queue: asyncio.Queue[Job] = asyncio.Queue()
        self._browser: Browser | None = None
        self._browser_lock = asyncio.Lock()
        self._workers: list[asyncio.Task] = []

    async def start(self) -> None:
        proxies = list(self.s.proxies)
        if self.s.webshare_api_key:
            proxies += await fetch_webshare_proxies(
                self.s.webshare_api_key, self.s.webshare_mode, self.s.webshare_plan)
        self.pool = ProxyPool(proxies, self.s.pages_per_proxy, self.s.cooldown)
        self._pw = await async_playwright().start()
        await self._get_browser()
        # A proxy is used by one worker at a time, so more workers than proxies would just idle.
        n = min(self.s.max_concurrency, len(self.pool.proxies))
        self._workers = [asyncio.create_task(self._worker()) for _ in range(n)]
        log.info("started %d worker(s) over %d proxy slot(s)", n, len(self.pool.proxies))

    async def stop(self) -> None:
        for w in self._workers:
            w.cancel()
        await asyncio.gather(*self._workers, return_exceptions=True)
        if self._browser:
            await self._browser.close()
        await self._pw.stop()

    async def scrape(self, urls: list[str]) -> list[dict]:
        """Queue every URL and wait for all results, returned in input order."""
        loop = asyncio.get_running_loop()
        jobs = [Job(u, loop.create_future()) for u in urls]
        for job in jobs:
            self.queue.put_nowait(job)
        return await asyncio.gather(*(j.future for j in jobs))

    async def _get_browser(self) -> Browser:
        async with self._browser_lock:  # relaunch if Chromium crashed
            if not (self._browser and self._browser.is_connected()):
                self._browser = await self._pw.chromium.launch(headless=True)
            return self._browser

    async def _new_context(self, proxy: Proxy):
        browser = await self._get_browser()
        ctx = await browser.new_context(
            viewport={"width": 1280, "height": 900}, locale="en-US", proxy=proxy.config
        )

        async def block(route):
            if route.request.resource_type in BLOCKED_RESOURCES:
                await route.abort()
            else:
                await route.continue_()

        await ctx.route("**/*", block)
        return ctx

    def _retry_or_fail(self, job: Job, err: Exception, proxy: Proxy) -> None:
        job.attempts += 1
        log.warning("%s via %s failed (attempt %d): %s", job.url, proxy.label, job.attempts, err)
        if job.attempts <= self.s.max_retries:
            self.queue.put_nowait(job)  # goes to another proxy
        elif not job.future.done():
            job.future.set_result({"url": job.url, "name": None,
                                   "error": f"{type(err).__name__}: {err}"})

    async def _worker(self) -> None:
        while True:
            job: Job | None = await self.queue.get()
            proxy = await self.pool.acquire()
            bench = False
            try:
                ctx = await self._new_context(proxy)
                try:
                    page = await ctx.new_page()
                    # Keep the random gap even when this IP just finished a previous batch.
                    first = time.monotonic() - proxy.last_used > self.s.delay_max
                    while job:
                        if not first:
                            await asyncio.sleep(random.uniform(self.s.delay_min, self.s.delay_max))
                        first = False
                        proxy.used += 1
                        try:
                            result = await scrape_page(page, job.url)
                            if not job.future.done():
                                job.future.set_result(result)
                        except (PWError, Blocked) as e:
                            # Dead proxy, timeout or login wall: retry elsewhere and rest this IP.
                            self._retry_or_fail(job, e, proxy)
                            job, bench = None, True
                            break
                        job = None
                        if proxy.used < self.pool.pages_per_proxy:
                            try:
                                job = self.queue.get_nowait()
                            except asyncio.QueueEmpty:
                                pass
                finally:
                    await ctx.close()
            except asyncio.CancelledError:
                raise
            except Exception as e:  # context creation failed, browser crash, etc.
                if job:
                    self._retry_or_fail(job, e, proxy)
                bench = True
            finally:
                await self.pool.release(proxy, bench)
