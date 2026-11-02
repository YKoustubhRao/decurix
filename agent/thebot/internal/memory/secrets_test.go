package memory

import (
	"context"
	"errors"
	"testing"
)

func TestLooksSecret(t *testing.T) {
	secret := []string{
		"The user's password is hunter2.",
		"The portal password Northwind#2026 works for the GST login.",
		"The user's PIN: 4821",
		"The OTP is 552910",
		"Use api key = abcd1234efgh",
		"The user's GitHub token is ghp_abcdefghijklmnopqrstuvwxyz0123",
		"Deploy key sk-abcdefghijklmnop1234",
		"Login works with https://admin:s3cret@erp.example.com/",
		"Header Bearer abcdefghijklmnop.qrstuvwx",
		"The session cookie is Zx9QkLm2Pq8RtY7uVw6XsA5BcD4EfG3H",
	}
	for _, s := range secret {
		if !LooksSecret(s) {
			t.Errorf("LooksSecret(%q) = false, want true", s)
		}
	}
	plain := []string{
		"The user's name is Alex Doe.",
		"The user prefers Go over Python.",
		"The user uses a password manager.",
		"The user wants the token budget kept low.",
		"The user's API key is stored in the vault.",
		"Build failure fix: when a build fails with \"No module named yaml\", run pip install pyyaml.",
		"The user's commit is 3f2a9c1d8e7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f.",
	}
	for _, s := range plain {
		if LooksSecret(s) {
			t.Errorf("LooksSecret(%q) = true, want false", s)
		}
	}
}

// A secret-shaped fact is never stored, however it is asked for.
func TestStoreRefusesASecret(t *testing.T) {
	embedder := &countingEmbedder{}
	l1, err := NewL1(t.TempDir(), &judgeStub{}, embedder, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	if err := l1.Store(context.Background(), "The user's bank PIN is 4821.", "explicit"); !errors.Is(err, ErrSecret) {
		t.Fatalf("Store(secret) = %v, want ErrSecret", err)
	}
	if n, _ := l1.Count(context.Background()); n != 0 || embedder.calls.Load() != 0 {
		t.Fatalf("a secret reached L1 (count %d) or the embedder (%d calls)", n, embedder.calls.Load())
	}
}

// A judged fact keeps only what the person said: a value the answer carried
// (a client's number from a document the work read) is not made a fact
// about the person, recalled into every later task.
func TestConsiderExchangeKeepsOnlyValuesThePersonSaid(t *testing.T) {
	ctx := context.Background()
	newL1 := func(judged string) *L1 {
		l1, err := NewL1(t.TempDir(), &judgeStub{response: "REMEMBER: yes\nFACT: " + judged}, &countingEmbedder{}, "test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l1.Close() })
		return l1
	}

	l1 := newL1("The user's client Acme has GSTIN 27AAACA1234B1Z5.")
	l1.ConsiderExchange(ctx, "remember that Acme is my client - file their return from the invoice", "Filed. Acme's GSTIN 27AAACA1234B1Z5 is on the invoice.", nil)
	if n, _ := l1.Count(ctx); n != 0 {
		t.Fatalf("stored a value only the documents showed (count %d)", n)
	}

	l1 = newL1("The user's phone number is 98450 12345.")
	l1.ConsiderExchange(ctx, "remember my phone number is 98450-12345", "Noted.", nil)
	if n, _ := l1.Count(ctx); n != 1 {
		t.Fatalf("did not store a value the person said (count %d)", n)
	}

	l1 = newL1("The user's portal password is Sunrise#77.")
	l1.ConsiderExchange(ctx, "remember my portal password is Sunrise#77", "Noted.", nil)
	if n, _ := l1.Count(ctx); n != 0 {
		t.Fatalf("stored a password the person asked to be remembered (count %d)", n)
	}
}
