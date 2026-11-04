package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The browser does not download or keep Chrome's on-device model, and does not preload its own UI pages, while site
// isolation and the sandbox stay as they are (resource plan P1 #3).
func TestTheBrowserCarriesNoOnDeviceModelAndKeepsItsIsolation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "agent", "chrome-policies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policies map[string]any
	if err := json.Unmarshal(raw, &policies); err != nil {
		t.Fatalf("chrome-policies.json is not JSON: %v", err)
	}
	if policies["GenAILocalFoundationalModelSettings"] != float64(1) {
		t.Errorf("GenAILocalFoundationalModelSettings = %v, want 1 (do not download the model)", policies["GenAILocalFoundationalModelSettings"])
	}

	init, err := os.ReadFile(filepath.Join("..", "..", "agent", "chromium-browser.init"))
	if err != nil {
		t.Fatal(err)
	}
	flags := regexp.MustCompile(`(?m)^CHROMIUM_FLAGS="([^"]*)"`).FindAllStringSubmatch(string(init), -1)
	if len(flags) != 1 {
		t.Fatalf("%d CHROMIUM_FLAGS assignments, want 1", len(flags))
	}
	line := flags[0][1]
	if strings.Count(line, "--disable-features=") != 1 {
		t.Fatal("more than one --disable-features: Chrome reads only the last")
	}
	for _, feature := range []string{"OptimizationGuideOnDeviceModel", "PreloadTopChromeWebUI"} {
		if !regexp.MustCompile(`--disable-features=\S*\b` + feature + `\b`).MatchString(line) {
			t.Errorf("%s is not disabled", feature)
		}
	}
	for _, kept := range []string{"--no-sandbox", "--disable-site-isolation-trials", "--disable-web-security", "IsolateOrigins", "SitePerProcess"} {
		if strings.Contains(line, kept) {
			t.Errorf("the launch flags weaken isolation: %s", kept)
		}
	}
}
