"""100-task evaluation of the browser agent against the real web.

Run against the user's OWN Chrome via ext_server.py's request inbox, because the
sites worth testing (search engines, marketplaces, job boards) rate-limit and
CAPTCHA-wall an automated throwaway profile within a couple of requests --
the same wall that stalled the earlier Playwright-driven eval.

Every task is public and read-mostly: search, navigate, filter, read,
extract, compare. Nothing sends, posts, buys, deletes or changes account
settings, because this runs against real logged-in sessions.

  python eval100.py enqueue [N]   send the first N tasks (default: all)
  python eval100.py sample [N]    send N per category instead
  python eval100.py status        progress counts
  python eval100.py report        per-task results + failure breakdown

Tasks are submitted to the extension's request inbox, so "Start listening"
in the popup is what actually works through them.

`check` is a coarse did-it-land-in-the-right-place signal, not a judgement
of the agent's answer -- extraction quality still needs the step log read by
hand, so `report` prints those too.
"""
import json
import sys
from collections import Counter

import requests

SERVER = "http://127.0.0.1:8092"


def t(id, category, start_url, task, check=None):
    return {"id": id, "category": category, "start_url": start_url, "task": task, "check": check}


def url(v):
    return {"type": "url_contains", "value": v}


def body(v):
    return {"type": "body_contains", "value": v}


def title(v):
    return {"type": "title_contains", "value": v}


def anytab(v):
    return {"type": "any_tab_url_contains", "value": v}


