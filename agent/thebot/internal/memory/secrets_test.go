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

// The detector looks for a secret VALUE next to a credential word, not for the word: a fix memory that mentions a
// token, a key or pinning a version is kept, and a password written without "is" or with a symbol is still caught.
func TestLooksSecretReadsValuesNotWords(t *testing.T) {
	secret := []string{
		"passwd hunter2",
		"pw hunter2",
		"the portal login is Tally@123",
		"password: valid8",
		"password - hunter22",
		"otp 552910",
		"The verification code is 482913",
		"one-time password is swordfish",
		"access token = 9f8e7d6c5b4a3210",
		"client secret: q8Zr2kLm9Xp4",
	}
	for _, s := range secret {
		if !LooksSecret(s) {
			t.Errorf("LooksSecret(%q) = false, want true", s)
		}
	}
	plain := []string{
		`build failure fix: when a build fails with "SyntaxError: unexpected token: foo", the cause is a stray brace.`,
		"build failure fix: pin numpy<2 to fix the ABI error",
		"The fix that actually worked: pip install 'numpy<2' (pin the version)",
		"my api key is stored in the vault",
		"set the token budget to 4000",
		"the key: value pairs go in config.yaml",
		"export the password=$DB_PASS from the environment",
		"the login: admin@example.com account",
		"the login page is at https://erp.example.com/login",
		"password-protected PDFs need qpdf",
		"remember my PAN ABCDE1234F",
		"Office address PIN code 560001",
	}
	for _, s := range plain {
		if LooksSecret(s) {
			t.Errorf("LooksSecret(%q) = true, want false", s)
		}
	}
}

// A store an older soul wrote, holding a secret its check let in, is cleaned when it is opened.
func TestOpeningL1PurgesSecretsAlreadyStored(t *testing.T) {
	root := t.TempDir()
	l1, err := NewL1(root, &judgeStub{}, &countingEmbedder{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Written past Store, as an older soul's weaker check would have let it through.
	for _, f := range []string{"The user's passwd hunter2 opens the portal.", "The user prefers Go."} {
		if _, err := l1.db.Exec(`INSERT INTO facts (fact, source, embedding, created_at) VALUES (?, 'explicit', '[]', 0)`, f); err != nil {
			t.Fatal(err)
		}
	}
	l1.Close()

	l1, err = NewL1(root, &judgeStub{}, &countingEmbedder{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	if n, _ := l1.Count(context.Background()); n != 1 {
		t.Fatalf("after reopening, L1 holds %d facts, want only the one without a secret", n)
	}
}
