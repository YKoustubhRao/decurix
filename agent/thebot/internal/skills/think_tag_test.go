package skills

// A reasoning model's closing tag at the end of a command is taken off before the command runs (withoutThinkTag).

import "testing"

func TestACommandLosesATrailingThinkTag(t *testing.T) {
	cases := map[string]string{
		"COMMAND:\nls -la ~/work/</think>":                                   "ls -la ~/work/",
		"COMMAND:\noffice read ~/a.docx | grep -A2 \"TOTAL\"</think>  ":      "office read ~/a.docx | grep -A2 \"TOTAL\"",
		"COMMAND:\ngui click --see \"the icon at 50,250\"</think>\n</think>": "gui click --see \"the icon at 50,250\"",
		"COMMAND:\necho '</think> is a tag'":                                 "echo '</think> is a tag'",
	}
	for reply, want := range cases {
		if action, got := parseControl(reply); action != "command" || got != want {
			t.Errorf("parseControl(%q) = %q %q, want %q", reply, action, got, want)
		}
	}
}
