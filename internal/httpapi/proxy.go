package httpapi

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/flowera/internal/desktop"
)

// desktopProxyHandler reverse-proxies HTTP requests to the desktop container's internal address.
func (s *Server) desktopProxyHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(
		strings.TrimPrefix(r.URL.Path, "/desktop/"),
		"/",
		2,
	)

	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	proxyURL, desktopID, err := s.Desktops.ProxyTarget(r.Context(), parts[0])

	switch {
	case errors.Is(err, desktop.ErrNotFound):
		http.NotFound(w, r)
		return

	case errors.Is(err, desktop.ErrNotRunning):
		http.Error(
			w,
			"desktop is not running",
			http.StatusServiceUnavailable,
		)
		return

	case errors.Is(err, desktop.ErrNoNetwork):
		http.Error(
			w,
			"desktop is not connected to Flowera network",
			http.StatusServiceUnavailable,
		)
		return

	case errors.Is(err, desktop.ErrNoAddress):
		http.Error(
			w,
			"desktop has no network IP",
			http.StatusServiceUnavailable,
		)
		return

	case err != nil:
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(proxyURL)

	// Remove /desktop/<id> from the URL.
	prefix := "/desktop/" + desktopID

	r.URL.Path = strings.TrimPrefix(
		r.URL.Path,
		prefix,
	)

	if r.URL.Path == "" || r.URL.Path == "/" {
		r.URL.Path = "/vnc.html"
	}

	// Query parameters (e.g. index.html's open-in-a-new-tab resize=remote&ui=touch)
	// are preserved automatically since only r.URL.Path was rewritten above.
	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		if err := rewriteFloweraBranding(resp); err != nil {
			return err
		}

		return injectMobileEnhancements(resp)
	}
	reverseProxy.ErrorHandler = func(
		rw http.ResponseWriter,
		req *http.Request,
		err error,
	) {
		log.Printf(
			"desktop proxy error for %s: %v",
			desktopID,
			err,
		)

		http.Error(
			rw,
			"desktop unavailable",
			http.StatusBadGateway,
		)
	}

	reverseProxy.ServeHTTP(w, r)
}

// floweraBrandingRewrites are the literal noVNC markup fragments swapped for
// Flowera's own branding by rewriteFloweraBranding, in order: the page
// title, the browser tab favicon, the small logo noVNC shows in its
// control-bar sidebar once connected, and the large logo on its pre-connect
// screen (the first thing a user actually sees when opening a desktop). Each
// replacement keeps the original noVNC_logo class (other noVNC CSS, e.g.
// `.noVNC_logo + hr`, selects on it) and overrides noVNC's own styling
// inline so it doesn't depend on cascade order against noVNC's stylesheets.
// The icon is /favicon.svg - the control plane's own root-relative brand
// mark, resolved against the control plane's origin (not the desktop
// container's) since this page is only ever reached through the
// /desktop/<id>/ proxy.
var floweraBrandingRewrites = [][2][]byte{
	{
		[]byte(`<title>noVNC</title>`),
		[]byte(`<title>Flowera</title>`),
	},
	{
		[]byte(`<link rel="icon" type="image/x-icon" href="app/images/icons/novnc.ico">`),
		[]byte(`<link rel="icon" type="image/svg+xml" href="/favicon.svg">`),
	},
	{
		[]byte(`<h1 class="noVNC_logo" translate="no"><span>no</span><br>VNC</h1>`),
		[]byte(`<h1 class="noVNC_logo flowera-logo" translate="no" style="font-size:13px;line-height:1.2;display:flex;align-items:center;gap:8px;text-shadow:none;color:#f2f0ea;font-family:ui-sans-serif,system-ui,sans-serif"><img src="/favicon.svg" alt="" width="20" height="20" style="display:block;border-radius:4px"><span style="color:inherit;font-weight:700">Flowera</span></h1>`),
	},
	{
		[]byte(`<p class="noVNC_logo" translate="no"><span>no</span>VNC</p>`),
		[]byte(`<p class="noVNC_logo flowera-logo" translate="no" style="font-size:20px;line-height:1.2;display:flex;flex-direction:column;align-items:center;gap:10px;text-shadow:none;color:#f2f0ea;font-family:ui-sans-serif,system-ui,sans-serif"><img src="/favicon.svg" alt="" width="56" height="56" style="display:block;border-radius:12px"><span style="color:inherit;font-weight:700;letter-spacing:.02em">Flowera</span></p>`),
	},
}

