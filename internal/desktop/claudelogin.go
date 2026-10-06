package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// This is the interactive half of the shared Claude Code login: a person
// running "claude auth login" through the dashboard's popup instead of
// pasting an ANTHROPIC_API_KEY into .env. The CLI itself drives the flow -
// see internal/claudeauth's package doc for why the resulting session lives
// in SQLite rather than a Docker volume - this file only has to get one
// process's stdin/stdout to and from an HTTP request/response pair.
// Confirmed live against the real `claude auth login --claudeai`, piped (not
// a tty, so no ANSI/OSC8 noise to parse):
//
//	Opening browser to sign in…
//	If the browser didn't open, visit: <url>
//	Paste code here if prompted >
//
// and on a bad code, "Invalid code. Please make sure the full code was
// copied." with the process still alive for another attempt - the "Paste
// code here if prompted" prompt is therefore the sync point that guarantees
// the visit: line above it is fully flushed before we go looking for it.
const (
	claudeLoginStartTimeout = 20 * time.Second
	claudeLoginCodeTimeout  = 30 * time.Second
	claudeLoginIdleTimeout  = 5 * time.Minute
	claudeLoginPollInterval = 100 * time.Millisecond

	claudeCredentialsPath = "/home/headless/.claude/.credentials.json"
)

// ErrClaudeLoginInvalidCode means the CLI rejected the pasted code and is
// still waiting for another attempt on the same login ID.
var ErrClaudeLoginInvalidCode = errors.New("claude rejected that code")

// ErrClaudeLoginNotFound means loginID names no in-progress attempt - it was
// never started, already finished, timed out, or was cancelled.
var ErrClaudeLoginNotFound = errors.New("no claude login in progress for that id")

// claudeLoginSession is one in-progress "claude auth login" attempt: a
// throwaway container running just that command, captured so the dashboard's
// popup can be handed its sign-in URL and later feed back the verification
// code a human pastes there.
type claudeLoginSession struct {
	id          string
	containerID string
	conn        net.Conn
	output      *claudeLoginBuffer
	waitCh      <-chan container.WaitResponse
	waitErrCh   <-chan error
	timer       *time.Timer

	// submitMu serializes attempts on this session - only one HTTP request
	// (one pasted code) talks to the container's stdin at a time.
	submitMu sync.Mutex
}

// claudeLoginBuffer accumulates a login container's combined stdout/stderr so
// the HTTP handlers - which come and go per request - can poll for the text
// that marks each stage of the flow, without owning the read loop themselves.
type claudeLoginBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *claudeLoginBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.b.Write(p)
}

// Len returns the number of bytes written so far, for a caller to remember as
// the start of "what came after I did X".
func (b *claudeLoginBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.b.Len()
}

// Since returns everything written from byte offset `from` onward.
func (b *claudeLoginBuffer) Since(from int) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	all := b.b.String()
	if from >= len(all) {
		return ""
	}

	return all[from:]
}

// waitForSubstring polls (there is no interrupting a blocked Read on the
// docker connection to wake it early, and the volumes here are a few KB over
// at most tens of seconds, so a 100ms poll costs nothing a human would
// notice) until Since(from) contains substr or timeout elapses.
func (b *claudeLoginBuffer) waitForSubstring(from int, substr string, timeout time.Duration) (found bool) {
	deadline := time.Now().Add(timeout)

	for {
		if strings.Contains(b.Since(from), substr) {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(claudeLoginPollInterval)
	}
}

// randomHex returns n random bytes hex-encoded, for login IDs and container name suffixes.
func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)

	return hex.EncodeToString(buf)
}

// extractClaudeSignInURL pulls the URL out of claude auth login's "If the
// browser didn't open, visit: <url>" line.
func extractClaudeSignInURL(text string) string {
	const marker = "visit: "

	idx := strings.Index(text, marker)
	if idx < 0 {
		return ""
	}

	rest := text[idx+len(marker):]
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	}

	return strings.TrimSpace(rest)
}