TASKS = [
    # ---- web search: the single most common ask -------------------------
    t("s01", "search", "https://duckduckgo.com", "Search for 'best mechanical keyboards 2026'.", url("q=")),
    t("s02", "search", "https://duckduckgo.com", "Search for 'python asyncio tutorial' and open the first result.", None),
    t("s03", "search", "https://duckduckgo.com", "Search for 'electric cars India price' and go to the second page of results.", url("q=")),
    t("s04", "search", "https://duckduckgo.com", "Search for 'RBI repo rate' and read what the current rate is.", url("q=")),
    t("s05", "search", "https://duckduckgo.com", "Use the bang syntax !w quantum computing to jump straight to Wikipedia.", anytab("wikipedia.org")),
    t("s06", "search", "https://duckduckgo.com", "Search site:github.com langchain and open the first github.com result.", anytab("github.com")),
    t("s07", "search", "https://duckduckgo.com", "Search for 'weather Mumbai tomorrow' and report the forecast.", url("q=")),
    t("s08", "search", "https://www.bing.com", "Search for 'NASA Artemis latest news'.", url("q=")),
    t("s09", "search", "https://duckduckgo.com", "Search for 'how to renew Indian passport', then refine the search by adding 'online'.", url("q=")),
    t("s10", "search", "https://duckduckgo.com", "Search for 'GDP of India 2025' and switch to the News tab of results.", url("q=")),

    # ---- reference lookup ------------------------------------------------
    t("r01", "reference", "https://en.wikipedia.org", "Search Wikipedia for 'Transformer (machine learning)' and open the article.", url("/wiki/")),
    t("r02", "reference", "https://en.wikipedia.org/wiki/Python_(programming_language)", "Find who created Python and in what year.", url("Python")),
    t("r03", "reference", "https://en.wikipedia.org/wiki/India", "Scroll to the Economy section and read the first paragraph.", url("India")),
    t("r04", "reference", "https://en.wikipedia.org/wiki/Artificial_intelligence", "Open the 'History' section link from the table of contents.", url("Artificial_intelligence")),
    t("r05", "reference", "https://en.wikipedia.org/wiki/Mumbai", "Find the population figure listed in the infobox.", body("Mumbai")),
    t("r06", "reference", "https://en.wikipedia.org", "Search for 'Large language model', open it, then follow a link to a related article.", url("/wiki/")),
    t("r07", "reference", "https://www.wikidata.org", "Search Wikidata for 'Tesla Inc'.", None),
    t("r08", "reference", "https://en.wikipedia.org/wiki/Special:Random", "Report the title of this random article and how many sections it has.", url("wikipedia.org")),

    # ---- shopping research (browse only, never add to cart) --------------
    t("p01", "shopping", "https://www.amazon.in", "Search for 'wireless mouse' and report the first result's title.", url("/s?k=")),
    t("p02", "shopping", "https://www.amazon.in", "Search for 'laptop backpack', then sort results by price low to high.", url("/s?k=")),
    t("p03", "shopping", "https://www.amazon.in", "Search for 'usb c hub' and filter to 4 stars and up.", url("/s?k=")),
    t("p04", "shopping", "https://www.amazon.in", "Search 'mechanical keyboard', open the first product and report its price.", url("/s?k=")),
    t("p05", "shopping", "https://www.flipkart.com", "Search for 'bluetooth speaker' and report the first result.", url("q=")),
    t("p06", "shopping", "https://www.flipkart.com", "Search for 'running shoes' and apply a brand filter.", url("q=")),
    t("p07", "shopping", "https://www.amazon.in", "Search for 'ssd 1tb', scroll down and report the 10th result's name.", url("/s?k=")),
    t("p08", "shopping", "https://www.myntra.com", "Search for 'men t shirt' and report how many results are shown.", url("?rawQuery=")),
    t("p09", "shopping", "https://www.amazon.in", "Find today's deals page and report the first deal listed.", url("/deal")),
    t("p10", "shopping", "https://www.flipkart.com", "Search 'air purifier' and compare the prices of the first two results.", url("q=")),

    # ---- travel ----------------------------------------------------------
    t("v01", "travel", "https://www.google.com/travel/flights", "Search flights from Mumbai to Delhi for next month.", None),
    t("v02", "travel", "https://www.makemytrip.com", "Search for flights from Mumbai to Bangalore.", url("makemytrip")),
    t("v03", "travel", "https://www.booking.com", "Search hotels in Goa for a weekend stay.", url("booking.com")),
    t("v04", "travel", "https://www.irctc.co.in/nget/train-search", "Look up trains from Mumbai to Pune.", None),
    t("v05", "travel", "https://www.skyscanner.co.in", "Search for flights from Delhi to Dubai.", None),
    t("v06", "travel", "https://www.redbus.in", "Search buses from Pune to Mumbai.", url("redbus")),
    t("v07", "travel", "https://www.tripadvisor.in", "Search for 'things to do in Jaipur'.", None),
    t("v08", "travel", "https://www.openstreetmap.org", "Search for 'Gateway of India' on the map.", url("openstreetmap")),

    # ---- jobs ------------------------------------------------------------
    t("j01", "jobs", "https://www.naukri.com", "Search for 'python developer' jobs in Bangalore.", url("-jobs")),
    t("j02", "jobs", "https://in.indeed.com", "Search for 'data analyst' jobs in Mumbai.", None),
    t("j03", "jobs", "https://in.indeed.com", "Search 'remote software engineer' and filter to remote only.", None),
    t("j04", "jobs", "https://www.naukri.com", "Search 'machine learning engineer' and report the first listing's company.", url("-jobs")),
    t("j05", "jobs", "https://weworkremotely.com", "Find the newest remote programming job listing.", None),
    t("j06", "jobs", "https://news.ycombinator.com/jobs", "Report the first job posting listed.", url("ycombinator")),

    # ---- finance ---------------------------------------------------------
    t("f01", "finance", "https://finance.yahoo.com", "Look up the current stock price of AAPL.", None),
    t("f02", "finance", "https://finance.yahoo.com", "Search for TSLA and report its day range.", None),
    t("f03", "finance", "https://coinmarketcap.com", "Report the current Bitcoin price.", url("coinmarketcap")),
    t("f04", "finance", "https://coinmarketcap.com", "Find Ethereum's 24-hour percentage change.", url("coinmarketcap")),
    t("f05", "finance", "https://www.nseindia.com", "Look up the current NIFTY 50 level.", None),
    t("f06", "finance", "https://www.google.com/finance", "Search for INFY and report its price.", None),
    t("f07", "finance", "https://www.xe.com", "Convert 100 USD to INR.", url("xe.com")),
    t("f08", "finance", "https://finance.yahoo.com", "Compare the prices of MSFT and GOOGL.", None),

    # ---- developer lookups ----------------------------------------------
    t("d01", "dev", "https://github.com", "Search GitHub for 'fastapi' and open the top repository.", url("/search?q=")),
    t("d02", "dev", "https://github.com/fastapi/fastapi", "Report how many stars this repository has.", url("fastapi")),
    t("d03", "dev", "https://github.com", "Search for repositories written in Rust about 'web framework'.", url("/search?q=")),
    t("d04", "dev", "https://github.com/python/cpython", "Open the Issues tab and report the number of open issues.", url("cpython")),
    t("d05", "dev", "https://pypi.org", "Search PyPI for 'requests' and report the latest version.", url("/search/?q=")),
    t("d06", "dev", "https://www.npmjs.com", "Search npm for 'express' and report its weekly downloads.", url("/search?q=")),
    t("d07", "dev", "https://developer.mozilla.org", "Look up the documentation for Array.prototype.map.", url("mozilla.org")),
    t("d08", "dev", "https://stackoverflow.com", "Search for 'python list comprehension nested' and open the top question.", url("/questions/")),
    t("d09", "dev", "https://stackoverflow.com/questions", "Filter questions by the 'python' tag.", url("stackoverflow")),
    t("d10", "dev", "https://news.ycombinator.com", "Report the title of the current top story.", url("ycombinator")),
    t("d11", "dev", "https://arxiv.org", "Search arXiv for 'retrieval augmented generation' papers.", url("arxiv")),
    t("d12", "dev", "https://hub.docker.com", "Search Docker Hub for the 'postgres' image.", None),

    # ---- media -----------------------------------------------------------
    t("m01", "media", "https://www.youtube.com", "Search YouTube for 'lofi hip hop'.", url("results?search_query=")),
    t("m02", "media", "https://www.youtube.com", "Search for 'python tutorial' and report the first video's title.", url("results?search_query=")),
    t("m03", "media", "https://www.youtube.com", "Search for 'cooking pasta' and filter results to this year.", url("results?search_query=")),
    t("m04", "media", "https://open.spotify.com/search", "Search Spotify for 'jazz playlist'.", None),
    t("m05", "media", "https://www.imdb.com", "Search IMDb for 'Inception' and report its rating.", url("imdb")),
    t("m06", "media", "https://www.imdb.com/chart/top", "Report the current number one film on the Top 250 chart.", url("imdb")),

    # ---- news & weather --------------------------------------------------
    t("n01", "news", "https://news.google.com", "Report the top headline right now.", None),
    t("n02", "news", "https://www.bbc.com/news", "Find the top technology story.", url("bbc.com")),
    t("n03", "news", "https://www.reuters.com", "Search Reuters for 'inflation India'.", None),
    t("n04", "news", "https://timesofindia.indiatimes.com", "Open the Business section and report the lead story.", None),
    t("n05", "news", "https://weather.com", "Look up the 7-day forecast for Mumbai.", None),
    t("n06", "news", "https://www.accuweather.com", "Search for the current temperature in Delhi.", None),
    t("n07", "news", "https://www.reddit.com/r/technology", "Report the current top post.", url("reddit")),
    t("n08", "news", "https://news.ycombinator.com/newest", "Report the newest submission's title.", url("ycombinator")),

    # ---- maps / local ----------------------------------------------------
    t("l01", "local", "https://www.google.com/maps", "Search for coffee shops near Bandra Mumbai.", None),
    t("l02", "local", "https://www.openstreetmap.org", "Find Chhatrapati Shivaji Terminus on the map.", url("openstreetmap")),
    t("l03", "local", "https://www.zomato.com", "Search for restaurants in Powai.", None),
    t("l04", "local", "https://www.justdial.com", "Search for dentists in Andheri.", None),
    t("l05", "local", "https://www.google.com/maps", "Find the opening hours of a pharmacy near Connaught Place Delhi.", None),

    # ---- form mechanics (public demo sites, safe to submit) --------------
    t("w01", "forms", "https://demoqa.com/text-box", "Fill Full Name with 'Priya Sharma' and Email with 'priya@example.com', then submit.", body("priya@example.com")),
    t("w02", "forms", "https://demoqa.com/checkbox", "Expand the tree and tick the Documents checkbox.", body("documents")),
    t("w03", "forms", "https://demoqa.com/radio-button", "Select the 'Impressive' radio button.", body("Impressive")),
    t("w04", "forms", "https://demoqa.com/select-menu", "Open the Old Style Select Menu and choose 'Purple'.", None),
    t("w05", "forms", "https://demoqa.com/date-picker", "Set the date picker to 15 January 2027.", None),
    t("w06", "forms", "https://demoqa.com/webtables", "Add a new record with first name 'Arjun', last name 'Rao', age 30, email 'arjun@example.com', salary 50000, department 'Engineering'.", body("arjun@example.com")),
    t("w07", "forms", "https://demoqa.com/dynamic-properties", "Wait for the button that enables after five seconds, then click it.", None),
    t("w08", "forms", "https://demoqa.com/alerts", "Click the button that shows a simple alert and dismiss it.", None),
    t("w09", "forms", "https://demoqa.com/modal-dialogs", "Open the small modal and then close it.", None),
    t("w10", "forms", "https://httpbin.org/forms/post", "Fill in customer name 'Test User', pick Large size, choose bacon topping, and submit the order.", body("Test User")),

    # ---- data extraction -------------------------------------------------
    t("x01", "data", "https://en.wikipedia.org/wiki/List_of_countries_by_GDP_(nominal)", "Report the top three countries by nominal GDP.", url("GDP")),
    t("x02", "data", "https://www.worldometers.info/world-population/", "Report the current world population figure.", None),
    t("x03", "data", "https://en.wikipedia.org/wiki/List_of_largest_cities", "Find where Mumbai ranks in the list of largest cities.", url("largest_cities")),
    t("x04", "data", "https://www.imdb.com/chart/top", "List the top five films on the chart.", url("imdb")),
    t("x05", "data", "https://coinmarketcap.com", "Report the top five cryptocurrencies by market cap.", url("coinmarketcap")),

    # ---- multi-step composites ------------------------------------------
    t("c01", "composite", "https://duckduckgo.com", "Search for 'best budget laptop 2026', open the first result, and report its top recommendation.", None),
    t("c02", "composite", "https://en.wikipedia.org", "Search for 'Nikola Tesla', open the article, then open a linked article about one of his inventions.", url("/wiki/")),
    t("c03", "composite", "https://github.com", "Search GitHub for 'ollama', open the top repo, then open its README and report what it does.", url("github.com")),
    t("c04", "composite", "https://www.amazon.in", "Search for 'noise cancelling headphones', filter to 4 stars and up, then report the cheapest result.", url("amazon.in")),
    t("c05", "composite", "https://duckduckgo.com", "Find the current price of gold in India, then find the price of silver.", url("q=")),
    t("c06", "composite", "https://pypi.org", "Search PyPI for 'pandas', open it, then find the project's homepage link.", url("pypi.org")),
    t("c07", "composite", "https://www.youtube.com", "Search for 'react hooks explained', open the first video, and report its channel name.", url("youtube.com")),
    t("c08", "composite", "https://en.wikipedia.org", "Look up the population of Tokyo, then look up the population of Delhi.", url("/wiki/")),
]


