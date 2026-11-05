package devices

import (
	"errors"
	"testing"
)

// A token whose device is no longer paired - left behind by an AddToken that
// raced a Revoke - does not pass the gate (review 2026-11-03, B-18).
func TestATokenWithoutItsDeviceIsNotValid(t *testing.T) {
	store := newStore(t)
	if _, err := store.db.Exec(`INSERT INTO device_tokens (token_hash, device_id) VALUES (?, ?)`, hashToken("orphan"), "gone"); err != nil {
		t.Fatal(err)
	}
	if store.Valid("orphan") {
		t.Fatal("a token whose device is not paired is valid")
	}
}

func TestAddTokenForAnUnpairedDeviceAddsNothing(t *testing.T) {
	store := newStore(t)
	if _, err := store.AddToken("gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AddToken for no device: %v", err)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM device_tokens`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d token rows, %v", n, err)
	}
}

// Revoking a device ends every token and tells whoever holds a live link.
func TestRevokeEndsEveryTokenAndCallsTheHooks(t *testing.T) {
	store := newStore(t)
	token, err := store.Pair("phone")
	if err != nil {
		t.Fatal(err)
	}
	dev, ok := store.Lookup(token)
	if !ok {
		t.Fatal("lookup")
	}
	extra, err := store.AddToken(dev.ID)
	if err != nil {
		t.Fatal(err)
	}

	var dropped []string
	store.OnRevoke(func(id string) { dropped = append(dropped, id) })

	if err := store.Forget(extra); err != nil {
		t.Fatal(err)
	}
	if store.Valid(token) || store.Valid(extra) {
		t.Fatal("a token of a revoked device is still valid")
	}
	if len(dropped) != 1 || dropped[0] != dev.ID {
		t.Fatalf("revoke hooks called with %v", dropped)
	}
	if err := store.Revoke(dev.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	if len(dropped) != 1 {
		t.Fatal("a revoke of nothing called the hooks")
	}
}
