package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// The extension already runs its own atomize -> act -> verify loop against
// extension_pipeline/ext_server.py (agent/browser-agent, listening on
// 127.0.0.1:8092 in this same container) and keeps itself listening for
// work on its own (extension/background.js's ensureListening, woken by a
// chrome.alarms watchdog so it survives the service worker being evicted
// and restarted) - this skill only ever has to hand it a task and read
// back the answer.
type BrowserSkill struct {
	// BaseURL defaults to ext_server.py's own address; overridable (and,
	// via EXT_SERVER_URL, settable without a rebuild) for a container laid
	// out differently, and by tests.
	BaseURL string
	// Timeout bounds one browsing task end to end. Zero means
	// defaultBrowserTimeout.
	Timeout time.Duration
	client  *http.Client
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
	if err := b.postJSON(rc.Ctx, "/requests", map[string]any{"kind": "instruction", "task": task}, &submitted); err != nil {
		return finish("Could not reach the browser extension's backend (ext_server.py on :8092): "+err.Error(), false, map[string]any{
			"error": err.Error(), "needsUserInput": true,
			"failureReasoning": "ext_server.py is not reachable - the desktop's browser extension may not have started yet",
		})
	}
	if submitted.ID == "" {
		return finish("The browser extension's backend accepted the task but returned no request id.", false,
			map[string]any{"error": "empty request id", "needsUserInput": true})
	}

	deadline := time.Now().Add(b.timeout())
	for time.Now().Before(deadline) {
		if err := rc.Ctx.Err(); err != nil {
			return finish("Stopped before the browser finished: "+err.Error(), false, map[string]any{"error": err.Error(), "fatal": true})
		}

		var status extRequestsStatus
		if err := b.getJSON(rc.Ctx, "/requests/status", &status); err == nil {
			for _, r := range status.Requests {
				if r.ID != submitted.ID {
					continue
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
		"needsUserInput":   true,
		"failureReasoning": "the browser extension took too long - it may be stuck, or nothing is listening for requests",
	})
}

// report turns the extension's own outcome into a skill Result. r.Result is
// never nil here - the caller only reaches this once the queue entry itself
// is "done", which background.js only sets alongside posting a result.
func (b *BrowserSkill) report(subtask Subtask, r extRequest) (Result, error) {
	meta := map[string]any{"userFacing": true}
	res := r.Result
	if res == nil {
		return Result{Subtask: subtask, SkillName: b.Name(), Output: "The browser finished with no result.", Verified: false, Meta: meta}, nil
	}
	if res.FinalURL != "" {
		meta["url"] = res.FinalURL
	}

	switch res.Status {
	case "done":
		if res.Answer != "" {
			return Result{Subtask: subtask, SkillName: b.Name(), Output: res.Answer, Verified: true, Meta: meta}, nil
		}
		var steps []string
		for _, s := range res.Steps {
			if s.Instruction != "" {
				steps = append(steps, s.Instruction)
			}
		}
		output := "Done in the browser."
		if len(steps) > 0 {
			output = "Done in the browser: " + strings.Join(steps, "; ")
		}
		return Result{Subtask: subtask, SkillName: b.Name(), Output: output, Verified: true, Meta: meta}, nil

	case "blocked":
		meta["needsUserInput"] = true
		meta["failureReasoning"] = "the page blocked the browser (a captcha or bot check): " + res.Error
		return Result{Subtask: subtask, SkillName: b.Name(), Output: "The browser was blocked: " + res.Error, Verified: false, Meta: meta}, nil

	case "paused", "stalled":
		reason := res.Error
		if reason == "" {
			reason = "the browser paused and needs a human to look"
		}
		meta["needsUserInput"] = true
		meta["failureReasoning"] = reason
		return Result{Subtask: subtask, SkillName: b.Name(), Output: "The browser paused: " + reason, Verified: false, Meta: meta}, nil

	case "timeout":
		meta["needsUserInput"] = true
		meta["failureReasoning"] = "the browser task's own time limit ran out"
		return Result{Subtask: subtask, SkillName: b.Name(), Output: "The browser task timed out.", Verified: false, Meta: meta}, nil

	default: // "error", or anything this version of the extension doesn't send
		errMsg := res.Error
		if errMsg == "" {
			errMsg = "unknown error"
		}
		meta["needsUserInput"] = true
		meta["failureReasoning"] = errMsg
		return Result{Subtask: subtask, SkillName: b.Name(), Output: "The browser could not finish: " + errMsg, Verified: false, Meta: meta}, nil
	}
}
