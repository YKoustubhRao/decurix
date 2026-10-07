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
// It also skips agent/thebot, without a word: a directory with a go.mod is
// another module. TheBot's sources ride along as TheBotSource instead, which
// `go generate .` writes (embed_gen.go) and internal/desktop unpacks back to
// agent/thebot/... when it streams the context. A binary built without that
// step cannot build the desktop images, and says so.
//
//go:generate go run embed_gen.go
//go:embed docker/desktop.basic.Dockerfile
//go:embed docker/desktop.developer.Dockerfile
//go:embed docker/desktop.ai.Dockerfile
//go:embed docker/tools.Dockerfile
//go:embed agent
//go:embed agent/decurix_agent/__init__.py agent/decurix_agent/*/__init__.py agent/decurix_agent/*/__main__.py
//go:embed agent/browser-agent/*/_pathfix.py
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
