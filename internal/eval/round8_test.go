package eval

import (
	"context"
	"strings"
	"testing"
)

// The oracle runs inside its case's task, as a worker's tools do once a request has arrived: before any step, the
// case's task is made the worker's current one with the case's request, so gui can declare a record, the guards read
// the request a person sent, and the steps record under the task (core-v2 calibration, round 8).
func TestTheOracleRunsInsideItsCasesTask(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"}, Arms: []Arm{{Name: "oracle", Kind: ArmOracle}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	sb.mu.Lock()
	execs := append([][]string{}, sb.execs...)
	sb.mu.Unlock()
	first := -1
	for i, argv := range execs {
		if argv[0] == "/usr/bin/python3" && strings.Contains(strings.Join(argv, " "), "task_context.start") {
			first = i
			if argv[len(argv)-2] != rows[0].TaskID || argv[len(argv)-1] != "Total in.txt into out.txt." {
				t.Fatalf("the context names %q", argv[len(argv)-2:])
			}
			break
		}
	}
	if first < 0 {
		t.Fatalf("no task context before the oracle's steps: %q", execs)
	}
	for _, argv := range execs[:first] {
		if argv[0] == "/bin/bash" {
			t.Fatalf("an oracle step ran before its task context: %q", execs)
		}
	}

	// A context that cannot be set is the runner's failure (infrastructure), and no step runs.
	s2, sb2, _, _, _ := newService(t, diligent)
	sb2.execExit = func(argv []string) (int, []byte) {
		if argv[0] == "/usr/bin/python3" {
			return 1, nil
		}
		return 0, nil
	}
	run, err = s2.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"}, Arms: []Arm{{Name: "oracle", Kind: ArmOracle}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s2, run.ID)
	rows, _ = s2.Store.Rows(run.ID)
	if len(rows) != 1 || !rows[0].Infra || rows[0].Passed {
		t.Fatalf("rows: %+v", rows)
	}
	sb2.mu.Lock()
	defer sb2.mu.Unlock()
	for _, argv := range sb2.execs {
		if argv[0] == "/bin/bash" {
			t.Fatalf("a step ran without its task: %q", sb2.execs)
		}
	}
}
