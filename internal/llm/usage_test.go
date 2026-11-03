package llm

import (
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// The proxy counts what each answer says it used, per worker and day, from what the provider reported (C-08).
func TestTheProxyMetersWhatAnswersUsed(t *testing.T) {
	if u := ParseUsage([]byte(`{"usage":{"prompt_tokens":1200,"completion_tokens":300}}`)); u.Tokens() != 1500 {
		t.Fatalf("usage = %+v", u)
	}
	if u := ParseUsage([]byte(`not json`)); u.Tokens() != 0 {
		t.Fatalf("usage of nothing = %+v", u)
	}

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hub, err := NewHub(db, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := hub.Meter("w1", now, Usage{Input: 100, Output: 50}); err != nil {
			t.Fatal(err)
		}
	}
	hub.Meter("w2", now, Usage{Input: 9})
	hub.Meter("w1", now.AddDate(0, 0, -3), Usage{Input: 1000})

	if got, err := hub.Metered("w1", now); err != nil || got != 450 {
		t.Fatalf("today = %d, %v", got, err)
	}
	if got, _ := hub.Metered("w1", now.AddDate(0, 0, -7)); got != 1450 {
		t.Fatalf("this week = %d", got)
	}
}
