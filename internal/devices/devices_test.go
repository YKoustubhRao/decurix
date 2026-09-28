package devices

import (
	"errors"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func newStore(t *testing.T) *Store {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return store
}

func TestAPairedTokenIsValidUntilRevoked(t *testing.T) {
	store := newStore(t)

	token, err := store.Pair("  My   phone ")
	if err != nil {
		t.Fatal(err)
	}

	if !store.Valid(token) {
		t.Fatal("a freshly paired token is not valid")
	}
	if store.Valid(token+"x") || store.Valid("") {
		t.Fatal("a wrong or empty token is valid")
	}

	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if list[0].Name != "My phone" {
		t.Errorf("name = %q, want it trimmed to %q", list[0].Name, "My phone")
	}

	if err := store.Revoke(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if store.Valid(token) {
		t.Fatal("a revoked token is still valid")
	}
	if err := store.Revoke(list[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking twice = %v, want ErrNotFound", err)
	}
}

func TestForgetUnpairsOnlyThatDevice(t *testing.T) {
	store := newStore(t)

	phone, _ := store.Pair("phone")
	laptop, _ := store.Pair("laptop")

	if err := store.Forget(phone); err != nil {
		t.Fatal(err)
	}

	if store.Valid(phone) {
		t.Error("the forgotten device is still valid")
	}
	if !store.Valid(laptop) {
		t.Error("forgetting one device unpaired another")
	}
}

func TestTheTokenItselfIsNeverStored(t *testing.T) {
	store := newStore(t)

	token, _ := store.Pair("phone")

	var stored string
	if err := store.db.QueryRow(`SELECT token_hash FROM paired_devices`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, token) {
		t.Fatal("the raw token is in the database")
	}
}

func TestNamesAreCapped(t *testing.T) {
	store := newStore(t)

	if _, err := store.Pair(strings.Repeat("a", 500)); err != nil {
		t.Fatal(err)
	}

	list, _ := store.List()
	if got := len([]rune(list[0].Name)); got != maxNameLen {
		t.Errorf("name length = %d, want %d", got, maxNameLen)
	}
}

func TestLookupNamesTheDevice(t *testing.T) {
	store := newStore(t)

	token, _ := store.Pair("Pixel")

	d, ok := store.Lookup(token)
	if !ok || d.Name != "Pixel" || d.ID == "" {
		t.Fatalf("Lookup = %+v, %v", d, ok)
	}
	if _, ok := store.Lookup("nope"); ok {
		t.Fatal("an unknown token was found")
	}
}

func TestAnExtraTokenIsTheSameDeviceAndGoesWithIt(t *testing.T) {
	store := newStore(t)

	browser, _ := store.Pair("Pixel")
	d, _ := store.Lookup(browser)

	link, err := store.AddToken(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := store.Lookup(link); !ok || got.ID != d.ID || got.Name != "Pixel" {
		t.Fatalf("the extra token is %+v, %v - want the same device", got, ok)
	}
	if list, _ := store.List(); len(list) != 1 {
		t.Fatalf("the list has %d devices, want the one", len(list))
	}

	if err := store.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	if store.Valid(browser) || store.Valid(link) {
		t.Fatal("revoking the device left one of its tokens working")
	}

	if _, err := store.AddToken("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a token for no device: %v", err)
	}
}
