# Playwright (Python, sync API) Reference

This is a reference for writing short Python snippets against an already-open
Playwright `page` object (sync API). It covers navigation, locating elements,
acting on them, reading page state, waiting, and the mistakes that most often
break otherwise-reasonable-looking code.

Everything here assumes a variable named `page` already exists and is ready
to use. Do not create a browser, context, or page yourself.

## 1. Core concepts

**Auto-waiting.** Every action method (`click`, `fill`, `check`, etc.)
automatically waits (up to a default timeout, commonly 30s unless changed)
for its target element to exist, be visible, be stable (not animating), and
be enabled/editable before acting. You do not need manual `sleep`/`wait`
calls before a normal action — but auto-waiting only waits for the element to
become *actionable*; it does not wait for unrelated page state.

**Strict mode.** A `Locator` can match zero, one, or many elements. Calling
an action directly on a locator that currently matches more than one element
raises a **strict mode violation** error rather than guessing which one you
meant. Resolve this with `.first`, `.last`, `.nth(i)`, or a more specific
locator.

**Locators are lazy.** Creating a `Locator` (e.g. `page.locator(...)`,
`page.get_by_role(...)`) does not query the DOM immediately — it's a
saved *strategy* for finding elements, re-evaluated fresh every time you use
it. This is why re-using the same locator variable across multiple actions is
safe even if the page changes in between.

## 2. Navigation

```python
page.goto("https://example.com")
page.goto("https://example.com", wait_until="domcontentloaded")
page.go_back()
page.go_forward()
page.reload()
```

`wait_until` controls what "done navigating" means:

| Value | Waits for |
|---|---|
| `"commit"` | Navigation started, response headers received. Fastest, least safe. |
| `"domcontentloaded"` | The DOM is fully parsed (`DOMContentLoaded` fired). Elements exist and are queryable. **Usually the right default** — the page is interactive well before every last resource finishes. |
| `"load"` (default if omitted) | Every resource (images, stylesheets, iframes, trackers, ads, analytics beacons) has finished loading. On a heavy commercial page this can take a very long time or never fully settle, causing a timeout even though the page has been usable for seconds. |
| `"networkidle"` | No network connections for 500ms. Discouraged by Playwright itself for being unreliable on pages with polling/streaming.

**Rule of thumb:** prefer `wait_until="domcontentloaded"` unless you have a
specific reason to wait for full load. Never set `timeout=0` (waits forever)
as a fix for a slow page — that just turns a timeout into a hang.

Current page info:
```python
page.url          # current URL, a plain string
page.title()      # <title> text
```

## 3. Locators — finding elements

Prefer, in this order: an `id`, a role+accessible-name, a placeholder/label,
visible text, then a structural CSS/attribute selector. Never invent a
selector for text/attributes you weren't actually shown — ground every
selector in something real on the page.

```python
page.locator("#submit-button")                        # by id
page.locator(".product-card")                          # by class
page.locator('a[href*="/dp/"]')                         # CSS attribute-contains
page.locator('input[name="q"]')                         # CSS attribute-equals
page.locator("xpath=//button[text()='Buy']")            # XPath (rarely needed)

page.get_by_role("button", name="Sign in")               # by ARIA role + accessible name
page.get_by_role("link", name="Videos")
page.get_by_role("textbox", name="Search")
page.get_by_text("Add to cart")                          # by visible text (substring by default)
page.get_by_text("Add to cart", exact=True)              # exact text match
page.get_by_label("Email address")                       # by an associated <label>
page.get_by_placeholder("Search products")                # by placeholder attribute
page.get_by_alt_text("Company logo")                      # by an <img alt="...">
page.get_by_title("Close")                                # by the title="" attribute
page.get_by_test_id("checkout-button")                    # by data-testid (if the site sets one)
```

**Why role/text locators can still fail to match what you expect:** the
*accessible name* Playwright computes for `get_by_role`/`get_by_label` is not
always identical to what a human sees as "the text on the button" — it's
computed from ARIA rules (aria-label, aria-labelledby, then visible text
content, recursively). A card or button whose visible text is split across
several nested `<span>` elements can end up with an accessible name that
doesn't match any single substring you'd guess, especially on complex
e-commerce/search-result markup. When a role/text locator resolves to
nothing (times out with "waiting for locator(...)"), that's the usual cause
— fall back to a structural CSS selector grounded in an actual attribute you
can see (an `id`, a `href` pattern, a stable `class`) instead of guessing
another text variant.

