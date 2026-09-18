() => {
  const SELECTOR = [
    'a[href]', 'button', 'input', 'textarea', 'select',
    '[role="button"]', '[role="link"]', '[role="textbox"]', '[role="checkbox"]',
    '[role="radio"]', '[role="combobox"]', '[role="menuitem"]', '[role="tab"]',
    '[onclick]', '[contenteditable="true"]',
  ].join(', ');

  const MAX_ELEMENTS = 60;
  const els = Array.from(document.querySelectorAll(SELECTOR));
  window.__agentElements = [];
  const lines = [];
  let skippedOffscreen = 0;

  for (const el of els) {
    const rect = el.getBoundingClientRect();
    const style = window.getComputedStyle(el);
    const visible = rect.width > 0 && rect.height > 0
      && style.visibility !== 'hidden' && style.display !== 'none'
      && el.offsetParent !== null;
    if (!visible) continue;

    // Only elements actually intersecting the current viewport -- large pages
    // (e.g. Wikipedia's sidebar of ~300 interlanguage links) otherwise blow
    // the prompt up to thousands of tokens. Use SCROLL to reach the rest.
    const inViewport = rect.bottom > 0 && rect.top < window.innerHeight
      && rect.right > 0 && rect.left < window.innerWidth;
    if (!inViewport) { skippedOffscreen++; continue; }

    if (window.__agentElements.length >= MAX_ELEMENTS) { skippedOffscreen++; continue; }

    const idx = window.__agentElements.length;
    window.__agentElements.push(el);

    const tag = el.tagName.toLowerCase();
    const label = (el.innerText || el.value || el.getAttribute('aria-label')
      || el.getAttribute('placeholder') || el.getAttribute('alt') || '')
      .trim().replace(/\s+/g, ' ').slice(0, 80);

    let extra = '';
    if (tag === 'a') extra = ` href="${(el.getAttribute('href') || '').slice(0, 100)}"`;
    if (tag === 'input') extra = ` type="${el.getAttribute('type') || 'text'}"`;

    lines.push(`[${idx}] <${tag}${extra}> "${label}"`);
  }

  // Also surface visible prose so the agent can read facts off the page, not
  // just see clickable elements. Leaf text-bearing blocks only (a node whose
  // subtree has no other matching block), to avoid duplicating nested text.
  const TEXT_SELECTOR = 'p, li, h1, h2, h3, h4, h5, h6, td, th, dt, dd, caption, figcaption';
  const textEls = Array.from(document.querySelectorAll(TEXT_SELECTOR));
  const textParts = [];
  let textBudget = 1500;
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

  return { url: location.href, title: document.title, elements: lines, skippedOffscreen, visibleText: textParts.join('\n') };
}
