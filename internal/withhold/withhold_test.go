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

// A postal PIN code is an address, not a secret (recheck 2026-11-03: "Bengaluru PIN 560001" and "Pin: Mumbai 400001"
// were withheld in the task records, "Mumbai" included); a card's, a bank's or a sign-in's PIN still is.
func TestTextKeepsPostalCodesAndWithholdsPins(t *testing.T) {
	for in, want := range map[string]string{
		"Ship to MG Road, Bengaluru PIN 560001":     "Ship to MG Road, Bengaluru PIN 560001",
		"Address: Andheri, Mumbai, Pin: 400053":     "Address: Andheri, Mumbai, Pin: 400053",
		"Pin: Mumbai 400001":                        "Pin: Mumbai 400001",
		"Pincode: 560001":                           "Pincode: 560001",
		"Udupi PIN-576104":                          "Udupi PIN-576104",
		"Invoice total 576104":                      "Invoice total 576104",
		"the pin 3 on the board":                    "the pin 3 on the board",
		"UPI PIN: 482193":                           "UPI PIN: [withheld]",
		"ATM PIN 4821":                              "ATM PIN [withheld]",
		"ATM PIN code: 1234":                        "ATM PIN code: [withheld]",
		"TPIN 4455":                                 "TPIN [withheld]",
		"pin is 1234":                               "pin is [withheld]",
		"Log in with user ram, PIN: 482193, thanks": "Log in with user ram, PIN: [withheld], thanks",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}

// Credentials known by their shape are withheld whatever is beside them, a value runs past ";" and a card number is
// withheld; words that only look like a label stay (review 2026-11-03: the Go side was label-regex only).
func TestTextWithholdsShapesAndKeepsLookalikes(t *testing.T) {
	for in, want := range map[string]string{
		"use key sk-proj-abcdef1234567890XYZ for the API":   "use key [withheld] for the API",
		"token ghp_16C7e42F292c6912E7710c838347Ae178B4a ok": "token [withheld] ok",
		"Authorization: Bearer abcdef1234567890.xyz":        "Authorization: Bearer [withheld]",
		"bearer token abc123def456ghi789":                   "bearer token [withheld]",
		"https://bob:pa55w0rd@example.com/x":                "https://bob:[withheld]@example.com/x",
		"password: Abc;123xyz":                              "password: [withheld]",
		"password: hunter2, user: ram":                      "password: [withheld], user: ram",
		"Card 4111 1111 1111 1111 exp 12/27":                "Card [withheld] exp 12/27",
		"card no 4111111111111111":                          "card no [withheld]",
		"A/c 50100123456789 IFSC HDFC0001234":               "A/c 50100123456789 IFSC HDFC0001234",
		"Order 4111111111111111 shipped":                    "Order 4111111111111111 shipped",
		"secret 2024 strategy deck":                         "secret 2024 strategy deck",
		"pwd /home/user/work":                               "pwd /home/user/work",
		"bearer cheque for Rs 500":                          "bearer cheque for Rs 500",
		"Password Northwind2026":                            "Password [withheld]",
		"CVV 123":                                           "CVV [withheld]",
		`password: "my pass 1" then log in`:                 `password: "[withheld]" then log in`,
		"token: ghp_abcdef":                                 "token: [withheld]",
		"Token: 45 at the counter":                          "Token: 45 at the counter",
		"PO token number: 7781":                             "PO token number: 7781",
		"Login code 552901":                                 "Login code [withheld]",
		"Login code sent to the client":                     "Login code sent to the client",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"password: hunter2", "Card 4111 1111 1111 1111", "UPI PIN 4821", "sk-proj-abcdef1234567890XYZ"} {
		once := Text(in)
		if twice := Text(once); twice != once {
			t.Errorf("Text is not idempotent on %q: %q then %q", in, once, twice)
		}
	}
}

// A bare answer to a question that asked for a secret is withheld in the records; an answer to any other question,
// a postal code's included, is kept as Text keeps it.
func TestReplyWithholdsAnAnswerToASecretQuestion(t *testing.T) {
	cases := []struct{ question, reply, want string }{
		{"What is the OTP the client received?", "552901", Marker},
		{"Please send the portal password", "Lift$2026", Marker},
		{"What is the UPI PIN?", "it is 4821.", "it is [withheld]."},
		{"What is the OTP?", "not received yet", "not received yet"},
		{"Approve paying Rs 400 to Acme?", "yes", "yes"},
		{"Which invoice, INV-12 or INV-13?", "INV-13", "INV-13"},
		{"What is the PIN code of the delivery address?", "576104", "576104"},
		{"What is the OTP?", "OTP is 552901", "OTP is [withheld]"},
	}
	for _, c := range cases {
		if got := Reply(c.question, c.reply); got != c.want {
			t.Errorf("Reply(%q, %q) = %q, want %q", c.question, c.reply, got, c.want)
		}
	}
}

// The review of freeze 2 (A1-A3): over-withholding is restored. A plain word after a label is withheld whatever
// follows it, and every word with a digit or a mark of a reply to a question naming a secret is withheld however long
// the reply is. An apostrophe between letters is still no quote mark.
func TestAPlainWordAfterALabelIsStillWithheld(t *testing.T) {
	for in, want := range map[string]string{
		"password: letmein then press login":               "password: [withheld] then press login",
		"The password wasn't accepted, so I'll ask again.": "The password wasn't accepted, so I'll ask again.",
		"the receiver's key - it's in the client's folder": "the receiver's key - [withheld] in the client's folder",
		"OTP: 604217":                      "OTP: [withheld]",
		"OTP: 604217 came just now":        "OTP: [withheld] came just now",
		"password: Lift$2026":              "password: [withheld]",
		"password: tiger.":                 "password: [withheld].",
		"password: tiger, then sign in":    "password: [withheld], then sign in",
		"pwd=x7#k":                         "pwd=[withheld]",
		"password: 'Tom's9' then sign in":  "password: '[withheld]' then sign in",
		"password: O'Brien7 then sign in":  "password: [withheld] then sign in",
		"Password: Tiger and then sign in": "Password: [withheld] and then sign in",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEveryValueShapedWordOfAReplyToASecretQuestionIsWithheld(t *testing.T) {
	cases := []struct{ question, reply, want string }{
		{"What is the OTP the client received?", "552901", Marker},
		{"What is the OTP?", "here it is: 552901", "here it is: [withheld]"},
		{"What is the OTP?", "the OTP is 552901, go ahead", "the OTP is [withheld], go ahead"},
		{"What is the OTP?", "552901\nand the next one comes tomorrow", "[withheld]\nand the next one comes tomorrow"},
		{"What is the passphrase?", "it is correct-horse-battery, then press sign in",
			"it is [withheld], then press sign in"},
	}
	for _, c := range cases {
		if got := Reply(c.question, c.reply); got != c.want {
			t.Errorf("Reply(%q, %q) = %q, want %q", c.question, c.reply, got, c.want)
		}
	}
}