// StartClaudeLogin begins a fresh "claude auth login" attempt in a throwaway
// container from the developer image (the smallest image with the `claude`
// binary), replacing any attempt already in progress. It blocks only long
// enough to capture the CLI's sign-in URL, which the dashboard's popup
// offers the human to open in their own browser.
func (m *Manager) StartClaudeLogin(ctx context.Context) (loginID string, signInURL string, err error) {

	if _, _, err := m.client.ImageInspectWithRaw(ctx, desktopImageDeveloper); err != nil {
		return "", "", fmt.Errorf(
			"the %s image isn't built yet - run --build-images first",
			desktopImageDeveloper,
		)
	}

	m.claudeLoginMu.Lock()
	defer m.claudeLoginMu.Unlock()

	m.teardownClaudeLoginLocked(context.Background())

	if err := m.EnsureToolsVolume(ctx); err != nil {
		log.Printf("warning: ensure tools volume: %v", err)
	}

	containerName := "decurix-claude-login-" + randomHex(4)

	resp, err := m.client.ContainerCreate(
		ctx,
		&container.Config{
			Image: desktopImageDeveloper,
			Env: []string{
				"PATH=/opt/tools/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			},
			// Bypasses the image's tini/startup.sh entrypoint (the full XFCE
			// desktop) entirely - this container only ever runs one CLI command.
			Entrypoint:   []string{"claude"},
			Cmd:          []string{"auth", "login", "--claudeai"},
			Tty:          false,
			OpenStdin:    true,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			StdinOnce:    false,
		},
		&container.HostConfig{
			Binds: []string{
				ToolsVolumeName + ":/opt/tools:ro",
			},
		},
		nil,
		nil,
		containerName,
	)
	if err != nil {
		return "", "", fmt.Errorf("create claude login container: %w", err)
	}

	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		m.destroyClaudeLoginContainer(context.Background(), resp.ID)
		return "", "", fmt.Errorf("start claude login container: %w", err)
	}

	attachResp, err := m.client.ContainerAttach(ctx, resp.ID, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		m.destroyClaudeLoginContainer(context.Background(), resp.ID)
		return "", "", fmt.Errorf("attach to claude login container: %w", err)
	}

	output := &claudeLoginBuffer{}

	go func() {
		_, _ = stdcopy.StdCopy(output, output, attachResp.Reader)
	}()

	waitCh, waitErrCh := m.client.ContainerWait(context.Background(), resp.ID, container.WaitConditionNotRunning)

	session := &claudeLoginSession{
		id:          randomHex(8),
		containerID: resp.ID,
		conn:        attachResp.Conn,
		output:      output,
		waitCh:      waitCh,
		waitErrCh:   waitErrCh,
	}

	// "Paste code here if prompted" is the sync point: it is printed after
	// the visit: line, so seeing it guarantees that line is fully flushed.
	if !output.waitForSubstring(0, "Paste code here if prompted", claudeLoginStartTimeout) {
		attachResp.Close()
		m.destroyClaudeLoginContainer(context.Background(), resp.ID)

		return "", "", fmt.Errorf(
			"claude did not ask for a sign-in code: %s",
			strings.TrimSpace(output.Since(0)),
		)
	}

	url := extractClaudeSignInURL(output.Since(0))
	if url == "" {
		attachResp.Close()
		m.destroyClaudeLoginContainer(context.Background(), resp.ID)

		return "", "", errors.New("could not find a sign-in URL in claude's output")
	}

	session.timer = time.AfterFunc(claudeLoginIdleTimeout, func() {
		m.claudeLoginMu.Lock()
		defer m.claudeLoginMu.Unlock()

		if m.claudeLogin == session {
			m.teardownClaudeLoginLocked(context.Background())
		}
	})

	m.claudeLogin = session

	return session.id, url, nil
}

