package skills

import "testing"

// TestACommandIsCutBackWhereTheModelsCallFramingSpilledIntoIt covers the
// replies seen live on a hosted model, and commands that must be left alone.
func TestACommandIsCutBackWhereTheModelsCallFramingSpilledIntoIt(t *testing.T) {
	cases := []struct{ reply, want string }{
		{"COMMAND:\ngui key enterดลองใช้ฟรี to=gui.key  彩神争霸安卓 code=0 stdout=\"Pressed enter.\\nWhat changed: active window is now \"New\"",
			"gui key enter"},
		{"COMMAND:\ngui key escape иҟоуп to=gui.key code=0 stdout=\"Pressed escape.\\nWhat changed: 114 element(s) disappeared",
			"gui key escape"},
		{"COMMAND:\ngui key Tab【อ่านข้อความเต็มanalysis to=final code_block format=plaintext  银航 to=final code_block format=plaintext छैन",
			"gui key Tab"},
		{"COMMAND:\ngui click e12 ابراهيم to=gui.click code=0 stdout=\"Clicked\"\nmore of the spill on the next line",
			"gui click e12"},

		{"COMMAND:\ngui type e8358 \"65000.00\" บาคาร่assistant to=all:final code", "gui type e8358 \"65000.00\""},

		{"COMMAND:\ngui double-click 69,369 /*<<<analysis to=commentary code ＿色less code block not needed here?", "gui double-click 69,369"},

		// Nothing leaked: every one of these is the command as written.
		{"COMMAND:\nrsync -a ./final to=backup:/srv/final", "rsync -a ./final to=backup:/srv/final"},
		{"COMMAND:\ngui type e7 \"₹15,000.00\"", "gui type e7 \"₹15,000.00\""},
		{"COMMAND:\ngui type e7 \"नमस्ते to=you\"", "gui type e7 \"नमस्ते to=you\""},
		{"COMMAND:\ncurl -s 'https://example.com/?to=a.b&code=0'", "curl -s 'https://example.com/?to=a.b&code=0'"},
		{"COMMAND:\npython3 - <<'EOF'\nprint('to=x code=0')\nEOF", "python3 - <<'EOF'\nprint('to=x code=0')\nEOF"},
	}

	for _, c := range cases {
		action, got := parseControl(c.reply)
		if action != "command" {
			t.Fatalf("%q: action %q, want command", c.reply, action)
		}
		if got != c.want {
			t.Errorf("parseControl(%q)\n  got  %q\n  want %q", c.reply, got, c.want)
		}
	}
}
