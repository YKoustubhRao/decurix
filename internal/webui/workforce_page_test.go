// Tests for the Workforce page's controls that sit on routes already built: a password reset and a policy edited in
// place on the Access tab, Publish picking from the worker's confirmed skills, and spend saying "not metered" for a
// worker whose model calls skip the control plane's proxy. The page's script is also checked to parse.
package webui

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// renderFullWorkforce renders the page with every section, as the primary listener serves it.
func renderFullWorkforce(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	data := WorkforcePageData{HasSkillHub: true, HasKnowledge: true, HasQueue: true, HasHandoffs: true, HasAccess: true}
	if err := NewRenderer().RenderWorkforce(&out, data); err != nil {
		t.Fatalf("RenderWorkforce: %v", err)
	}
	return out.String()
}

// TestWorkforcePageOffersTheControlsTheRoutesTake checks the Access, Skills and Queue tabs carry the new controls.
func TestWorkforcePageOffersTheControlsTheRoutesTake(t *testing.T) {
	page := renderFullWorkforce(t)
	for _, want := range []string{
		// Access: a person's password reset, and a policy edited in place (save with its id replaces it).
		`"Set password"`, `{id: u.id, password: pw.value}`, `id="policyCancel"`, `policy.id = replacing`, `"Edit"`,
		// Publish: a select filled from the worker's confirmed skills, not a typed name.
		`<select class="select" id="pubName" required>`, `"/api/desktops/skills?id="`, `sk.status === "ready"`,
		// Spend: "not metered" for a worker whose calls skip the proxy.
		`"not metered"`, `function metered(d)`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the workforce page is missing %q", want)
		}
	}
	for _, gone := range []string{`id="pubName" autocomplete`, "u.local"} {
		if strings.Contains(page, gone) {
			t.Errorf("the workforce page still has %q", gone)
		}
	}
}

// TestWorkforcePageScriptParses runs node --check on the page's inline script, when node is on PATH.
func TestWorkforcePageScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	page := renderFullWorkforce(t)
	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(page, -1)
	if len(scripts) == 0 {
		t.Fatal("no inline script on the workforce page")
	}
	dir := t.TempDir()
	for i, s := range scripts {
		path := filepath.Join(dir, "script"+string(rune('a'+i))+".js")
		if err := os.WriteFile(path, []byte(s[1]), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
			t.Errorf("inline script %d does not parse: %v\n%s", i, err, out)
		}
	}
}
