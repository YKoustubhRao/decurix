() => {
  // Interactive, visible-in-viewport elements only, rendered as compact synthetic
  // tags carrying just the attributes useful for building a Playwright locator
  // (id/name/role/type/placeholder/aria-label/href) plus their visible text.
  // Raw page.content() is too noisy for a small local model's context budget --
  // it buries the real input under menus/dialogs/icon buttons before the char
  // budget runs out. This is deliberately literal HTML-flavored output (not a
  // numeric index scheme) so the model can write ordinary CSS/role selectors.
  const SELECTOR = [
    'a[href]', 'button', 'input', 'textarea', 'select',
    '[role="button"]', '[role="link"]', '[role="textbox"]', '[role="checkbox"]',
    '[role="radio"]', '[role="combobox"]', '[role="menuitem"]', '[role="tab"]',
    '[role="searchbox"]', '[onclick]', '[contenteditable="true"]',
  ].join(', ');
  const MAX_ELEMENTS = 60;
  const KEEP_ATTRS = ['id', 'name', 'type', 'role', 'placeholder', 'aria-label', 'title', 'href'];

  const els = Array.from(document.querySelectorAll(SELECTOR));
  const lines = [];
  let skipped = 0;

  for (const el of els) {
    const rect = el.getBoundingClientRect();
    const style = window.getComputedStyle(el);
    const visible = rect.width > 0 && rect.height > 0
      && style.visibility !== 'hidden' && style.display !== 'none'
      && el.offsetParent !== null;
    if (!visible) continue;

    const inViewport = rect.bottom > 0 && rect.top < window.innerHeight
      && rect.right > 0 && rect.left < window.innerWidth;
    if (!inViewport) { skipped++; continue; }

    if (lines.length >= MAX_ELEMENTS) { skipped++; continue; }

    const tag = el.tagName.toLowerCase();
    let attrs = '';
    for (const name of KEEP_ATTRS) {
      const val = el.getAttribute(name);
      if (!val) continue;
      // href gets a much longer budget than other attrs -- tracking/redirect links
      // (Bing, Google, Amazon, ...) routinely run past 100 chars, and a truncated
      // href isn't just a cosmetic display issue: it's an invalid URL that fails
      // outright if the model (reasonably) copies what it was shown into goto().
      const cap = name === 'href' ? 300 : 100;
      attrs += ` ${name}="${val.replace(/"/g, "'").slice(0, cap)}"`;
    }
    if (tag === 'input' && el.value) attrs += ` value="${el.value.replace(/"/g, "'").slice(0, 60)}"`;

    const text = (el.innerText || '').trim().replace(/\s+/g, ' ').slice(0, 60);
    lines.push(`<${tag}${attrs}>${text}</${tag}>`);
  }

  // Plain visible prose too -- interactive elements alone miss things like prices,
  // descriptions, or article text that a REASONING-only instruction (e.g. "extract
  // prices as JSON") needs to actually read. Leaf text-bearing blocks only (a node
  // whose subtree has no other matching block), to avoid duplicating nested text.
  const TEXT_SELECTOR = 'p, li, h1, h2, h3, h4, h5, h6, td, th, dt, dd, span, caption, figcaption';
  const textEls = Array.from(document.querySelectorAll(TEXT_SELECTOR));
  const textParts = [];
  let textBudget = 2500;
  for (const el of textEls) {
    if (textBudget <= 0) break;
    if (el.querySelector(TEXT_SELECTOR)) continue; // not a leaf block, skip (avoid dupes)
    const rect = el.getBoundingClientRect();
    const style = window.getComputedStyle(el);
    const visible = rect.width > 0 && rect.height > 0
      && style.visibility !== 'hidden' && style.display !== 'none';
    if (!visible) continue;
    const inViewport = rect.bottom > 0 && rect.top < window.innerHeight
      && rect.right > 0 && rect.left < window.innerWidth;
    if (!inViewport) continue;
    const text = el.innerText.trim().replace(/\s+/g, ' ');
    if (!text) continue;
    textParts.push(text.slice(0, textBudget));
    textBudget -= text.length;
  }

  return {
    url: location.href, title: document.title, elements: lines.join('\n'), skipped,
    visibleText: textParts.join('\n'),
  };
}
