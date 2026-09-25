package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/keepawake"
	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// emptyMailbox is a mailbox with nothing in it; a login as
// "refused@gmail.com" is refused the way Gmail refuses a wrong password.
type emptyMailbox struct{}

func (emptyMailbox) Examine(string) (uint32, uint32, error)      { return 7, 100, nil }
func (emptyMailbox) UIDsAfter(uint32) ([]uint32, error)          { return nil, nil }
func (emptyMailbox) Headers([]uint32) (map[uint32][]byte, error) { return map[uint32][]byte{}, nil }
func (emptyMailbox) Text(uint32) ([]byte, error)                 { return nil, nil }
func (emptyMailbox) Close()                                      {}

func mailServer(t *testing.T) *Server {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := mailtrigger.NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}

	runner := &mailtrigger.Runner{Store: store, Dial: func(ctx context.Context, acct mailtrigger.Account) (mailtrigger.Mailbox, error) {
		if acct.Email == "refused@gmail.com" {
			return nil, fmt.Errorf("%w ([AUTHENTICATIONFAILED] Invalid credentials)", mailtrigger.ErrAuth)
		}

		return emptyMailbox{}, nil
	}}

	return &Server{Mail: store, MailRunner: runner, Desktops: fakeDesktopManager(t, "bordu")}
}

func TestConnectMailboxNeverEchoesThePassword(t *testing.T) {
	s := mailServer(t)

	rec := postForm(t, s.connectMailHandler, "/api/mail/accounts/connect",
		"email=me%40gmail.com&password=abcd+efgh+ijkl+mnop")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "abcd") {
		t.Fatalf("the password came back: %s", rec.Body.String())
	}

	state := httptest.NewRecorder()
	s.mailStateHandler(state, httptest.NewRequest(http.MethodGet, "/api/mail", nil))

	if strings.Contains(state.Body.String(), "abcd") {
		t.Fatalf("the password is in /api/mail: %s", state.Body.String())
	}

	var got struct {
		Accounts []mailtrigger.Account `json:"accounts"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if len(got.Accounts) != 1 || got.Accounts[0].Host != "imap.gmail.com" || got.Accounts[0].Status != "connected" {
		t.Errorf("accounts = %+v", got.Accounts)
	}
}

func TestConnectMailboxExplainsAnAppPasswordWhenGmailRefuses(t *testing.T) {
	s := mailServer(t)

	rec := postForm(t, s.connectMailHandler, "/api/mail/accounts/connect",
		"email=refused%40gmail.com&password=hunter2")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "App passwords") {
		t.Errorf("body = %s", rec.Body.String())
	}

	if accounts, _ := s.Mail.Accounts(); len(accounts) != 0 {
		t.Error("a refused login was saved")
	}
}

func TestCreateMailRuleChecksTheDesktopAndPattern(t *testing.T) {
	s := mailServer(t)

	postForm(t, s.connectMailHandler, "/api/mail/accounts/connect", "email=me%40gmail.com&password=x")
	accounts, _ := s.Mail.Accounts()
	acct := accounts[0].ID

	for body, want := range map[string]int{
		"account_id=" + acct + "&desktop_id=nope&subject_re=x&content=do+it":           http.StatusBadRequest,
		"account_id=" + acct + "&desktop_id=bordu&subject_re=%28open&content=do+it":    http.StatusBadRequest,
		"account_id=" + acct + "&desktop_id=bordu&subject_re=%5Einvoice&content=do+it": http.StatusOK,
	} {
		if rec := postForm(t, s.createMailRuleHandler, "/api/mail/rules/create", body); rec.Code != want {
			t.Errorf("%s: status = %d, want %d (%s)", body, rec.Code, want, rec.Body.String())
		}
	}
}

func TestKeepAwakeSaysWhetherTheHostShouldStayAwake(t *testing.T) {
	t.Setenv("DECURIX_KEEP_AWAKE", "")

	s := &Server{KeepAwake: &keepawake.Keeper{Wanted: func() []string { return []string{"mailboxes are watched"} }}}

	rec := httptest.NewRecorder()
	s.keepAwakeHandler(rec, httptest.NewRequest(http.MethodGet, "/api/keep-awake", nil))

	var got keepawake.State
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if !got.Wanted || got.Reasons[0] != "mailboxes are watched" {
		t.Errorf("state = %+v", got)
	}

	t.Setenv("DECURIX_KEEP_AWAKE", "0")

	rec = httptest.NewRecorder()
	s.keepAwakeHandler(rec, httptest.NewRequest(http.MethodGet, "/api/keep-awake", nil))

	if strings.Contains(rec.Body.String(), `"wanted":true`) {
		t.Errorf("DECURIX_KEEP_AWAKE=0 still wants the machine awake: %s", rec.Body.String())
	}
}

func TestTheMailboxIsNotAvailableToDesktops(t *testing.T) {
	s := mailServer(t)
	s.nets.subnets = []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}
	s.nets.gateways = []netip.Addr{netip.MustParseAddr("172.18.0.1")}
	s.nets.fetched = time.Now()

	handler := s.humansOnly(s.mailStateHandler)

	for addr, want := range map[string]int{
		"127.0.0.1:50000":           http.StatusOK,        // the binary on the host: a browser on loopback
		"172.18.0.1:50000":          http.StatusOK,        // compose: the browser, through the published port
		"192.168.1.20:50000":        http.StatusOK,        // a phone on the LAN listener
		"172.18.0.5:50000":          http.StatusForbidden, // a desktop
		"[::ffff:172.18.0.9]:50000": http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/mail", nil)
		req.RemoteAddr = addr

		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != want {
			t.Errorf("%s: status = %d, want %d", addr, rec.Code, want)
		}
	}
}

func TestTheEmailPageIsPartOfTheTriggersPage(t *testing.T) {
	s := mailServer(t)
	s.Web = webui.NewRenderer()
	s.nets.subnets = []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}
	s.nets.gateways = []netip.Addr{netip.MustParseAddr("172.18.0.1")}
	s.nets.fetched = time.Now()

	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	get := func(path, addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	page := get("/triggers", "127.0.0.1:50000")
	if page.Code != http.StatusOK {
		t.Fatalf("/triggers: status = %d", page.Code)
	}
	for _, want := range []string{"Triggers", `id="schedList"`, `id="ruleList"`, `id="accountList"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("/triggers does not show %s", want)
		}
	}

	moved := get("/email", "127.0.0.1:50000")
	if moved.Code != http.StatusMovedPermanently || moved.Header().Get("Location") != "/triggers#email" {
		t.Errorf("/email: %d to %q, want a permanent redirect to /triggers#email", moved.Code, moved.Header().Get("Location"))
	}

	// It holds the mailbox setup, so a desktop may not load it either.
	if code := get("/triggers", "172.18.0.5:50000").Code; code != http.StatusForbidden {
		t.Errorf("/triggers from a desktop: status = %d, want 403", code)
	}
}
