package main

import (
	"os"
	"path/filepath"
	"testing"

	"brainiac/internal/config"
	"brainiac/internal/engine"
	"brainiac/internal/install"
	"brainiac/internal/selector"
	"brainiac/internal/store"
)

func emptyEngine(t *testing.T) *engine.Engine {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.Options{Config: config.Default(), Store: st,
		Lock: &install.Lock{Plan: &selector.Plan{}, Units: map[string]install.Unit{}}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// With the defaults serve runs no update loop, so nothing but a person
// changes what is served.
func TestServeRunsNoUpdaterByDefault(t *testing.T) {
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := updateEvery(cfg); ok {
		t.Fatal("the update loop runs with the default settings")
	}
	cfg.UpdateEvery = "24h"
	if _, ok := updateEvery(cfg); !ok {
		t.Fatal("update_every 24h did not run the loop")
	}
	cfg.Web = config.WebOff
	if _, ok := updateEvery(cfg); ok {
		t.Fatal("the update loop runs with the web off")
	}
}

// serve swaps engines only when lock.json's bytes change (a person's
// promote), and once per change.
func TestServeFollowsOnlyAChangedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := os.WriteFile(path, []byte(`{"schema":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cur := emptyEngine(t)
	builds, swaps := 0, 0
	f := &lockFollower{path: path, seen: lockSHA(path), logf: t.Logf,
		build: func() (*engine.Engine, error) { builds++; return emptyEngine(t), nil },
		swap: func(e *engine.Engine) *engine.Engine {
			swaps++
			old := cur
			cur = e
			return old
		}}
	if f.check() || builds != 0 || swaps != 0 {
		t.Fatal("serve swapped engines with lock.json unchanged")
	}
	if err := os.WriteFile(path, []byte(`{"schema":1,"made_at":"2026-11-03T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !f.check() || swaps != 1 {
		t.Fatalf("a promoted lock was not followed: builds %d swaps %d", builds, swaps)
	}
	if f.check() || swaps != 1 {
		t.Fatal("the same change was followed twice")
	}
}