def _submit(tasks):
    payload = {
        "replace": True,
        "requests": [
            {"id": t["id"], "kind": "instruction", "task": t["task"],
             "start_url": t["start_url"],
             "meta": {"category": t["category"], "check": t["check"]}}
            for t in tasks
        ],
    }
    resp = requests.post(f"{SERVER}/requests/bulk", json=payload, timeout=30)
    resp.raise_for_status()
    return resp.json()["ids"]


def interleave(tasks):
    """Round-robin the categories so stopping the run early still leaves a
    spread across every category, instead of only the first few categories
    the list happens to start with."""
    buckets = {}
    for t in tasks:
        buckets.setdefault(t["category"], []).append(t)
    out, order = [], list(buckets)
    while any(buckets[c] for c in order):
        for c in order:
            if buckets[c]:
                out.append(buckets[c].pop(0))
    return out


def enqueue(limit=None):
    tasks = interleave(TASKS)
    if limit:
        tasks = tasks[: int(limit)]
    ids = _submit(tasks)
    print(f"sent {len(ids)} tasks (interleaved across categories)")
    print("Now click 'Start listening' in the extension popup.")


def sample(per_category=2):
    """A few from every category rather than the first N, which would be all
    search and reference -- breadth finds distinct bugs far faster than depth
    when the point is to shake out failures."""
    per_category = int(per_category)
    picked, seen = [], Counter()
    for t in TASKS:
        if seen[t["category"]] < per_category:
            picked.append(t)
            seen[t["category"]] += 1
    ids = _submit(picked)
    print(f"sent {len(ids)} tasks across {len(seen)} categories")


