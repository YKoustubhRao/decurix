package desktop

import (
	"encoding/json"
	"testing"
)

type seccompRule struct {
	Names    []string `json:"names"`
	Action   string   `json:"action"`
	Includes struct {
		Caps []string `json:"caps"`
	} `json:"includes"`
	Excludes struct {
		Caps []string `json:"caps"`
	} `json:"excludes"`
	Args []json.RawMessage `json:"args"`
}

// The profile must open exactly the namespace syscalls Chromium's sandbox
// needs, unconditionally, and keep everything else Docker's default denies.
func TestDesktopSeccompProfileOnlyOpensNamespaceCreation(t *testing.T) {
	var profile struct {
		DefaultAction string        `json:"defaultAction"`
		Syscalls      []seccompRule `json:"syscalls"`
	}
	if err := json.Unmarshal([]byte(desktopSeccompProfile), &profile); err != nil {
		t.Fatalf("the embedded profile is not valid JSON: %v", err)
	}
	if profile.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Errorf("defaultAction = %q, want the deny-by-default of Docker's own profile", profile.DefaultAction)
	}

	unconditional := map[string]bool{}
	for _, r := range profile.Syscalls {
		if r.Action != "SCMP_ACT_ALLOW" || len(r.Includes.Caps) > 0 || len(r.Excludes.Caps) > 0 || len(r.Args) > 0 {
			continue
		}
		for _, n := range r.Names {
			unconditional[n] = true
		}
	}
	for _, want := range []string{"clone", "setns", "unshare"} {
		if !unconditional[want] {
			t.Errorf("%s is not allowed unconditionally - Chromium cannot build its sandbox", want)
		}
	}
	for _, denied := range []string{"mount", "umount2", "bpf", "kexec_load", "init_module", "reboot", "swapon"} {
		if unconditional[denied] {
			t.Errorf("%s is allowed unconditionally - the profile must only open namespace creation", denied)
		}
	}
}

func TestBuildContainerConfigsAppliesSandboxAndCoreLimit(t *testing.T) {
	_, host, _ := buildContainerConfigs("img", "desktop_x", "basic", "ollama", 2, 4096, nil, "vol", []string{"seccomp=" + desktopSeccompProfile})

	if len(host.SecurityOpt) != 2 || host.SecurityOpt[0][:8] != "seccomp=" || host.SecurityOpt[1] != "no-new-privileges:true" {
		t.Errorf("SecurityOpt = %v, want the desktop seccomp profile and no-new-privileges", host.SecurityOpt)
	}
	found := false
	for _, u := range host.Ulimits {
		if u.Name == "core" && u.Soft == 0 && u.Hard == 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("Ulimits = %v, want core dumps disabled", host.Ulimits)
	}
}
