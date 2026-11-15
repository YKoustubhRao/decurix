// typed_test.go checks typed handoff results: the contract subset is read and refused as it should be, a verified
// result is held to it with one retry turn that re-sends the contract, an empty result fails, and an interrupted
// receiver hands back its last partial result marked interrupted.
package handoff

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const contract = `{"type":"object","required":["total","items"],"properties":{"total":{"type":"number"},` +
	`"items":{"type":"array","items":{"type":"string"}},"note":{"type":"string"}}}`

func TestTheContractSubset(t *testing.T) {
	s, err := ParseSchema(json.RawMessage(contract))
	if err != nil || s == nil {
		t.Fatalf("parse: %v", err)
	}
	if errs := s.Validate(json.RawMessage(`{"total": 12, "items": ["a", "b"]}`)); len(errs) != 0 {
		t.Fatalf("a matching result failed: %v", errs)
	}
	bad := map[string]string{
		`{"total": "12", "items": []}`: `key "total" is string`,
		`{"items": ["a"]}`:             `"total" is missing`,
		`{"total": 1, "items": [1]}`:   `item 0 of key "items"`,
		`{}`:                           "empty",
		`[1, 2]`:                       "not a JSON object",
	}
	for data, want := range bad {
		errs := s.Validate(json.RawMessage(data))
		if !strings.Contains(strings.Join(errs, "; "), want) {
			t.Errorf("%s: %v, want %q", data, errs, want)
		}
	}
	for _, refused := range []string{`{"type":"array"}`, `{"type":"object","required":["x"]}`,
		`{"type":"object","properties":{"x":{"type":"date"}}}`, `{"type":"object","patternProperties":{}}`, `"text"`} {
		if _, err := ParseSchema(json.RawMessage(refused)); err == nil {
			t.Errorf("%s was taken", refused)
		}
	}
	if s, err := ParseSchema(nil); s != nil || err != nil {
		t.Fatalf("no contract: %v %v", s, err)
	}
	data, ok := ExtractResult("Done.\n" + ResultMarker + "\n```json\n{\"total\": 3, \"items\": [\"x\"]}\n```\n")
	if !ok || string(data) != `{"total": 3, "items": ["x"]}` {
		t.Fatalf("extract: %q %v", data, ok)
	}
	if _, ok := ExtractResult("no marker here {\"total\": 3}"); ok {
		t.Fatal("a result was read with no marker")
	}
}

// delegateTyped sends a typed handoff from a to b and returns it running.
func delegateTyped(t *testing.T, r *rig) Handoff {
	t.Helper()
	h, err := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", FromTask: "t-a", ToWorker: "b",
		Request: "Total the open invoices.", OutputSchema: json.RawMessage(contract)})
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != StatusRunning || !strings.Contains(r.submit.calls[0].content, ResultMarker) ||
		!strings.Contains(r.submit.calls[0].content, `"required":["total","items"]`) {
		t.Fatalf("sent %+v: %q", h, r.submit.calls[0].content)
	}
	return h
}

