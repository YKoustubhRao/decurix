// search_test.go checks that a purge reaches the run search index, which holds copies of the task records' text.
package purge

import (
	"context"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/runsearch"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

func TestAPurgeReachesTheRunSearchIndex(t *testing.T) {
	st := testStores(t)
	ix, err := runsearch.New(st.DB, st.Tasks)
	if err != nil {
		t.Fatal(err)
	}
	st.Search = ix
	route, err := st.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "box", Name: "box"}, "log in with "+leaked,
		tasks.Origin{Kind: tasks.OriginPerson})
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IndexTask(route.TaskID); err != nil {
		t.Fatal(err)
	}
	if res, _ := ix.Search(runsearch.Query{Words: leaked}); len(res.Hits) != 1 {
		t.Fatalf("the value was not indexed: the test would prove nothing (%+v)", res)
	}
	res, err := st.Purge(context.Background(), leaked, "typed into a request", owner, "privacy")
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts.RunSearch != 1 {
		t.Fatalf("counts = %+v", res.Counts)
	}
	if found, _ := ix.Search(runsearch.Query{Words: leaked}); len(found.Hits) != 0 {
		t.Fatalf("the purged value is still found: %+v", found.Hits)
	}
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM run_search_docs WHERE instr(body, ?) > 0`, leaked).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows still holding it: %d %v", n, err)
	}
}
