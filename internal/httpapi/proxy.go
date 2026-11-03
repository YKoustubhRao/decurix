package httpapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"path"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/webui"
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

	isWebSocket := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")

	// If the desktop is actively provisioning and not yet ready for GUI connections,
	// display a sleek launch progress screen rather than connection failures or 404s.
	provisioning := func() bool {
		prov, ok := s.Desktops.GetProvisioning(parts[0])
		if !ok || prov.Ready {
			return false
		}
		if isWebSocket {
			http.Error(w, "desktop is still provisioning", http.StatusServiceUnavailable)
		} else {
			s.renderProvisioningLaunchScreen(w, r, prov)
		}
		return true
	}
	if provisioning() {
		return
	}

	// The screen itself is the control plane's page (screen.go); everything
	// under it - the WebSocket, and noVNC's own vnc.html for anyone who wants
	// it - is still the desktop's, proxied below.
	if (len(parts) == 1 || parts[1] == "") && !isWebSocket && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		s.screenViewerHandler(w, r, parts[0])
		return
	}

	proxyURL, desktopID, err := s.Desktops.ProxyTarget(r.Context(), parts[0])

	if err != nil && provisioning() {
		return
	}

	if proxyTargetFailed(w, r, err) {
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

	if r.URL.Path == "/vnc.html" {
		query := r.URL.Query()
		if query.Get("autoconnect") == "" {
			query.Set("autoconnect", "true")
			r.URL.RawQuery = query.Encode()
		}
	}

	// Query parameters (e.g. index.html's open-in-a-new-tab resize=remote&ui=touch)
	// are preserved automatically since only r.URL.Path was rewritten above.
	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") && resp.StatusCode == http.StatusOK {
			resp.Header.Set("Cache-Control", "public, max-age=86400, stale-while-revalidate=3600")
		}
		sandboxContainerContent(resp)
		return rewriteDesktopHTML(resp, desktopID)
	}
	reverseProxy.ErrorHandler = func(
		rw http.ResponseWriter,
		req *http.Request,
		err error,
	) {
		s.Desktops.InvalidateProxyTarget(desktopID)
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

	if isWebSocket {
		done := make(chan struct{})
		defer close(done)
		go s.keepScreenViewed(desktopID, done)
	}

	reverseProxy.ServeHTTP(w, r)
}

// containerContentPolicy is the policy on anything a desktop serves through the control plane's address: a sandbox
// (an opaque origin, which the guard refuses and the browser keeps from reading the control plane), framed by nothing
// but the control plane's own pages.
const containerContentPolicy = "sandbox allow-scripts allow-forms allow-popups; frame-ancestors 'self'"

// sandboxContainerContent marks what a desktop serves through the control plane's address as a sandbox. A page or
// script from a desktop shown at /desktop/<id>/ or /agent/<id>/ would otherwise run as the control plane's own origin
// and could call every route a person can (review 2026-11-02). Sandboxed, it has an opaque origin: GuardPrimary
// refuses its writes and WebSockets, and the browser keeps it from reading the control plane's answers. The screen a
// person uses is the control plane's own page (screen.go) with its own noVNC, so it is not affected.
func sandboxContainerContent(resp *http.Response) {
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return
	}
	resp.Header.Set("Content-Security-Policy", containerContentPolicy)
	resp.Header.Set("X-Content-Type-Options", "nosniff")
}

// decurixBrandingRewrites are the literal noVNC markup fragments swapped for
// Decurix's own branding by rewriteDecurixBranding, in order: the page
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
var decurixBrandingRewrites = brandingRewrites("/favicon.svg")

