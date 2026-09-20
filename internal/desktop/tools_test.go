package desktop

import (
	"strings"
	"testing"
)

func TestBuildContainerConfigsMountsSharedToolsAndCacheVolume(t *testing.T) {
	_, host, _ := buildContainerConfigs("img", "desktop_test", "developer", "ollama", 2, 4096, nil, "vol_test", nil)

	toolsMounted := false
	cacheMounted := false
	for _, b := range host.Binds {
		if b == ToolsVolumeName+":/opt/tools:ro" {
			toolsMounted = true
		}
		if b == CacheVolumeName+":/var/cache/flowera:rw" {
			cacheMounted = true
		}
	}

	if !toolsMounted {
		t.Fatalf("expected host binds to contain %s:/opt/tools:ro, got: %v", ToolsVolumeName, host.Binds)
	}
	if !cacheMounted {
		t.Fatalf("expected host binds to contain %s:/var/cache/flowera:rw, got: %v", CacheVolumeName, host.Binds)
	}
	if host.Resources.CPUShares != 2*1024 {
		t.Errorf("expected CPUShares to be 2048, got: %d", host.Resources.CPUShares)
	}
	if host.Resources.MemorySwappiness == nil || *host.Resources.MemorySwappiness != 0 {
		t.Errorf("expected MemorySwappiness to be 0, got: %v", host.Resources.MemorySwappiness)
	}
	if host.Tmpfs == nil || host.Tmpfs["/tmp"] == "" || host.Tmpfs["/run"] == "" {
		t.Errorf("expected Tmpfs to contain /tmp and /run, got: %v", host.Tmpfs)
	}
	if host.LogConfig.Type != "json-file" || host.LogConfig.Config["max-size"] != "10m" {
		t.Errorf("expected LogConfig with 10m max-size, got: %v", host.LogConfig)
	}
}

func TestBuildDesktopEnvIncludesToolsAndCachePaths(t *testing.T) {
	env := buildDesktopEnv("box1", "ollama", "dev", "")

	var pathVal, gorootVal, gomodVal, npmVal, pipVal, hfVal string
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			pathVal = strings.TrimPrefix(e, "PATH=")
		}
		if strings.HasPrefix(e, "GOROOT=") {
			gorootVal = strings.TrimPrefix(e, "GOROOT=")
		}
		if strings.HasPrefix(e, "GOMODCACHE=") {
			gomodVal = strings.TrimPrefix(e, "GOMODCACHE=")
		}
		if strings.HasPrefix(e, "NPM_CONFIG_CACHE=") {
			npmVal = strings.TrimPrefix(e, "NPM_CONFIG_CACHE=")
		}
		if strings.HasPrefix(e, "PIP_CACHE_DIR=") {
			pipVal = strings.TrimPrefix(e, "PIP_CACHE_DIR=")
		}
		if strings.HasPrefix(e, "HF_HOME=") {
			hfVal = strings.TrimPrefix(e, "HF_HOME=")
		}
	}

	if !strings.Contains(pathVal, "/opt/tools/bin") {
		t.Errorf("expected PATH to contain /opt/tools/bin, got %s", pathVal)
	}
	if !strings.Contains(pathVal, "/opt/tools/go/bin") {
		t.Errorf("expected PATH to contain /opt/tools/go/bin, got %s", pathVal)
	}
	if gorootVal != "/opt/tools/go" {
		t.Errorf("expected GOROOT to be /opt/tools/go, got %s", gorootVal)
	}
	if gomodVal != "/var/cache/flowera/go" {
		t.Errorf("expected GOMODCACHE to be /var/cache/flowera/go, got %s", gomodVal)
	}
	if npmVal != "/var/cache/flowera/npm" {
		t.Errorf("expected NPM_CONFIG_CACHE to be /var/cache/flowera/npm, got %s", npmVal)
	}
	if pipVal != "/var/cache/flowera/pip" {
		t.Errorf("expected PIP_CACHE_DIR to be /var/cache/flowera/pip, got %s", pipVal)
	}
	if hfVal != "/var/cache/flowera/huggingface" {
		t.Errorf("expected HF_HOME to be /var/cache/flowera/huggingface, got %s", hfVal)
	}
}

func TestOptimizeHostKSMDoesNotPanic(t *testing.T) {
	// Must execute without panic on any platform
	OptimizeHostKSM()
}

func TestResolveDesktopImageSupportsProfiles(t *testing.T) {
	profiles := []string{"basic", "developer", "ai"}
	for _, p := range profiles {
		img, err := resolveDesktopImage(p)
		if err != nil {
			t.Errorf("resolveDesktopImage(%q) returned error: %v", p, err)
		}
		if img == "" {
			t.Errorf("resolveDesktopImage(%q) returned empty image string", p)
		}
	}
}

func TestBuildDesktopEnvControlPlaneURL(t *testing.T) {
	want := func(env []string, v string) {
		t.Helper()
		for _, e := range env {
			if e == "CONTROL_PLANE_URL="+v {
				return
			}
		}
		t.Fatalf("CONTROL_PLANE_URL=%s not in %v", v, env)
	}

	t.Setenv("FLOWERA_CONTROL_PLANE_URL", "")
	want(buildDesktopEnv("box1", "ollama", "dev", ""), "http://host.docker.internal:8080")

	t.Setenv("FLOWERA_CONTROL_PLANE_URL", "http://flowera:8080")
	want(buildDesktopEnv("box1", "ollama", "dev", ""), "http://flowera:8080")
}
