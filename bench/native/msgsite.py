"""
A messaging site built the way LinkedIn's is, for testing without anyone's account.

What matters about LinkedIn's Messaging page (measured 2026-10-03) is its shape,
not its looks: the page a tab shows is a shell (a navigation bar with a search
box of its own), and the messaging app - "Search messages", the conversations,
the message box and Send - is drawn inside a frame of the same site. A layer
that reads only the tab's own document sees a navigation bar and nothing else.

    python bench/native/msgsite.py [port]          serves on 127.0.0.1 (default 8765)

    /                the feed, with a "Messaging" link in the navigation bar
    /messaging/      the shell: navigation + the frame
    /app/            the messaging app itself (what the frame shows)
    /api/state       {"sent": [{"to", "text"}], "searches": [...]}   what a test checks
    /api/reset       POST: forget everything sent

The title carries "Slack" on purpose: it is one of the names the approval gate
knows as a place where Enter sends what was typed, as it knows
linkedin.com/messaging - so Enter in this site's search box is judged as it is
on LinkedIn.
"""

import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PEOPLE = [
    ("Ohona Sahoo", "This message has been deleted."),
    ("Neetika Thakur", "Strategy & Operations Job Opportunity"),
    ("Adish Shah", "Sounds good, talk soon"),
    ("Adish Sharma", "Thanks for connecting"),
    ("Priya Nair", "See you on Friday"),
    ("Rahul Verma", "Sent you the deck"),
]

STATE = {"sent": [], "searches": []}
LOCK = threading.Lock()

NAV = """
<nav aria-label="Global Navigation" style="display:flex;gap:28px;align-items:center;padding:10px 60px;border-bottom:1px solid #ddd;background:#fff">
  <b style="color:#0a66c2;font-size:22px">ch</b>
  <input aria-label="Search" placeholder="Search" style="width:260px;padding:8px;border:1px solid #999;border-radius:16px">
  <a href="/">Home</a>
  <a href="/messaging/" aria-label="Messaging, 0 new notifications">Messaging</a>
  <a href="/">Notifications</a>
</nav>
"""

FEED = """<!doctype html><html><head><meta charset="utf-8"><title>Feed | Slack clone</title></head>
<body style="margin:0;font-family:Segoe UI,Arial,sans-serif;background:#f4f2ee">%s
<main style="padding:30px 60px"><h1>Feed</h1><p>Nothing new today.</p></main></body></html>""" % NAV

SHELL = """<!doctype html><html><head><meta charset="utf-8"><title>Messaging | Slack clone</title></head>
<body style="margin:0;font-family:Segoe UI,Arial,sans-serif;background:#f4f2ee">%s
<div role="status" style="position:absolute;left:-9999px">Navigating to Messaging</div>
<iframe src="/app/" title="Messaging" style="display:block;border:0;width:100%%;height:580px"></iframe>
</body></html>""" % NAV

