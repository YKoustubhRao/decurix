package access

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func init() { PasswordIterations = 1000 } // keep tests fast; the format records the count

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	// Migrate again: a second start must be a no-op.
	if _, err := NewStore(db); err != nil {
		t.Fatal(err)
	}
	return s
}

func f(v float64) *float64 { return &v }

func TestPasswordHashing(t *testing.T) {
	h1, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := HashPassword("correct horse")
	if h1 == h2 {
		t.Fatal("two hashes of one password should differ by salt")
	}
	if !strings.HasPrefix(h1, "pbkdf2-sha256$1000$") {
		t.Fatalf("hash format: %s", h1)
	}
	if !CheckPassword(h1, "correct horse") || CheckPassword(h1, "correct horsE") || CheckPassword(h1, "") {
		t.Fatal("CheckPassword wrong")
	}
	for _, bad := range []string{"", "x", "md5$1$aa$bb", "pbkdf2-sha256$0$aa$bb", "pbkdf2-sha256$10$zz$bb", "pbkdf2-sha256$10$aa$"} {
		if CheckPassword(bad, "x") {
			t.Fatalf("malformed hash %q accepted", bad)
		}
	}
	// A hash made with another iteration count still checks.
	PasswordIterations = 1500
	h3, _ := HashPassword("pw-123456")
	PasswordIterations = 1000
	if !CheckPassword(h3, "pw-123456") {
		t.Fatal("hash with a different count failed")
	}
}

