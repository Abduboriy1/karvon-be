"""HTTP API for the Facebook details scraper.

    POST /scrape   {"pages": ["FreshThymeMarket", "https://www.facebook.com/metroflats"]}
                   -> {"results": [{url, name, category, email, phone, website, lines} | {url, error}]}
    GET  /health   -> worker/proxy status

Run with ONE uvicorn worker: proxy cooldowns live in this process.
"""
import logging
import os
import secrets
from contextlib import asynccontextmanager

from fastapi import FastAPI, Header, HTTPException
from pydantic import BaseModel, Field

from pool import Dispatcher, Settings
from scraper import normalize_url

logging.basicConfig(level=os.getenv("LOG_LEVEL", "INFO"),
                    format="%(asctime)s %(levelname)s %(name)s: %(message)s")

API_KEY = os.getenv("API_KEY")  # optional; when set, clients must send X-API-Key
MAX_PAGES = int(os.getenv("MAX_PAGES_PER_REQUEST", 500))

dispatcher = Dispatcher(Settings.from_env())


@asynccontextmanager
async def lifespan(_: FastAPI):
    await dispatcher.start()
    yield
    await dispatcher.stop()


app = FastAPI(title="fb-scrape", lifespan=lifespan)


class ScrapeRequest(BaseModel):
    pages: list[str] = Field(min_length=1, max_length=MAX_PAGES,
                             description="Page names or URLs, e.g. 'FreshThymeMarket'")


def check_key(key: str | None) -> None:
    if API_KEY and not (key and secrets.compare_digest(key, API_KEY)):
        raise HTTPException(status_code=401, detail="invalid or missing X-API-Key")


@app.post("/scrape")
async def scrape(req: ScrapeRequest, x_api_key: str | None = Header(default=None)):
    check_key(x_api_key)
    results = await dispatcher.scrape([normalize_url(p) for p in req.pages])
    return {"results": results}


@app.get("/health")
async def health():
    return {"ok": True, "queued": dispatcher.queue.qsize(), "proxies": dispatcher.pool.status()}
