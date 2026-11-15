// index_test.go checks the run search index: that the driver has FTS5, that a task's stored text is indexed and
// found, one hit per task with a person's run ranked above a program's, the read and scroll shapes with their cuts,
// the worker scope, time bounds and exclusions, the backfill sweep, and that a secret is never indexed and a purge
// reaches the index.
package runsearch

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// The pure-Go driver this build uses (modernc.org/sqlite) is built with FTS5: the index rests on it.
func TestTheDriverHasFTS5(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE probe USING fts5(body)`); err != nil {
		t.Fatalf("FTS5 is not available: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO probe(rowid, body) VALUES (1, 'the march invoice was filed'), (2, 'nothing')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	var rank float64
	if err := db.QueryRow(`SELECT rowid, bm25(probe) FROM probe WHERE probe MATCH '"invoice"'`).Scan(&id, &rank); err != nil || id != 1 {
		t.Fatalf("match: %d %v", id, err)
	}
}

type fixture struct {
	db    *sql.DB
	tasks *tasks.Store
	index *Index
}

func open(t *testing.T) fixture {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ts, err := tasks.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := New(db, ts)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{db: db, tasks: ts, index: ix}
}

// task records one task on worker: its request, steps, and a done outcome with report.
func (f fixture) task(t *testing.T, worker, origin, request string, steps []string, report string) string {
	t.Helper()
	w := tasks.Worker{ID: worker, Name: worker, Engine: "thebot"}
	route, err := f.tasks.Plan(worker, request)
	if err != nil {
		t.Fatal(err)
	}
	route, err = f.tasks.Commit(route, w, request, tasks.Origin{Kind: origin})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if _, err := f.tasks.AddStep(worker, route.TaskID, tasks.Step{Text: s}); err != nil {
			t.Fatal(err)
		}
	}
	checked := true
	if _, err := f.tasks.RecordOutcome(worker, route.TaskID, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: report, Checked: &checked}); err != nil {
		t.Fatal(err)
	}
	if err := f.index.IndexTask(route.TaskID); err != nil {
		t.Fatal(err)
	}
	return route.TaskID
}

// One hit per task however many of its texts match; a person's run before a program's that matches more; only the
// top hit opened, the others a snippet.
func TestDiscoveryFoldsByTaskAndRanksAPersonsRunFirst(t *testing.T) {
	f := open(t)
	scheduled := f.task(t, "box", tasks.OriginSchedule, "reconcile the invoice ledger for march",
		[]string{"opened invoice list", "invoice 1 matched", "invoice 2 matched", "invoice 3 matched"}, "march invoices reconciled")
	person := f.task(t, "box", tasks.OriginPerson, "find the march invoice from the north office",
		[]string{"searched the inbox"}, "the invoice is saved as north-march.pdf")
	f.task(t, "box", tasks.OriginPerson, "water the plants", nil, "done")

	res, err := f.index.Search(Query{Words: "march invoice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 || res.Tasks != 2 {
		t.Fatalf("hits %+v", res.Hits)
	}
	if res.Hits[0].TaskID != person || res.Hits[1].TaskID != scheduled || !res.Hits[1].Automated {
		t.Fatalf("order: %s then %s", res.Hits[0].TaskID, res.Hits[1].TaskID)
	}
	if len(res.Hits[0].Messages) == 0 || res.Hits[0].Messages[0].Kind != KindRequest {
		t.Fatalf("top hit not opened with its request: %+v", res.Hits[0].Messages)
	}
	if len(res.Hits[1].Messages) != 0 || !strings.Contains(res.Hits[1].Snippet, "[[") {
		t.Fatalf("second hit: %+v", res.Hits[1])
	}
	last := res.Hits[0].Messages[len(res.Hits[0].Messages)-1]
	if last.Kind != KindOutcome || !strings.Contains(last.Text, "north-march.pdf") {
		t.Fatalf("top hit's report missing: %+v", last)
	}
	// A program's run is still found when it is the only match.
	only, err := f.index.Search(Query{Words: "reconciled"})
	if err != nil || len(only.Hits) != 1 || only.Hits[0].TaskID != scheduled {
		t.Fatalf("program-only match: %+v %v", only, err)
	}
}

// A worker's search sees only its own runs, and reading another's is "no such run".
func TestAWorkersSearchIsItsOwnRunsOnly(t *testing.T) {
	f := open(t)
	mine := f.task(t, "box", tasks.OriginPerson, "renew the parking permit", nil, "renewed")
	theirs := f.task(t, "other", tasks.OriginPerson, "renew the parking permit for the van", nil, "renewed")

	res, err := f.index.Search(Query{Words: "parking permit", Worker: "box"})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].TaskID != mine {
		t.Fatalf("scoped search: %+v %v", res, err)
	}
	if _, err := f.index.Read(theirs, "box", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read of another worker's run: %v", err)
	}
	if _, err := f.index.Scroll(theirs, "box", 0, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scroll of another worker's run: %v", err)
	}
	if _, err := f.index.Read(theirs, "", 0); err != nil {
		t.Fatalf("a person reads any run: %v", err)
	}
}

// A read cuts each text at MaxMessageChars and says how much; past its limit it returns both ends and names the gap; a
// scroll returns the window around its anchor.
func TestReadAndScrollReturnStoredTextCutNotSummarised(t *testing.T) {
	f := open(t)
	long := strings.Repeat("x", MaxMessageChars+500)
	var steps []string
	for i := 0; i < 30; i++ {
		steps = append(steps, "step number "+string(rune('a'+i%26)))
	}
	steps[0] = long
	id := f.task(t, "box", tasks.OriginPerson, "do thirty things", steps, "all thirty done")

	run, err := f.index.Read(id, "box", 0)
	if err != nil {
		t.Fatal(err)
	}
	if run.Total != 32 || len(run.Messages) != 32 {
		t.Fatalf("read %d of %d", len(run.Messages), run.Total)
	}
	if m := run.Messages[1]; len(m.Text) != MaxMessageChars || m.Cut != 500 {
		t.Fatalf("long text: %d chars, cut %d", len(m.Text), m.Cut)
	}
	short, err := f.index.Read(id, "box", 10)
	if err != nil || len(short.Messages) != 10 || short.Messages[9].Seq != 31 || !strings.Contains(short.Note, "not shown") {
		t.Fatalf("bounded read: %+v %v", short, err)
	}
	win, err := f.index.Scroll(id, "box", 15, 2)
	if err != nil || len(win.Messages) != 5 || win.Messages[0].Seq != 13 || win.Messages[4].Seq != 17 {
		t.Fatalf("scroll: %+v %v", win.Messages, err)
	}
}

// Time bounds and excluded tasks narrow a discovery; FTS operators a caller types are words, never syntax.
func TestBoundsExclusionsAndOperatorsAreTakenAsWords(t *testing.T) {
	f := open(t)
	a := f.task(t, "box", tasks.OriginPerson, "book the venue", nil, "booked")
	b := f.task(t, "box", tasks.OriginPerson, "book the venue again", nil, "booked")

	res, err := f.index.Search(Query{Words: "venue", Exclude: []string{a}})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].TaskID != b {
		t.Fatalf("exclude: %+v %v", res, err)
	}
	res, err = f.index.Search(Query{Words: "venue", After: time.Now().Add(time.Hour)})
	if err != nil || len(res.Hits) != 0 {
		t.Fatalf("after: %+v %v", res, err)
	}
	res, err = f.index.Search(Query{Words: "venue", Before: time.Now().Add(time.Hour)})
	if err != nil || len(res.Hits) != 2 {
		t.Fatalf("before: %+v %v", res, err)
	}
	if _, err := f.index.Search(Query{Words: `venue" OR NEAR(x* AND`}); err != nil {
		t.Fatalf("operators were read as syntax: %v", err)
	}
	if _, err := f.index.Search(Query{Words: "  ...  "}); !errors.Is(err, ErrNoWords) {
		t.Fatalf("no words: %v", err)
	}
}