**Narrowing and chaining:**
```python
page.locator(".result").first                      # first match
page.locator(".result").last                        # last match
page.locator(".result").nth(2)                       # zero-indexed
page.locator(".result").count()                      # how many currently match (int)
page.locator(".result").all()                        # list of individual Locators, one per match

page.locator(".card").filter(has_text="In stock")     # narrow by descendant text
page.locator(".card").filter(has=page.locator(".badge"))  # narrow by a descendant locator

page.locator(".card").locator(".price")               # descendant: a .price inside each .card
```

## 4. Actions

```python
loc = page.locator("#submit")

loc.click()
loc.click(button="right")               # right-click
loc.dblclick()
loc.click(force=True)                   # skip actionability checks (last resort, e.g. a hidden overlay)

loc.fill("hello@example.com")            # clears the field, then types the full value
loc.press("Enter")                       # a single key
loc.press("Control+A")                   # a chord
loc.type("hello")                        # types character-by-character (slower; rarely needed over fill)

loc.check()                              # tick a checkbox/radio
loc.uncheck()
loc.select_option("value1")              # <select>: by value
loc.select_option(label="Option One")    # <select>: by visible label

loc.hover()
loc.focus()
loc.scroll_into_view_if_needed()

loc.set_input_files("/path/to/file.pdf")  # <input type="file">

page.mouse.wheel(0, 800)                  # scroll the page: (delta_x, delta_y)
page.keyboard.press("PageDown")
```

**Extracting a real element's `href`/attribute at execution time** (rather
than copying text you were shown, which can be stale, truncated, or
formatted differently than the live DOM):
```python
href = page.locator("a.result-link").first.get_attribute("href")
page.goto(href)   # goto resolves a relative href against the current page automatically
```

## 5. Reading page content

```python
loc.text_content()      # raw text content of the element, including hidden descendants
loc.inner_text()        # rendered/visible text only (respects CSS visibility) -- usually what you want
loc.inner_html()        # HTML markup inside the element
loc.get_attribute("href")
loc.input_value()       # current value of an <input>/<textarea>/<select>

loc.is_visible()
loc.is_enabled()
loc.is_checked()
loc.is_editable()

page.content()           # full page HTML, as a string
```

## 6. Waiting

Most of the time you don't need these explicitly — actions auto-wait for
their own target. Use them when you need to wait for something that isn't
the direct target of the next action:

```python
page.wait_for_selector("#results", state="visible")   # state: "attached"|"detached"|"visible"|"hidden"
page.wait_for_url("**/checkout")
page.wait_for_load_state("domcontentloaded")            # or "load", "networkidle"
page.wait_for_timeout(1000)                              # a plain sleep, milliseconds -- last resort only
loc.wait_for(state="visible")
```

## 7. Common pitfalls (in order of how often they actually bite)

1. **`wait_until="load"` timing out on a heavy page.** Fix: use
   `"domcontentloaded"` for navigation.
2. **Strict mode violation ("resolved to N elements").** Fix: add `.first`
   (if any match is fine) or narrow the selector (if a *specific* one
   matters — `.first` on the wrong locator silently picks the wrong element
   rather than erroring, which is worse than the violation was).
3. **A role/text locator matches nothing** even though the text is visible
   on screen. Fix: the accessible name computation didn't match what you
   guessed — use a structural selector (id, href pattern, class) grounded in
   the real HTML instead of another text guess.
4. **Copying a long `href` string directly into `page.goto("...")`.**
   Tracking/redirect URLs are often very long and easy to see truncated or
   miscopied. Fix: get the element and read its `href` live with
   `.get_attribute("href")`, then `page.goto()` that value.
5. **Clicking something that opens a new tab.** A link with
   `target="_blank"` opens a new page/tab in the same browser context; the
   original `page` object does **not** follow it. (If your environment's
   `page` variable is managed for you, this is usually handled by always
   giving you the most recently active tab on the next call — but the
   in-flight code that did the click will not itself see the new tab.)
6. **An element exists but isn't interactable yet** (still animating in,
   covered by a modal/overlay, or below the fold). Auto-waiting handles most
   of this, but a stubborn case may need `.scroll_into_view_if_needed()`
   first, or waiting for the covering element to disappear.
7. **`timeout=0`** on any call means "wait forever," not "don't wait." Never
   use it to work around a slow page — fix the underlying wait condition
   instead.
