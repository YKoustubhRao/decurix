package config

import (
	"strings"
	"testing"
)

func TestNoKeyMeansLoopbackOnly(t *testing.T) {
	c := Default()
	c.Mode = ModeLocal
	if err := c.Validate(); err != nil {
		t.Fatalf("loopback without a key: %v", err)
	}
	c.Listen = "0.0.0.0:8484"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("open address without a key was allowed: %v", err)
	}
	t.Setenv("TEST_BRAINIAC_KEY", "k")
	c.APIKeyEnv = "TEST_BRAINIAC_KEY"
	if err := c.Validate(); err != nil {
		t.Fatalf("open address with a key: %v", err)
	}
}

func TestRemoteFromEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BRAINIAC_REMOTE", "decurix:claude")
	c, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeHybrid || c.Remote.URL != "http://127.0.0.1:8080" || c.Remote.Model != "default" {
		t.Fatalf("config %+v remote %+v", c, c.Remote)
	}
	t.Setenv("BRAINIAC_REMOTE", "decurix:gemini")
	if _, err := Load(home); err == nil {
		t.Fatal("an unknown hosted brain was accepted")
	}
}

func TestHybridNeedsARemote(t *testing.T) {
	c := Default()
	c.Mode = ModeHybrid
	if err := c.Validate(); err == nil {
		t.Fatal("hybrid without a remote was accepted")
	}
}

func TestWebDefaultsToOnRequestAndLocalRefusesAuto(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Web != WebOnRequest {
		t.Fatalf("default web %q", c.Web)
	}
	c.Web = WebAuto
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "local") {
		t.Fatalf("local mode with web auto was accepted: %v", err)
	}
	c.Mode, c.Remote = ModeHybrid, &Remote{Kind: RemoteOpenAI, URL: "http://127.0.0.1:1"}
	if err := c.Validate(); err != nil {
		t.Fatalf("hybrid with web auto: %v", err)
	}
}

func TestUpdatesAreOffByDefault(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.UpdateEvery != "off" {
		t.Fatalf("update_every defaults to %q", c.UpdateEvery)
	}
}

func TestTheApplianceRequiresTheSandbox(t *testing.T) {
	t.Setenv("BRAINIAC_APPLIANCE", "")
	if c, err := Load(t.TempDir()); err != nil || c.Sandbox != "" {
		t.Fatalf("a laptop: %q %v", c.Sandbox, err)
	}
	t.Setenv("BRAINIAC_APPLIANCE", "1")
	t.Setenv("TEST_BRAINIAC_KEY", "k")
	t.Setenv("BRAINIAC_API_KEY_ENV", "TEST_BRAINIAC_KEY")
	c, err := Load(t.TempDir())
	if err != nil || c.Sandbox != SandboxRequired {
		t.Fatalf("the appliance: %q %v", c.Sandbox, err)
	}
	c.Sandbox = SandboxOff
	if err := c.Validate(); err == nil {
		t.Fatal("sandbox off was accepted on the appliance")
	}
	c.Sandbox = "maybe"
	if err := c.Validate(); err == nil {
		t.Fatal("an unknown sandbox setting was accepted")
	}
}

func TestTheApplianceNeedsAKeyEvenOnLoopback(t *testing.T) {
	t.Setenv("BRAINIAC_APPLIANCE", "1")
	c := Default()
	c.Mode, c.Sandbox = ModeLocal, SandboxRequired
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("an appliance without a key: %v", err)
	}
	t.Setenv("TEST_BRAINIAC_KEY", "k")
	c.APIKeyEnv = "TEST_BRAINIAC_KEY"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
