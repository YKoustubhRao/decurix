package desktop

import (
	"strings"
	"testing"
)

func TestChosenMemoryIsACeilingNotAReservation(t *testing.T) {
	for _, mb := range []int64{2048, 4096, 8192} {
		_, host, _ := buildContainerConfigs("img", "desktop_test", "basic", "thebot", 2, mb, nil, "vol_test", nil)
		if host.Memory != mb*1024*1024 {
			t.Fatalf("%d MB desktop: limit %d, want the size chosen", mb, host.Memory)
		}
		if host.MemoryReservation != desktopIdleMB*1024*1024 {
			t.Fatalf("%d MB desktop reserves %d bytes; only the idle floor may be held back", mb, host.MemoryReservation)
		}
	}
	if got := desktopReservation(256); got != 256*1024*1024 {
		t.Fatalf("a desktop smaller than the floor reserves %d, want all of it", got)
	}
}

func TestBuildContainerConfigsMountsSharedToolsAndCacheVolume(t *testing.T) {
	_, host, _ := buildContainerConfigs("img", "desktop_test", "developer", "ollama", 2, 4096, nil, "vol_test", nil)

	toolsMounted := false
	cacheMounted := false
	for _, b := range host.Binds {
		if b == ToolsVolumeName+":/opt/tools:ro" {
			toolsMounted = true
		}
		if b == CacheVolumeName+":/var/cache/decurix:rw" {
			cacheMounted = true
		}
	}

	if !toolsMounted {
		t.Fatalf("expected host binds to contain %s:/opt/tools:ro, got: %v", ToolsVolumeName, host.Binds)
	}
	if !cacheMounted {
		t.Fatalf("expected host binds to contain %s:/var/cache/decurix:rw, got: %v", CacheVolumeName, host.Binds)
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
	env := buildDesktopEnv("box1", "ollama", "dev", "", Brain{})

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
	if gomodVal != "/var/cache/decurix/go" {
		t.Errorf("expected GOMODCACHE to be /var/cache/decurix/go, got %s", gomodVal)
	}
	if npmVal != "/var/cache/decurix/npm" {
		t.Errorf("expected NPM_CONFIG_CACHE to be /var/cache/decurix/npm, got %s", npmVal)
	}
	if pipVal != "/var/cache/decurix/pip" {
		t.Errorf("expected PIP_CACHE_DIR to be /var/cache/decurix/pip, got %s", pipVal)
	}
	if hfVal != "/var/cache/decurix/huggingface" {
		t.Errorf("expected HF_HOME to be /var/cache/decurix/huggingface, got %s", hfVal)
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

	t.Setenv("DECURIX_CONTROL_PLANE_URL", "")
	want(buildDesktopEnv("box1", "ollama", "dev", "", Brain{}), "http://host.docker.internal:8080")

	t.Setenv("DECURIX_CONTROL_PLANE_URL", "http://decurix:8080")
	want(buildDesktopEnv("box1", "ollama", "dev", "", Brain{}), "http://decurix:8080")
}

func TestAHostedBrainIsGivenTheProxyNeverAKey(t *testing.T) {
	t.Setenv("DECURIX_CONTROL_PLANE_URL", "http://decurix:8080")

	local := buildDesktopEnv("box1", HarnessTheBot, "", "", Brain{})
	if envValue(local, "DECURIX_BRAIN") != BrainOllama || envValue(local, "LLM_PROVIDER") != "" || envValue(local, "DECURIX_LLM") != "" {
		t.Fatalf("local brain env: %v", local)
	}

	env := buildDesktopEnv("box1", HarnessTheBot, "", "", Brain{Kind: BrainClaude, Model: "claude-opus-5-5"})
	want := map[string]string{
		"DECURIX_BRAIN":        BrainClaude,
		"LLM_PROVIDER":         "openai",
		"OPENAI_BASE_URL":      "http://decurix:8080/api/llm/claude",
		"EMBED_PROVIDER":       "ollama",
		"DECURIX_LLM":          BrainClaude,
		"DECURIX_BRAIN_MODEL":  "claude-opus-5-5",
		"DECURIX_BRAIN_VISION": "0",
	}
	for k, v := range want {
		if got := envValue(env, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if seeing := buildDesktopEnv("box1", HarnessTheBot, "", "", Brain{Kind: BrainClaude, Vision: true}); envValue(seeing, "DECURIX_BRAIN_VISION") != "1" {
		t.Error("a brain that reads images is not told so")
	}
	if envValue(local, "DECURIX_BRAIN_VISION") != "" {
		t.Error("the Ollama brain was told about vision - TheBot asks Ollama itself")
	}
	if desktopModel(env) != "claude-opus-5-5" {
		t.Errorf("desktopModel = %q", desktopModel(env))
	}
	for _, kv := range env {
		if strings.Contains(kv, "sk-") || strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "AZURE_KEY=") {
			t.Errorf("a key reached the desktop: %s", kv)
		}
	}
}
