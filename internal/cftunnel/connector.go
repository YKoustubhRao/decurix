package cftunnel

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Connector states, as Status reports them.
const (
	stateStopped  = "stopped"
	stateStarting = "starting"
	stateRunning  = "running"
	stateMissing  = "missing"
	stateFailed   = "failed"
)

// connectedLine is what cloudflared logs once a connection to Cloudflare's
// edge is up ("Registered tunnel connection connIndex=0 ...").
const connectedLine = "Registered tunnel connection"

// connector keeps one cloudflared process running a tunnel, restarting it
// with backoff when it exits, until it is told to stop.
type connector struct {
	// command builds the process for token; tests replace it.
	command func(ctx context.Context, token string) (*exec.Cmd, error)

	mu      sync.Mutex
	token   string
	cancel  context.CancelFunc
	done    chan struct{}
	current string
	lastErr string
}

func newConnector() *connector {
	return &connector{command: cloudflaredCommand, current: stateStopped}
}

// cloudflaredCommand runs cloudflared for the tunnel whose token is token.
// The token goes in the environment, not the arguments, so it is not in
// every process listing on the machine.
func cloudflaredCommand(ctx context.Context, token string) (*exec.Cmd, error) {
	path, err := exec.LookPath("cloudflared")
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, path, "tunnel", "--no-autoupdate", "run")
	cmd.Env = append(os.Environ(), "TUNNEL_TOKEN="+token)

	return cmd, nil
}

// run makes the connector run token: nothing changes when it already is,
// the old process is stopped first when it runs another, and "" stops it.
func (c *connector) run(token string) {
	c.mu.Lock()
	if token == c.token && (token == "" || c.cancel != nil) {
		c.mu.Unlock()
		return
	}

	cancel, done := c.cancel, c.done
	c.token, c.cancel, c.done = token, nil, nil
	c.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}

	if token == "" {
		c.setState(stateStopped, "")
		return
	}

	ctx, stop := context.WithCancel(context.Background())
	finished := make(chan struct{})

	c.mu.Lock()
	c.cancel, c.done = stop, finished
	c.mu.Unlock()

	go func() {
		defer close(finished)
		c.supervise(ctx, token)
	}()
}

// supervise runs cloudflared until ctx ends, restarting it after a
// doubling pause (2 s up to a minute) whenever it exits.
func (c *connector) supervise(ctx context.Context, token string) {
	backoff := 2 * time.Second

	for ctx.Err() == nil {
		started := time.Now()

		if err := c.once(ctx, token); err != nil && ctx.Err() == nil {
			log.Printf("cftunnel: connector: %v", err)
		}

		if ctx.Err() != nil {
			break
		}

		// A run that stayed up a while was not a crash loop.
		if time.Since(started) > 5*time.Minute {
			backoff = 2 * time.Second
		}

		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, time.Minute)
	}

	c.setState(stateStopped, "")
}

// once runs one cloudflared process to its end.
func (c *connector) once(ctx context.Context, token string) error {
	cmd, err := c.command(ctx, token)
	if err != nil {
		c.setState(stateMissing, "cloudflared is not installed on this machine")
		return err
	}

	out, err := cmd.StderrPipe()
	if err != nil {
		c.setState(stateFailed, err.Error())
		return err
	}
	cmd.Stdout = cmd.Stderr

	c.setState(stateStarting, "")

	if err := cmd.Start(); err != nil {
		c.setState(stateFailed, err.Error())
		return err
	}

	last := c.watch(out)

	err = cmd.Wait()
	if ctx.Err() == nil {
		reason := last()
		if reason == "" && err != nil {
			reason = err.Error()
		}
		c.setState(stateFailed, "cloudflared exited: "+reason)
	}

	return err
}

// watch reads cloudflared's log, marking the connector running at its first
// registered connection, and returns a function giving the last error line
// it printed.
func (c *connector) watch(r io.Reader) func() string {
	var mu sync.Mutex
	var lastErr string

	scanner := bufio.NewScanner(r)
	done := make(chan struct{})

	go func() {
		defer close(done)

		for scanner.Scan() {
			line := scanner.Text()

			if strings.Contains(line, connectedLine) {
				c.setState(stateRunning, "")
			}

			if strings.Contains(line, " ERR ") {
				mu.Lock()
				lastErr = line
				mu.Unlock()
			}
		}
	}()

	return func() string {
		<-done
		mu.Lock()
		defer mu.Unlock()

		// cloudflared's lines start with a timestamp and level; the rest is the message.
		if i := strings.Index(lastErr, " ERR "); i >= 0 {
			return strings.TrimSpace(lastErr[i+5:])
		}

		return lastErr
	}
}

func (c *connector) setState(state, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.current = state
	c.lastErr = reason
}

// state returns the connector's state and, when it failed, why.
func (c *connector) state() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.current, c.lastErr
}
