# Decurix website

A single static page (no build step), live at https://decurix.computer: the vision, how it works, downloads, and an early-access list.

| File | What it is |
|---|---|
| `index.html` | The page |
| `styles.css` | All styling, on the Decurix app's own design tokens (`web/templates/theme.html`), light and dark |
| `main.js` | Scroll reveals, the animated demo and learning loop, the download buttons (`DOWNLOADS`), early-access submission |
| `favicon.svg`, `og.png` | Tab icon and the 1200×630 link-preview image |

Preview locally by opening `index.html` in a browser, or `python -m http.server -d website 8000`.

## Waitlist

Signups are sent by [Web3Forms](https://web3forms.com) (free, 250 submissions/month) to the inbox the access key was created for. The key is set in `main.js` (`WAITLIST.accessKey`). It is safe to publish: it doesn't reveal the inbox and can only send to it.

Each signup arrives as an email titled "New Decurix waitlist signup", with the subscriber's address in the body.
Web3Forms rejects requests from curl, scripts and headless browsers, so test from a normal browser.
If the request fails, the form shows an error and never pretends to have saved the email.

## Downloads

The Download buttons read `DOWNLOADS` at the top of `main.js`. A platform with an empty `url` shows "Coming soon" and points at the early-access form. To publish a build, upload it as a release asset on the public `YKoustubhRao/decurix-site` repo under a fixed name, then fill in `url`, `file`, `size` and `sha256`. A `releases/latest/download/<file>` url keeps working for every later release.

## Deploy

GitHub Pages serves the public repo `YKoustubhRao/decurix-site` (branch `main`, root) at decurix.computer. That repo holds only these files plus a `CNAME` file containing `decurix.computer`, never Decurix's source. To update the site, copy this folder's files (not this README) into a clone of that repo, commit and push.

DNS at GoDaddy: four A records on `@` (185.199.108.153, .109, .110, .111) and a CNAME `www` to `ykoustubhrao.github.io`.
