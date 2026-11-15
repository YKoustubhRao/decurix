package llm

// runaway_test.go: the runaway stream watch cuts loops and leaves output that is meant to repeat alone.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestALoopIsSeenAndWhatCameBeforeItKept(t *testing.T) {
	before := "I read the ledger and found the totals.\n"
	for name, text := range map[string]string{
		"one line over and over": before + strings.Repeat("I will now run the command to read the file again and check it.\n", 80),
		"a loop inside one line": before + strings.Repeat("and then the same fragment of words comes round once more again, ", 60),
		"a long unit, no lines":  before + strings.Repeat(paragraph(0), 25),
	} {
		hit, keep := runawayRepetition(text)
		if !hit {
			t.Errorf("%s: not seen as a loop", name)
			continue
		}
		if kept := text[:keep]; kept != before {
			t.Errorf("%s: kept %q, want what came before the loop", name, truncateForError(kept))
		}
	}
}

func TestOutputMeantToRepeatIsNotALoop(t *testing.T) {
	var csv, paths, list strings.Builder
	csv.WriteString("date,description,account,amount\n")
	for i := 1; i <= 30; i++ {
		// Rows that share a start longer than the window: the window scan alone would trip on them.
		fmt.Fprintf(&csv, "2026-11-12,Payment received into the operating account from the customer,ACC-%04d,%d.%02d\n", i, 1000+i*37, i)
	}
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&paths, "/home/worker/Documents/statements/2026/november/scanned/statement_page_%03d.pdf\n", i)
	}
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&list, "| %2d | Item number %d of the order, quantity %d, checked against the delivery note | ok |\n", i, i, i*3)
	}
	if hit, _ := repetitionDominated(csv.String()); !hit {
		t.Fatal("the table rows should trip the window scan: it is the line check that must tell them from a loop")
	}
	var prose strings.Builder
	for i := 0; prose.Len() < 6000; i++ {
		prose.WriteString(paragraph(i))
	}
	for name, text := range map[string]string{
		"30 rows of a table": csv.String(), "40 similar paths": paths.String(), "a markdown table": list.String(),
		"plain prose": prose.String(), "short repeats": strings.Repeat("ok\n", 100),
	} {
		if hit, _ := runawayRepetition(text); hit {
			t.Errorf("%s was taken for a loop", name)
		}
		// Nor in the stream, at any check.
		var w runawayWatch
		for i := 1; i <= len(text); i++ {
			if hit, _ := w.check(text[:i]); hit {
				t.Errorf("%s was cut in the stream at %d characters", name, i)
				break
			}
		}
	}
}

// paragraph is a few hundred characters of distinct sentences, numbered by i.
func paragraph(i int) string {
	return fmt.Sprintf("Section %d. The statement lists the receipts of the month in date order, each with its "+
		"reference and amount. Totals are carried to the summary at the end, where the closing balance is compared with "+
		"the bank's own figure for the same day. Any difference is listed with its cause and the entry that settles it. ", i)
}

func TestTheWatchChecksAtDoublingGapsReadingOnlyTheTail(t *testing.T) {
	var w runawayWatch
	var checks []int
	text := strings.Repeat("x", 40000)
	for n := 1; n <= len(text); n++ {
		before := w.next
		w.check(text[:n])
		if before != 0 && w.next != before {
			checks = append(checks, n)
		}
	}
	want := []int{1500, 3000, 6000, 12000, 18000, 24000, 30000, 36000}
	if fmt.Sprint(checks) != fmt.Sprint(want) {
		t.Fatalf("checked at %v, want %v", checks, want)
	}
}

// loopingStream is an Ollama stream that never ends: the same fragment chunk after chunk.
type loopingStream struct {
	sent, max int
	pending   []byte
}

func (s *loopingStream) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		if s.sent >= s.max {
			return 0, io.EOF
		}
		s.sent++
		content := "Checking the totals again now. "
		if s.sent == 1 {
			content = "The totals are:\n"
		}
		line, _ := json.Marshal(map[string]any{"message": map[string]any{"content": content}})
		s.pending = append(line, '\n')
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func TestAnOllamaStreamThatLoopsIsCutAndReturnedAsCutOff(t *testing.T) {
	stream := &loopingStream{max: 100000}
	res, err := readOllamaStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.FinishReason != FinishRepetition {
		t.Fatalf("result %+v: want a cut-off reply, finish reason %q", res, FinishRepetition)
	}
	if res.Text != "The totals are:" {
		t.Errorf("text %q: want what came before the loop, without the loop", truncateForError(res.Text))
	}
	if stream.sent > 200 {
		t.Errorf("read %d chunks: the stream was not stopped early", stream.sent)
	}
}
