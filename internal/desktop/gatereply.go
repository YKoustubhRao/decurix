package desktop

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// A person's reply to a question or an approval reaches the worker's agent as a request like any other, and the agent
// runs as the same user as the worker's shell, so nothing it does with the reply can be trusted as the person's. What
// makes a yes is this: the control plane signs the reply with the desktop's gate key (GateKey) and sends it to the
// approval gate, a process of another user in the same container (docs/gate-boundary.md). The gate verifies it and
// alone creates grants. A reply that reaches the agent without being signed to the gate is only text.

// SignGateReply is the signature the gate checks: over the task, the time, a one-use nonce and digests of the reply
// and of the question it answers.
func SignGateReply(key, taskID, ts, nonce, text, asked, kind string) string {
	mac := hmac.New(sha256.New, []byte(key))
	th, ah := sha256.Sum256([]byte(text)), sha256.Sum256([]byte(asked))
	for _, part := range []string{"reply", taskID, ts, nonce, hex.EncodeToString(th[:]), hex.EncodeToString(ah[:]), kind} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// SendGateReply gives desktop id's approval gate the person's reply, signed. asked is the text of the question the
// reply answers and kind its kind ("approval" or "query"). A desktop without a gate (made before it existed) answers
// nothing here; the error says so and the caller goes on, since that desktop still settles replies in its agent.
func (m *Manager) SendGateReply(ctx context.Context, id, taskID, text, asked, kind string) error {
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
		"task_id": taskID, "ts": ts, "nonce": n, "text": text, "asked": asked, "kind": kind,
		"sig": SignGateReply(key, taskID, ts, n, text, asked, kind),
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
