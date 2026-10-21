package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// BrowserSkill hands an interactive web task ("Open google.com. Search for
// this. Do that.") to the browser-agent Chrome extension loaded into this
// desktop's real, already-logged-in Chromium
// (agent/chromium-browser.init's --load-extension; see
// agent/BROWSER_INTEGRATION.md), instead of driving pages through the
// operator's generic coordinate/xdotool loop.
//
// The extension already runs its own atomize -> act loop against
// extension_pipeline/ext_server.py (agent/browser-agent, listening on
// 127.0.0.1:8092 in this same container) and listens for work on its own by
// default (extension/background.js's LISTENER, persisted across service-
// worker and browser restarts) - this skill only ever has to hand it a task
// and read back the answer.
type BrowserSkill struct {
	// BaseURL defaults to ext_server.py's own address; overridable (and,
	// via EXT_SERVER_URL, settable without a rebuild) for a container laid
	// out differently, and by tests.
	BaseURL string
	// Timeout bounds one browsing task end to end. Zero means
	// defaultBrowserTimeout.
	Timeout time.Duration
	// PickupTimeout is how long a request may stay unclaimed before the
	// skill concludes no extension is listening. Zero means
	// defaultPickupTimeout.
	PickupTimeout time.Duration
	client        *http.Client
}

func NewBrowser() *BrowserSkill { return &BrowserSkill{client: &http.Client{}} }

const (
	defaultExtServerURL = "http://127.0.0.1:8092"
	// defaultBrowserTimeout must stay above the extension's own per-request
	// cap (extension/background.js's runListenerLoop, 900s/15min as of this
	// writing - Devstral measured at ~90-180s per step there) so the
	// extension's own "timeout" outcome is what fires and gets reported,
	// not this skill giving up on the poll first and reporting a less
	// informative one of its own.
	defaultBrowserTimeout = 16 * time.Minute
	browserPollInterval   = 2 * time.Second
	browserHTTPTimeout    = 10 * time.Second
	// defaultPickupTimeout bounds how long a request may wait for the
	// extension to take it at all. The extension's listener asks for work
	// every few seconds, so a request still "pending" after this means none
	// is listening - measured live: the desktop's Chromium was started
	// without the extension, the request sat pending, and the task waited
	// out the whole 16-minute budget before saying anything.
	defaultPickupTimeout = 30 * time.Second
)

func (b *BrowserSkill) Name() string { return "browser" }
func (b *BrowserSkill) Description() string {
	return "Carries out a task in the real browser (opening a site, searching, clicking, filling forms) through the browser-agent Chrome extension."
}

func (b *BrowserSkill) Matches(subtask Subtask) bool { return subtask.Type == "browser" }

func (b *BrowserSkill) baseURL() string {
	if b.BaseURL != "" {
		return b.BaseURL
	}
	if v := os.Getenv("EXT_SERVER_URL"); v != "" {
		return v
	}
	return defaultExtServerURL
}

func (b *BrowserSkill) timeout() time.Duration {
	if b.Timeout > 0 {
		return b.Timeout
	}
	return defaultBrowserTimeout
}

// extStep is one step of the extension's own run, as ext_server.py's
// /requests/status echoes back what background.js posted to
// /requests/result.
type extStep struct {
	Instruction string `json:"instruction"`
	Status      string `json:"status"`
	Error       string `json:"error"`
}

// extResult is the outcome background.js posts once a request finishes -
// its own Status (done/error/blocked/timeout/paused/stalled), distinct
// from extRequest.Status below (the queue state: pending/running/done).
type extResult struct {
	Status      string    `json:"status"`
	Answer      string    `json:"answer"`
	Error       string    `json:"error"`
	Log         []string  `json:"log"`
	FinalURL    string    `json:"final_url"`
	CheckPassed *bool     `json:"check_passed"`
	ElapsedS    float64   `json:"elapsed_s"`
	Steps       []extStep `json:"steps"`
	// Approval is the action the extension's approval gate stopped before
	// (Status "needs_approval").
	Approval *browserCheckpoint `json:"approval"`
}

// browserCheckpoint is the action a browser run stopped before, waiting for a
// person's yes. On a yes the step runs again and the extension is told that
// one action is approved; it lets it through once.
type browserCheckpoint struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// extRequest is one entry of ext_server.py's /requests/status listing.
type extRequest struct {
	ID     string     `json:"id"`
	Status string     `json:"status"` // pending | running | done (queue state)
	Result *extResult `json:"result"`
}

type extRequestsStatus struct {
	Requests []extRequest `json:"requests"`
}

