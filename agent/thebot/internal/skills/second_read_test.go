package skills

import (
	"strings"
	"testing"

	"thebot/internal/llm"
)

// Found live (2026-10-23): a phone photo's two lines were written into a workbook as 8,750 and 5,000 for 2,750 and
// 6,000. Values only a picture showed are set against a second, independent read that never sees them.
func TestValuesOnlyAPictureShowedAreSetAgainstASecondRead(t *testing.T) {
	st := &operatorState{Hosted: true, Vision: true, UntracedWrites: []string{"5000", "2750"},
		Messages: []llm.Message{{Role: "user", Content: "office read IMG_0932.jpg" + pagesNote,
			Images: [][]byte{[]byte(pngHead + "photo")}, ImageLabels: []string{"IMG_0932.jpg"}}}}
	chat := &scriptedChat{replies: []string{"Sona masoori rice 1006 50 KGS 55.00 5% 2,750.00\nMango pickle 2001 40 NOS 150.00 12% 6,000.00"}}
	issues := (&OperatorSkill{}).secondRead(weighRC(chat), st)
	if len(issues) != 1 || !strings.Contains(issues[0].What, "5000") || issues[0].Settle != "recheck" {
		t.Fatalf("issues = %+v", issues)
	}
	sent := chat.calls[0][1]
	if strings.Contains(sent.Content, "5000") || strings.Contains(sent.Content, "2750") || len(sent.Images) != 1 {
		t.Errorf("the second read was shown what was written, or not the picture: %q (%d pictures)", sent.Content, len(sent.Images))
	}
	if !st.unitHolds("2750") {
		t.Error("a value both reads agree on is not traced")
	}
	if again := (&OperatorSkill{}).secondRead(weighRC(&scriptedChat{}), st); again != nil {
		t.Errorf("values already set against a second read were read again: %+v", again)
	}
	st.Vision = false
	st.UntracedWrites = append(st.UntracedWrites, "9999")
	if got := (&OperatorSkill{}).secondRead(weighRC(&scriptedChat{}), st); got != nil {
		t.Error("a brain that cannot see was asked to read pictures")
	}
}
