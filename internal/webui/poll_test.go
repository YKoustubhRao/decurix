package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Decurix.poll, run for real in node with a fake clock and document (resource plan P1 #9): a tick while the last call
// is still in flight is skipped, a failed call doubles the interval (up to 8x) and a good one restores it, nothing is
// asked while the page is hidden, and the page seen again is asked at once. Skipped where there is no node.
func TestPollWaitsForItsCallAndBacksOffOnErrors(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "templates", "theme.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := strings.ReplaceAll(string(raw), "\r\n", "\n")
	start := strings.Index(page, "  function poll(fn, ms, opts){")
	end := strings.Index(page[start:], "\n  }\n")
	if start < 0 || end < 0 {
		t.Fatal("poll is not where it was")
	}
	poll := page[start : start+end+4]
	harness := `
const assert = require("node:assert/strict");
let timers = [], listeners = {};
let clock = 1000000;
Date.now = () => clock;
const document = { visibilityState: "visible", addEventListener(n, f){ listeners[n] = f; }, removeEventListener(n){ delete listeners[n]; } };
function setInterval(fn, ms){ timers.push({fn, ms}); return timers.length; }
function clearInterval(id){ if(id){ timers[id - 1] = null; } }
const live = () => timers.filter(Boolean);
const tick = () => live().forEach((t) => t.fn());
` + poll + `
(async () => {
  // In flight: the next ticks are skipped until the call settles.
  let calls = 0, finish;
  poll(() => { calls++; return new Promise((r) => { finish = r; }); }, 1000);
  tick(); tick(); tick();
  assert.equal(calls, 1, "ticks while a call is in flight must be skipped");
  finish(); await new Promise((r) => setImmediate(r));
  tick();
  assert.equal(calls, 2);

  // Errors: the interval doubles, up to 8x; a success restores it.
  timers = []; listeners = {};
  let fail = true;
  poll(() => fail ? Promise.reject(new Error("down")) : Promise.resolve(), 1000);
  const settle = () => new Promise((r) => setImmediate(r));
  for (const want of [2000, 4000, 8000, 8000]) { tick(); await settle(); assert.equal(live()[0].ms, want); }
  fail = false; tick(); await settle();
  assert.equal(live()[0].ms, 1000, "a success restores the interval");

  // A call that never settles is given up on after max(30 s, 4 intervals), and the poll goes on; its late answer
  // changes nothing.
  timers = []; listeners = {};
  let stuck = 0, lateFinish;
  poll(() => { stuck++; return stuck === 1 ? new Promise((r) => { lateFinish = r; }) : Promise.resolve(); }, 1000);
  tick(); assert.equal(stuck, 1);
  clock += 29999; tick(); assert.equal(stuck, 1, "a call in flight under 30 s is waited for");
  clock += 2; tick(); await settle();
  assert.equal(stuck, 2, "a call in flight past 30 s must be given up on");
  lateFinish(); await settle();
  tick(); await settle();
  assert.equal(stuck, 3, "the late answer of a call given up on must not stop the poll");

  // Hidden: nothing scheduled; seen again: asked at once.
  timers = []; listeners = {};
  let seen = 0;
  poll(() => { seen++; }, 1000);
  document.visibilityState = "hidden"; listeners.visibilitychange();
  assert.equal(live().length, 0, "a hidden page is not polled");
  document.visibilityState = "visible"; listeners.visibilitychange();
  assert.equal(seen, 1, "the page seen again is asked at once");
  assert.equal(live().length, 1);
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
`
	out, err := exec.Command(node, "-e", harness).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("%v\n%s", err, out)
	}
}