func (b *BrowserSkill) postJSON(ctx context.Context, path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, browserHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, b.baseURL()+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (b *BrowserSkill) getJSON(ctx context.Context, path string, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, browserHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, b.baseURL()+path, nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (b *BrowserSkill) pickupTimeout() time.Duration {
	if b.PickupTimeout > 0 {
		return b.PickupTimeout
	}
	return defaultPickupTimeout
}

func (b *BrowserSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	finish := func(output string, verified bool, meta map[string]any) (Result, error) {
		if meta == nil {
			meta = map[string]any{}
		}
		meta["userFacing"] = true
		return Result{Subtask: subtask, SkillName: b.Name(), Output: output, Verified: verified, Meta: meta}, nil
	}

	task := strings.TrimSpace(subtask.Step)
	rc.Logf("browser", "handing off to the browser extension: %s", task)

	var submitted struct {
		ID string `json:"id"`
	}
	// fatal on every path below that reaches no real browser observation at
	// all (never got a request id, or the poll itself never got an answer):
	// there is nothing here a single cleverer retry could fix, and the
	// scheduler's own failure-reasoning call (an extra model round-trip)
	// would just spend a minute concluding the same thing. A run that DID
	// observe something, even one that didn't finish the actual task, is
	// handled entirely differently below (see report()) - that is not a
	// failure to recover from, it is an answer.
	request := map[string]any{"kind": "instruction", "task": task}
	if cp, ok := rc.Resume.(*browserCheckpoint); ok && cp.Action != "" && rc.IsApproved(cp.Action) {
		request["approved"] = []string{cp.Action}
	}
	if err := b.postJSON(rc.Ctx, "/requests", request, &submitted); err != nil {
		return finish("Could not reach the browser extension's backend (ext_server.py on :8092): "+err.Error(), false, map[string]any{
			"error": err.Error(), "fatal": true, "needsUserInput": true,
			"failureReasoning": "ext_server.py is not reachable - the desktop's browser extension may not have started yet",
		})
	}
	if submitted.ID == "" {
		return finish("The browser extension's backend accepted the task but returned no request id.", false,
			map[string]any{"error": "empty request id", "fatal": true, "needsUserInput": true})
	}

	deadline := time.Now().Add(b.timeout())
	pickup := time.Now().Add(b.pickupTimeout())
	for time.Now().Before(deadline) {
		if err := rc.Ctx.Err(); err != nil {
			// TheBot's own job was cancelled (POST /api/jobs/{id}/cancel,
			// cmd/soul/main.go) - propagate that to the browser itself, or
			// it just keeps running orphaned, still acting on the real page
			// after this skill has stopped watching it. rc.Ctx is already
			// done, so the cancel call (and the short wait below for the
			// extension to actually notice and report) needs its own
			// context, never rc.Ctx.
			return b.cancelAndReport(subtask, submitted.ID, err)
		}

		var status extRequestsStatus
		if err := b.getJSON(rc.Ctx, "/requests/status", &status); err == nil {
			for _, r := range status.Requests {
				if r.ID != submitted.ID {
					continue
				}
				if r.Status == "pending" && time.Now().After(pickup) {
					// Nothing took it: no extension is listening. Withdraw
					// it (so a listener that turns up later does not act on
					// it out of the blue) and let the operator, which works
					// the screen with the gui command, do the task instead.
					_ = b.postJSON(context.Background(), "/requests/"+url.PathEscape(submitted.ID)+"/cancel", map[string]any{}, nil)
					rc.Logf("browser", "the browser extension is not running here - doing it on the screen instead")
					return finish("The browser extension did not pick the task up.", false, map[string]any{"reroute": "operator"})
				}
				if r.Status != "done" {
					break
				}
				return b.report(subtask, r)
			}
		}
		time.Sleep(browserPollInterval)
	}

	return finish(fmt.Sprintf("The browser did not finish within %s.", b.timeout()), false, map[string]any{
		"fatal": true, "needsUserInput": true,
		"failureReasoning": "the browser extension took too long - it may be stuck, or nothing is listening for requests",
	})
}

// cancelAndReport tells the extension to stop a request TheBot itself has
// already given up waiting on (rc.Ctx is done), then gives it a short
// window to actually notice, stop, and post its real result - so the
// answer, if one comes back in time, is a genuine compiled account of
// whatever the browser saw before it was cancelled (see report()), not a
// bare "stopped" with nothing in it. Uses its own background context
// throughout: rc.Ctx is already cancelled and cannot be used to make a new
// request at all, let alone wait on one.
const cancelGraceWindow = 8 * time.Second

func (b *BrowserSkill) cancelAndReport(subtask Subtask, requestID string, causeErr error) (Result, error) {
	meta := map[string]any{"userFacing": true, "error": causeErr.Error(), "fatal": true}
	if requestID == "" {
		return Result{Subtask: subtask, SkillName: b.Name(), Output: "Stopped before the browser started: " + causeErr.Error(), Verified: false, Meta: meta}, nil
	}

	bg := context.Background()
	_ = b.postJSON(bg, "/requests/"+url.PathEscape(requestID)+"/cancel", map[string]any{}, nil)

	deadline := time.Now().Add(cancelGraceWindow)
	for time.Now().Before(deadline) {
		var status extRequestsStatus
		if err := b.getJSON(bg, "/requests/status", &status); err == nil {
			for _, r := range status.Requests {
				if r.ID == requestID && r.Status == "done" {
					return b.report(subtask, r)
				}
			}
		}
		time.Sleep(browserPollInterval)
	}

	return Result{Subtask: subtask, SkillName: b.Name(), Output: "Cancelled: " + causeErr.Error() + " (the browser had not confirmed stopping yet).", Verified: false, Meta: meta}, nil
}

// report turns the extension's own outcome into a skill Result. r.Result is
// never nil here - the caller only reaches this once the queue entry itself
// is "done", which background.js only sets alongside posting a result.
//
// Anything the browser actually did - even a run that never finished the
// task - is reported as a verified answer, never as a failure to recover
// from: a captcha, a stalled step or a run that used up its own time budget
// almost never goes differently on a second try, and the scheduler's retry/
// decompose/ask-user machinery exists for failures a different approach
// could fix, not this. What it actually saw along the way (compiled from
// the extension's own step-by-step log) is real information TheBot should
// be able to answer follow-up questions about regardless of whether the
// original task was fully completed.
func (b *BrowserSkill) report(subtask Subtask, r extRequest) (Result, error) {
	meta := map[string]any{"userFacing": true}
	res := r.Result
	if res == nil {
		meta["fatal"] = true
		return Result{Subtask: subtask, SkillName: b.Name(), Output: "The browser finished with no result.", Verified: false, Meta: meta}, nil
	}
	if res.FinalURL != "" {
		meta["url"] = res.FinalURL
	}

	if res.Status == "needs_approval" && res.Approval != nil && res.Approval.Action != "" {
		return NeedsApproval(subtask, b.Name(), res.Approval.Action, res.Approval.Reason, res.Approval), nil
	}

	if res.Status == "done" && res.Answer != "" {
		return Result{Subtask: subtask, SkillName: b.Name(), Output: res.Answer, Verified: true, Meta: meta}, nil
	}
	return Result{Subtask: subtask, SkillName: b.Name(), Output: summarizeBrowserRun(res), Verified: true, Meta: meta}, nil
}

// statusPrefixes are the one-line framing summarizeBrowserRun opens with,
// before the compiled log - what kind of outcome this was, in plain words.
var statusPrefixes = map[string]string{
	"done":      "Finished browsing, but without a specific answer to report.",
	"blocked":   "Blocked partway through by the page itself (a captcha or bot check).",
	"paused":    "Paused partway through and did not finish the task.",
	"stalled":   "Stopped making progress and did not finish the task.",
	"timeout":   "Ran out of time before finishing the task.",
	"cancelled": "Stopped early because the request was cancelled.",
}

// summarizeBrowserRun compiles what the browser actually saw and did into
// one narrative, from the extension's own step-by-step log (background.js's
// `log`, the same lines its popup shows live) - real observations, not a
// guess at what probably happened.
func summarizeBrowserRun(res *extResult) string {
	var b strings.Builder

	prefix, ok := statusPrefixes[res.Status]
	if !ok {
		prefix = "Hit an error before finishing the task."
	}
	b.WriteString(prefix)
	if res.Error != "" {
		fmt.Fprintf(&b, " Reason: %s", res.Error)
	}
	b.WriteString("\n\nWhat it actually saw and did:\n")

	switch {
	case len(res.Log) > 0:
		b.WriteString(strings.Join(res.Log, "\n"))
	case len(res.Steps) > 0:
		var lines []string
		for _, s := range res.Steps {
			if s.Instruction == "" {
				continue
			}
			line := "- " + s.Instruction
			if s.Status != "" {
				line += " (" + s.Status + ")"
			}
			lines = append(lines, line)
		}
		b.WriteString(strings.Join(lines, "\n"))
	default:
		b.WriteString("(nothing was recorded)")
	}

	if res.FinalURL != "" {
		fmt.Fprintf(&b, "\n\nLast page: %s", res.FinalURL)
	}
	return strings.TrimSpace(b.String())
}
