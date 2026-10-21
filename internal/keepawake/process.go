package keepawake

import (
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// processHolder holds the request by keeping a helper process running
// (caffeinate, systemd-inhibit): the lock lives exactly as long as it does.
type processHolder struct {
	command func() *exec.Cmd

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *processHolder) set(on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.stopLocked()

	if !on {
		return nil
	}

	cmd := p.command()

	done, err := startHelper(cmd)
	if err != nil {
		return err
	}

	p.cmd, p.done = cmd, done

	return nil
}

func (p *processHolder) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return running(p.done)
}

func (p *processHolder) stopLocked() {
	if p.cmd == nil {
		return
	}

	_ = p.cmd.Process.Kill()
	<-p.done
	p.cmd, p.done = nil, nil
}

func (p *processHolder) close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.stopLocked()
}

// pipeHolder holds the request through a long-lived helper that reads "on"
// and "off" lines on its stdin (PowerShell reached from WSL). Its stdin
// closing - this process exiting, however it exits - ends the helper, and
// Windows drops the request of a thread that has ended.
type pipeHolder struct {
	command func() *exec.Cmd

	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
}

func (p *pipeHolder) set(on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !running(p.done) {
		if !on {
			return nil
		}

		if err := p.startLocked(); err != nil {
			return err
		}
	}

	line := "off\n"
	if on {
		line = "on\n"
	}

	if _, err := io.WriteString(p.stdin, line); err != nil {
		return fmt.Errorf("telling the keep-awake helper: %w", err)
	}

	return nil
}

func (p *pipeHolder) startLocked() error {
	cmd := p.command()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	done, err := startHelper(cmd)
	if err != nil {
		return err
	}

	p.cmd, p.stdin, p.done = cmd, stdin, done

	return nil
}

func (p *pipeHolder) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return running(p.done)
}

func (p *pipeHolder) close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stdin != nil {
		p.stdin.Close()
	}

	if !running(p.done) {
		return
	}

	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// startHelper starts a holder's helper process with its output discarded, and returns a channel closed once it has
// exited.
func startHelper(cmd *exec.Cmd) (chan struct{}, error) {
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", cmd.Path, err)
	}

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	return done, nil
}

// running reports whether the helper whose exit closes done (nil: none started) is still running.
func running(done chan struct{}) bool {
	if done == nil {
		return false
	}

	select {
	case <-done:
		return false
	default:
		return true
	}
}
