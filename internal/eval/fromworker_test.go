package eval

// Tests of a run started from a taught worker (MatrixRequest.FromWorker, docs/evals.md "Teaching once"): every case
// starts from that worker's state, the arms without their own @rev run its agent tree, and the run, its table and its
// run records name what was taught.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// fakeWorkers is a taught worker's desktop: its agent tree and its state, or an error for either.
type fakeWorkers struct {
	tree, snapshot []byte
	failTree       bool
	failSnapshot   bool
	asked          []string
}

func (w *fakeWorkers) PracticeAgentTree(_ context.Context, id string) ([]byte, string, error) {
	w.asked = append(w.asked, "tree:"+id)
	if w.failTree {
		return nil, "", errors.New("no such desktop: /opt/decurix_agent")
	}
	return w.tree, "treesha0123456789abcdef", nil
}

func (w *fakeWorkers) PracticeSnapshot(_ context.Context, id string) ([]byte, string, error) {
	w.asked = append(w.asked, "snapshot:"+id)
	if w.failSnapshot {
		return nil, "", errors.New("permission denied")
	}
	return w.snapshot, "snapsha0123456789abcdef", nil
}

// taughtSnapshot is a worker's state with one skill in it.
func taughtSnapshot(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte(`{"name": "enter-a-bill", "status": "ready"}`)
	_ = tw.WriteHeader(&tar.Header{Name: "skills/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "skills/enter-a-bill.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	return buf.Bytes()
}

// snapSandbox keeps the snapshot every reset restored.
type snapSandbox struct {
	*fakeSandbox
	mu    sync.Mutex
	snaps [][]byte
}

func (s *snapSandbox) ResetPractice(ctx context.Context, id string, snapshot []byte) error {
	s.mu.Lock()
	s.snaps = append(s.snaps, append([]byte{}, snapshot...))
	s.mu.Unlock()
	return s.fakeSandbox.ResetPractice(ctx, id, snapshot)
}

func TestARunFromAWorkerStartsEveryCaseFromItsState(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	snap := &snapSandbox{fakeSandbox: sb}
	s.Sandbox = snap
	workerTree, ownTree := agentTree(t), agentTree(t)
	ownTree = append([]byte{}, ownTree...)
	w := &fakeWorkers{tree: workerTree, snapshot: taughtSnapshot(t)}
	s.Workers = w
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 2, FromWorker: " desk-7 ",
		Arms: []Arm{{Name: "default+expert", Kind: ArmModel, Expert: true},
			{Name: "default+expert@abc123", Kind: ArmModel, Expert: true, AgentTree: ownTree, AgentTreeRev: "abc123"}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	if run.Status != RunDone {
		t.Fatalf("run: %+v", run)
	}
	// The worker was read once, for the whole run.
	if strings.Join(w.asked, ",") != "tree:desk-7,snapshot:desk-7" {
		t.Fatalf("worker reads: %v", w.asked)
	}
	// Both desktops were made with the worker's state; the arm without a rev ran the worker's tree and is no legacy
	// gate, the @rev arm kept its own tree (and its legacy gate).
	if len(sb.specs) != 2 {
		t.Fatalf("%d specs", len(sb.specs))
	}
	for i, spec := range sb.specs {
		if !bytes.Equal(spec.Snapshot, w.snapshot) {
			t.Fatalf("spec %d made with another snapshot", i)
		}
	}
	if !bytes.Equal(sb.specs[0].AgentTree, workerTree) || sb.specs[0].LegacyGate {
		t.Fatalf("the arm without a rev: tree from the worker %v, legacy gate %v", bytes.Equal(sb.specs[0].AgentTree, workerTree), sb.specs[0].LegacyGate)
	}
	if !bytes.Equal(sb.specs[1].AgentTree, ownTree) || !sb.specs[1].LegacyGate {
		t.Fatalf("the @rev arm: own tree %v, legacy gate %v", bytes.Equal(sb.specs[1].AgentTree, ownTree), sb.specs[1].LegacyGate)
	}
	// Every reset, between cases and between repeats, restored the worker's state, never an empty one.
	if len(snap.snaps) != 10 {
		t.Fatalf("%d resets, want 10 (every case but each arm's first)", len(snap.snaps))
	}
	for i, b := range snap.snaps {
		if !bytes.Equal(b, w.snapshot) {
			t.Fatalf("reset %d restored another snapshot", i)
		}
	}
	// The run keeps what it started from, stored and read back.
	if run.FromWorker != "desk-7" || run.SnapshotSHA != "snapsha0123456789abcdef" || run.WorkerTreeSHA != "treesha0123456789abcdef" {
		t.Fatalf("run pins: %q %q %q", run.FromWorker, run.SnapshotSHA, run.WorkerTreeSHA)
	}
	if run.Arms[0].AgentTreeRev != WorkerTreeRev || run.Arms[0].AgentTreeSHA != "treesha0123456789abcdef" || run.Arms[1].AgentTreeRev != "abc123" {
		t.Fatalf("arm trees: %+v", run.Arms)
	}
	listed, err := s.Store.List(5)
	if err != nil || len(listed) != 1 || listed[0].FromWorker != "desk-7" {
		t.Fatalf("list: %+v %v", listed, err)
	}
	// The table's head names it, and so does each run record's labels.eval.
	rows, _ := s.Store.Rows(run.ID)
	md := Summarise(run, rows).Markdown()
	if !strings.Contains(md, "Started from worker `desk-7` (agent tree treesha01234, snapshot snapsha01234)") ||
		!strings.Contains(md, "agent tree worker (treesha01234)") {
		t.Fatalf("table head:\n%s", md)
	}
	label, ok, err := s.Store.EvalLabel("", rows[0].TaskID)
	if err != nil || !ok || label.FromWorker != "desk-7" || label.SnapshotSHA != "snapsha0123456789abcdef" {
		t.Fatalf("label: %+v %v %v", label, ok, err)
	}
}

func TestARunWithoutAWorkerIsUnchanged(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "nothing", Kind: ArmNothing}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	if !bytes.Equal(sb.specs[0].Snapshot, emptySnapshot) || sb.specs[0].AgentTree != nil || run.FromWorker != "" || run.SnapshotSHA != "" {
		t.Fatalf("a run without a worker: %+v", run)
	}
	rows, _ := s.Store.Rows(run.ID)
	if md := Summarise(run, rows).Markdown(); strings.Contains(md, "Started from worker") {
		t.Fatalf("the head names a worker:\n%s", md)
	}
	label, ok, _ := s.Store.EvalLabel("", rows[0].TaskID)
	if !ok || label.FromWorker != "" || label.SnapshotSHA != "" {
		t.Fatalf("label: %+v", label)
	}
}

func TestAWorkerThatCannotBeReadRefusesTheRun(t *testing.T) {
	for name, w := range map[string]*fakeWorkers{
		"tree":     {failTree: true},
		"snapshot": {tree: []byte("x"), failSnapshot: true},
		"no tree":  {tree: nil},
	} {
		t.Run(name, func(t *testing.T) {
			s, sb, _, slot, _ := newService(t, diligent)
			s.Workers = w
			_, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, FromWorker: "desk-7",
				Arms: []Arm{{Name: "nothing", Kind: ArmNothing}}})
			var bad BadRequest
			if !errors.As(err, &bad) || !strings.Contains(string(bad), "could not be read") {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(string(bad), "permission denied") || strings.Contains(string(bad), "no such desktop") {
				t.Fatalf("the refusal echoes the error: %q", bad)
			}
			if len(sb.specs) != 0 {
				t.Fatal("a desktop was made")
			}
			release, err := slot.Reserve()
			if err != nil {
				t.Fatalf("the practice slot was kept: %v", err)
			}
			release()
			if list, _ := s.Store.List(5); len(list) != 0 {
				t.Fatalf("a run was stored: %+v", list)
			}
		})
	}
}

func TestOnlyADesktopIdIsAWorkerToStartFrom(t *testing.T) {
	s, _, _, _, _ := newService(t, diligent)
	for _, id := range []string{"practice-e1-a1", "readiness-x", "a b", "../x", strings.Repeat("a", 200)} {
		s.Workers = &fakeWorkers{tree: []byte("x")}
		if _, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, FromWorker: id,
			Arms: []Arm{{Name: "nothing", Kind: ArmNothing}}}); err == nil {
			t.Fatalf("%q was taken as a worker", id)
		}
	}
	// No way to read a worker: refused, never run empty under the worker's name.
	s.Workers = nil
	if _, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, FromWorker: "desk-7",
		Arms: []Arm{{Name: "nothing", Kind: ArmNothing}}}); err == nil {
		t.Fatal("a run from a worker started with no way to read it")
	}
	if !ValidWorkerID("8f2c1a") || ValidWorkerID("practice-1") || ValidWorkerID("") {
		t.Fatal("ValidWorkerID")
	}
}