// The sweep backfills tasks recorded before the index existed, moves past them, and picks up a later change.
func TestTheSweepBackfillsAndFollowsChanges(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ts, err := tasks.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	w := tasks.Worker{ID: "box", Name: "box"}
	route, _ := ts.Plan("box", "count the chairs in the hall")
	if route, err = ts.Commit(route, w, "count the chairs in the hall", tasks.Origin{Kind: tasks.OriginPerson}); err != nil {
		t.Fatal(err)
	}

	ix, err := New(db, ts)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := ix.Sweep(0); err != nil || n != 1 {
		t.Fatalf("backfill: %d %v", n, err)
	}
	if n, err := ix.Sweep(0); err != nil || n != 0 {
		t.Fatalf("second sweep read %d again (%v)", n, err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := ts.RecordOutcome("box", route.TaskID, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "forty two chairs"}); err != nil {
		t.Fatal(err)
	}
	if n, err := ix.Sweep(0); err != nil || n != 1 {
		t.Fatalf("change not swept: %d %v", n, err)
	}
	res, err := ix.Search(Query{Words: "forty two"})
	if err != nil || len(res.Hits) != 1 {
		t.Fatalf("after the change: %+v %v", res, err)
	}
}

// A reply to a question asked for a secret is kept withheld by the records, and the index holds only the mark; a
// purge of a value that slipped into a step reaches the index.
func TestSecretsAreNotIndexedAndAPurgeReachesTheIndex(t *testing.T) {
	f := open(t)
	w := tasks.Worker{ID: "box", Name: "box"}
	route, _ := f.tasks.Plan("box", "sign in to the portal")
	route, err := f.tasks.Commit(route, w, "sign in to the portal", tasks.Origin{Kind: tasks.OriginPerson})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.RecordOutcome("box", route.TaskID, tasks.Outcome{Kind: tasks.OutcomeQuery, Reply: "What is the code?", Secret: true}); err != nil {
		t.Fatal(err)
	}
	reply, _ := f.tasks.Plan("box", "zebra-quartz-9911")
	if _, _, err := f.tasks.CommitReply(reply, w, "zebra-quartz-9911", false, tasks.ReplyBy{Actor: tasks.Actor{ID: "owner", Kind: tasks.ActorPerson}}, tasks.Origin{Kind: tasks.OriginPerson}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.AddStep("box", route.TaskID, tasks.Step{Text: "typed leaky-value-31337 into the box"}); err != nil {
		t.Fatal(err)
	}
	if err := f.index.IndexTask(route.TaskID); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.index.Search(Query{Words: "zebra quartz"}); len(res.Hits) != 0 {
		t.Fatalf("a secret reply was indexed: %+v", res.Hits)
	}
	run, err := f.index.Read(route.TaskID, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range run.Messages {
		if strings.Contains(m.Text, "zebra") {
			t.Fatalf("secret in a read: %q", m.Text)
		}
	}
	if res, _ := f.index.Search(Query{Words: "leaky"}); len(res.Hits) != 1 {
		t.Fatalf("step not indexed: %+v", res)
	}
	n, err := f.index.PurgeValue([]string{"leaky-value-31337"}, "[purged]")
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if res, _ := f.index.Search(Query{Words: "leaky"}); len(res.Hits) != 0 {
		t.Fatalf("a purged value is still found: %+v", res.Hits)
	}
}

func TestParseBound(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":                     {},
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"24h":                  now.Add(-24 * time.Hour),
		"2w":                   now.Add(-14 * 24 * time.Hour),
		"2026-07-01":           time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		"2026-07-01T05:00:00Z": time.Date(2026, 7, 1, 5, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := ParseBound(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	if _, err := ParseBound("last tuesday", now); err == nil {
		t.Error("an unreadable bound was taken")
	}
}
