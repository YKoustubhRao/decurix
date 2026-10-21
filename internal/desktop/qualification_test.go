package desktop

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/qualification"
)

func TestQualificationBoundaryHasNoProductionAccess(t *testing.T) {
	config, host := qualificationConfigs(qualification.Definition{Image: "sha256:example"})
	if host.NetworkMode != "none" || !host.ReadonlyRootfs || len(host.Binds) != 0 || len(host.Mounts) != 0 || len(host.PortBindings) != 0 || len(host.ExtraHosts) != 0 || host.Privileged {
		t.Fatalf("production access in isolated host config: %+v", host)
	}
	if config.User != "1001:1001" || len(config.ExposedPorts) != 0 || len(config.Volumes) != 0 || len(config.Entrypoint) != 1 || config.Entrypoint[0] != "/usr/bin/sleep" {
		t.Fatal("image entrypoint or authority inherited")
	}
	for _, env := range config.Env {
		if strings.Contains(env, "TOKEN") || strings.Contains(env, "KEY") || strings.Contains(env, "CONTROL") || strings.Contains(env, "OLLAMA") {
			t.Fatal("production configuration inherited")
		}
	}
	if len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" || host.SecurityOpt[0] != "no-new-privileges:true" {
		t.Fatal("capabilities not dropped")
	}
}

func TestQualificationClearsEveryInheritedEnvironmentValue(t *testing.T) {
	env := qualificationEnvironment([]string{"API_KEY=secret", "CONTROL_PLANE_URL=http://host", "HOME=/production", "PATH=/custom", "BRAIN_TOKEN=secret"})
	for _, v := range env {
		if strings.Contains(v, "secret") || strings.Contains(v, "production") || strings.Contains(v, "http://host") || strings.Contains(v, "/custom") {
			t.Fatalf("image environment leaked: %s", v)
		}
	}
}

func TestQualificationExtractionRejectsLinksTraversalAndInputChanges(t *testing.T) {
	for _, bad := range []tar.Header{{Name: "work/result.txt", Typeflag: tar.TypeSymlink, Linkname: "/secret"}, {Name: "work/../secret", Typeflag: tar.TypeReg}, {Name: "work/sub/result.txt", Typeflag: tar.TypeReg}} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		_ = tw.WriteHeader(&tar.Header{Name: "work/main.py", Typeflag: tar.TypeReg, Size: 0})
		_ = tw.WriteHeader(&bad)
		_ = tw.Close()
		if _, err := qualificationArtifacts(&b, "main.py", nil); err == nil {
			t.Fatalf("unsafe tar entry accepted: %+v", bad)
		}
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "work/main.py", Typeflag: tar.TypeReg})
	_ = tw.WriteHeader(&tar.Header{Name: "work/input.txt", Typeflag: tar.TypeReg, Size: 3})
	_, _ = tw.Write([]byte("bad"))
	_ = tw.Close()
	if _, err := qualificationArtifacts(&b, "main.py", map[string][]byte{"input.txt": []byte("original")}); err == nil {
		t.Fatal("modified input accepted")
	}
}

func TestQualificationExtractionCollectsOnlyNewRegularArtifacts(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "work/", Typeflag: tar.TypeDir})
	for n, content := range map[string]string{"main.py": "print('ok')", "input.txt": "original", "out.txt": "result"} {
		_ = tw.WriteHeader(&tar.Header{Name: "work/" + n, Typeflag: tar.TypeReg, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	out, err := qualificationArtifacts(&b, "main.py", map[string][]byte{"input.txt": []byte("original")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || string(out["out.txt"]) != "result" {
		t.Fatalf("wrong collected artifacts: %+v", out)
	}
}
