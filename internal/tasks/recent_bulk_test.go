package tasks

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// One query answers what RecentForWorker answers for each worker: the same tasks, newest first, at most limit each,
// and an empty list for a worker with none.
func TestRecentForWorkersMatchesRecentForWorker(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	for i := 0; i < 4; i++ {
		for _, id := range []string{"box", "desk"} {
			send(t, s, worker(id, "thebot"), fmt.Sprintf("%s task %d", id, i))
			c.advance(time.Second)
		}
	}
	send(t, s, worker("other", "thebot"), "not asked for")

	got, err := s.RecentForWorkers([]string{"box", "desk", "empty", "box", ""}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("workers = %v, want box, desk and empty", workerKeys(got))
	}
	for _, id := range []string{"box", "desk"} {
		want, err := s.RecentForWorker(id, 3)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[id], want) {
			t.Errorf("%s = %+v\nwant %+v", id, got[id], want)
		}
		if len(got[id]) != 3 || got[id][0].Request != id+" task 3" {
			t.Errorf("%s: %d tasks, newest %q", id, len(got[id]), got[id][0].Request)
		}
	}
	if list, ok := got["empty"]; !ok || list == nil || len(list) != 0 {
		t.Errorf("empty = %#v, %v; want an empty list", list, ok)
	}

	none, err := s.RecentForWorkers(nil, 3)
	if err != nil || len(none) != 0 {
		t.Fatalf("no workers: %v, %v", none, err)
	}
}

func workerKeys(m map[string][]Task) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
