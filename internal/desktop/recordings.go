package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// vmAgentPort is where a desktop's own agent (vm_server.py) listens: the port
// SendRequest delivers to, and the one that serves the desktop's recordings.
const vmAgentPort = "8081"

// agentClient has no overall timeout on purpose: exporting a recording streams
// for as long as the recording is large. Callers bound each request with its
// context instead.
var agentClient = &http.Client{}

// AgentRequest sends one request to a desktop's agent API and returns the
// response for the caller to relay and close. A desktop's recordings - the
// full-size frames included - stay on the desktop's own volume and are read
// from it on demand; nothing copies them into the control plane.
func (m *Manager) AgentRequest(ctx context.Context, rawID, method, path string, body io.Reader) (*http.Response, error) {
	target, _, err := m.resolveProxyTarget(ctx, rawID, vmAgentPort)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, target.String()+path, body)
	if err != nil {
		return nil, fmt.Errorf("build agent request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := agentClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrAgentUnreachable, err)
	}

	return resp, nil
}
