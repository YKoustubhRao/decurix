package skills

import (
	"fmt"
	"strings"

	"thebot/internal/llm"
)

// ============================================================
// A second, independent read of the pictures (docs/harness-v2.md: "two independent reads must agree"). A value the
// work typed or wrote that no text holds - read off a scan or a photo, the one source a model reads by eye - is set
// against a fresh transcription of the same pictures, made by a call that is never shown what the work wrote. A value
// the transcription holds becomes traced; one it does not is a point to check against the picture. Found live
// (2026-10-23): a phone photo's two lines were written into a workbook as 8,750 and 5,000 for 2,750 and 6,000.
// ============================================================

const transcribeSystem = `Transcribe the document pages shown, exactly as printed: every line of text and every row of a table, one per line, with all its words and numbers in order (numbers digit for digit, as written). No summary, no corrections, no comments - only what the pages show.`

const (
	maxSecondReadPictures = 4
	maxSecondReadFindings = 6
)

// secondRead is the values typed or written that only a picture showed which an independent transcription of the
// document pictures does not hold, as recheck points; nil when there is nothing to check (no such value, no pictures,
// no hosted brain) or the read cannot be made. Values the transcription holds are traced from then on.
func (o *OperatorSkill) secondRead(rc RunContext, st *operatorState) []reviewIssue {
	if rc.Provider == nil || !st.Hosted || !st.Vision {
		return nil
	}
	var values []string
	for _, v := range append(append([]string(nil), st.PictureOnly...), st.UntracedWrites...) {
		if !contains(values, v) && !st.SecondReadChecked[v] {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return nil
	}
	imgs, labels := st.documentPicturesKept(maxSecondReadPictures)
	if len(imgs) == 0 {
		return nil
	}
	msg := llm.Message{Role: "user", Content: "Transcribe these pages.", Images: imgs, ImageLabels: labels}
	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{{Role: "system", Content: transcribeSystem}, msg},
		llm.Options{TimeoutMs: verifyTimeoutMs, Temperature: 0, Purpose: llm.PurposeSecondRead})
	if err != nil || strings.TrimSpace(resp.Text) == "" {
		rc.Logf("operator", "could not read the pictures a second time: %v", err)
		return nil
	}
	read := evidenceUnit{Kind: "ocr", Source: "a second read of " + strings.Join(labels, ", "), Text: resp.Text}
	if st.SecondReadChecked == nil {
		st.SecondReadChecked = map[string]bool{}
	}
	var out []reviewIssue
	held := 0
	for _, v := range values {
		st.SecondReadChecked[v] = true
		if read.holds(v) {
			held++
			continue
		}
		if len(out) < maxSecondReadFindings {
			out = append(out, reviewIssue{
				What: fmt.Sprintf("%s was typed or written from a picture, and a second, independent read of the pictures "+
					"does not show it", v),
				Why:    "check that value against the page itself (zoom in, or read the file again) before it is relied on",
				Settle: "recheck",
			})
		}
	}
	// What the second read holds is a source from now on: the values both reads agree on are traced.
	st.addUnit(read.Kind, read.Source, read.Text)
	st.noteSeen(read.Text)
	rc.Logf("operator", "a second read of the pictures: %d of %d value(s) only a picture showed agree, %d to check",
		held, len(values), len(out))
	return out
}
