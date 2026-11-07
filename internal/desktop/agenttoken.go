package desktop

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
)

// A desktop's agent listens on ports of its own (8081, soul's 4900) for what the control plane sends it: requests, a
// person's replies, approvals. Every page in a desktop's browser can send to localhost, and a desktop made before
// desktops had networks of their own (desknet.go) still shares decurix-network with the others, so without a check a
// web page or another desktop could answer an approval in a person's name (review 2026-11-02). The gate's own :8083
// has a check of its own (a reply is signed with the desktop's gate key, GateKey). AgentToken is the check for the
// agent's ports: a secret per desktop, derived from the control plane's own secret so nothing new is stored, handed
// to the desktop at creation (DECURIX_AGENT_TOKEN) and sent by the control plane as a bearer token on every call to
// it. A desktop created before this has no token, and
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
// open another's screen over the shared network (they all had "admin", and .env.example's DEFAULT_PASSWORD kept it
// that way - it is not read any more). VNC's own authentication reads 8 characters, so it is 8 base64 characters
// (48 bits). BeginProvisioning refuses to make a desktop without the secret, so there is no shared fallback.
func (m *Manager) screenPassword(id string) string {
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

// GateKeyFile is where a new desktop's approval key is put before its first start: a root-only file (0400 in a 0700
// directory) that the entrypoint moves into the gate's own directory (agent/decurix-entrypoint.sh). It is never in the
// container's environment: every `docker exec` inherits that, and a worker that can get the control plane to exec as
// it (the memory route) could read the key from there (round-3 recheck).
const GateKeyFile = "/etc/decurix-gate/key"

// GateRequiredEnv is set to "1" in every desktop the control plane makes (not a secret): the agent and TheBot then
// treat a missing or silent gate as one that allows nothing, never as a desktop without a gate whose decisions live in
// a store the worker can write (round-4 recheck). It can only turn the gate on.
const GateRequiredEnv = "DECURIX_GATE_REQUIRED"

// GateUser is the Linux user an image's approval gate runs as. An image without it has no gate and makes no desktop.
const GateUser = "decurixgate"

// GatePort is where a desktop's approval gate listens for the control plane's signed replies.
const GatePort = "8083"

// GateKey is the key a desktop's approval gate verifies a person's reply with: derived from the control plane's secret
// with a label of its own, so it is not the agent token (which the worker's environment holds) and cannot be made from
// it. "" when the control plane has no secret.
func (m *Manager) GateKey(id string) string {
	if m == nil || len(m.HostGUISecret) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, m.HostGUISecret)
	mac.Write([]byte("decurix-gate-key\x00" + sanitizeName(id)))
	return hex.EncodeToString(mac.Sum(nil))
}
