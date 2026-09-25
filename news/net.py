import threading
import time
from urllib.parse import urlparse

import requests

from news.config import SEC_USER_AGENT, DEFAULT_USER_AGENT, HTTP_TIMEOUT

# SEC allows 10 req/s; stay well under it across worker threads
_SEC_MIN_INTERVAL = 0.15
_sec_lock = threading.Lock()
_sec_last = 0.0


def _is_sec(url: str) -> bool:
    return urlparse(url).netloc.endswith("sec.gov")


def get(url: str, **kwargs) -> requests.Response:
    global _sec_last
    headers = kwargs.pop("headers", {})
    if _is_sec(url):
        headers.setdefault("User-Agent", SEC_USER_AGENT)
        with _sec_lock:
            wait = _SEC_MIN_INTERVAL - (time.monotonic() - _sec_last)
            if wait > 0:
                time.sleep(wait)
            _sec_last = time.monotonic()
    else:
        headers.setdefault("User-Agent", DEFAULT_USER_AGENT)
    return requests.get(url, headers=headers, timeout=kwargs.pop("timeout", HTTP_TIMEOUT), **kwargs)