APP = """<!doctype html><html><head><meta charset="utf-8"><title>app</title>
<style>
 body{margin:0;font-family:Segoe UI,Arial,sans-serif}
 #wrap{display:flex;margin:24px 60px;background:#fff;border:1px solid #ddd;border-radius:8px;height:520px}
 #left{width:390px;border-right:1px solid #ddd;overflow:auto}
 #right{flex:1;display:flex;flex-direction:column}
 .conv{padding:14px 18px;border-bottom:1px solid #eee;cursor:pointer}
 .conv:hover,.conv.on{background:#eef3f8}
 .conv b{display:block;font-weight:600}
 .conv span{color:#666;font-size:14px}
 #thread{flex:1;overflow:auto;padding:18px}
 #box{min-height:60px;margin:0 18px;padding:12px;background:#f4f2ee;border-radius:8px;outline:none}
 #box:empty:before{content:attr(data-placeholder);color:#777}
 #bar{display:flex;justify-content:flex-end;padding:12px 18px}
 #send{padding:6px 18px;border-radius:16px;border:0;background:#0a66c2;color:#fff}
 #send:disabled{background:#ddd;color:#888}
 .msg{margin:6px 0;padding:8px 12px;background:#e8f1fb;border-radius:8px;display:inline-block}
</style></head><body>
<div id="wrap">
 <div id="left">
  <div style="display:flex;align-items:center;gap:14px;padding:14px 18px;border-bottom:1px solid #ddd">
   <h2 style="margin:0;font-size:18px">Messaging</h2>
   <input id="q" aria-label="Search messages" placeholder="Search messages" style="flex:1;padding:8px;border:1px solid #999">
  </div>
  <div id="list"></div>
 </div>
 <div id="right">
  <h3 id="who" style="margin:0;padding:16px 18px;border-bottom:1px solid #ddd">Select a conversation</h3>
  <div id="thread"></div>
  <div id="box" role="textbox" aria-label="Write a message…" aria-multiline="true" contenteditable="true" data-placeholder="Write a message…"></div>
  <div id="bar"><button id="send" disabled>Send</button></div>
 </div>
</div>
<script>
const PEOPLE = %s;
let current = null, sent = [];
const list = document.getElementById('list'), q = document.getElementById('q');
const box = document.getElementById('box'), send = document.getElementById('send');
function show(term) {
  list.innerHTML = '';
  const words = (term || '').toLowerCase().split(/\\s+/).filter(Boolean);
  for (const [name, last] of PEOPLE) {
    if (words.length && !words.every(w => name.toLowerCase().includes(w))) continue;
    const row = document.createElement('div');
    row.className = 'conv' + (name === current ? ' on' : '');
    row.innerHTML = '<b></b><span></span>';
    row.children[0].textContent = name; row.children[1].textContent = last;
    row.onclick = () => open(name);
    list.appendChild(row);
  }
  if (!list.children.length) list.innerHTML = '<p style="padding:18px;color:#666">No conversations found</p>';
}
function open(name) {
  current = name;
  document.getElementById('who').textContent = name;
  draw(); show(q.value.trim() && q.dataset.applied === '1' ? q.value : '');
}
function draw() {
  const thread = document.getElementById('thread');
  thread.innerHTML = '';
  for (const m of sent.filter(m => m.to === current)) {
    const d = document.createElement('div'); const s = document.createElement('span');
    s.className = 'msg'; s.textContent = m.text; d.appendChild(s); thread.appendChild(d);
  }
}
// Like LinkedIn's: the list is searched when Enter is pressed, not as you type.
q.addEventListener('keydown', e => {
  if (e.key !== 'Enter') { q.dataset.applied = '0'; return; }
  q.dataset.applied = '1';
  fetch('/api/search', {method: 'POST', body: JSON.stringify({term: q.value})});
  show(q.value);
});
box.addEventListener('input', () => { send.disabled = !box.innerText.trim() || !current; });
send.onclick = async () => {
  const text = box.innerText.trim();
  if (!text || !current) return;
  await fetch('/api/send', {method: 'POST', body: JSON.stringify({to: current, text})});
  sent.push({to: current, text}); box.textContent = ''; send.disabled = true; draw();
};
// Enter in the message box sends too, as it does in a chat.
box.addEventListener('keydown', e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send.click(); } });
show('');
</script></body></html>""" % json.dumps(PEOPLE)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _send(self, body, kind="text/html; charset=utf-8", status=200):
        data = body.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", kind)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/":
            self._send(FEED)
        elif path.startswith("/messaging"):
            self._send(SHELL)
        elif path.startswith("/app"):
            self._send(APP)
        elif path == "/api/state":
            with LOCK:
                self._send(json.dumps(STATE), "application/json")
        else:
            self._send("not found", status=404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            body = json.loads(raw.decode("utf-8") or "{}")
        except ValueError:
            body = {}
        with LOCK:
            if self.path == "/api/send":
                STATE["sent"].append({"to": str(body.get("to")), "text": str(body.get("text"))})
            elif self.path == "/api/search":
                STATE["searches"].append(str(body.get("term")))
            elif self.path == "/api/reset":
                STATE["sent"].clear()
                STATE["searches"].clear()
        self._send("{}", "application/json")


def serve(port=8765):
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    server.serve_forever()


if __name__ == "__main__":
    serve(int(sys.argv[1]) if len(sys.argv) > 1 else 8765)
