package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// claudeCredentialsPath is where Claude Code reads its login inside a desktop.
const claudeCredentialsPath = "/home/headless/.claude/.credentials.json"

// randomHex returns n random bytes hex-encoded, for container name suffixes.
func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)

	return hex.EncodeToString(buf)
}

// PersistClaudeCredentials writes a stored Claude Code session's credentials into a desktop container, the same way
// PersistMemoryFile seeds a memory entry. The dashboard's login popup that made such sessions was removed
// (2026-11-07); a desktop created with no session is given none.
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

	return m.execBash(ctx, containerName, cmd, "claude credentials write exec")
}