// rewriteFloweraBranding replaces noVNC's own "noVNC" branding in a desktop
// screen's HTML with Flowera's, per floweraBrandingRewrites. Applied to every
// load (unlike injectMobileEnhancements, it isn't gated on ?ui=touch) so a
// user is never shown that a desktop's screen is noVNC/VNC under the hood -
// the control plane presents it purely as "your desktop".
func rewriteFloweraBranding(resp *http.Response) error {
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err != nil {
		return err
	}

	for _, rewrite := range floweraBrandingRewrites {
		body = bytes.Replace(body, rewrite[0], rewrite[1], 1)
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))

	return nil
}

// injectMobileEnhancements rewrites the noVNC vnc.html response to splice
// mobileViewerEnhancements in before </body>, but only for requests carrying
// ?ui=touch - the marker index.html's "open the screen in a new tab" button
// puts on the full-size, interactive screen URL, so this never lands in the
// same page's small, non-interactive dcard-preview thumbnail (same vnc.html,
// same query-free URL otherwise). The injected script itself further gates on
// the browser actually being touch-capable, so a mouse-and-keyboard visitor
// opening the same screen sees nothing extra either.
//
// That button is now the only thing emitting ?ui=touch: the per-desktop page
// at /view/<id> is the chat alone and embeds no screen. Anything that opens a
// desktop screen for real use needs to carry the marker, or these affordances
// silently stop being injected.
func injectMobileEnhancements(resp *http.Response) error {
	if resp.Request.URL.Query().Get("ui") != "touch" {
		return nil
	}

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err != nil {
		return err
	}

	marker := []byte("</body>")

	if !bytes.Contains(body, marker) {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}

	rewritten := bytes.Replace(
		body,
		marker,
		append([]byte(mobileViewerEnhancements), marker...),
		1,
	)

	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))

	return nil
}