// SubmitClaudeLoginCode pastes a human-provided verification code into the
// login attempt named by loginID. On success it returns the raw
// ~/.claude/.credentials.json content and, best-effort, the account's email.
// A rejected code returns ErrClaudeLoginInvalidCode with the same loginID
// still usable for another attempt.
func (m *Manager) SubmitClaudeLoginCode(ctx context.Context, loginID, code string) (credentialsJSON []byte, email string, err error) {

	m.claudeLoginMu.Lock()
	session := m.claudeLogin
	m.claudeLoginMu.Unlock()

	if session == nil || session.id != loginID {
		return nil, "", ErrClaudeLoginNotFound
	}

	session.submitMu.Lock()
	defer session.submitMu.Unlock()

	from := session.output.Len()

	if _, err := session.conn.Write([]byte(strings.TrimSpace(code) + "\n")); err != nil {
		m.claudeLoginMu.Lock()
		m.teardownClaudeLoginLocked(context.Background())
		m.claudeLoginMu.Unlock()

		return nil, "", fmt.Errorf("sending code to claude: %w", err)
	}

	deadline := time.Now().Add(claudeLoginCodeTimeout)

	for {
		if strings.Contains(session.output.Since(from), "Invalid code") {
			// The process stays alive for another try - confirmed live - so
			// the session (and its idle timer) is left exactly as it was.
			return nil, "", ErrClaudeLoginInvalidCode
		}

		select {
		case res := <-session.waitCh:
			if strings.Contains(session.output.Since(from), "Invalid code") {
				return nil, "", ErrClaudeLoginInvalidCode
			}

			if res.StatusCode != 0 {
				m.endClaudeLoginSession(session)
				m.destroyClaudeLoginContainer(context.Background(), session.containerID)

				return nil, "", fmt.Errorf(
					"claude auth login exited %d: %s",
					res.StatusCode,
					strings.TrimSpace(session.output.Since(from)),
				)
			}

			// finishClaudeLogin removes the container itself, once it is done
			// reading the credentials back out of it.
			credentialsJSON, email, err = m.finishClaudeLogin(ctx, session)
			m.endClaudeLoginSession(session)

			return credentialsJSON, email, err

		case err := <-session.waitErrCh:
			m.endClaudeLoginSession(session)
			m.destroyClaudeLoginContainer(context.Background(), session.containerID)

			return nil, "", fmt.Errorf("waiting for claude auth login: %w", err)

		default:
			if time.Now().After(deadline) {
				return nil, "", errors.New("timed out waiting for claude to accept the code")
			}

			time.Sleep(claudeLoginPollInterval)
		}
	}
}

// CancelClaudeLogin tears down an in-progress login attempt - the dashboard
// calls this when the human closes the popup without finishing.
func (m *Manager) CancelClaudeLogin(loginID string) {

	m.claudeLoginMu.Lock()
	defer m.claudeLoginMu.Unlock()

	if m.claudeLogin != nil && m.claudeLogin.id == loginID {
		m.teardownClaudeLoginLocked(context.Background())
	}
}

// endClaudeLoginSession clears session as the manager's current login attempt
// (if it still is one) and closes its hijacked attach connection, without
// touching its container - the caller owns that cleanup because
// finishClaudeLogin still needs the container to read the credentials file
// back out of it.
func (m *Manager) endClaudeLoginSession(session *claudeLoginSession) {
	if session.timer != nil {
		session.timer.Stop()
	}

	if session.conn != nil {
		_ = session.conn.Close()
	}

	m.claudeLoginMu.Lock()
	defer m.claudeLoginMu.Unlock()

	if m.claudeLogin == session {
		m.claudeLogin = nil
	}
}

