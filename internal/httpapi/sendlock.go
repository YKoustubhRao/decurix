package httpapi

import (
	"context"
	"strings"
	"sync"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Every send to a desktop - a person's, a schedule's, a DAG node's, an
// email trigger's - takes that desktop's send lock, so "is it free?" and
// "send it" happen as one step for automation. Without it, a person's
// request could land between an automated sender's busy check and its send,
// and the automated request would reach an agent now waiting on that person
// - which the watcher takes as the person's answer (see tasks.Plan).

// sendLocks holds one mutex per desktop ID ever sent to; never removed, the
// same bounded cost dag.Runner's desktopLocks accepts.
type sendLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lockDesktop takes desktopID's send lock and returns its release.
func (s *Server) lockDesktop(desktopID string) func() {
	s.sends.mu.Lock()
	if s.sends.locks == nil {
		s.sends.locks = map[string]*sync.Mutex{}
	}

	mu, ok := s.sends.locks[desktopID]
	if !ok {
		mu = &sync.Mutex{}
		s.sends.locks[desktopID] = mu
	}
	s.sends.mu.Unlock()

	mu.Lock()

	return mu.Unlock
}

// DesktopBusyError is an automated send refused because the desktop has a
// task running, queued or waiting on a person. Callers outside this package
// recognise it by its DesktopBusy method (internal/schedule,
// internal/mailtrigger and internal/dag cannot import httpapi): it means
// "try again later", not "failed".
type DesktopBusyError struct{ Reason string }

func (e *DesktopBusyError) Error() string { return "the desktop is busy: " + e.Reason }

// DesktopBusy says why the desktop is busy.
func (e *DesktopBusyError) DesktopBusy() string { return e.Reason }

// submitWhenFree is the door every automated sender uses: it sends only to a
// desktop with nothing running, queued or waiting, and says so with a
// *DesktopBusyError otherwise. A records error is also a refusal - sending
// blind is what this exists to prevent - and the caller simply tries again.
func (s *Server) submitWhenFree(ctx context.Context, desktopID, content string) (tasks.Route, error) {
	id := desktop.SanitizeID(desktopID)

	unlock := s.lockDesktop(id)
	defer unlock()

	why, err := s.Tasks.Busy(id)
	if err != nil {
		return tasks.Route{}, err
	}

	if why != "" {
		return tasks.Route{}, &DesktopBusyError{Reason: why}
	}

	return s.submitLocked(ctx, desktopID, content)
}

// SubmitTrackedWithFiles is SubmitTracked for a request that names files it
// brought along (an email trigger's attachments): under the same send lock
// and busy check, the files are written into dir on the desktop first, so
// they are there when the agent reads the request. Implements
// mailtrigger.FileSubmitter.
func (s *Server) SubmitTrackedWithFiles(ctx context.Context, desktopID, content, dir string, files []mailtrigger.Attachment) (string, error) {
	id := desktop.SanitizeID(desktopID)

	unlock := s.lockDesktop(id)
	defer unlock()

	why, err := s.Tasks.Busy(id)
	if err != nil {
		return "", err
	}

	if why != "" {
		return "", &DesktopBusyError{Reason: why}
	}

	out := make([]desktop.File, len(files))
	for i, f := range files {
		out[i] = desktop.File{Name: f.Name, Data: f.Data}
	}

	if err := s.Desktops.CopyFiles(ctx, id, dir, out); err != nil {
		return "", err
	}

	// The request names the files under /home/headless; a native worker's
	// home is a folder of this computer, and its agent finds them there.
	for _, f := range out {
		named := dir + "/" + f.Name
		content = strings.ReplaceAll(content, named, s.Desktops.AgentPath(id, named))
	}

	route, err := s.submitLocked(ctx, desktopID, content)
	return route.TaskID, err
}

var _ mailtrigger.FileSubmitter = (*Server)(nil)