// mobileViewerEnhancements adds two touch-only affordances on top of stock
// noVNC, which otherwise leaves mobile visitors clicking a tiny keyboard
// icon buried in a sidebar that auto-collapses after 2s (app/ui.js's
// idleControlbarTimeout), with no way to zoom in on small UI targets short
// of the browser's own (disabled - noVNC sets touch-action:none on body)
// pinch-zoom:
//
//  1. A large, persistent "show keyboard" button that never auto-hides,
//     wired to noVNC's own #noVNC_keyboard_button so it reuses noVNC's
//     existing focus/blur and one-keyboard-at-a-time state instead of
//     duplicating it.
//  2. A two-finger pinch-to-zoom/pan layer on #noVNC_container, left
//     entirely off the single-finger path so ordinary taps and drags still
//     reach noVNC's own mouse emulation unchanged - only a second
//     simultaneous touch is intercepted (capture phase, so it never also
//     reaches noVNC's gesture handler on the canvas).
//
// Paired with `resize=remote` on the same URL, which asks the desktop's Xvnc
// (TigerVNC, RandR-capable per `xrandr` inside the container) to resize the X
// screen to fill the viewport, so the common case needs no zooming or panning
// at all - this is purely for zooming in on fine detail afterward.
const mobileViewerEnhancements = `
<style>
#noVNC_container{transform-origin:0 0}
#noVNC_container.flowera-panning{transition:none}
#noVNC_container:not(.flowera-panning){transition:transform .12s ease-out}

.flowera-touch-btn{
  position:fixed;
  z-index:60;
  bottom:calc(14px + env(safe-area-inset-bottom, 0px));
  width:52px;height:52px;
  border-radius:999px;
  display:flex;align-items:center;justify-content:center;
  background:rgba(30,30,32,.78);
  box-shadow:0 2px 10px rgba(0,0,0,.35);
  -webkit-tap-highlight-color:transparent;
  touch-action:manipulation;
}
.flowera-touch-btn img{width:24px;height:24px;filter:invert(1)}
#flowera-kb-btn{left:calc(14px + env(safe-area-inset-left, 0px))}
#flowera-kb-btn.flowera-active{background:rgba(74,144,255,.92)}

#flowera-reset-btn{
  right:calc(14px + env(safe-area-inset-right, 0px));
  width:auto;
  padding:0 16px;
  font:600 13px/1 sans-serif;
  color:#fff;
  opacity:0;
  pointer-events:none;
  transition:opacity .15s ease-out;
}
#flowera-reset-btn.flowera-visible{opacity:1;pointer-events:auto}
</style>
<script>
(function(){
"use strict";
if (!("ontouchstart" in window || navigator.maxTouchPoints > 0)) return;

var container = document.getElementById("noVNC_container");
var nativeKbBtn = document.getElementById("noVNC_keyboard_button");
if (!container || !nativeKbBtn) return;

// --- persistent keyboard button -------------------------------------
var kbBtn = document.createElement("button");
kbBtn.id = "flowera-kb-btn";
kbBtn.type = "button";
kbBtn.className = "flowera-touch-btn";
kbBtn.setAttribute("aria-label", "Show keyboard");
kbBtn.innerHTML = '<img src="app/images/keyboard.svg" alt="">';
kbBtn.addEventListener("click", function(e){
  e.preventDefault();
  nativeKbBtn.click();
});
document.body.appendChild(kbBtn);

new MutationObserver(function(){
  kbBtn.classList.toggle("flowera-active", nativeKbBtn.classList.contains("noVNC_selected"));
}).observe(nativeKbBtn, {attributes: true, attributeFilter: ["class"]});

// --- two-finger pinch-to-zoom + pan ----------------------------------
var resetBtn = document.createElement("button");
resetBtn.id = "flowera-reset-btn";
resetBtn.type = "button";
resetBtn.className = "flowera-touch-btn";
resetBtn.textContent = "Reset zoom";
resetBtn.addEventListener("click", function(e){
  e.preventDefault();
  resetTransform();
});
document.body.appendChild(resetBtn);

var scale = 1, tx = 0, ty = 0, pinch = null;

function apply(){
  container.style.transform = "translate(" + tx + "px," + ty + "px) scale(" + scale + ")";
  resetBtn.classList.toggle("flowera-visible", scale > 1.01);
}

function resetTransform(){
  scale = 1; tx = 0; ty = 0;
  container.classList.remove("flowera-panning");
  apply();
}

function dist(a, b){
  return Math.hypot(b.clientX - a.clientX, b.clientY - a.clientY);
}
function mid(a, b){
  return {x: (a.clientX + b.clientX) / 2, y: (a.clientY + b.clientY) / 2};
}

function clamp(){
  var w = container.clientWidth, h = container.clientHeight;
  var vw = window.innerWidth, vh = window.innerHeight;
  var minTx = Math.min(0, vw - w * scale);
  var minTy = Math.min(0, vh - h * scale);
  tx = Math.min(0, Math.max(minTx, tx));
  ty = Math.min(0, Math.max(minTy, ty));
}

document.addEventListener("touchstart", function(e){
  if (e.touches.length !== 2) return;
  var t0 = e.touches[0], t1 = e.touches[1];
  pinch = {
    dist: dist(t0, t1),
    scale: scale,
    mid: mid(t0, t1),
    tx: tx,
    ty: ty
  };
  container.classList.add("flowera-panning");
  e.preventDefault();
  e.stopPropagation();
}, {capture: true, passive: false});

document.addEventListener("touchmove", function(e){
  if (!pinch || e.touches.length < 2) return;
  var t0 = e.touches[0], t1 = e.touches[1];
  var m = mid(t0, t1);

  scale = Math.min(4, Math.max(1, pinch.scale * (dist(t0, t1) / pinch.dist)));
  tx = pinch.tx + (m.x - pinch.mid.x);
  ty = pinch.ty + (m.y - pinch.mid.y);
  clamp();
  apply();

  e.preventDefault();
  e.stopPropagation();
}, {capture: true, passive: false});

function endPinch(e){
  if (!pinch) return;
  if (e.touches.length >= 2) return;
  pinch = null;
  container.classList.remove("flowera-panning");
  if (scale <= 1.02) {
    resetTransform();
  } else {
    clamp();
    apply();
  }
  e.preventDefault();
  e.stopPropagation();
}
document.addEventListener("touchend", endPinch, {capture: true, passive: false});
document.addEventListener("touchcancel", endPinch, {capture: true, passive: false});

window.addEventListener("resize", function(){
  if (scale === 1) return;
  clamp();
  apply();
});
})();
</script>
`