// finishClaudeLogin reads the credentials a successful login left behind and
// removes the now-finished container. email is best-effort: a failure to
// read it back is not a failure of the login itself.
func (m *Manager) finishClaudeLogin(ctx context.Context, session *claudeLoginSession) (credentialsJSON []byte, email string, err error) {
	defer m.destroyClaudeLoginContainer(context.Background(), session.containerID)

	credentialsJSON, err = m.readFileFromContainer(ctx, session.containerID, claudeCredentialsPath)
	if err != nil {
		return nil, "", fmt.Errorf("claude logged in but its credentials could not be read back: %w", err)
	}

	if out, code, execErr := m.execCaptured(ctx, session.containerID, []string{"claude", "auth", "status", "--json"}); execErr == nil && code == 0 {
		var status struct {
			Email string `json:"email"`
		}

		if json.Unmarshal([]byte(out), &status) == nil {
			email = status.Email
		}
	}

	return credentialsJSON, email, nil
}

// teardownClaudeLoginLocked discards the current login attempt, if any.
// Callers must hold claudeLoginMu.
func (m *Manager) teardownClaudeLoginLocked(ctx context.Context) {
	if m.claudeLogin == nil {
		return
	}

	session := m.claudeLogin

	if session.timer != nil {
		session.timer.Stop()
	}

	if session.conn != nil {
		_ = session.conn.Close()
	}

	m.destroyClaudeLoginContainer(ctx, session.containerID)

	m.claudeLogin = nil
}

func (m *Manager) destroyClaudeLoginContainer(ctx context.Context, containerID string) {
	_ = m.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// readFileFromContainer reads one file out of a container via the same
// mechanism `docker cp` uses (a tar stream of the path), returning its raw content.
func (m *Manager) readFileFromContainer(ctx context.Context, containerID, path string) ([]byte, error) {
	rc, _, err := m.client.CopyFromContainer(ctx, containerID, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	tr := tar.NewReader(rc)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		if hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}

	return nil, fmt.Errorf("%s not found in container", path)
}

// execCaptured runs one command in a container to completion and returns its
// combined stdout/stderr and exit code, for the small one-shot reads this
// file needs (claude auth status --json) - PersistMemoryFile's exec-write
// counterpart, reading instead of writing.
func (m *Manager) execCaptured(ctx context.Context, containerID string, cmd []string) (output string, exitCode int, err error) {
	execID, err := m.client.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", 0, err
	}

	attach, err := m.client.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", 0, err
	}
	defer attach.Close()

	var out bytes.Buffer

	if _, err := stdcopy.StdCopy(&out, &out, attach.Reader); err != nil && err != io.EOF {
		return "", 0, err
	}

	inspect, err := m.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return out.String(), 0, err
	}

	return out.String(), inspect.ExitCode, nil
}

// PersistClaudeCredentials writes the shared Claude session's credentials
// into a desktop container, the same way PersistMemoryFile seeds a memory
// entry - so a newly created claude-harness desktop authenticates as
// whoever last completed the dashboard's login popup, without a per-desktop
// OAuth round trip.
func (m *Manager) PersistClaudeCredentials(ctx context.Context, desktopID, credentialsJSON string) error {

	containerName := "desktop_" + sanitizeName(desktopID)
	delimiter := fmt.Sprintf("DECURIX_CLAUDE_CREDENTIALS_%d", time.Now().UnixNano())

	cmd := fmt.Sprintf(`mkdir -p "$(dirname %s)" && cat <<'%s' > %s
%s
%s
chmod 600 %s`,
		claudeCredentialsPath,
		delimiter,
		claudeCredentialsPath,
		strings.TrimSpace(credentialsJSON),
		delimiter,
		claudeCredentialsPath,
	)

	execConfig := container.ExecOptions{
		Cmd:          []string{"bash", "-lc", cmd},
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := m.client.ContainerExecCreate(ctx, containerName, execConfig)
	if err != nil {
		return fmt.Errorf("create claude credentials write exec for %s: %w", containerName, err)
	}

	if err := m.client.ContainerExecStart(ctx, execID.ID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("start claude credentials write exec for %s: %w", containerName, err)
	}

	return waitExec(ctx, m.client, execID.ID, fmt.Sprintf("claude credentials write exec for %s", containerName))
}