def status():
    s = requests.get(f"{SERVER}/requests/status", timeout=10).json()
    print(s["counts"])


def report():
    s = requests.get(f"{SERVER}/requests/status", timeout=30).json()
    done = [t for t in s["requests"] if t["status"] == "done" and t.get("result")]
    if not done:
        print("no results yet")
        return

    outcomes, by_cat = Counter(), {}
    for t in done:
        r = t["result"]
        ok = r.get("status") == "done" and r.get("check_passed") is not False
        outcomes[r.get("status")] += 1
        cat = (t.get("meta") or {}).get("category", "?")
        by_cat.setdefault(cat, [0, 0])
        by_cat[cat][1] += 1
        if ok:
            by_cat[cat][0] += 1

    print(f"=== {len(done)} tasks completed ===")
    print("outcomes:", dict(outcomes))
    print("\nby category (landed-correctly / total):")
    for cat, (good, total) in sorted(by_cat.items()):
        print(f"  {cat:<10} {good}/{total}")

    print("\n=== failures ===")
    for t in done:
        r = t["result"]
        if r.get("status") == "done" and r.get("check_passed") is not False:
            continue
        print(f"\n#{t['id']} [{t['category']}] {r.get('status')} check={r.get('check_passed')} {r.get('elapsed_s')}s")
        print(f"  task: {t['task']}")
        print(f"  url:  {r.get('final_url')}")
        if r.get("error"):
            print(f"  err:  {str(r['error'])[:300]}")
        for st in (r.get("steps") or [])[:6]:
            print(f"    {st.get('index')}. [{st.get('status')}] {st.get('instruction')} -> {json.dumps(st.get('action'))[:110]}")


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "status"
    if cmd == "enqueue":
        enqueue(sys.argv[2] if len(sys.argv) > 2 else None)
    elif cmd == "sample":
        sample(sys.argv[2] if len(sys.argv) > 2 else 2)
    elif cmd == "report":
        report()
    else:
        status()
