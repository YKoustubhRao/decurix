package desktop

import (
	"context"
	_ "embed"
	"log"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// desktopSeccompProfile is Docker's own default seccomp profile
// (github.com/moby/profiles, seccomp/default.json) with one change: clone,
// setns and unshare are allowed without CAP_SYS_ADMIN, so an unprivileged
// process can create user, PID and network namespaces.
//
// That is exactly what Chromium's sandbox is built from, and nothing else.
// Under Docker's stock profile it cannot create them - the setuid helper
// fails with "Failed to move to new namespace" and the browser refuses to
// start - which is why the base image ships Chromium with --no-sandbox, and
// why every desktop's browser opened under a yellow "You are using an
// unsupported command-line flag: --no-sandbox" bar. Every other rule of the
// default profile stays in force (mount, bpf, ptrace and the rest are still
// denied), and nothing is granted to the container that its own user did
// not already have outside a namespace. The trade is a small increase in
// the kernel surface a process inside the desktop can reach, for renderer
// processes that are actually isolated from the pages they render - the
// same trade Chrome's own documentation recommends for containers.
//
// Regenerate from a newer upstream default.json by deleting its two
// masked "clone" rules and adding the rule named in the file's comment.
//
//go:embed seccomp-desktop.json
var desktopSeccompProfile string

// desktopSecurityOpt is the SecurityOpt a new desktop is created with: the
// namespace-enabled seccomp profile when the daemon enforces seccomp at
// all, and nothing otherwise.
//
// The check is not a formality. A daemon without seccomp support (a kernel
// built without it, some rootless or VM setups) rejects a container that
// names a profile outright, and a desktop that cannot be created is worse
// than a browser without its sandbox. The image's own Chromium launcher
// (agent/chromium-browser.init) probes for working namespaces at launch and
// only then drops --no-sandbox, so both halves degrade to exactly the old
// behaviour.
func (m *Manager) desktopSecurityOpt(ctx context.Context) []string {
	info, err := m.client.Info(ctx)
	if err != nil {
		log.Printf("warning: could not ask the Docker daemon about seccomp (%v) - creating the desktop without the Chromium sandbox profile", err)
		return nil
	}

	for _, opt := range info.SecurityOptions {
		if strings.HasPrefix(opt, "name=seccomp") {
			return []string{"seccomp=" + desktopSeccompProfile}
		}
	}

	log.Println("warning: this Docker daemon does not enforce seccomp - Chromium in new desktops will run without its sandbox")

	return nil
}

// desktopUlimits turns core dumps off inside desktops.
//
// A desktop is not a debugging box, and a crashing process there leaves a
// ~50 MB core.<pid> in whatever directory it was started from. Chromium
// launched from the XFCE menu starts in ~/Desktop, and its utility processes
// crashing in a restart loop filled that folder - the "core.351 and so on"
// files a human found on their desktop - at one every fifteen seconds.
func desktopUlimits() []*container.Ulimit {
	return []*container.Ulimit{
		{Name: "core", Soft: 0, Hard: 0},
	}
}
