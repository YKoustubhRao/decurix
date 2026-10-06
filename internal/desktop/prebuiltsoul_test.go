package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestADesktopDockerfileCopiesAPrebuiltSoulInsteadOfCompilingIt(t *testing.T) {
	for _, name := range []string{"desktop.basic.Dockerfile", "desktop.developer.Dockerfile", "desktop.ai.Dockerfile"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "docker", name))
		if err != nil {
			t.Fatal(err)
		}

		got := prebuiltSoul(string(data))
		if strings.Contains(got, "golang:") || strings.Contains(got, "go build") || strings.Contains(got, "go mod download") {
			t.Errorf("%s still builds TheBot:\n%s", name, got)
		}
		if !strings.Contains(got, "FROM scratch AS thebot-builder\nCOPY agent/thebot-soul /soul\n") {
			t.Errorf("%s has no stage copying the prebuilt soul", name)
		}
		// Everything after the stage is as it was: the desktop's own stage, and
		// the COPY --from that takes the soul out of the replaced one.
		desktop := string(data)[strings.Index(string(data), "FROM accetto/"):]
		if !strings.HasSuffix(got, desktop) {
			t.Errorf("%s: the desktop's own stage changed", name)
		}
		if !strings.Contains(got, "COPY --from=thebot-builder /soul /opt/soul") {
			t.Errorf("%s no longer takes the soul from the stage", name)
		}
	}
}

func TestADockerfileWithNoGoStageIsLeftAlone(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docker", "tools.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if got := prebuiltSoul(string(data)); got != string(data) {
		t.Error("tools.Dockerfile was changed")
	}
}