func TestUsersAndAuthenticate(t *testing.T) {
	s := newStore(t)
	u, err := s.CreateUser(" Asha ", "password-1", []string{"Finance", "finance", "clerk"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "Asha" || strings.Join(u.Roles, ",") != "clerk,finance" {
		t.Fatalf("user = %+v", u)
	}
	if _, err := s.CreateUser("asha", "password-2", nil); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := s.CreateUser("b", "short", nil); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("short password: %v", err)
	}
	if _, err := s.CreateUser("c", "password-1", []string{"bad role!"}); !errors.Is(err, ErrBadRoles) {
		t.Fatalf("bad role: %v", err)
	}
	if _, err := s.CreateUser("", "password-1", nil); !errors.Is(err, ErrBadName) {
		t.Fatalf("empty name: %v", err)
	}

	got, err := s.Authenticate("ASHA", "password-1")
	if err != nil || got.ID != u.ID {
		t.Fatalf("authenticate: %v %+v", err, got)
	}
	if _, err := s.Authenticate("asha", "wrong-pass"); !errors.Is(err, ErrSignInFailed) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := s.Authenticate("nobody", "password-1"); !errors.Is(err, ErrSignInFailed) {
		t.Fatalf("unknown user: %v", err)
	}

	if err := s.SetRoles(u.ID, []string{"approver"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.User(u.ID); strings.Join(got.Roles, ",") != "approver" {
		t.Fatalf("roles = %v", got.Roles)
	}
	if err := s.SetRoles("missing", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set roles on missing: %v", err)
	}

	if err := s.SetDisabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("asha", "password-1"); !errors.Is(err, ErrSignInFailed) {
		t.Fatalf("disabled user signed in: %v", err)
	}
	_ = s.SetDisabled(u.ID, false)

	if err := s.SetPassword(u.ID, "new-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("asha", "new-password"); err != nil {
		t.Fatal(err)
	}

	users, _ := s.Users()
	if len(users) != 1 {
		t.Fatalf("users = %d", len(users))
	}
	if err := s.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestLockout(t *testing.T) {
	s := newStore(t)
	clock := time.Now()
	s.now = func() time.Time { return clock }
	if _, err := s.CreateUser("ravi", "password-1", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxFailures; i++ {
		_, _ = s.Authenticate("ravi", "nope-nope")
	}
	if _, err := s.Authenticate("ravi", "password-1"); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("expected lockout, got %v", err)
	}
	clock = clock.Add(lockoutFor + time.Second)
	if _, err := s.Authenticate("ravi", "password-1"); err != nil {
		t.Fatalf("after lockout: %v", err)
	}
}

func TestSessionsExpire(t *testing.T) {
	s := newStore(t)
	clock := time.Now()
	s.now = func() time.Time { return clock }
	u, _ := s.CreateUser("mei", "password-1", []string{"finance"})

	token, expires, err := s.NewSession(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(clock.Add(SessionTTL)) {
		t.Fatalf("expires = %v", expires)
	}
	if got, ok := s.SessionUser(token); !ok || got.ID != u.ID {
		t.Fatal("fresh session not valid")
	}
	if _, ok := s.SessionUser(token + "x"); ok {
		t.Fatal("wrong token accepted")
	}
	if _, ok := s.SessionUser(""); ok {
		t.Fatal("empty token accepted")
	}

	// The token itself is never stored.
	var stored string
	_ = s.db.QueryRow(`SELECT token_hash FROM access_sessions`).Scan(&stored)
	if stored == token || stored != tokenHash(token) {
		t.Fatal("session stored in the clear")
	}

	clock = clock.Add(SessionTTL)
	if _, ok := s.SessionUser(token); ok {
		t.Fatal("expired session accepted")
	}

	// Disabling ends sessions; a password change ends them too.
	clock = time.Now()
	t2, _, _ := s.NewSession(u.ID)
	_ = s.SetDisabled(u.ID, true)
	if _, ok := s.SessionUser(t2); ok {
		t.Fatal("disabled user's session accepted")
	}
	_ = s.SetDisabled(u.ID, false)
	t3, _, _ := s.NewSession(u.ID)
	_ = s.SetPassword(u.ID, "another-pass")
	if _, ok := s.SessionUser(t3); ok {
		t.Fatal("session survived a password change")
	}
	t4, _, _ := s.NewSession(u.ID)
	_ = s.EndSession(t4)
	if _, ok := s.SessionUser(t4); ok {
		t.Fatal("ended session accepted")
	}
}

func TestLargestAmount(t *testing.T) {
	cases := []struct {
		text string
		want *float64
	}{
		{"pay $1,250.50 to the supplier", f(1250.5)},
		{"Pay ₹1,00,000 and ₹500", f(100000)},
		{"transfer 2500 EUR", f(2500)},
		{"transfer USD 75", f(75)},
		{"Amount: 980 for invoice INV-2024-00123", f(980)},
		{"Rs. 4,000 owed", f(4000)},
		{"send the report to the team on 27/10", nil},
		{"delete 3 files", nil},
		// A tax code is no currency and a rate is never an amount; Indian grouping and lakh/crore are amounts.
		{"Pay 4,50,000 incl GST 18%", f(450000)},
		{"Pay 12,34,56,789 to the vendor", f(123456789)},
		{"Pay ₹4.5 lakh to the vendor", f(450000)},
		{"Pay Rs 2 crore, fee Rs 500", f(20000000)},
		{"Pay ₹5L now", f(500000)},
		{"Pay Rs. 1.2 Cr", f(12000000)},
		{"GST 18% on the bill", nil},
		{"Rs.78 paid", f(78)},
	}
	for _, c := range cases {
		got := LargestAmount(c.text)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%q: got %v want %v", c.text, deref(got), deref(c.want))
		}
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestPolicyValidationAndCRUD(t *testing.T) {
	s := newStore(t)
	bad := []Policy{
		{ApproverRoles: nil},
		{ApproverRoles: []string{"finance"}, MinApprovers: 3},
		{ApproverRoles: []string{"finance"}, ActionMatch: "(unclosed"},
		{ApproverRoles: []string{"finance"}, AmountOver: f(-1)},
		{ApproverRoles: []string{"Bad Role"}},
	}
	for i, p := range bad {
		if _, err := s.SavePolicy(p); err == nil {
			t.Errorf("bad policy %d accepted", i)
		}
	}

	p, err := s.SavePolicy(Policy{ActionMatch: `\b(pay|send)\b`, ApproverRoles: []string{"Finance"}, Note: "money out"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" || p.Worker != "*" || p.MinApprovers != 1 || p.ApproverRoles[0] != "finance" {
		t.Fatalf("defaults: %+v", p)
	}
	p.AmountOver = f(1000)
	p.MinApprovers = 2
	p2, err := s.SavePolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	if p2.AmountOver == nil || *p2.AmountOver != 1000 || p2.MinApprovers != 2 {
		t.Fatalf("update: %+v", p2)
	}
	if _, err := s.SavePolicy(Policy{ID: "nope", ApproverRoles: []string{"x"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	list, _ := s.Policies()
	if len(list) != 1 {
		t.Fatalf("policies = %d", len(list))
	}
	if err := s.DeletePolicy(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePolicy(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestRequiredMatching(t *testing.T) {
	s := newStore(t)
	money, _ := s.SavePolicy(Policy{ActionMatch: `\b(pay|transfer)\b`, AmountOver: f(1000), ApproverRoles: []string{"finance"}})
	mail, _ := s.SavePolicy(Policy{Worker: "desk-a", ActionMatch: `\bsend\b`, ApproverRoles: []string{"lead"}})
	role, _ := s.SavePolicy(Policy{WorkerRole: "filing", ActionMatch: "submit|file", ApproverRoles: []string{"partner"}, MinApprovers: 2})

	ids := func(r Requirement) string {
		var out []string
		for _, p := range r.Policies {
			out = append(out, p.ID)
		}
		return strings.Join(out, ",")
	}

	cases := []struct {
		worker string
		roles  []string
		text   string
		want   string
		min    int
	}{
		{"desk-b", nil, "Approval needed before I click Pay: pays $5,000.", money.ID, 1},
		{"desk-b", nil, "Approval needed before I click Pay: pays $500.", "", 0},
		{"desk-b", nil, "Approval needed before I PAY the bill", money.ID, 1}, // no figure: the stricter reading
		{"desk-a", nil, "Approval needed before I send the email", mail.ID, 1},
		{"DESK-A", nil, "approval needed before I send", mail.ID, 1},
		{"desk-b", nil, "Approval needed before I send the email", "", 0},
		{"desk-b", []string{"filing"}, "Approval needed before I submit the return", role.ID, 2},
		{"desk-b", []string{"other"}, "Approval needed before I submit the return", "", 0},
		{"desk-a", []string{"filing"}, "send and pay USD 2000, then file", money.ID + "," + mail.ID + "," + role.ID, 2},
	}
	for _, c := range cases {
		req, err := s.Required(c.worker, c.roles, c.text)
		if err != nil {
			t.Fatal(err)
		}
		if ids(req) != c.want || req.MinApprovers != c.min {
			t.Errorf("%s %v %q: got %q/%d want %q/%d", c.worker, c.roles, c.text, ids(req), req.MinApprovers, c.want, c.min)
		}
	}
}

func TestCanApprove(t *testing.T) {
	req := Requirement{Policies: []Policy{
		{ApproverRoles: []string{"finance", "treasury"}},
		{ApproverRoles: []string{"lead"}},
	}}
	cases := []struct {
		u    User
		want bool
	}{
		{LocalOwner, true},
		{User{ID: "u1", Roles: []string{"owner"}}, true},
		{User{ID: "u1", Roles: []string{"finance", "lead"}}, true},
		{User{ID: "u1", Roles: []string{"treasury", "lead"}}, true},
		{User{ID: "u1", Roles: []string{"finance"}}, false},
		{User{ID: "u1", Roles: []string{"finance", "lead"}, Disabled: true}, false},
		{User{}, false},
	}
	for i, c := range cases {
		if got := CanApprove(c.u, req); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
	if !CanApprove(User{}, Requirement{}) {
		t.Error("with no policy anybody who can reply may")
	}
}

func TestClassifyReply(t *testing.T) {
	cases := map[string]string{
		"yes":                      ReplyYes,
		"Yes.":                     ReplyYes,
		"ok, go ahead":             ReplyYes,
		"Approve":                  ReplyYes,
		"no":                       ReplyNo,
		"No!":                      ReplyNo,
		"don't":                    ReplyNo,
		"do not do it":             ReplyNo,
		"cancel please":            ReplyNo,
		"no, pay 900 to B instead": ReplyOther,
		"yes but don't send it":    ReplyOther,
		"use the other account":    ReplyOther,
		"":                         ReplyOther,
	}
	for in, want := range cases {
		if got := ClassifyReply(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

func TestDecide(t *testing.T) {
	s := newStore(t)
	_, _ = s.SavePolicy(Policy{ActionMatch: `\bpay\b`, AmountOver: f(1000), ApproverRoles: []string{"finance"}})
	_, _ = s.SavePolicy(Policy{ActionMatch: `\bdelete\b`, ApproverRoles: []string{"admin"}, MinApprovers: 2})

	fin, _ := s.CreateUser("fin", "password-1", []string{"finance"})
	clerk, _ := s.CreateUser("clerk", "password-1", []string{"clerk"})
	a1, _ := s.CreateUser("admin1", "password-1", []string{"admin"})
	a2, _ := s.CreateUser("admin2", "password-1", []string{"admin"})

	pay := "Approval needed before I click Pay: pays $5,000 to the supplier."

	t.Run("no policy applies", func(t *testing.T) {
		d, err := s.Decide(User{}, "w1", nil, "t0", "Approval needed before I click Save", "yes")
		if err != nil || !d.Allowed || d.Outcome != OutcomeAllowed {
			t.Fatalf("%+v %v", d, err)
		}
	})

	t.Run("lacking role is refused", func(t *testing.T) {
		d, _ := s.Decide(clerk, "w1", nil, "t1", pay, "yes")
		if d.Allowed || d.Outcome != OutcomeRefused || d.Message != "This needs approval from a finance approver." {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("not signed in is refused with a hint", func(t *testing.T) {
		d, _ := s.Decide(User{}, "w1", nil, "t1", pay, "yes")
		if d.Allowed || !strings.Contains(d.Message, "Sign in") {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("plain no from anyone is delivered", func(t *testing.T) {
		d, _ := s.Decide(clerk, "w1", nil, "t1", pay, "no")
		if !d.Allowed || d.RecordedAs != ReplyNo {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("an instruction needs the role", func(t *testing.T) {
		d, _ := s.Decide(clerk, "w1", nil, "t1", pay, "no, pay 9000 to someone else")
		if d.Allowed || d.RecordedAs != ReplyOther {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("approver allowed", func(t *testing.T) {
		d, _ := s.Decide(fin, "w1", nil, "t1", pay, "yes")
		if !d.Allowed {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("local owner allowed", func(t *testing.T) {
		d, _ := s.Decide(LocalOwner, "w1", nil, "t1", pay, "yes")
		if !d.Allowed {
			t.Fatalf("%+v", d)
		}
	})

	del := "Approval needed before I click Delete: removes the records."

	t.Run("two approvers", func(t *testing.T) {
		d, _ := s.Decide(a1, "w2", nil, "t2", del, "yes")
		if d.Allowed || d.Outcome != OutcomeHeld || !strings.Contains(d.Message, "second approver") {
			t.Fatalf("first yes: %+v", d)
		}
		d, _ = s.Decide(a1, "w2", nil, "t2", del, "yes")
		if d.Allowed || d.Outcome != OutcomeRefused || !strings.Contains(d.Message, "already approved") {
			t.Fatalf("same approver again: %+v", d)
		}
		d, _ = s.Decide(a2, "w2", nil, "t2", del, "do something else")
		if d.Allowed {
			t.Fatalf("instruction under two approvers: %+v", d)
		}
		d, _ = s.Decide(fin, "w2", nil, "t2", del, "yes")
		if d.Allowed {
			t.Fatalf("non-approver as second: %+v", d)
		}
		d, _ = s.Decide(a2, "w2", nil, "t2", del, "yes")
		if !d.Allowed {
			t.Fatalf("second distinct approver: %+v", d)
		}
		// Settled: the next time the same action is asked, it starts over.
		d, _ = s.Decide(a2, "w2", nil, "t2", del, "yes")
		if d.Allowed || d.Outcome != OutcomeHeld {
			t.Fatalf("new round: %+v", d)
		}
		// A held yes on another task does not count here.
		d, _ = s.Decide(a1, "w2", nil, "t3", del, "yes")
		if d.Allowed {
			t.Fatalf("other task: %+v", d)
		}
	})

	t.Run("worker roles stored are used", func(t *testing.T) {
		_, _ = s.SavePolicy(Policy{WorkerRole: "restricted", ApproverRoles: []string{"lead"}})
		_ = s.SetWorkerRoles("w9", []string{"restricted"})
		d, _ := s.Decide(clerk, "w9", nil, "t9", "Approval needed before I click Save", "yes")
		if d.Allowed {
			t.Fatalf("%+v", d)
		}
		d, _ = s.Decide(clerk, "w8", nil, "t9", "Approval needed before I click Save", "yes")
		if !d.Allowed {
			t.Fatalf("%+v", d)
		}
	})

	log, err := s.Approvals("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) < 15 {
		t.Fatalf("log has %d entries", len(log))
	}
	var sawAnon, sawHeld, sawRefused bool
	for _, a := range log {
		switch {
		case a.DecidedBy == anonymousID:
			sawAnon = true
		case a.Outcome == OutcomeHeld && a.DecidedBy == a1.ID && a.DecidedByName == "admin1":
			sawHeld = true
		case a.Outcome == OutcomeRefused && a.DecidedBy == clerk.ID:
			sawRefused = true
		}
	}
	if !sawAnon || !sawHeld || !sawRefused {
		t.Fatalf("log missing entries: anon=%v held=%v refused=%v", sawAnon, sawHeld, sawRefused)
	}
	w2, _ := s.Approvals("w2", 0)
	for _, a := range w2 {
		if a.Worker != "w2" {
			t.Fatal("worker filter")
		}
	}
	if err := s.RecordDecision(fin, "w5", "t5", "something", ReplyYes, OutcomeAllowed); err != nil {
		t.Fatal(err)
	}
}

// --- handlers ---

func newHandler(t *testing.T, gated bool) (*Handler, *http.ServeMux) {
	s := newStore(t)
	h := &Handler{Store: s, Middleware: Middleware{Store: s, Gated: func(*http.Request) bool { return gated }}}
	mux := http.NewServeMux()
	h.RegisterAuth(mux)
	h.RegisterAdmin(mux, nil)
	return h, mux
}

func do(mux http.Handler, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestGatedLoginFlow(t *testing.T) {
	h, mux := newHandler(t, true)
	_, _ = h.Store.CreateUser("asha", "password-1", []string{"finance"})

	if rec := do(mux, http.MethodGet, "/api/auth/me", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me before login: %d", rec.Code)
	}
	if rec := do(mux, http.MethodPost, "/api/auth/login", map[string]string{"name": "asha", "password": "wrong-one"}); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "do not match") {
		t.Fatalf("bad login: %d %s", rec.Code, rec.Body)
	}

	// A form post (what a cross-site page could send) is refused.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("name=asha&password=password-1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form login: %d", rec.Code)
	}

	rec = do(mux, http.MethodPost, "/api/auth/login", map[string]string{"name": "asha", "password": "password-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Value == "" {
		t.Fatalf("cookie: %+v", cookie)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatal("login response mentions the password hash")
	}

	rec = do(mux, http.MethodGet, "/api/auth/me", nil, cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"asha"`) {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}

	// Admin routes refuse the gated listener even when registered there.
	if rec := do(mux, http.MethodGet, "/api/access/users", nil, cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("admin on gated: %d", rec.Code)
	}

	if rec := do(mux, http.MethodPost, "/api/auth/logout", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := do(mux, http.MethodGet, "/api/auth/me", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", rec.Code)
	}
}

func TestPrimaryIsLocalOwner(t *testing.T) {
	_, mux := newHandler(t, false)
	rec := do(mux, http.MethodGet, "/api/auth/me", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), LocalOwnerID) {
		t.Fatalf("me on primary: %d %s", rec.Code, rec.Body)
	}
	rec = do(mux, http.MethodPost, "/api/auth/login", map[string]string{"name": "x", "password": "y"})
	if rec.Code != http.StatusOK || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("login on primary: %d %v", rec.Code, rec.Header())
	}
}

func TestAdminRoutes(t *testing.T) {
	_, mux := newHandler(t, false)

	rec := do(mux, http.MethodPost, "/api/access/users/create", map[string]any{"name": "asha", "password": "password-1", "roles": []string{"finance"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct{ User User }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec := do(mux, http.MethodPost, "/api/access/users/create", map[string]any{"name": "asha", "password": "password-1"}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	if rec := do(mux, http.MethodPost, "/api/access/users/create", map[string]any{"name": "b", "password": "x"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "8 to 256") {
		t.Fatalf("short pw: %d %s", rec.Code, rec.Body)
	}

	rec = do(mux, http.MethodPost, "/api/access/users/update", map[string]any{"id": created.User.ID, "roles": []string{"lead", "finance"}, "disabled": true})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"disabled":true`) || !strings.Contains(rec.Body.String(), `"finance","lead"`) {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	if rec := do(mux, http.MethodPost, "/api/access/users/update", map[string]any{"id": "missing"}); rec.Code != http.StatusNotFound {
		t.Fatalf("update missing: %d", rec.Code)
	}

	rec = do(mux, http.MethodGet, "/api/access/users", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "pbkdf2") {
		t.Fatalf("list leaks hash or failed: %d %s", rec.Code, rec.Body)
	}

	rec = do(mux, http.MethodPost, "/api/access/policies/save", map[string]any{"action_match": "pay", "amount_over": 1000, "approver_roles": []string{"finance"}, "min_approvers": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("save policy: %d %s", rec.Code, rec.Body)
	}
	var saved struct{ Policy Policy }
	_ = json.Unmarshal(rec.Body.Bytes(), &saved)
	if rec := do(mux, http.MethodPost, "/api/access/policies/save", map[string]any{"action_match": "(", "approver_roles": []string{"finance"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad regex: %d", rec.Code)
	}

	rec = do(mux, http.MethodPost, "/api/access/policies/check", map[string]any{"worker": "w1", "action": "pay $2,000"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), saved.Policy.ID) || !strings.Contains(rec.Body.String(), "a finance approver") {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}

	rec = do(mux, http.MethodPost, "/api/access/workers/roles", map[string]any{"worker": "w1", "roles": []string{"Filing"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"filing"`) {
		t.Fatalf("worker roles: %d %s", rec.Code, rec.Body)
	}
	if rec := do(mux, http.MethodGet, "/api/access/workers", nil); !strings.Contains(rec.Body.String(), `"w1"`) {
		t.Fatalf("workers: %s", rec.Body)
	}

	if rec := do(mux, http.MethodGet, "/api/access/approvals?limit=5", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"approvals":[]`) {
		t.Fatalf("approvals: %d %s", rec.Code, rec.Body)
	}
	if rec := do(mux, http.MethodGet, "/api/access/policies", nil); !strings.Contains(rec.Body.String(), saved.Policy.ID) {
		t.Fatalf("policies: %s", rec.Body)
	}
	if rec := do(mux, http.MethodPost, "/api/access/policies/delete", map[string]any{"id": saved.Policy.ID}); rec.Code != http.StatusOK {
		t.Fatalf("delete policy: %d", rec.Code)
	}
	if rec := do(mux, http.MethodPost, "/api/access/users/delete", map[string]any{"id": created.User.ID}); rec.Code != http.StatusOK {
		t.Fatalf("delete user: %d", rec.Code)
	}
	if rec := do(mux, http.MethodGet, "/api/access/users/create", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET create: %d", rec.Code)
	}
}

func TestMessageNeverEchoesUnknownErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	storeError(rec, errors.New("sql: secret internal detail"))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
