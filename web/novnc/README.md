# web/novnc

`rfb.js` is the RFB client the desktop screen viewer (`web/templates/screen.html`)
runs: noVNC 1.6.0's `core/` and its vendored pako, bundled into one ES module.
It is embedded in the binary (`web/embed.go`) and served once, cached for good,
at `/static/novnc/rfb-1.6.0.js` for every desktop. Loading noVNC's own page from
each desktop took 81 requests and 763 KB per desktop, from a Python static
server on a 1-vCPU container; this is one request of 185 KB (56 KB gzipped).

MPL 2.0 (noVNC core) and MIT (pako): `LICENSE.txt` is noVNC's own. The bundle
is unmodified noVNC source; only the entry point below is ours.

## Rebuilding

The source is the noVNC every desktop image already ships (the base image's
`/usr/libexec/noVNCdim`). From any running desktop:

```bash
mkdir -p /tmp/nv && cd /tmp/nv
docker cp desktop_<id>:/usr/libexec/noVNCdim/core .
docker cp desktop_<id>:/usr/libexec/noVNCdim/vendor .
cat > entry.js <<'EOF'
export { default as RFB } from "./core/rfb.js";
export { default as KeyTable } from "./core/input/keysym.js";
export { default as keysyms } from "./core/input/keysymdef.js";
EOF
npx -y esbuild@0.24.0 entry.js --bundle --format=esm --minify --target=es2022 --outfile=rfb.min.js
```

Then put the three-line licence header back on top (see the current file) and
bump the version in the served path (`httpapi.noVNCPath`) if the noVNC version
changed, so browsers holding the old one never mix the two. `es2022` is needed:
`core/util/browser.js` uses a top-level `await`.
