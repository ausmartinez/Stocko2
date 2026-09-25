import re
from urllib.parse import urljoin

from bs4 import BeautifulSoup

from news import net
from news.config import MAX_ARTICLE_CHARS
from news.models import NewsItem

WS_RE = re.compile(r"\s+")
# Press-release exhibits first, then the 8-K body itself
SEC_DOC_PRIORITY = ("EX-99.1", "EX-99", "8-K")


def _visible_text(html: str) -> str:
    soup = BeautifulSoup(html, "lxml")
    for tag in soup(["script", "style", "nav", "header", "footer", "aside", "form", "noscript"]):
        tag.decompose()
    paragraphs = [p.get_text(" ", strip=True) for p in soup.find_all("p")]
    text = " ".join(p for p in paragraphs if len(p) > 40)
    if len(text) < 200:  # SEC filings and some sites don't use <p>
        text = soup.get_text(" ", strip=True)
    return WS_RE.sub(" ", text)


def _sec_primary_doc(index_url: str):
    resp = net.get(index_url)
    resp.raise_for_status()
    table = BeautifulSoup(resp.text, "lxml").find("table", class_="tableFile")
    if not table:
        return None
    docs = {}
    for row in table.find_all("tr")[1:]:
        cells = row.find_all("td")
        link = row.find("a")
        if len(cells) >= 4 and link:
            docs.setdefault(cells[3].get_text(strip=True).upper(), link["href"].replace("/ix?doc=", ""))
    for doc_type in SEC_DOC_PRIORITY:
        for found_type, href in docs.items():
            if found_type.startswith(doc_type):
                return urljoin(index_url, href)
    return None


def fetch_text(item: NewsItem) -> str:
    """Best-effort article body; falls back to the feed title + summary."""
    fallback = f"{item.title}. {item.summary}"
    try:
        url = _sec_primary_doc(item.link) if item.source == "SEC 8-K" else item.link
        if not url:
            return fallback
        resp = net.get(url)
        resp.raise_for_status()
        body = _visible_text(resp.text)
    except Exception:
        return fallback
    if len(body) < len(item.summary):
        return fallback
    return f"{item.title}. {body}"[:MAX_ARTICLE_CHARS]
