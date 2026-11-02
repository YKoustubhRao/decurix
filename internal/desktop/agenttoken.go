package desktop

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
)

// A desktop's agent listens on ports of its own (8081, soul's 4900, the browser extension's 8092) for what the
// control plane sends it: requests, a person's replies, approvals. Every desktop shares the Docker network and every
// page in a desktop's browser can send to localhost, so without a check another desktop or a web page could answer
// an approval in a person's name (review 2026-11-02). AgentToken is the check: a secret per desktop, derived from the
// control plane's own secret so nothing new is stored, handed to the desktop at creation (DECURIX_AGENT_TOKEN) and
// sent by the control plane as a bearer token on every call to it. A desktop created before this has no token, and
// its agent accepts calls as before until it is recreated (desktop state is fixed at creation).

// AgentTokenEnv is the environment variable a desktop's agent reads its token from.
const AgentTokenEnv = "DECURIX_AGENT_TOKEN"

// AgentToken is the token of desktop id's agent, "" when the control plane has no secret to derive it from.
func (m *Manager) AgentToken(id string) string {
	if m == nil || len(m.HostGUISecret) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, m.HostGUISecret)
	mac.Write([]byte("decurix-agent-token\x00" + sanitizeName(id)))
	return hex.EncodeToString(mac.Sum(nil))
}

// AuthorizeAgentRequest adds desktop id's agent token to req.
func (m *Manager) AuthorizeAgentRequest(req *http.Request, id string) {
	if token := m.AgentToken(id); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// screenPassword is the VNC password a new desktop gets: its own, derived like its agent token, so one desktop cannot
// open another's screen over the shared network (they all had "admin"). VNC's own authentication reads 8 characters,
// so it is 8 base64 characters (48 bits). DEFAULT_PASSWORD in the environment still sets one for every desktop, and
// with no secret the old default stays.
func (m *Manager) screenPassword(id string) string {
	if v := os.Getenv("DEFAULT_PASSWORD"); v != "" {
		return v
	}
	if m == nil || len(m.HostGUISecret) == 0 {
		return "admin"
	}
	mac := hmac.New(sha256.New, m.HostGUISecret)
	mac.Write([]byte("decurix-screen-password\x00" + sanitizeName(id)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:8]
}

// withScreenPassword sets env's VNC_PW to desktop id's own screen password.
func (m *Manager) withScreenPassword(env []string, id string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "VNC_PW=") {
			out = append(out, kv)
		}
	}
	return append(out, "VNC_PW="+m.screenPassword(id))
}