func TestAnUploadedTreeCannotPassForTheWorkers(t *testing.T) {
	s, _, _, _, _ := newService(t, diligent)
	_, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1,
		Arms: []Arm{{Name: "default", Kind: ArmModel, AgentTree: agentTree(t), AgentTreeRev: WorkerTreeRev}}})
	var bad BadRequest
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v", err)
	}
}

// The step that keeps a run's worker applies on a database the earlier steps made, and a run stored before it reads
// back as started empty.
func TestTheWorkerColumnsMigrateAnExistingDatabase(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlitedb.Migrate(db, "eval", schema[:5]); err != nil { // (the steps before the worker's)
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO eval_runs (id, created_ns, suite_name, suite_sha, arms, repeats, status) VALUES ('e1', 1, 's', 'x', '[]', 1, 'done')`); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Get("e1")
	if err != nil || run.FromWorker != "" || run.SnapshotSHA != "" || run.WorkerTreeSHA != "" {
		t.Fatalf("old run: %+v %v", run, err)
	}
	if err := store.Create(Run{ID: "e2", SuiteName: "s", SuiteSHA: "x", FromWorker: "desk-7", SnapshotSHA: "s1", WorkerTreeSHA: "t1"}); err != nil {
		t.Fatal(err)
	}
	if run, err := store.Get("e2"); err != nil || run.FromWorker != "desk-7" || run.SnapshotSHA != "s1" || run.WorkerTreeSHA != "t1" {
		t.Fatalf("new run: %+v %v", run, err)
	}
}

// The step that counts declined approvals applies on a database every earlier step made: a row stored before it reads
// back with q_declined 0, and a new row keeps its count.
func TestTheDeclinedColumnMigratesAnExistingDatabase(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlitedb.Migrate(db, "eval", schema[:6]); err != nil { // (every step before q_declined's)
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO eval_runs (id, created_ns, suite_name, suite_sha, arms, repeats, status) VALUES ('e1', 1, 's', 'x', '[]', 1, 'done')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO eval_rows (run_id, arm, repeat, case_id, observed, passed, false_done, done_unchecked_wrong, q_required, q_other, at_ns)
		VALUES ('e1', 'arm', 1, 'c1', 'complete', 1, 0, 0, 2, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddRow(Row{RunID: "e1", Arm: "arm", Repeat: 2, CaseID: "c1", Observed: OutcomeComplete, QRequired: 1, QDeclined: 3, At: time.Unix(2, 0)}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Rows("e1")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	if old := rows[0]; old.QDeclined != 0 || old.QRequired != 2 || old.QOther != 1 {
		t.Fatalf("old row: %+v", old)
	}
	if rows[1].QDeclined != 3 {
		t.Fatalf("new row: %+v", rows[1])
	}
}