func TestATypedResultThatMatchesIsDone(t *testing.T) {
	r := newRig(t)
	h := delegateTyped(t, r)
	r.tasks.set(h.TaskID, TaskOutcome{Status: "done", Checked: yes(),
		Report: "Totalled them.\n" + ResultMarker + "\n{\"total\": 1250.5, \"items\": [\"INV-1\", \"INV-2\"]}"})
	got, err := r.svc.Get(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || !got.Result.Verified || string(got.Result.Data) != `{"total": 1250.5, "items": ["INV-1", "INV-2"]}` {
		t.Fatalf("got %+v", got)
	}
	if len(got.Result.SchemaErrors) != 0 || len(got.Result.Tasks) != 1 || got.Result.Interrupted {
		t.Fatalf("result %+v", got.Result)
	}
}

// A verified report whose result does not match is sent the contract once more; a match then is done, a second
// mismatch is not done; never a third turn.
func TestAMismatchGetsOneRetryTurn(t *testing.T) {
	for _, second := range []string{`{"total": 9, "items": ["a"]}`, `{"total": "nine"}`} {
		r := newRig(t)
		h := delegateTyped(t, r)
		first := h.TaskID
		r.tasks.set(first, TaskOutcome{Status: "done", Checked: yes(), Report: "Totalled.\n" + ResultMarker + `{"total": "nine"}`})
		got, _ := r.svc.Get(context.Background(), h.ID)
		if got.Status != StatusRunning || got.SchemaRetries != 1 || got.TaskID == first || len(r.submit.calls) != 2 {
			t.Fatalf("no retry turn: %+v (%d sent)", got, len(r.submit.calls))
		}
		retry := r.submit.calls[1].content
		if !strings.Contains(retry, "Do not do the work again") || !strings.Contains(retry, `key "total" is string`) ||
			!strings.Contains(retry, `"required":["total","items"]`) {
			t.Fatalf("retry turn: %q", retry)
		}
		if strings.Join(got.Result.Tasks, ",") != first+","+got.TaskID || len(got.Result.SchemaErrors) == 0 {
			t.Fatalf("result while retrying: %+v", got.Result)
		}
		r.tasks.set(got.TaskID, TaskOutcome{Status: "done", Checked: yes(), Report: ResultMarker + second})
		final, _ := r.svc.Get(context.Background(), h.ID)
		matched := strings.Contains(second, "items")
		if matched && (final.Status != StatusDone || !final.Result.Verified || string(final.Result.Data) != second) {
			t.Fatalf("a matching retry: %+v", final)
		}
		if !matched && (final.Status != StatusNotDone || final.Result.Verified || !strings.Contains(final.Reason, "after one retry")) {
			t.Fatalf("a second mismatch: %+v", final)
		}
		if len(r.submit.calls) != 2 {
			t.Fatalf("%d turns sent", len(r.submit.calls))
		}
	}
}

// A retry turn the receiving worker is too busy for waits, and is sent once it is free.
func TestARetryTurnWaitsForABusyWorker(t *testing.T) {
	r := newRig(t)
	h := delegateTyped(t, r)
	r.tasks.set(h.TaskID, TaskOutcome{Status: "done", Checked: yes(), Report: "no result object"})
	r.submit.busy["b"] = true
	got, _ := r.svc.Get(context.Background(), h.ID)
	if got.Status != StatusRunning || got.SchemaRetries != 0 || !strings.Contains(got.Reason, "waiting for it to be free") {
		t.Fatalf("busy: %+v", got)
	}
	r.submit.busy["b"] = false
	got, _ = r.svc.Get(context.Background(), h.ID)
	if got.SchemaRetries != 1 || got.TaskID == h.TaskID {
		t.Fatalf("not sent once free: %+v", got)
	}
}

// An unchecked report is not done whatever its result object says: no retry is spent on it.
func TestAnUncheckedTypedResultIsNotDone(t *testing.T) {
	r := newRig(t)
	h := delegateTyped(t, r)
	r.tasks.set(h.TaskID, TaskOutcome{Status: "done", Checked: no(), Report: ResultMarker + `{"total": 1, "items": []}`})
	got, _ := r.svc.Get(context.Background(), h.ID)
	if got.Status != StatusNotDone || got.Result.Verified || len(r.submit.calls) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// An empty report is a failure even when a check passed.
func TestAnEmptyResultIsAFailure(t *testing.T) {
	r := newRig(t)
	h, err := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "Do it."})
	if err != nil {
		t.Fatal(err)
	}
	r.tasks.set(h.TaskID, TaskOutcome{Status: "done", Checked: yes(), Report: "  \n"})
	got, _ := r.svc.Get(context.Background(), h.ID)
	if got.Status != StatusFailed || got.Result.Verified {
		t.Fatalf("got %+v", got)
	}
}

// A receiver stopped part way hands back its last partial result, marked interrupted and never verified.
func TestAnInterruptedReceiverReturnsItsPartialResult(t *testing.T) {
	r := newRig(t)
	h := delegateTyped(t, r)
	r.tasks.set(h.TaskID, TaskOutcome{Status: "abandoned", Partial: []string{"opened the ledger", "read 14 invoices"}})
	got, _ := r.svc.Get(context.Background(), h.ID)
	if got.Status != StatusFailed || !got.Result.Interrupted || got.Result.Verified ||
		!strings.Contains(got.Result.Report, "read 14 invoices") || !strings.Contains(got.Reason, "interrupted") {
		t.Fatalf("got %+v", got)
	}

	r2 := newRig(t)
	h2 := delegateTyped(t, r2)
	r2.tasks.set(h2.TaskID, TaskOutcome{Status: "query", Report: "Halfway.\n" + ResultMarker + `{"total": 4}`})
	got2, _ := r2.svc.Get(context.Background(), h2.ID)
	if !got2.Result.Interrupted || string(got2.Result.Data) != `{"total": 4}` || got2.Status != StatusFailed {
		t.Fatalf("partial data: %+v", got2)
	}
}

// A contract outside the subset is refused when the handoff is made, through the route too.
func TestABadContractIsRefusedAtTheDoor(t *testing.T) {
	r := newRig(t)
	_, err := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "x",
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"when":{"type":"date"}}}`)})
	var in *InputError
	if err == nil || !asInput(err, &in) {
		t.Fatalf("err %v", err)
	}
	stored, _ := r.svc.Store.List("", 10)
	if len(stored) != 0 {
		t.Fatal("a refused handoff was stored")
	}
}

func asInput(err error, target **InputError) bool {
	e, ok := err.(*InputError)
	if ok {
		*target = e
	}
	return ok
}
