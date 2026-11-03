package desktop

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A person's reply to a question or an approval reaches the worker's agent as a request like any other, and the agent
// runs as the same user as the worker's shell, so nothing it does with the reply can be trusted as the person's. What
// makes a yes is this: the control plane signs the reply with the desktop's gate key (GateKey) and sends it to the
// approval gate, a process of another user in the same container (docs/gate-boundary.md). The gate verifies it and
// alone creates grants. A reply that reaches the agent without being signed to the gate is only text.

// SignGateReply is the signature the gate checks: over the task, the time, a one-use nonce, digests of the reply and
// of the question it answers, its kind and the id of the gate's request it answers ("" for any other question).
func SignGateReply(key, taskID, ts, nonce, text, asked, kind, requestID string) string {
	th, ah := sha256.Sum256([]byte(GateText(text))), sha256.Sum256([]byte(GateText(asked)))
	return gateMAC(key, "reply", taskID, ts, nonce, hex.EncodeToString(th[:]), hex.EncodeToString(ah[:]), kind, requestID)
}

// gateMAC is the HMAC-SHA256 of parts, each NUL-terminated, with key (gate_server._mac).
func gateMAC(key string, parts ...string) string {
	mac := hmac.New(sha256.New, []byte(key))
	for _, part := range parts {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyGateQuestion reports whether question is desktop id's approval gate's own wording of its request requestID:
// the gate signs the question it puts to a person (gate_server.sign_question), and only then does the control plane
// show those words and bind the person's reply to that request. A question the worker wrote cannot carry this.
func (m *Manager) VerifyGateQuestion(id, requestID, question, sig string) bool {
	key := m.GateKey(id)
	if key == "" || requestID == "" || question == "" || sig == "" {
		return false
	}
	qh := sha256.Sum256([]byte(strings.Join(strings.Fields(question), " ")))
	want := gateMAC(key, "question", requestID, hex.EncodeToString(qh[:]))
	return hmac.Equal([]byte(want), []byte(sig))
}

// SendGateReply gives desktop id's approval gate the person's reply, signed. asked is the text of the question the
// reply answers, kind its kind ("approval" or "query") and requestID the gate's request it answers, when the question
// was the gate's own (VerifyGateQuestion); the gate settles nothing for a reply without one. A desktop without a gate (made before it existed) answers
// nothing here; the error says so and the caller goes on, since that desktop still settles replies in its agent.
func (m *Manager) SendGateReply(ctx context.Context, id, taskID, text, asked, kind, requestID string) error {
	key := m.GateKey(id)
	if key == "" {
		return fmt.Errorf("no secret to sign with")
	}
	desktopID := sanitizeName(id)
	name := "desktop_" + desktopID
	inspect, err := m.client.ContainerInspect(ctx, name)
	if err != nil {
		return fmt.Errorf("find desktop %s: %w", desktopID, ErrNotFound)
	}
	host := name
	if inspect.NetworkSettings != nil {
		if n, ok := inspect.NetworkSettings.Networks[NetworkName]; ok && n != nil && n.IPAddress != "" {
			host = n.IPAddress
		}
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	n := hex.EncodeToString(nonce)
	body, err := json.Marshal(map[string]string{
		"task_id": taskID, "ts": ts, "nonce": n, "text": GateText(text), "asked": GateText(asked), "kind": kind, "request_id": requestID,
		"sig": SignGateReply(key, taskID, ts, n, text, asked, kind, requestID),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+":"+GatePort+"/reply", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	cli := m.httpClient
	if cli == nil {
		cli = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("send reply to desktop %s's gate: %w", desktopID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("desktop %s's gate returned HTTP %d", desktopID, resp.StatusCode)
	}
	return nil
}

// SignGateRequest is the signature on a request the control plane gives the gate (/request): its own label, so a
// signed request can never stand as a signed reply, nor a reply as a request (gate_server.sign_request).
func SignGateRequest(key, taskID, ts, nonce, text string) string {
	th := sha256.Sum256([]byte(text))
	return gateMAC(key, "request", taskID, ts, nonce, hex.EncodeToString(th[:]))
}

// SignGateSecrets is the signature on a task's secret values given to the gate (/secrets): its own label, over the
// SHA-256 of each value's SHA-256 followed by a NUL, in order, so nothing of JSON has to match across the two
// languages (gate_server.sign_secrets).
func SignGateSecrets(key, taskID, ts, nonce string, values []string) string {
	var digests strings.Builder
	for _, v := range values {
		h := sha256.Sum256([]byte(v))
		digests.WriteString(hex.EncodeToString(h[:]))
		digests.WriteByte(0)
	}
	d := sha256.Sum256([]byte(digests.String()))
	return gateMAC(key, "secrets", taskID, ts, nonce, hex.EncodeToString(d[:]))
}

// SendGateRequest gives desktop id's approval gate its own copy of a new request, signed, so whether a site is named
// is decided from that copy and never from a file the worker could write (docs/gate-boundary.md). text is the
// request as the person sent it, or its withheld form for one marked secret (the gate then names no site for it).
func (m *Manager) SendGateRequest(ctx context.Context, id, taskID, text string) error {
	text = GateText(text)
	return m.postToGate(ctx, id, "/request", func(key, ts, nonce string) map[string]any {
		return map[string]any{"task_id": taskID, "ts": ts, "nonce": nonce, "text": text,
			"sig": SignGateRequest(key, taskID, ts, nonce, text)}
	})
}

// SendGateSecrets gives desktop id's approval gate the values the records withheld from a task's request or reply,
// signed, so the gate's own wording of an approval question withholds them (C14).
func (m *Manager) SendGateSecrets(ctx context.Context, id, taskID string, values []string) error {
	clean := make([]string, len(values))
	for i, v := range values {
		clean[i] = GateText(v)
	}
	values = clean
	return m.postToGate(ctx, id, "/secrets", func(key, ts, nonce string) map[string]any {
		return map[string]any{"task_id": taskID, "ts": ts, "nonce": nonce, "secrets": values,
			"sig": SignGateSecrets(key, taskID, ts, nonce, values)}
	})
}

// ErrGateNotTold is a desktop's approval gate that could not be given what it must know before the agent has a
// request (its copy, the withheld values): the request is then not sent at all.
var ErrGateNotTold = errors.New("the approval gate could not be told what was asked")

// GateText is s as it is signed and sent to a gate: each byte that is not valid UTF-8 replaced by U+FFFD, exactly as
// encoding/json writes it into the body, so the gate's hash of the string it decodes is the hash that was signed
// (review CP-A N6). The Python gate does nothing of its own: it hashes the string it received.
func GateText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// GateLabel is the label the control plane puts on every desktop it creates with an approval gate.
const GateLabel = "decurix.gate"

// HasGate reports whether desktop id runs an approval gate, from what the control plane itself wrote: the label set
// at create, and the key it signs with. "0" (a desktop the control plane made without one) is the only "no"; a
// desktop with no label (made before the label) is taken as gated (fail closed), never decided by a file inside the
// desktop, which its worker could rewrite (review CP-A-3 N2).
func (m *Manager) HasGate(ctx context.Context, id string) bool {
	if m.GateKey(id) == "" {
		return false
	}
	inspect, err := m.client.ContainerInspect(ctx, "desktop_"+sanitizeName(id))
	if err != nil || inspect.Config == nil {
		return true
	}
	return inspect.Config.Labels[GateLabel] != "0"
}

// postToGate posts one signed body to desktop id's gate at path: body makes it from the key, the time and a fresh
// one-use nonce. A stopped desktop is ErrNotRunning, and a paused one is woken, as SendRequest does; a gate that
// does not answer is ErrGateNotTold and ErrAgentUnreachable (the caller may wait for a desktop just started), and
// one that answers anything but 2xx is ErrGateNotTold.
func (m *Manager) postToGate(ctx context.Context, id, path string, body func(key, ts, nonce string) map[string]any) error {
	key := m.GateKey(id)
	if key == "" {
		return fmt.Errorf("no secret to sign with")
	}
	desktopID := sanitizeName(id)
	name := "desktop_" + desktopID
	inspect, err := m.client.ContainerInspect(ctx, name)
	if err != nil {
		return fmt.Errorf("find desktop %s: %w", desktopID, ErrNotFound)
	}
	if inspect.State == nil || !inspect.State.Running {
		return fmt.Errorf("desktop %s: %w", desktopID, ErrNotRunning)
	}
	if inspect.State.Paused {
		if err := m.client.ContainerUnpause(ctx, name); err != nil {
			return fmt.Errorf("auto-wake desktop %s: %w", desktopID, err)
		}
	}
	host := name
	if inspect.NetworkSettings != nil {
		if n, ok := inspect.NetworkSettings.Networks[NetworkName]; ok && n != nil && n.IPAddress != "" {
			host = n.IPAddress
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	payload, err := json.Marshal(body(key, strconv.FormatInt(time.Now().Unix(), 10), hex.EncodeToString(raw)))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+":"+GatePort+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	cli := m.httpClient
	if cli == nil {
		cli = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w: send %s to desktop %s's gate: %v", ErrGateNotTold, ErrAgentUnreachable, path, desktopID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: desktop %s's gate returned HTTP %d for %s", ErrGateNotTold, desktopID, resp.StatusCode, path)
	}
	return nil
}
