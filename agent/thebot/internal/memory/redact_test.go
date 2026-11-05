package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"thebot/internal/withheld"
)

// RedactSecrets takes out exactly what LooksSecret finds: after it, no secret sample still looks secret, and a plain
// sentence is unchanged.
func TestRedactSecretsWithholdsWhatLooksSecret(t *testing.T) {
	secret := map[string]string{
		"The user's password is hunter2.":                               "hunter2",
		"The portal password Northwind#2026 works for the GST login.":   "Northwind#2026",
		"The user's PIN: 4821":                                          "4821",
		"The OTP is 552910":                                             "552910",
		"Use api key = abcd1234efgh":                                    "abcd1234efgh",
		"The user's GitHub token is ghp_abcdefghijklmnopqrstuvwxyz0123": "ghp_abcdefghijklmnopqrstuvwxyz0123",
		"Deploy key sk-abcdefghijklmnop1234":                            "sk-abcdefghijklmnop1234",
		"Login works with https://admin:s3cret@erp.example.com/":        "s3cret",
		"Header Bearer abcdefghijklmnop.qrstuvwx":                       "abcdefghijklmnop.qrstuvwx",
		"The session cookie is Zx9QkLm2Pq8RtY7uVw6XsA5BcD4EfG3H":        "Zx9QkLm2Pq8RtY7uVw6XsA5BcD4EfG3H",
		"passwd hunter2":                                                "hunter2",
		"the portal login is Tally@123":                                 "Tally@123",
		"one-time password is swordfish":                                "swordfish",
		"client secret: q8Zr2kLm9Xp4":                                   "q8Zr2kLm9Xp4",
		"gui type e4 'x' # password: valid8":                            "valid8",
	}
	for text, value := range secret {
		got := RedactSecrets(text)
		if strings.Contains(got, value) || LooksSecret(got) || !strings.Contains(got, Withheld) {
			t.Errorf("RedactSecrets(%q) = %q", text, got)
		}
	}
	plain := []string{
		"The user uses a password manager.",
		"The user's API key is stored in the vault.",
		"build failure fix: pin numpy<2 to fix the ABI error",
		"export the password=$DB_PASS from the environment",
		"the login page is at https://erp.example.com/login",
		"Office address PIN code 560001",
		"The user's commit is 3f2a9c1d8e7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f.",
		"gui type 'textbox \"Total\"' '5900.00'",
	}
	for _, text := range plain {
		if got := RedactSecrets(text); got != text {
			t.Errorf("RedactSecrets(%q) = %q, want it unchanged", text, got)
		}
	}
}

// L1 refuses a fact that holds one of the job's secret values, whatever its shape.
func TestL1RefusesAJobsSecret(t *testing.T) {
	l1 := &L1{}
	ctx := withheld.With(context.Background(), []string{"plumtree-cabinet-41"})
	if err := l1.Store(ctx, "The portal sign-in is plumtree-cabinet-41.", "explicit"); !errors.Is(err, ErrSecret) {
		t.Fatalf("Store = %v, want ErrSecret", err)
	}
}
