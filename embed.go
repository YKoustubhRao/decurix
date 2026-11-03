// Package decurix holds the build context for the desktop images, embedded
// into the control-plane binary.
//
// This lives at the module root because go:embed can only reach files at or
// below its own directory, and the build context spans both docker/ and
// agent/. Embedding it is what lets a downloaded decurix executable build its
// own desktop images with no repo checkout and no bundled scripts.
package decurix

import "embed"

// DesktopBuildContext is the Docker build context for the desktop images:
// the desktop Dockerfiles plus the agent sources they COPY in. Paths inside
// it are exactly the paths the Dockerfiles reference (agent/vm_server.py,
// docker/desktop.basic.Dockerfile, ...), so it can be streamed to the Docker
// daemon as-is.
//
// go:embed skips names beginning with "." or "_", so agent/__pycache__ and
// any stray .pyc directories are excluded automatically. That also drops the
// agent's own __init__.py, __main__.py and _pathfix.py, so they are named by
// pattern below (a pattern that names a file embeds it whatever its name):
// without decurix_agent/computer/__main__.py the `gui` command, every engine's
// way to the screen, failed in every desktop the app built.
// TestEveryAgentFileIsEmbedded checks the checkout against this list.
//
// agent/ is named by what the images are built from, not whole: agent/tests
// (7.5 MB of the agent's unit tests and their fixtures) is no image's and no
// running binary's, and went into every decurix binary until the review of
// 2026-11-02. go:embed cannot leave one folder out of a pattern, so the
// patterns are: every top-level file with a dot or a dash in its name (the
// launch scripts, the desktop's settings files, vm_server.py, chromium-browser,
// and thebot.tar.gz and thebot-soul when go generate made them), the four
// commands without either (gui, office, lookup, handoff), and the source
// folder decurix_agent. A pattern must match something or the build fails, so
// the optional thebot-soul rides in agent/*-*, which chromium-browser always
// matches. TestTheBuildContextIsWhat
// TheImagesCopy (embed_test.go) checks the Dockerfiles' COPY lines against it.
//
// It also skips agent/thebot, without a word: a directory with a go.mod is
// another module. TheBot's sources ride along as TheBotSource instead, which
// `go generate .` writes (embed_gen.go) and internal/desktop unpacks back to
// agent/thebot/... when it streams the context. A binary built without that
// step cannot build the desktop images, and says so.
//
//go:generate go run embed_gen.go
//go:embed docker/desktop.basic.Dockerfile
//go:embed docker/tools.Dockerfile
//go:embed agent/[^._]*.* agent/[^._]*-* agent/gui agent/office agent/lookup agent/handoff agent/decurix_agent
//go:embed agent/decurix_agent/__init__.py agent/decurix_agent/*/__init__.py agent/decurix_agent/*/__main__.py
var DesktopBuildContext embed.FS

// TheBotSource is the archive of agent/thebot inside DesktopBuildContext, and
// TheBotDir where its files belong in the build context.
const (
	TheBotSource = "agent/thebot.tar.gz"
	TheBotDir    = "agent/thebot"
	// TheBotSoul is TheBot's engine compiled for the desktops by `go
	// generate .`, when it was run; the desktop image then copies it instead of
	// compiling it (internal/desktop.prebuiltSoul).
	TheBotSoul = "agent/thebot-soul"
)
