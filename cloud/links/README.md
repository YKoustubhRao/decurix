# Link service

A Cloudflare Worker that gives every Decurix install a permanent public link, like `calm-river-4821.decurix.computer`, in place of a `trycloudflare.com` address that changes on every restart.

## How it works

1. The person turns on **Public link** in the app's Mobile menu.
2. The app (`desktop/electron/public-name.js`) asks the service for a name: `POST /v1/claim {port}`.
3. The Worker picks a free random name, creates a remotely managed Cloudflare Tunnel routed **only** to `http://127.0.0.1:<port>` on that machine (the gated access-code listener; the app checks the port is that one before it asks), adds a proxied CNAME, and returns `{hostname, key, token}`.
4. The app keeps `{hostname, key}` in `public-name.json` in its data folder and runs the cloudflared it ships with `TUNNEL_TOKEN` in the environment.
5. On every later start the app trades its key for the token again (`POST /v1/token`). The name never changes.

If the service is unreachable or refuses, that start falls back to a quick tunnel, as before. `DECURIX_LINK_SERVICE=off` (env or the data folder's `.env`) turns the service off; `DECURIX_LINK_SERVICE=https://...` points the app at another deployment.

## Limits and safety

- **Anyone who installs may claim a name** (owner decision, 2026-11-11). Names are random, not chosen, so they are poor material for phishing.
- At most `CLAIMS_PER_IP_PER_DAY` new names per network per day, and `MAX_NAMES` in total, which stays under the free plan's DNS record limit for the zone.
- Names unused for `STALE_DAYS` are removed by a daily cron, with their tunnel and DNS record.
- The key is shown once and only its SHA-256 is stored. Client IPs are stored only as hashes, for the rate limit.
- The Cloudflare API token is a Worker secret, scoped to Cloudflare Tunnel Edit (account) and DNS Edit (this zone), and never leaves the Worker.
- **To take an abused name down:**
  ```
  curl -X POST https://links.decurix.computer/v1/admin/revoke \
    -H "Authorization: Bearer $ADMIN_KEY" -d '{"hostname":"calm-river-4821.decurix.computer"}'
  ```

## Deploy

One time:

```
npx wrangler d1 create decurix-links          # put the printed database_id into wrangler.toml
npx wrangler secret put CF_API_TOKEN          # Cloudflare Tunnel Edit + DNS Edit on the zone
npx wrangler secret put ADMIN_KEY             # any long random string; keep it for revokes
```

Each change: `npx wrangler deploy` from this folder. The tables are created on first use.

Tests: `node --test worker.test.js` (Node 22+, uses `node:sqlite` in place of D1).
