# Decurix teaser site

A single static page (no build step) announcing Decurix and collecting waitlist emails.

| File | What it is |
|---|---|
| `index.html` | The page |
| `styles.css` | All styling, on the Decurix app's own design tokens (`web/templates/theme.html`), light and dark |
| `main.js` | Scroll reveals, the animated hero demo, waitlist submission |
| `favicon.svg`, `og.png` | Tab icon and the 1200×630 link-preview image |

Preview locally by opening `index.html` in a browser, or `python -m http.server -d website 8000`.

## Waitlist

Signups are sent by [Web3Forms](https://web3forms.com) (free, 250 submissions/month) to the inbox the access key was created for. The key is set in `main.js` (`WAITLIST.accessKey`). It is safe to publish: it doesn't reveal the inbox and can only send to it.

Each signup arrives as an email titled "New Decurix waitlist signup", with the subscriber's address in the body.
Web3Forms rejects requests from curl, scripts and headless browsers, so test from a normal browser.
If the request fails, the form shows an error and never pretends to have saved the email.

## Deploy

Any static host works: drag the `website/` folder into Netlify Drop or Cloudflare Pages, or point Vercel / GitHub Pages at it.
After it has a domain, change `og:image` and `twitter:image` in `index.html` to the absolute URL (`https://your-domain/og.png`), since link previews on LinkedIn and X need absolute URLs.
