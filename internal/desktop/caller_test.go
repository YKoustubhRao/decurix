package desktop

import (
	"net/http/httptest"
	"testing"
)

// Each desktop's agent token is its own, stable across restarts of the control plane, and absent without a secret.
func TestAgentToken(t *testing.T) {
	m := &Manager{HostGUISecret: []byte("secret")}
	a, b := m.AgentToken("box-a"), m.AgentToken("box-b")
	if a == "" || a == b || len(a) != 64 {
		t.Fatalf("tokens: %q %q", a, b)
	}
	if again := (&Manager{HostGUISecret: []byte("secret")}).AgentToken("box-a"); again != a {
		t.Error("the token changed with a new Manager on the same secret")
	}
	if m.AgentToken("Box A") != m.AgentToken("box-a") {
		t.Error("the token is not keyed by the desktop ID the agent knows itself as")
	}
	if (&Manager{}).AgentToken("box-a") != "" {
		t.Error("a token without a secret")
	}
	req := httptest.NewRequest("POST", "http://10.0.0.2:8081/request", nil)
	m.AuthorizeAgentRequest(req, "box-a")
	if req.Header.Get("Authorization") != "Bearer "+a {
		t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
	}
}

func TestDesktopIDFromNames(t *testing.T) {
	if got := desktopIDFromNames([]string{"/desktop_box-a"}); got != "box-a" {
		t.Errorf("got %q", got)
	}
	if got := desktopIDFromNames([]string{"/decurix"}); got != "" {
		t.Errorf("the control plane's own container was taken for a desktop: %q", got)
	}
}

// Each new desktop's screen has a password of its own; DEFAULT_PASSWORD (once "admin" for all) is not read.
func TestScreenPasswordIsPerDesktop(t *testing.T) {
	t.Setenv("DEFAULT_PASSWORD", "admin")
	m := &Manager{HostGUISecret: []byte("secret")}
	a, b := m.screenPassword("box-a"), m.screenPassword("box-b")
	if a == b || len(a) != 8 || a == "admin" {
		t.Fatalf("passwords %q %q", a, b)
	}
	env := m.withScreenPassword([]string{"VNC_PW=admin", "X=1"}, "box-a")
	if len(env) != 2 || env[1] != "VNC_PW="+a {
		t.Errorf("env = %v", env)
	}
}

// No secret, no desktop: it would have no agent token and a guessable screen.
func TestNoDesktopWithoutTheSecret(t *testing.T) {
	m := &Manager{}
	if _, err := m.BeginProvisioning("box", "basic", Brain{}, 1, 2048, "", "", nil, nil, nil); err == nil {
		t.Fatal("a desktop was begun without the control plane's secret")
	}
}
