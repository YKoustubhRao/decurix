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
// any stray .pyc directories are excluded automatically.
//
//go:embed docker/desktop.basic.Dockerfile
//go:embed docker/desktop.developer.Dockerfile
//go:embed docker/desktop.ai.Dockerfile
//go:embed docker/tools.Dockerfile
//go:embed agent
var DesktopBuildContext embed.FS