// agentProxyHandler reverse-proxies HTTP requests to a desktop's TheBot soul
// HTTP port (4900), the way desktopProxyHandler proxies to its VNC port
// (6901). Unlike VNC's noVNC assets, TheBot's web/index.html is a single
// self-contained page whose inline JS calls its own API with root-relative
// paths ('/api/...', both fetch() and EventSource) rather than paths
// relative to the page - those would otherwise resolve against the control
// plane's own origin instead of this desktop's prefix once loaded inside
// /agent/<id>/. ModifyResponse rewrites those literal '/api/ references in
// the HTML response to carry the "/agent/<id>" prefix along, so the exact
// same soul binary needs no changes to be proxied per-desktop.
func (s *Server) agentProxyHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(
		strings.TrimPrefix(r.URL.Path, "/agent/"),
		"/",
		2,
	)

	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	proxyURL, desktopID, err := s.Desktops.AgentProxyTarget(r.Context(), parts[0])

	switch {
	case errors.Is(err, desktop.ErrNotFound):
		http.NotFound(w, r)
		return

	case errors.Is(err, desktop.ErrNotRunning):
		http.Error(
			w,
			"desktop is not running",
			http.StatusServiceUnavailable,
		)
		return

	case errors.Is(err, desktop.ErrNoNetwork):
		http.Error(
			w,
			"desktop is not connected to Flowera network",
			http.StatusServiceUnavailable,
		)
		return

	case errors.Is(err, desktop.ErrNoAddress):
		http.Error(
			w,
			"desktop has no network IP",
			http.StatusServiceUnavailable,
		)
		return

	case err != nil:
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(proxyURL)

	prefix := "/agent/" + desktopID

	r.URL.Path = strings.TrimPrefix(
		r.URL.Path,
		prefix,
	)

	if r.URL.Path == "" {
		r.URL.Path = "/"
	}

	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			return nil
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if readErr != nil {
			return readErr
		}

		rewritten := bytes.ReplaceAll(body, []byte("'/api/"), []byte("'"+prefix+"/api/"))
		rewritten = bytes.ReplaceAll(rewritten, []byte("`/api/"), []byte("`"+prefix+"/api/"))

		resp.Body = io.NopCloser(bytes.NewReader(rewritten))
		resp.ContentLength = int64(len(rewritten))
		resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))

		return nil
	}

	reverseProxy.ErrorHandler = func(
		rw http.ResponseWriter,
		req *http.Request,
		err error,
	) {
		log.Printf(
			"agent proxy error for %s: %v",
			desktopID,
			err,
		)

		http.Error(
			rw,
			"agent unavailable",
			http.StatusBadGateway,
		)
	}

	reverseProxy.ServeHTTP(w, r)
}