// init puts the embedded favicon itself, as a data: URI, in place of
// /favicon.svg when the build has one.
func init() {
	if len(webui.FaviconSVG) > 0 {
		decurixBrandingRewrites = brandingRewrites("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(webui.FaviconSVG))
	}
}

// brandingRewrites is decurixBrandingRewrites with icon as the brand mark's address.
func brandingRewrites(icon string) [][2][]byte {
	return [][2][]byte{
		{
			[]byte(`<title>noVNC</title>`),
			[]byte(`<title>Decurix</title>`),
		},
		{
			[]byte(`<link rel="icon" type="image/x-icon" href="app/images/icons/novnc.ico">`),
			[]byte(`<link rel="icon" type="image/svg+xml" href="` + icon + `">`),
		},
		{
			[]byte(`<h1 class="noVNC_logo" translate="no"><span>no</span><br>VNC</h1>`),
			[]byte(`<h1 class="noVNC_logo decurix-logo" translate="no" style="font-size:13px;line-height:1.2;display:flex;align-items:center;gap:8px;text-shadow:none;color:#f2f0ea;font-family:ui-sans-serif,system-ui,sans-serif"><img src="` + icon + `" alt="" width="20" height="20" style="display:block;border-radius:4px"><span style="color:inherit;font-weight:700">Decurix</span></h1>`),
		},
		{
			[]byte(`<p class="noVNC_logo" translate="no"><span>no</span>VNC</p>`),
			[]byte(`<p class="noVNC_logo decurix-logo" translate="no" style="font-size:20px;line-height:1.2;display:flex;flex-direction:column;align-items:center;gap:10px;text-shadow:none;color:#f2f0ea;font-family:ui-sans-serif,system-ui,sans-serif"><img src="` + icon + `" alt="" width="56" height="56" style="display:block;border-radius:12px"><span style="color:inherit;font-weight:700;letter-spacing:.02em">Decurix</span></p>`),
		},
	}
}

// rewriteDesktopHTML executes all desktop HTML modifications in a single pass
// over the response body, avoiding multiple read/re-wrap cycles: replacing
// noVNC's own "noVNC" branding with Decurix's (per decurixBrandingRewrites,
// applied to every load so a user is never shown that a desktop's screen is
// noVNC/VNC under the hood - the control plane presents it purely as "your
// desktop"), splicing in the ticket toolbar, and - only for requests carrying
// ?ui=touch - the mobile touch enhancements.
func rewriteDesktopHTML(resp *http.Response, desktopID string) error {
	return rewriteHTML(resp, func(body []byte) []byte {
		for _, rewrite := range decurixBrandingRewrites {
			body = bytes.Replace(body, rewrite[0], rewrite[1], 1)
		}

		marker := []byte("</body>")
		if bytes.Contains(body, marker) {
			var injections strings.Builder
			injections.WriteString(strings.ReplaceAll(titleOverrideScript, "__DESKTOP_ID__", desktopID))
			injections.WriteString(strings.ReplaceAll(ticketToolbar, "__DESKTOP_ID__", desktopID))
			injections.WriteString(strings.ReplaceAll(resourceAlertPopup, "__DESKTOP_ID__", desktopID))
			injections.WriteString(autoConnectHelper)
			if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.Query().Get("ui") == "touch" {
				injections.WriteString(mobileViewerEnhancements)
			}
			body = bytes.Replace(body, marker, append([]byte(injections.String()), marker...), 1)
		}

		// The proxied container's own Content-Type carries no charset, and this
		// is where every rewrite above appends UTF-8 text (the ticket toolbar's
		// own copy, e.g. its em dashes) well past a browser's ~1024-byte <meta
		// charset> sniffing window. Without an explicit charset here a browser
		// guesses one for the whole document before any of that text is even in
		// it, and every non-ASCII byte added above renders as mojibake.
		resp.Header.Set("Content-Type", "text/html; charset=utf-8")
		return body
	})
}

// rewriteHTML replaces an HTML response's body with edit's version of it,
// keeping its length right; any other response passes untouched.
func rewriteHTML(resp *http.Response, edit func([]byte) []byte) error {
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	body = edit(body)

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

// ticketToolbar is the GUI screen's own recording controls - the only place
// they live now; a dashboard card no longer has a Record button of its own
// (index.html), since every desktop screen already carries this - a small
// tab at the top of the page that drops down into a ticket's lifecycle - +
// opens a ticket (POST /api/desktops/record, the same call the chat's
// "/record" request goes through), Record starts capturing for it, Stop
// pauses (for a private step such as signing in), Continue resumes, and End
// finishes it - which is what makes its recording's export downloadable,
// offered right there once it does. A ticket can be given files from the
// person's own computer - the documents they will work from - when it is
// opened (the paperclip beside +) and while it is open (Attach): they are
// saved into the desktop and kept with the ticket's recording, so a lesson
// taught from it reads them (attachments.go). The tab is also a drag handle: pressing
// and moving it repositions the whole toolbar anywhere on the screen (see
// the pointer handlers below), since docked at top-center it can sit over
// whatever the person actually needs to click on the desktop underneath.
//
// It talks to /api/desktops/ticket[...] - record_task.py's state machine,
// proxied by ticketStatusHandler/ticketActionHandler - polling GET
// /api/desktops/ticket every ticketPollMs so every open tab (and the
// recordings page, which shows the same ticket through its own "demonstration
// being recorded" panel) agrees on whether it is armed, recording, or
// paused. Deliberately separate from the recordings page's own ad-hoc
// "Record a demonstration" (POST /api/desktops/demonstration, no ticket,
// no task) - this toolbar only ever drives a ticket, never a bare
// demonstration, so the two controls can never fight over the same session.
const ticketToolbar = `
<style>
#decurix-ticket{
  position:fixed;
  top:0;left:50%;
  transform:translateX(-50%);
  z-index:70;
  display:flex;
  flex-direction:column;
  align-items:center;
  font:13px/1.4 ui-sans-serif,system-ui,sans-serif;
  color:#f2f0ea;
  -webkit-tap-highlight-color:transparent;
}
/* Once the tab has actually been dragged, position switches from the
   centered/top-docked default (above) to an explicit pixel left/top set by
   the drag handler - this just has to stop fighting that with its own
   centering transform. */
#decurix-ticket.decurix-pinned{
  transform:none;
}
#decurix-ticket-tab{
  display:flex;
  align-items:center;
  gap:5px;
  padding:5px 14px 6px;
  border:0;
  border-radius:0 0 10px 10px;
  background:rgba(24,22,20,.86);
  color:inherit;
  cursor:grab;
  touch-action:none;
  box-shadow:0 2px 8px rgba(0,0,0,.3);
}
#decurix-ticket-tab:active{cursor:grabbing}
#decurix-ticket-tab svg{transition:transform .15s ease;flex:none}
#decurix-ticket.decurix-open #decurix-ticket-tab svg{transform:rotate(180deg)}
#decurix-ticket-dot{
  width:7px;height:7px;border-radius:50%;
  background:#8a8578;flex:none;
}
#decurix-ticket-dot.decurix-armed{background:#e8b13a}
#decurix-ticket-dot.decurix-rec{background:#e8503a;animation:decurix-pulse 1.4s ease-in-out infinite}
#decurix-ticket-dot.decurix-paused{background:#8a8578}
@keyframes decurix-pulse{50%{opacity:.35}}

#decurix-ticket-panel{
  display:none;
  width:min(320px,92vw);
  margin-top:2px;
  padding:12px 14px 14px;
  border-radius:10px;
  background:rgba(24,22,20,.94);
  box-shadow:0 6px 20px rgba(0,0,0,.35);
}
#decurix-ticket.decurix-open #decurix-ticket-panel{display:block}

#decurix-ticket-panel input[type=text],
#decurix-ticket-panel textarea{
  width:100%;
  box-sizing:border-box;
  padding:8px 10px;
  border:1px solid rgba(255,255,255,.22);
  border-radius:7px;
  background:rgba(255,255,255,.07);
  color:inherit;
  font:inherit;
  outline:none;
}
#decurix-ticket-panel textarea{resize:vertical;min-height:44px}
#decurix-ticket-panel input[type=text]:focus,
#decurix-ticket-panel textarea:focus{border-color:rgba(255,255,255,.5)}
#decurix-ticket-panel input[type=text]::placeholder,
#decurix-ticket-panel textarea::placeholder{color:rgba(242,240,234,.5)}

.decurix-reason{margin-top:10px}
.decurix-reason-hint{font-size:11px;color:rgba(242,240,234,.6);margin-bottom:6px}

#decurix-ticket-title{
  font-weight:650;
  margin-bottom:2px;
  overflow-wrap:anywhere;
}
#decurix-ticket-state{color:rgba(242,240,234,.65);font-size:11.5px;margin-bottom:10px}

.decurix-ticket-row{display:flex;gap:7px;margin-top:10px}
.decurix-ticket-row.decurix-first{margin-top:0}
#decurix-ticket-panel button{
  flex:1;
  display:flex;align-items:center;justify-content:center;gap:6px;
  padding:8px 10px;
  border:1px solid rgba(255,255,255,.22);
  border-radius:7px;
  background:rgba(255,255,255,.09);
  color:inherit;
  font:inherit;font-weight:600;
  cursor:pointer;
}
#decurix-ticket-panel button:hover:not(:disabled){background:rgba(255,255,255,.16)}
#decurix-ticket-panel button:disabled{opacity:.5;cursor:default}
#decurix-ticket-panel button.decurix-primary{background:#c2410c;border-color:#c2410c}
#decurix-ticket-panel button.decurix-primary:hover:not(:disabled){background:#dd4f14}
#decurix-ticket-panel button.decurix-danger{color:#f5b3a3}
#decurix-ticket-panel button.decurix-active{background:rgba(255,255,255,.22);border-color:rgba(255,255,255,.42)}
#decurix-ticket-panel a.decurix-download{
  display:flex;align-items:center;justify-content:center;gap:6px;
  padding:9px 10px;margin-top:10px;
  border-radius:7px;
  background:#2f7d55;
  color:#fff;text-decoration:none;font-weight:650;
}
#decurix-ticket-error{color:#f5a793;font-size:11.5px;margin-top:8px;display:none}
#decurix-ticket-error.decurix-visible{display:block}
.decurix-ticket-loading{
  display:flex;align-items:center;gap:9px;
  padding:4px 0 2px;
  color:rgba(242,240,234,.82);font-size:12px;
}
.decurix-ticket-spinner{
  width:14px;height:14px;box-sizing:border-box;flex:none;
  border:2px solid rgba(255,255,255,.24);border-top-color:#fff;border-radius:50%;
  animation:decurix-spin .75s linear infinite;
}
@keyframes decurix-spin{to{transform:rotate(360deg)}}

.decurix-files{display:flex;flex-wrap:wrap;gap:5px;margin-top:8px}
.decurix-file{
  display:inline-flex;align-items:center;gap:4px;
  max-width:100%;
  padding:2px 7px;
  border:1px solid rgba(255,255,255,.2);
  border-radius:6px;
  background:rgba(255,255,255,.06);
  font-size:11.5px;
}
.decurix-file span{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:190px}
#decurix-ticket-panel .decurix-file button{
  flex:none;
  padding:0 2px;border:0;background:none;
  color:rgba(242,240,234,.6);font-size:14px;line-height:1;font-weight:400;
}
#decurix-ticket-panel .decurix-file button:hover:not(:disabled){background:none;color:#f5a793}
#decurix-ticket-panel button.decurix-clip{flex:none;padding:8px 9px}
.decurix-files-hint{font-size:11px;color:rgba(242,240,234,.6);margin-top:8px}

#decurix-ticket-browse{
  display:block;
  margin-top:10px;
  font-size:11.5px;
  color:rgba(242,240,234,.6);
  text-decoration:none;
}
#decurix-ticket-browse:hover{color:rgba(242,240,234,.9)}
</style>
<div id="decurix-ticket">
  <button type="button" id="decurix-ticket-tab" aria-label="Recording ticket">
    <span id="decurix-ticket-dot"></span>
    <span id="decurix-ticket-label">Record</span>
    <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>
  </button>
  <div id="decurix-ticket-panel"></div>
</div>
<script>
(function(){
"use strict";

var DESKTOP_ID = "__DESKTOP_ID__";
var POLL_MS = 2500;

var root = document.getElementById("decurix-ticket");
var tab = document.getElementById("decurix-ticket-tab");
var dot = document.getElementById("decurix-ticket-dot");
var label = document.getElementById("decurix-ticket-label");
var panel = document.getElementById("decurix-ticket-panel");

var ticket = null;        // last known {task_id, title, session_id, recording, paused, events}, or null
var lastSession = null;   // the most recent session_id seen, kept after End clears the ticket - for the download link
var lastTitle = "";       // that ticket's title - what the saved recording is called until someone renames it
var draftName = null;     // the "Recording saved" view's name box, while it differs from lastTitle
var busy = false;         // an action is in flight - buttons disabled meanwhile
var ticketPending = false; // create was accepted/in flight, but the agent has not exposed the ticket yet
var pendingTimer = null;
var pendingTimeout = null;
var error = "";

function clearPendingState(){
  if(pendingTimer){ clearInterval(pendingTimer); pendingTimer = null; }
  if(pendingTimeout){ clearTimeout(pendingTimeout); pendingTimeout = null; }
  ticketPending = false;
}
var userToggled = false;  // once a person opens/closes the panel by hand, stop auto-opening it for them
var draftTask = "";       // the create-ticket input's text, kept across re-renders (see renderCreate)
var draftFiles = [];      // the files chosen for the ticket about to be opened, kept the same way
var reasonOpen = false;   // the Reason box is expanded
var draftReason = "";     // its text, kept across re-renders the same way draftTask is
var endedStarted = "";    // "replay" or "learn" once one was started from the saved-recording view
var endedStarting = "";   // which of them is being started right now
var reasonAutoPaused = false; // opening the Reason box is what paused recording, so closing
                               // it without submitting should undo that pause (see startReason)

function api(path, fields){
  var body = new URLSearchParams({id: DESKTOP_ID});
  Object.keys(fields || {}).forEach(function(k){ body.append(k, fields[k]); });

  return fetch(path, {method: "POST", headers: {"Content-Type": "application/x-www-form-urlencoded"}, body: body})
    .then(function(r){
      return r.json().catch(function(){ return {}; }).then(function(data){
        if(!r.ok){
          var failure = new Error(data.error || "that didn't work");
          failure.status = r.status;
          throw failure;
        }
        return data;
      });
    });
}

// api() with files: multipart, each file in the "files" field (attachments.go).
function apiFiles(path, fields, files){
  if(!files || !files.length){ return api(path, fields); }

  var body = new FormData();
  body.append("id", DESKTOP_ID);
  Object.keys(fields || {}).forEach(function(k){ body.append(k, fields[k]); });
  files.forEach(function(f){ body.append("files", f, f.name); });

  return fetch(path, {method: "POST", body: body})
    .then(function(r){
      return r.json().catch(function(){ return {}; }).then(function(data){
        if(!r.ok){
          var failure = new Error(data.error || "that didn't work");
          failure.status = r.status;
          throw failure;
        }
        return data;
      });
    });
}

var MAX_FILES = 10;
var MAX_FILE_BYTES = 25 * 1024 * 1024;

// One file picker for the whole toolbar, outside the panel: the panel is
// rebuilt on every render, and a picker rebuilt under an open file dialog
// would never hear what was chosen. onPicked is who asked.
var picker = document.createElement("input");
picker.type = "file";
picker.multiple = true;
picker.style.display = "none";
root.appendChild(picker);

var onPicked = null;

picker.addEventListener("change", function(){
  var chosen = Array.prototype.slice.call(picker.files || []);
  picker.value = "";

  var problem = "";
  chosen = chosen.filter(function(f){
    if(f.size > MAX_FILE_BYTES){ problem = f.name + " is too large - at most 25 MB a file."; return false; }
    if(!f.size){ problem = f.name + " is empty."; return false; }
    return true;
  });

  // Whoever asked is answered once: a later choice belongs to a later click.
  var then = onPicked;
  onPicked = null;

  if(problem){ error = problem; }
  if(then && (chosen.length || problem)){ then(chosen); }
});

function pickFiles(then){
  onPicked = then;
  picker.click();
}

function baseName(path){
  return String(path).split(/[\\/]/).pop();
}

// The files as chips; onRemove makes each removable.
function fileChips(names, onRemove){
  var list = document.createElement("div");
  list.className = "decurix-files";

  names.forEach(function(name, index){
    var chip = document.createElement("span");
    chip.className = "decurix-file";
    chip.title = name;

    var text = document.createElement("span");
    text.textContent = baseName(name);
    chip.appendChild(text);

    if(onRemove){
      var x = document.createElement("button");
      x.type = "button";
      x.setAttribute("aria-label", "Remove " + baseName(name));
      x.textContent = "\u00d7";
      x.disabled = busy;
      x.addEventListener("click", function(){ onRemove(index); });
      chip.appendChild(x);
    }

    list.appendChild(chip);
  });

  return list;
}

// Adds files to the ticket that is open: a document reached for part way
// through, kept with the recording like the ones the ticket was opened with.
function attachToTicket(files){
  if(busy || !files.length){ render(); return; }
  busy = true;
  render();

  apiFiles("/api/desktops/ticket/attach", {}, files).then(function(data){
    busy = false;
    setTicket(data.ticket || ticket);
  }, function(e){
    busy = false;
    error = e.message;
    render();
  });
}

function act(action, fields){
  if(busy){ return; }
  busy = true;
  error = "";
  render();

  api("/api/desktops/ticket/action", Object.assign({action: action}, fields || {})).then(function(data){
    busy = false;

    if(action === "end"){
      // record_task.end() clears its ticket on its own thread a moment after
      // answering, so the ticket this response carries can still show it
      // open for an instant - trusting it here would flash the ticket panel
      // once more before the next poll corrects it. End always means ended.
      reasonOpen = false;
      draftReason = "";
      reasonAutoPaused = false;
      if(data.ticket && data.ticket.session_id){ lastSession = data.ticket.session_id; }
      ticket = null;
      render();
      return;
    }

    setTicket(data.ticket || null);
  }, function(e){
    busy = false;
    error = e.message;
    render();
  });
}

function setTicket(next){
  if(next){ clearPendingState(); }
  if(next && next.session_id){ lastSession = next.session_id; }
  if(next){ lastTitle = next.title || ""; draftName = null; }
  if(next){ endedStarted = ""; }   // a new ticket: the last recording's replay/lesson is history

  // A ticket ending - however that happens, not just through this tab's own
  // End - leaves nothing for an open Reason box to attach to.
  if(!next){
    reasonOpen = false; draftReason = ""; reasonAutoPaused = false;
  }else if(reasonOpen && reasonAutoPaused && !next.paused){
    // Resumed some other way - the plain Continue button, another tab -
    // while a reason we paused for was still being written. The box no
    // longer has a paused moment to sit in.
    reasonOpen = false; draftReason = ""; reasonAutoPaused = false;
  }

  ticket = next;

  // A ticket appearing (or starting to record) is worth surfacing even if
  // the panel was closed - it is easy to open a ticket from the recordings
  // page or a second tab and not notice this one never showed it.
  if(ticket && !userToggled){ open(true); }

  render();
}

function open(state){
  root.classList.toggle("decurix-open", state);
}

// --- dragging: the tab doubles as a handle for the whole toolbar ----------
// Starts out docked top-center (the CSS default); the first real drag - past
// DRAG_THRESHOLD, so an ordinary tap-to-open still works - switches it to an
// explicit pixel position it keeps from then on, clamped to stay on screen.
var DRAG_THRESHOLD = 6;
var drag = null;   // {pointerId, startX, startY, baseLeft, baseTop} while a drag is live
var dragged = false; // true once this pointer gesture actually moved the toolbar -
                      // read by the click handler so a real drag doesn't also toggle open/closed

function clampToViewport(left, top){
  var r = root.getBoundingClientRect();
  var maxLeft = Math.max(0, window.innerWidth - r.width);
  var maxTop = Math.max(0, window.innerHeight - r.height);
  return {left: Math.min(Math.max(0, left), maxLeft), top: Math.min(Math.max(0, top), maxTop)};
}

function placeAt(left, top){
  var pos = clampToViewport(left, top);
  root.style.left = pos.left + "px";
  root.style.top = pos.top + "px";
}

function pinToCurrentPosition(){
  if(root.classList.contains("decurix-pinned")){ return; }
  var r = root.getBoundingClientRect();
  root.classList.add("decurix-pinned");
  placeAt(r.left, r.top);
}

tab.addEventListener("pointerdown", function(e){
  if(e.button !== undefined && e.button !== 0){ return; }
  var r = root.getBoundingClientRect();
  drag = {pointerId: e.pointerId, startX: e.clientX, startY: e.clientY, baseLeft: r.left, baseTop: r.top};
  dragged = false;
  tab.setPointerCapture(e.pointerId);
});

tab.addEventListener("pointermove", function(e){
  if(!drag || drag.pointerId !== e.pointerId){ return; }
  var dx = e.clientX - drag.startX, dy = e.clientY - drag.startY;
  if(!dragged && Math.hypot(dx, dy) < DRAG_THRESHOLD){ return; }
  dragged = true;
  pinToCurrentPosition();
  placeAt(drag.baseLeft + dx, drag.baseTop + dy);
});

function endDrag(e){
  if(!drag || (e.pointerId !== undefined && drag.pointerId !== e.pointerId)){ return; }
  drag = null;
}
tab.addEventListener("pointerup", endDrag);
tab.addEventListener("pointercancel", endDrag);

window.addEventListener("resize", function(){
  if(!root.classList.contains("decurix-pinned")){ return; }
  var r = root.getBoundingClientRect();
  placeAt(r.left, r.top);
});

function refresh(){
  fetch("/api/desktops/ticket?id=" + encodeURIComponent(DESKTOP_ID), {cache: "no-store"})
    .then(function(r){ return r.ok ? r.json() : {ticket: null}; })
    .then(function(data){
      if(busy){ return; }

      var next = data.ticket || null;

      // When waiting for a new ticket, skip re-render if the ticket hasn't appeared yet.
      if(ticketPending && !next){ return; }
      // Rebuilding the panel on every poll would wipe out a task name being
      // typed into the create-ticket input mid-keystroke (and drop focus
      // generally); skip it when nothing actually changed.
      if(!ticketPending && JSON.stringify(next) === JSON.stringify(ticket)){ return; }

      setTicket(next);
    })
    .catch(function(){});
}

function downloadUrl(session){
  return "/api/desktops/recordings/export?id=" + encodeURIComponent(DESKTOP_ID) + "&session=" + encodeURIComponent(session);
}

function button(text, handler, cls, disabled){
  var b = document.createElement("button");
  b.type = "button";
  if(cls){ b.className = cls; }
  b.textContent = text;
  b.disabled = !!disabled || busy;
  b.addEventListener("click", handler);
  return b;
}

function row(first){
  var r = document.createElement("div");
  r.className = "decurix-ticket-row" + (first ? " decurix-first" : "");
  return r;
}

function renderCreate(){
  panel.innerHTML = "";

  var input = document.createElement("input");
  input.type = "text";
  input.placeholder = "What are you about to do?";
  input.maxLength = 300;
  input.autocomplete = "off";
  input.disabled = busy || ticketPending;
  // Carried across re-renders (an error re-render included) so a failed
  // submit - "a task is still running", a dropped request - does not make
  // someone retype what they had already written.
  input.value = draftTask;
  input.addEventListener("input", function(){ draftTask = input.value; });

  function submit(){
    var task = input.value.trim();
    if(!task || busy || ticketPending){ return; }
    busy = true; ticketPending = true; error = ""; render();

    if(pendingTimer){ clearInterval(pendingTimer); }
    if(pendingTimeout){ clearTimeout(pendingTimeout); }

    // Fast poll every 250ms while ticket is pending so it appears immediately
    pendingTimer = setInterval(function(){
      if(!ticketPending){ clearInterval(pendingTimer); pendingTimer = null; return; }
      refresh();
    }, 250);

    // Timeout safety: if the ticket has not appeared after 25s, abort pending state
    pendingTimeout = setTimeout(function(){
      if(ticketPending){
        clearPendingState();
        error = "Opening ticket timed out. Please try again.";
        render();
      }
    }, 25000);

    apiFiles("/api/desktops/record", {task: task}, draftFiles).then(function(){
      draftTask = "";
      draftFiles = [];
      busy = false;
      render();
      refresh();
    }, function(e){
      busy = false; clearPendingState();
      error = e.message;
      render();
    });
  }

  input.addEventListener("keydown", function(e){ if(e.key === "Enter"){ e.preventDefault(); submit(); } });

  var clip = button("\ud83d\udcce", function(){
    pickFiles(function(files){
      draftFiles = draftFiles.concat(files).slice(0, MAX_FILES);
      render();
    });
  }, "decurix-clip", ticketPending);
  clip.title = "Attach the documents you will work from (from this computer)";
  clip.setAttribute("aria-label", "Attach files");

  var r = row(true);
  r.appendChild(input);
  r.appendChild(clip);
  r.appendChild(button("+", submit, "decurix-primary", ticketPending));
  panel.appendChild(r);

  if(draftFiles.length){
    panel.appendChild(fileChips(draftFiles.map(function(f){ return f.name; }), function(index){
      draftFiles.splice(index, 1);
      render();
    }));

    var hint = document.createElement("div");
    hint.className = "decurix-files-hint";
    hint.textContent = "Saved in this desktop's work/inbox folder and kept with the recording.";
    panel.appendChild(hint);
  }
}

function renderPending(){
  panel.innerHTML = "";
  var loading = document.createElement("div");
  loading.className = "decurix-ticket-loading";
  loading.innerHTML = '<span class="decurix-ticket-spinner" aria-hidden="true"></span><span style="flex:1">Opening ticket…<br><small>Starting the desktop if needed</small></span>';

  function cancelPending(){
    clearPendingState();
    busy = false;
    render();
  }

  var cancelBtn = button("Cancel", cancelPending, "decurix-action");
  cancelBtn.style.padding = "3px 8px";
  cancelBtn.style.fontSize = "11px";
  loading.appendChild(cancelBtn);

  panel.appendChild(loading);
}

function renderTicket(){
  panel.innerHTML = "";

  var title = document.createElement("div");
  title.id = "decurix-ticket-title";
  title.textContent = ticket.title || "Untitled";
  panel.appendChild(title);

  var state = document.createElement("div");
  state.id = "decurix-ticket-state";
  state.textContent = !ticket.session_id ? "Ready to record — open the app you'll use, then press Record."
    : ticket.paused ? "Paused — nothing is being recorded"
    : ticket.recording ? (ticket.events === 1 ? "Recording — 1 action" : "Recording — " + ticket.events + " actions")
    : "Recording";
  panel.appendChild(state);

  var r = row(true);

  if(!ticket.session_id){
    r.appendChild(button("● Record", function(){ act("begin"); }, "decurix-primary"));
  }else if(ticket.paused){
    r.appendChild(button("▶ Continue", function(){ act("resume"); }, "decurix-primary"));
  }else{
    r.appendChild(button("■ Stop", function(){ act("pause", {reason: "paused from the screen toolbar"}); }));
  }

  // Reachable whether currently recording or already paused - writing a note
  // works either way (demonstration.note()) - just not before recording has
  // started, when there is no timeline yet for it to sit on.
  if(ticket.session_id){
    r.appendChild(button("Reason", startReason, reasonOpen ? "decurix-active" : ""));
  }

  r.appendChild(button("End", function(){ act("end"); }, "decurix-danger"));
  panel.appendChild(r);

  var attachRow = row(false);
  var attach = button("\ud83d\udcce Attach files", function(){ pickFiles(attachToTicket); });
  attach.title = "Add a document from this computer to this ticket";
  attachRow.appendChild(attach);
  panel.appendChild(attachRow);

  if(ticket.files && ticket.files.length){ panel.appendChild(fileChips(ticket.files)); }

  if(reasonOpen){ panel.appendChild(reasonBox()); }
}

function startReason(){
  if(reasonOpen){
    // Closing without submitting: undo the pause only if opening the box is
    // what caused it - a pause already in effect (the plain Stop button)
    // stays exactly as the person left it.
    reasonOpen = false;
    draftReason = "";
    if(reasonAutoPaused && ticket && ticket.paused){
      reasonAutoPaused = false;
      act("resume");
    }else{
      render();
    }
    return;
  }

  reasonOpen = true;

  if(ticket && !ticket.paused){
    reasonAutoPaused = true;
    act("pause", {reason: "adding a reason"});
  }else{
    render();
  }
}

function reasonBox(){
  var wrap = document.createElement("div");
  wrap.className = "decurix-reason";

  var hint = document.createElement("div");
  hint.className = "decurix-reason-hint";
  hint.textContent = "Recording is paused while you write this - submitting continues it.";
  wrap.appendChild(hint);

  var textarea = document.createElement("textarea");
  textarea.placeholder = "What are you doing right now?";
  textarea.maxLength = 1000;
  textarea.rows = 2;
  textarea.disabled = busy;
  // Carried across re-renders the same way the create-ticket input's text is
  // (see renderCreate) - a poll landing, or a failed submit, must not lose
  // what was typed.
  textarea.value = draftReason;
  textarea.addEventListener("input", function(){ draftReason = textarea.value; });

  function submit(){
    var text = textarea.value.trim();
    if(!text || busy){ return; }
    busy = true; error = ""; render();

    // note() always ends paused - snapshot whether this Reason click is what
    // paused it before the request clears that flag, since only then should
    // submitting also continue: a pause already in effect (Stop) is left
    // exactly as the person set it, in case what follows is why they set it.
    var autoResume = reasonAutoPaused;

    api("/api/desktops/ticket/action", {action: "note", text: text}).then(function(data){
      draftReason = "";
      reasonOpen = false;
      reasonAutoPaused = false;
      busy = false;

      if(autoResume){
        act("resume");
      }else{
        setTicket(data.ticket || null);
      }
    }, function(e){
      busy = false;
      error = e.message;
      render();
    });
  }

  // Enter adds the note (matching the create-ticket input); Shift+Enter is a
  // newline, since a reason is more often more than one line than a task name is.
  textarea.addEventListener("keydown", function(e){
    if(e.key === "Enter" && !e.shiftKey){ e.preventDefault(); submit(); }
  });

  wrap.appendChild(textarea);

  var submitRow = row(false);
  submitRow.appendChild(button("Add & continue", submit, "decurix-primary"));
  wrap.appendChild(submitRow);

  if(!busy){ textarea.focus(); }

  return wrap;
}

// What the "Recording saved" view can do with the recording it just made. Replay
// and Learn are the same two calls the recordings page makes - a task like any
// other, refused while the desktop has one running - and Download is the
// export. Neither Replay nor Learn is given a value to type, so a recording
// that typed something secret halts and asks: the recordings page is where
// values are given.
function startFromRecording(kind){
  if(busy || !lastSession){ return; }

  busy = true;
  error = "";
  endedStarting = kind;
  render();

  // End answers before the ticket's own task has finished closing, and a replay
  // or a lesson is refused (409) while one is still open. That is a moment, not
  // a real conflict, so give it a few tries before showing the reason.
  var tries = 0;

  // Learn names the skill with whatever the name box says; the recording was
  // renamed to it already (saveName), or is being now.
  var fields = {session: lastSession};
  if(kind === "learn"){ fields.name = currentName(); }

  (function attempt(){
    api("/api/desktops/recordings/" + kind, fields).then(function(){
      busy = false;
      endedStarting = "";
      endedStarted = kind;
      render();
    }, function(e){
      if(e.status === 409 && ++tries < 6){ setTimeout(attempt, 1000); return; }

      busy = false;
      endedStarting = "";
      error = e.message;
      render();
    });
  })();
}

function currentName(){
  return (draftName === null ? lastTitle : draftName).trim();
}

// Renames the saved recording to what the name box says. The skill Learn
// teaches from it is called the same (startFromRecording).
function saveName(){
  var wanted = currentName();
  if(!lastSession || !wanted || wanted === lastTitle){ return; }

  api("/api/desktops/recordings/update", {session: lastSession, title: wanted}).then(function(){
    lastTitle = wanted;
    if(draftName !== null && draftName.trim() === wanted){ draftName = null; }
  }, function(e){
    error = e.message;
    render();
  });
}

function renderJustEnded(){
  panel.innerHTML = "";

  var title = document.createElement("div");
  title.id = "decurix-ticket-title";
  title.textContent = "Recording saved";
  panel.appendChild(title);

  // Its name - the ticket's task until someone changes it here. Carried
  // across re-renders the way draftTask is.
  var nameRow = row(false);
  var name = document.createElement("input");
  name.type = "text";
  name.maxLength = 80;
  name.autocomplete = "off";
  name.placeholder = "Name this recording";
  name.setAttribute("aria-label", "Name of the recording and the skill taught from it");
  name.value = draftName === null ? lastTitle : draftName;
  name.disabled = busy;
  name.addEventListener("input", function(){ draftName = name.value; });
  name.addEventListener("change", saveName);
  name.addEventListener("keydown", function(e){ if(e.key === "Enter"){ e.preventDefault(); name.blur(); } });
  nameRow.appendChild(name);
  panel.appendChild(nameRow);

  if(endedStarted){
    var started = document.createElement("div");
    started.id = "decurix-ticket-state";
    started.style.marginTop = "8px";
    started.textContent = endedStarted === "replay"
      ? "Replay started. It runs on this screen; follow it in the chat."
      : "Teaching started. It asks its questions in the chat.";
    panel.appendChild(started);

    var chat = document.createElement("a");
    chat.className = "decurix-download";
    chat.href = "/view/" + encodeURIComponent(DESKTOP_ID);
    chat.target = "_blank";
    chat.rel = "noopener";
    chat.textContent = "Open the chat ↗";
    panel.appendChild(chat);
  }else{
    var actions = row(false);
    actions.appendChild(button(endedStarting === "replay" ? "Starting…" : "Replay",
      function(){ startFromRecording("replay"); }, "decurix-primary"));
    actions.appendChild(button(endedStarting === "learn" ? "Starting…" : "Learn",
      function(){ startFromRecording("learn"); }));
    panel.appendChild(actions);
  }

  var link = document.createElement("a");
  link.className = "decurix-download";
  link.href = downloadUrl(lastSession);
  link.textContent = "Download recording";
  panel.appendChild(link);

  var r = row(false);
  r.appendChild(button("New ticket", function(){ lastSession = null; lastTitle = ""; draftName = null; endedStarted = ""; render(); }));
  panel.appendChild(r);
}

function render(){
  if(ticket){
    dot.className = !ticket.session_id ? "decurix-armed" : ticket.paused ? "decurix-paused" : "decurix-rec";
    label.textContent = !ticket.session_id ? "Ticket open" : ticket.paused ? "Paused" : "Recording";
    renderTicket();
  }else if(ticketPending){
    dot.className = "decurix-armed";
    label.textContent = "Opening…";
    renderPending();
  }else if(lastSession){
    dot.className = "";
    label.textContent = "Record";
    renderJustEnded();
  }else{
    dot.className = "";
    label.textContent = "Record";
    renderCreate();
  }

  var err = document.getElementById("decurix-ticket-error");
  if(!err){
    err = document.createElement("div");
    err.id = "decurix-ticket-error";
    panel.appendChild(err);
  }
  err.textContent = error;
  err.classList.toggle("decurix-visible", !!error);

  // Browsing/replaying/exporting past recordings used to be a second icon on
  // the dashboard card; it lives only here now, so it needs to stay reachable
  // from every state of this panel, not just when a ticket is open.
  var browse = document.createElement("a");
  browse.id = "decurix-ticket-browse";
  browse.href = "/recordings/" + encodeURIComponent(DESKTOP_ID);
  browse.textContent = "Browse recordings ↗";
  panel.appendChild(browse);
}

tab.addEventListener("click", function(){
  // A real drag already moved the toolbar - the pointerup that ends it also
  // fires a click, which must not also toggle the panel open/closed.
  if(dragged){ dragged = false; return; }
  userToggled = true;
  open(!root.classList.contains("decurix-open"));
});

render();
refresh();
setInterval(refresh, POLL_MS);
})();
</script>
`

// titleOverrideScript pins the browser tab's title to the desktop's own
// assigned name (e.g. "burga"). The static <title> swap in
// decurixBrandingRewrites only lasts until noVNC's own JS runs: once the RFB
// connection reports the container's X11 desktop name - built from the
// container hostname, "desktop_<id>:1 (headless) - noVNC" - noVNC overwrites
// document.title with it asynchronously, after this page has already loaded.
// Object.defineProperty shadows document.title's own accessor with one that
// ignores whatever value anything on the page (noVNC included) tries to set
// afterward, so every later attempt keeps landing on the desktop's name.
const titleOverrideScript = `
<script>
(function(){
"use strict";
var desiredTitle = "__DESKTOP_ID__";
var titleEl = document.querySelector("title");
if(!titleEl){
  titleEl = document.createElement("title");
  document.head.appendChild(titleEl);
}
Object.defineProperty(document, "title", {
  configurable: true,
  get: function(){ return desiredTitle; },
  set: function(){ titleEl.textContent = desiredTitle; }
});
document.title = desiredTitle;
})();
</script>
`

// autoConnectHelper ensures the user connects directly into the VM GUI
// without stalling on noVNC's manual connect button or VNC password dialog.
const autoConnectHelper = `
<script>
(function(){
  "use strict";
  function autoFillAndConnect(){
    // noVNC 1.6 calls the dialog noVNC_credentials_*; older ones noVNC_password_*.
    var pwdInput = document.getElementById('noVNC_password_input');
    var pwdDlg = document.getElementById('noVNC_credentials_dlg') || document.getElementById('noVNC_password_dlg');
    var pwdBtn = document.getElementById('noVNC_credentials_button') || document.getElementById('noVNC_password_button');
    if (pwdDlg && pwdDlg.classList.contains('noVNC_open') && pwdInput && pwdBtn) {
      if (!pwdInput.value) {
        pwdInput.value = "admin";
      }
      pwdBtn.click();
    }

    var connBtn = document.getElementById('noVNC_connect_button');
    var connDlg = document.getElementById('noVNC_connect_dlg');
    if (connDlg && connDlg.classList.contains('noVNC_open') && connBtn) {
      connBtn.click();
    }
  }

  if (window.MutationObserver && document.documentElement) {
    var observer = new MutationObserver(function() {
      autoFillAndConnect();
    });
    observer.observe(document.documentElement, { attributes: true, subtree: true, attributeFilter: ['class', 'style'] });
  }

  var attempts = 0;
  var interval = setInterval(function(){
    attempts++;
    autoFillAndConnect();
    if (attempts > 60) {
      clearInterval(interval);
    }
  }, 50);

  window.addEventListener('DOMContentLoaded', autoFillAndConnect);
  window.addEventListener('load', autoFillAndConnect);
})();
</script>
`

// resourceAlertPopup is injected into the VM GUI viewer at /desktop/<id>/,
// displaying a sleek notification banner when container resources are about
// to exceed. It prompts the user with "Yes" (to apply predicted CPU/RAM increase)
// and "No" (to dismiss).
const resourceAlertPopup = `
<style>
#decurix-res-alert-wrap {
  position: fixed;
  top: 18px;
  right: 18px;
  z-index: 10000;
  max-width: 360px;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  display: none;
}
#decurix-res-alert-box {
  background: rgba(28, 25, 23, 0.95);
  backdrop-filter: blur(12px);
  -webkit-backdrop-filter: blur(12px);
  border: 1px solid #d97706;
  box-shadow: 0 10px 25px -5px rgba(0, 0, 0, 0.6), 0 0 15px rgba(217, 119, 6, 0.25);
  border-radius: 10px;
  padding: 14px 16px;
  color: #fef3c7;
  animation: decurix-slide-down 0.25s cubic-bezier(0.16, 1, 0.3, 1);
}
@keyframes decurix-slide-down {
  from { opacity: 0; transform: translateY(-10px); }
  to { opacity: 1; transform: translateY(0); }
}
.fra-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}
.fra-title {
  display: flex;
  align-items: center;
  gap: 6px;
  font-weight: 600;
  font-size: 13.5px;
  color: #fbbf24;
}
.fra-close {
  background: transparent;
  border: none;
  color: #a8a29e;
  font-size: 18px;
  cursor: pointer;
  line-height: 1;
  padding: 0 4px;
}
.fra-close:hover { color: #f5f5f4; }
.fra-body {
  font-size: 12.5px;
  line-height: 1.45;
  color: #e7e5e4;
  margin-bottom: 12px;
}
.fra-actions {
  display: flex;
  gap: 8px;
  justify-content: flex-end;
}
.fra-btn {
  font-size: 12px;
  font-weight: 600;
  padding: 6px 14px;
  border-radius: 6px;
  border: 1px solid transparent;
  cursor: pointer;
  transition: all 0.15s ease;
}
.fra-btn-yes {
  background: #d97706;
  color: #ffffff;
}
.fra-btn-yes:hover {
  background: #b45309;
}
.fra-btn-no {
  background: #292524;
  color: #d6d3d1;
  border-color: #44403c;
}
.fra-btn-no:hover {
  background: #44403c;
  color: #fafaf9;
}
</style>
<div id="decurix-res-alert-wrap">
  <div id="decurix-res-alert-box">
    <div class="fra-header">
      <div class="fra-title">⚠️ Resource Warning</div>
      <button class="fra-close" id="fra-close-btn">&times;</button>
    </div>
    <div class="fra-body" id="fra-body-text">Resources are about to exceed. Automatically increase CPU/RAM?</div>
    <div class="fra-actions">
      <button class="fra-btn fra-btn-no" id="fra-no-btn">No</button>
      <button class="fra-btn fra-btn-yes" id="fra-yes-btn">Yes, Increase</button>
    </div>
  </div>
</div>
<script>
(function() {
  var desktopID = "__DESKTOP_ID__";
  var wrap = document.getElementById("decurix-res-alert-wrap");
  var bodyText = document.getElementById("fra-body-text");
  var yesBtn = document.getElementById("fra-yes-btn");
  var noBtn = document.getElementById("fra-no-btn");
  var closeBtn = document.getElementById("fra-close-btn");
  var activeAlert = null;
  var dismissed = false;

  function pollAlert() {
    if (dismissed) return;
    fetch("/api/desktops/resource-alerts?id=" + encodeURIComponent(desktopID))
      .then(function(r) { return r.json(); })
      .then(function(data) {
        if (data && data.alert) {
          activeAlert = data.alert;
          bodyText.textContent = data.alert.message || (
            "CPU: " + Math.round(data.alert.cpu_percent) + "%, RAM: " + Math.round(data.alert.memory_percent) +
            "%. Increase to " + data.alert.predicted_cpus + " CPUs & " + data.alert.predicted_memory_mb + " MB RAM?"
          );
          wrap.style.display = "block";
        } else if (!activeAlert) {
          wrap.style.display = "none";
        }
      })
      .catch(function() {});
  }

  function dismissAlert() {
    dismissed = true;
    wrap.style.display = "none";
    activeAlert = null;
    fetch("/api/desktops/resource-dismiss", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: "desktop_id=" + encodeURIComponent(desktopID)
    }).catch(function() {});
    setTimeout(function() { dismissed = false; }, 300000);
  }

  function acceptIncrease() {
    if (!activeAlert) return;
    yesBtn.disabled = true;
    yesBtn.textContent = "Increasing...";
    fetch("/api/desktops/resource-increase", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: "desktop_id=" + encodeURIComponent(desktopID) +
            "&cpus=" + encodeURIComponent(activeAlert.predicted_cpus) +
            "&memory_mb=" + encodeURIComponent(activeAlert.predicted_memory_mb)
    })
    .then(function(r) { return r.json(); })
    .then(function(res) {
      bodyText.textContent = "✓ Resources increased to " + res.cpus + " CPUs and " + res.memory_mb + " MB RAM!";
      yesBtn.style.display = "none";
      noBtn.style.display = "none";
      setTimeout(function() {
        wrap.style.display = "none";
        activeAlert = null;
        yesBtn.disabled = false;
        yesBtn.style.display = "";
        noBtn.style.display = "";
        yesBtn.textContent = "Yes, Increase";
      }, 3000);
    })
    .catch(function() {
      yesBtn.disabled = false;
      yesBtn.textContent = "Retry";
      bodyText.textContent = "Failed to scale resources. Please try again.";
    });
  }

  if (noBtn) noBtn.addEventListener("click", dismissAlert);
  if (closeBtn) closeBtn.addEventListener("click", dismissAlert);
  if (yesBtn) yesBtn.addEventListener("click", acceptIncrease);

  setInterval(pollAlert, 4000);
  setTimeout(pollAlert, 1500);
})();
</script>
`

// mobileViewerEnhancements is spliced into the noVNC vnc.html response by
// rewriteDesktopHTML, but only for requests carrying ?ui=touch - the marker
// index.html's "open the screen in a new tab" button puts on the full-size,
// interactive screen URL, so this never lands in the same page's small,
// non-interactive dcard-preview thumbnail (same vnc.html, same query-free URL
// otherwise). The injected script itself further gates on the browser
// actually being touch-capable, so a mouse-and-keyboard visitor opening the
// same screen sees nothing extra either.
//
// That button is now the only thing emitting ?ui=touch: the per-desktop page
// at /view/<id> is the chat alone and embeds no screen. Anything that opens a
// desktop screen for real use needs to carry the marker, or these affordances
// silently stop being injected.
//
// It adds two touch-only affordances on top of stock
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
#noVNC_container.decurix-panning{transition:none}
#noVNC_container:not(.decurix-panning){transition:transform .12s ease-out}

.decurix-touch-btn{
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
.decurix-touch-btn img{width:24px;height:24px;filter:invert(1)}
#decurix-kb-btn{left:calc(14px + env(safe-area-inset-left, 0px))}
#decurix-kb-btn.decurix-active{background:rgba(74,144,255,.92)}

#decurix-reset-btn{
  right:calc(14px + env(safe-area-inset-right, 0px));
  width:auto;
  padding:0 16px;
  font:600 13px/1 sans-serif;
  color:#fff;
  opacity:0;
  pointer-events:none;
  transition:opacity .15s ease-out;
}
#decurix-reset-btn.decurix-visible{opacity:1;pointer-events:auto}
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
kbBtn.id = "decurix-kb-btn";
kbBtn.type = "button";
kbBtn.className = "decurix-touch-btn";
kbBtn.setAttribute("aria-label", "Show keyboard");
kbBtn.innerHTML = '<img src="app/images/keyboard.svg" alt="">';
kbBtn.addEventListener("click", function(e){
  e.preventDefault();
  nativeKbBtn.click();
});
document.body.appendChild(kbBtn);

new MutationObserver(function(){
  kbBtn.classList.toggle("decurix-active", nativeKbBtn.classList.contains("noVNC_selected"));
}).observe(nativeKbBtn, {attributes: true, attributeFilter: ["class"]});

// --- two-finger pinch-to-zoom + pan ----------------------------------
var resetBtn = document.createElement("button");
resetBtn.id = "decurix-reset-btn";
resetBtn.type = "button";
resetBtn.className = "decurix-touch-btn";
resetBtn.textContent = "Reset zoom";
resetBtn.addEventListener("click", function(e){
  e.preventDefault();
  resetTransform();
});
document.body.appendChild(resetBtn);

var scale = 1, tx = 0, ty = 0, pinch = null;

function apply(){
  container.style.transform = "translate(" + tx + "px," + ty + "px) scale(" + scale + ")";
  resetBtn.classList.toggle("decurix-visible", scale > 1.01);
}

function resetTransform(){
  scale = 1; tx = 0; ty = 0;
  container.classList.remove("decurix-panning");
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
  container.classList.add("decurix-panning");
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
  container.classList.remove("decurix-panning");
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

	// The gated listener (the tunnel, a phone) reaches soul only on gatedAgentRoutes: soul's memory, skills and stats
	// read what soul keeps, and its chat would drive the worker with no task record and no author. They answer on the
	// primary listener only (C14.4, F-S1).
	if isGated(r) {
		rest := "/"
		if len(parts) == 2 {
			rest = path.Clean("/" + parts[1])
		}
		if !gatedAgentRoute(r.Method, rest) {
			http.NotFound(w, r)
			return
		}
	}

	proxyURL, desktopID, err := s.Desktops.AgentProxyTarget(r.Context(), parts[0])

	if proxyTargetFailed(w, r, err) {
		return
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(proxyURL)

	prefix := "/agent/" + desktopID
	// Soul takes the desktop's agent token; the person's browser never sees it.
	director := reverseProxy.Director
	reverseProxy.Director = func(req *http.Request) {
		director(req)
		req.Header.Del("Authorization")
		s.Desktops.AuthorizeAgentRequest(req, desktopID)
	}

	r.URL.Path = strings.TrimPrefix(
		r.URL.Path,
		prefix,
	)

	if r.URL.Path == "" {
		r.URL.Path = "/"
	}

	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		sandboxContainerContent(resp)
		return rewriteHTML(resp, func(body []byte) []byte {
			rewritten := bytes.ReplaceAll(body, []byte("'/api/"), []byte("'"+prefix+"/api/"))
			return bytes.ReplaceAll(rewritten, []byte("`/api/"), []byte("`"+prefix+"/api/"))
		})
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

func (s *Server) renderProvisioningLaunchScreen(w http.ResponseWriter, r *http.Request, prov *desktop.ProvisioningDesktop) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)

	name := htmlEscape(prov.Name)
	if name == "" {
		name = htmlEscape(prov.ID)
	}

	progress := prov.Progress
	if progress < 10 {
		progress = 10
	}
	message := htmlEscape(prov.Message)
	if message == "" {
		message = "Spinning up desktop environment..."
	}

	fmt.Fprintf(w, launchScreenHTML, name, name, progress, message, prov.ID)
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

const launchScreenHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Starting %s - Decurix</title>
<style>
  :root {
    --bg: #090d16;
    --card: rgba(18, 24, 38, 0.75);
    --border: rgba(255, 255, 255, 0.08);
    --primary: #3b82f6;
    --accent: #60a5fa;
    --text: #f3f4f6;
    --text-muted: #9ca3af;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    background: radial-gradient(circle at top center, #1e293b 0%%, var(--bg) 70%%);
    color: var(--text);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 1.5rem;
  }
  .card {
    background: var(--card);
    backdrop-filter: blur(16px);
    -webkit-backdrop-filter: blur(16px);
    border: 1px solid var(--border);
    border-radius: 1.25rem;
    padding: 2.5rem 2rem;
    max-width: 480px;
    width: 100%%;
    text-align: center;
    box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.5), 0 0 40px rgba(59, 130, 246, 0.1);
    animation: fadeIn 0.4s ease-out;
  }
  @keyframes fadeIn {
    from { opacity: 0; transform: translateY(10px) scale(0.98); }
    to { opacity: 1; transform: translateY(0) scale(1); }
  }
  .icon-wrap {
    width: 64px;
    height: 64px;
    border-radius: 1rem;
    background: linear-gradient(135deg, rgba(59, 130, 246, 0.2), rgba(96, 165, 250, 0.05));
    border: 1px solid rgba(59, 130, 246, 0.3);
    display: flex;
    align-items: center;
    justify-content: center;
    margin: 0 auto 1.5rem;
    position: relative;
  }
  .spinner {
    position: absolute;
    inset: -4px;
    border: 2px solid transparent;
    border-top-color: var(--accent);
    border-right-color: rgba(96, 165, 250, 0.4);
    border-radius: 1.25rem;
    animation: spin 1.2s cubic-bezier(0.5, 0, 0.5, 1) infinite;
  }
  @keyframes spin {
    0%% { transform: rotate(0deg); }
    100%% { transform: rotate(360deg); }
  }
  h1 { font-size: 1.5rem; font-weight: 700; margin-bottom: 0.5rem; letter-spacing: -0.02em; }
  .name-badge {
    display: inline-block;
    padding: 0.25rem 0.75rem;
    background: rgba(255, 255, 255, 0.06);
    border: 1px solid var(--border);
    border-radius: 9999px;
    font-size: 0.85rem;
    color: var(--accent);
    margin-bottom: 1.5rem;
    font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  }
  .progress-track {
    background: rgba(255, 255, 255, 0.06);
    border-radius: 9999px;
    height: 8px;
    overflow: hidden;
    position: relative;
    margin-bottom: 1rem;
    border: 1px solid rgba(255, 255, 255, 0.04);
  }
  .progress-bar {
    height: 100%%;
    background: linear-gradient(90deg, #3b82f6, #60a5fa, #93c5fd);
    border-radius: 9999px;
    transition: width 0.4s ease;
    box-shadow: 0 0 12px rgba(96, 165, 250, 0.6);
  }
  .status-msg {
    font-size: 0.95rem;
    color: var(--text-muted);
    min-height: 1.5rem;
    transition: all 0.2s;
  }
  .hint {
    margin-top: 1.5rem;
    font-size: 0.8rem;
    color: rgba(156, 163, 175, 0.6);
  }
  .error-box {
    display: none;
    background: rgba(239, 68, 68, 0.1);
    border: 1px solid rgba(239, 68, 68, 0.3);
    color: #fca5a5;
    padding: 1rem;
    border-radius: 0.75rem;
    margin-top: 1rem;
    font-size: 0.9rem;
    text-align: left;
  }
  .back-btn {
    display: inline-block;
    margin-top: 1rem;
    padding: 0.5rem 1.25rem;
    background: rgba(255, 255, 255, 0.1);
    border: 1px solid var(--border);
    color: #fff;
    border-radius: 0.5rem;
    text-decoration: none;
    font-size: 0.85rem;
    cursor: pointer;
  }
  .back-btn:hover { background: rgba(255, 255, 255, 0.15); }
</style>
</head>
<body>
<div class="card">
  <div class="icon-wrap">
    <div class="spinner"></div>
    <svg width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="#60a5fa" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <rect x="2" y="3" width="20" height="14" rx="2" ry="2"></rect>
      <line x1="8" y1="21" x2="16" y2="21"></line>
      <line x1="12" y1="17" x2="12" y2="21"></line>
    </svg>
  </div>
  <h1>Spinning Up Desktop</h1>
  <div class="name-badge">%s</div>
  <div class="progress-track">
    <div class="progress-bar" id="pbar" style="width: %d%%;"></div>
  </div>
  <div class="status-msg" id="statusMsg">%s</div>
  <div class="error-box" id="errorBox"></div>
  <div class="hint" id="hint">Decurix is provisioning your container and copying desktop apps. The GUI will connect automatically as soon as it's ready.</div>
</div>
<script>
(function() {
  const desktopID = %q;
  const targetURL = window.location.href;
  const pbar = document.getElementById('pbar');
  const statusMsg = document.getElementById('statusMsg');
  const errorBox = document.getElementById('errorBox');
  const hint = document.getElementById('hint');

  let pollCount = 0;
  const poll = async () => {
    pollCount++;
    try {
      const res = await fetch('/api/desktops/status?id=' + encodeURIComponent(desktopID));
      if (!res.ok) {
        if (pollCount > 10) throw new Error('Status poll returned ' + res.status);
        setTimeout(poll, 800);
        return;
      }
      const data = await res.json();
      if (data.status === 'failed') {
        errorBox.style.display = 'block';
        // The error text can carry what a desktop or a request named: text, never markup.
        errorBox.replaceChildren();
        const head = document.createElement('strong');
        head.textContent = 'Creation Error:';
        const back = document.createElement('a');
        back.href = '/';
        back.className = 'back-btn';
        back.textContent = '← Return to Dashboard';
        errorBox.append(head, document.createElement('br'), String(data.error || data.message || 'Unknown error'),
          document.createElement('br'), back);
        statusMsg.textContent = 'Provisioning Failed';
        statusMsg.style.color = '#ef4444';
        pbar.style.background = '#ef4444';
        return;
      }
      if (data.ready || (data.status === 'running' && data.stage === 'ready')) {
        pbar.style.width = '100%%';
        statusMsg.textContent = 'Ready! Connecting to GUI...';
        statusMsg.style.color = '#10b981';
        setTimeout(() => {
          window.location.replace(targetURL);
        }, 500);
        return;
      }
      if (data.progress) {
        pbar.style.width = Math.max(15, data.progress) + '%%';
      }
      if (data.message) {
        statusMsg.textContent = data.message;
      }
    } catch (e) {
      console.warn('Status poll warning:', e);
    }
    setTimeout(poll, 600);
  };
  setTimeout(poll, 600);
})();
</script>
</body>
</html>
`

// proxyTargetFailed answers a request whose desktop proxy target could not be
// resolved (err from ProxyTarget or AgentProxyTarget) and reports whether it
// did; a nil err writes nothing.
func proxyTargetFailed(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false

	case errors.Is(err, desktop.ErrNotFound):
		http.NotFound(w, r)

	case errors.Is(err, desktop.ErrNotRunning):
		http.Error(
			w,
			"desktop is not running",
			http.StatusServiceUnavailable,
		)

	case errors.Is(err, desktop.ErrNoNetwork):
		http.Error(
			w,
			"desktop is not connected to Decurix network",
			http.StatusServiceUnavailable,
		)

	case errors.Is(err, desktop.ErrNoAddress):
		http.Error(
			w,
			"desktop has no network IP",
			http.StatusServiceUnavailable,
		)

	default:
		internalError(w, "desktop proxy", err)
	}

	return true
}

// gatedAgentRoutes are the soul routes (under /agent/<id>) the gated listener forwards, each read-only: soul's health,
// which reads nothing soul keeps. No page served on the gated listener needs more: the chat and the screen reach a
// worker through /api/desktops/* (task records, authors, the approval gate), never soul directly.
var gatedAgentRoutes = map[string]bool{"/api/health": true}

// gatedAgentRoute reports whether the gated listener forwards method on soul's path p (already cleaned).
func gatedAgentRoute(method, p string) bool {
	return (method == http.MethodGet || method == http.MethodHead) && gatedAgentRoutes[p]
}
