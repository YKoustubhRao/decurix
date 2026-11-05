package main

import (
	"encoding/json"
	"strings"
	"testing"

	"thebot/internal/skills"
)

// F-S3 (defence in depth): what a gui type types, unflagged, never leaves soul in the job log, the stream or the
// snapshot (its plan, results, commands and answer); the target and the rest of the line stay.
func TestTheJobNeverShowsWhatAGuiTypeTyped(t *testing.T) {
	const value = "quince-orchard-77"
	job := newJobRecord("j1", "type the code into e3")
	ch, _ := job.subscribe()
	job.appendLog("skill:operator -> $ gui type e3 '" + value + "'")
	streamed := <-ch
	job.Results = []skills.Result{{SkillName: "operator", Output: "Ran gui type e3 \"" + value + "\" and it is in the field",
		Meta: map[string]any{"commands": []string{"gui observe", "gui type e3 '" + value + "'"},
			"worked": []string{"gui type e3 '" + value + "'"}}}}
	job.FinalAnswer = "Done: `gui type e3 '" + value + "'` worked."
	snap, err := json.Marshal(job.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"stream": streamed, "log": strings.Join(job.Log, "\n"), "snapshot": string(snap)} {
		if strings.Contains(text, value) {
			t.Errorf("the %s shows the typed value: %s", where, text)
		}
		if !strings.Contains(text, "gui type e3 "+skills.TypedTextMark) {
			t.Errorf("the %s does not show the masked command: %s", where, text)
		}
	}
}
