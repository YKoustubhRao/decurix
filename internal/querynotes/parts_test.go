package querynotes

import "testing"

// A note opens with boilerplate; a card showed only that. The task and the
// agent's own words are what a person reads, for notes of either shape and
// for notes stored before they were sent separately.
func TestANoteIsReadForItsTaskAndMessage(t *testing.T) {
	query := "DECURIX QUERY REQUEST\n\nDesktop:\njonardhan\n\nDesktop ID:\njonardhan\n\nTask:\nchatgpt \"where is addid ababa\"\n\n" +
		"Problem:\nInstall curl using `sudo apt-get install curl` before running the command.\n\nQuery ID:\n9ac546b8\n\nThe agent is currently blocked."
	final := "DECURIX TASK COMPLETED\n\nDesktop:\njonardhan\n\nDesktop ID:\njonardhan\n\nTask:\ngoogle pizza\n\nStatus:\nCOMPLETED\n\n" +
		"Final reply:\nGoogle, for \"pizza\":\n\n1. Pizza - example.com\n\nCompleted at:\n2026-09-24"

	notes := []QueryNote{{Content: query}, {Content: final}, {Content: "a note in no known shape"}}

	if notes[0].Task() != `chatgpt "where is addid ababa"` || notes[0].Message() != "Install curl using `sudo apt-get install curl` before running the command." {
		t.Errorf("query note read as task %q, message %q", notes[0].Task(), notes[0].Message())
	}
	if notes[1].Task() != "google pizza" || notes[1].Message() != "Google, for \"pizza\":\n\n1. Pizza - example.com" {
		t.Errorf("final note read as task %q, message %q", notes[1].Task(), notes[1].Message())
	}
	if notes[2].Task() != "" || notes[2].Message() != "" {
		t.Errorf("an unknown note was given parts it does not have: %+v", notes[2])
	}
}
