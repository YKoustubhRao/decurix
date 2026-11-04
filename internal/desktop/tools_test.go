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

	toolsMounted, cacheMounted, modelsMounted := false, false, false
	for _, b := range host.Binds {
		if b == ToolsVolumeName+":/opt/tools:ro" {
			toolsMounted = true
		}
		if b == "desktop_test_cache:/var/cache/decurix:rw" {
			cacheMounted = true
		}
		if b == HFVolumeName+":"+HFMountPoint+":ro" {
			modelsMounted = true
		}
	}

	if !toolsMounted {
		t.Fatalf("expected host binds to contain %s:/opt/tools:ro, got: %v", ToolsVolumeName, host.Binds)
	}
	if !cacheMounted {
		t.Fatalf("expected the desktop's own cache at /var/cache/decurix, got: %v", host.Binds)
	}
	if !modelsMounted {
		t.Fatalf("expected the shared model cache read-only, got: %v", host.Binds)
	}
	if host.Resources.CPUShares != 2*1024 {
		t.Errorf("expected CPUShares to be 2048, got: %d", host.Resources.CPUShares)
	}
	// A desktop may swap (owner decision, B3 reversed): no swap limit of its own and no swappiness, so Docker's
	// default (memory plus swap up to twice the memory) holds on cgroup v1 and v2 alike.
	if host.Resources.MemorySwappiness != nil || host.Resources.MemorySwap != 0 || host.Resources.Memory == 0 {
		t.Errorf("swappiness %v, MemorySwap %d (memory %d): want swap allowed", host.Resources.MemorySwappiness, host.Resources.MemorySwap, host.Resources.Memory)
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

// TestNoTwoDesktopsShareAWritableVolume: desktops distrust each other, all run as uid 1001, and a cache one writes and
// another trusts on read is a way from one into the other (B-02). Whatever two desktops share is read-only.
func TestNoTwoDesktopsShareAWritableVolume(t *testing.T) {
	writable := func(name string) map[string]bool {
		_, host, _ := buildContainerConfigs("img", "desktop_"+name, "basic", "thebot", 1, 2048, nil, "desktop_"+name+"_home", nil)
		out := map[string]bool{}
		for _, b := range host.Binds {
			parts := strings.Split(b, ":")
			if len(parts) < 3 || parts[2] != "ro" {
				out[parts[0]] = true
			}
		}
		for _, m := range host.Mounts {
			if !m.ReadOnly {
				out[m.Source] = true
			}
		}
		return out
	}
	a, b := writable("a"), writable("b")
	for v := range a {
		if b[v] {
			t.Errorf("desktops a and b both write volume %s", v)
		}
	}
	if !a["desktop_a_cache"] || !a["desktop_a_home"] {
		t.Errorf("a desktop writes its own home and cache: %v", a)
	}
}

// TestEveryDesktopRunsWithNoNewPrivileges: also the processes the control plane starts with docker exec (B-21).
func TestEveryDesktopRunsWithNoNewPrivileges(t *testing.T) {
	for _, opt := range [][]string{nil, {"seccomp=x"}} {
		_, host, _ := buildContainerConfigs("img", "desktop_a", "basic", "thebot", 1, 2048, nil, "v", opt)
		found := false
		for _, o := range host.SecurityOpt {
			found = found || o == "no-new-privileges:true"
		}
		if !found {
			t.Errorf("SecurityOpt %v lacks no-new-privileges", host.SecurityOpt)
		}
	}
}
