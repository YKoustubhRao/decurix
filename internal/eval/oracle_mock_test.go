// Tests that an oracle step reaches the run's own mock app: MOCK_ROUTE names desktop.PracticeMockHost(run).
package eval

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// Found live (2026-11-15): since each run's mock answers only to its own name, the bare alias in MOCK_ROUTE resolved
// to nothing and every oracle step that curls the mock failed ("Could not resolve host: practice-mock").
func TestAnOracleStepCurlsTheRunsOwnMock(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine")
	}
	host := desktop.PracticeMockHost("r-abc123")
	argv := oracleArgv("t1", "case-a", `printf '%s' "$MOCK_ROUTE"`, host)
	if argv[len(argv)-1] != host {
		t.Fatalf("the mock host is not the step's last argument: %q", argv)
	}
	// Run the prelude itself, with the case folder replaced by one that exists here.
	argv[5] = t.TempDir()
	out, err := exec.Command(bash, argv[1:]...).Output()
	if err != nil {
		t.Fatalf("the prelude failed: %v", err)
	}
	want := "--connect-to ::" + host + ":" + desktop.PracticeMockPort
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("MOCK_ROUTE = %q, want %q", got, want)
	}
}
