package withhold

import "testing"

func TestTextWithholdsLabelledSecrets(t *testing.T) {
	for in, want := range map[string]string{
		"log in with password Xy7#q and file it": "log in with password [withheld] and file it",
		"password: hunter2":                      "password: [withheld]",
		"PIN: 482193":                            "PIN: [withheld]",
		"the OTP is 552901":                      "the OTP is [withheld]",
		"api_key=sk-abc123":                      "api_key=[withheld]",
		"the password is incorrect":              "the password is incorrect",
		"Udupi, PIN code 576104":                 "Udupi, PIN code 576104",
		"Pay Rs 4,50,000 to Acme for INV-1234":   "Pay Rs 4,50,000 to Acme for INV-1234",
		"GSTIN 29ABCDE1234F1Z5, total 1,18,000":  "GSTIN 29ABCDE1234F1Z5, total 1,18,000",
		"password: [withheld]":                   "password: [withheld]",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}
