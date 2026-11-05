package config

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("WORKSPACE_ROOT", "")
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("CHAT_MODEL", "")
	t.Setenv("EMBED_MODEL", "")
	t.Setenv("SOUL_PORT", "")
	t.Setenv("DOCLING_SCRIPT", "")

	cfg := Load()

	if cfg.WorkspaceRoot != "/home/headless/.decurix/ai" {
		t.Errorf("WorkspaceRoot default = %q, want /home/headless/.decurix/ai", cfg.WorkspaceRoot)
	}
	if cfg.OllamaHost != "http://127.0.0.1:11434" {
		t.Errorf("OllamaHost default = %q, want http://127.0.0.1:11434", cfg.OllamaHost)
	}
	if cfg.Port != "4900" {
		t.Errorf("Port default = %q, want 4900", cfg.Port)
	}
	if cfg.DoclingScript != "scripts/docling_convert.py" {
		t.Errorf("DoclingScript default = %q, want scripts/docling_convert.py", cfg.DoclingScript)
	}
	if cfg.Provider != "ollama" {
		t.Errorf("Provider default = %q, want ollama", cfg.Provider)
	}
	// Locks in the real deployed default - qwen2.5-coder:14b, not
	// devstral-small-2:24b (which repeatedly crashed on VRAM cold-load
	// during testing). This test exists specifically so this value can
	// never silently drift from what VMDockerfile actually ships again.
	if cfg.ChatModel != "qwen2.5-coder:14b" {
		t.Errorf("ChatModel default = %q, want qwen2.5-coder:14b", cfg.ChatModel)
	}
}

func TestValidateOllama(t *testing.T) {
	cfg := Config{Provider: "ollama", OllamaHost: "http://127.0.0.1:11434"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil for a valid ollama config", err)
	}
	if err := (Config{Provider: "ollama"}).Validate(); err == nil {
		t.Error("Validate() = nil for ollama with no OllamaHost, want an error")
	}
}

func TestValidateAnthropic(t *testing.T) {
	if err := (Config{Provider: "anthropic", AnthropicAPIKey: "sk-test", Standalone: true}).Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil for a valid anthropic config", err)
	}
	if err := (Config{Provider: "anthropic", Standalone: true}).Validate(); err == nil {
		t.Error("Validate() = nil for anthropic with no API key, want an error")
	}
}

func TestValidateOpenAI(t *testing.T) {
	if err := (Config{Provider: "openai", OpenAIAPIKey: "sk-test", Standalone: true}).Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil for a valid openai config", err)
	}
	if err := (Config{Provider: "openai"}).Validate(); err == nil {
		t.Error("Validate() = nil for openai with no API key, want an error")
	}
}

func TestValidateUnknownProvider(t *testing.T) {
	if err := (Config{Provider: "grok"}).Validate(); err == nil {
		t.Error("Validate() = nil for an unknown provider, want an error")
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("WORKSPACE_ROOT", "/tmp/custom")
	t.Setenv("SOUL_PORT", "9999")

	cfg := Load()

	if cfg.WorkspaceRoot != "/tmp/custom" {
		t.Errorf("WorkspaceRoot = %q, want /tmp/custom (env override ignored)", cfg.WorkspaceRoot)
	}
	if cfg.Port != "9999" {
		t.Errorf("Port = %q, want 9999 (env override ignored)", cfg.Port)
	}
}

// Inside Decurix a hosted model is reached only through the control plane's proxy: LLM_PROVIDER=anthropic is a
// configuration error there, key or not (plan R4). Standalone keeps it.
func TestAnthropicIsRefusedWhenEmbedded(t *testing.T) {
	err := (Config{Provider: "anthropic", AnthropicAPIKey: "sk-test"}).Validate()
	if !errors.Is(err, ErrAnthropicEmbedded) {
		t.Fatalf("Validate() = %v, want ErrAnthropicEmbedded", err)
	}
	if !strings.Contains(err.Error(), "through the control plane's proxy (LLM_PROVIDER=openai)") {
		t.Errorf("the error does not say what to use instead: %v", err)
	}
	if err := (Config{Provider: "anthropic", AnthropicAPIKey: "sk-test", Standalone: true}).Validate(); err != nil {
		t.Fatalf("standalone anthropic refused: %v", err)
	}
	if err := (Config{Provider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: "http://host.docker.internal:8080/api/llm/azure",
		ControlPlaneURL: "http://host.docker.internal:8080"}).Validate(); err != nil {
		t.Fatalf("embedded openai through the proxy refused: %v", err)
	}
}

func TestStandaloneIsReadFromTheEnvironment(t *testing.T) {
	t.Setenv("THEBOT_STANDALONE", "")
	if Load().Standalone {
		t.Fatal("standalone with THEBOT_STANDALONE unset")
	}
	t.Setenv("THEBOT_STANDALONE", "true")
	if Load().Standalone {
		t.Fatal("standalone with THEBOT_STANDALONE=true (only 1 turns it on)")
	}
	t.Setenv("THEBOT_STANDALONE", "1")
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if cfg := Load(); !cfg.Standalone || cfg.Validate() != nil {
		t.Fatalf("standalone anthropic from the environment: %+v / %v", cfg.Standalone, cfg.Validate())
	}
	t.Setenv("THEBOT_STANDALONE", "")
	if err := Load().Validate(); !errors.Is(err, ErrAnthropicEmbedded) {
		t.Fatalf("embedded anthropic from the environment: %v", err)
	}
}

// Embedded, a hosted brain is reached only through the control plane's proxy (review M5, plan R4): any other base URL
// is a configuration error at start, as Anthropic is. Standalone keeps any endpoint.
func TestEmbeddedHostedBrainMustBeTheProxy(t *testing.T) {
	cp := "http://host.docker.internal:8080"
	ok := []string{cp + "/api/llm/azure", cp + "/api/llm/claude/", "HTTP://HOST.docker.internal:8080/api/llm/x"}
	bad := []string{"", "https://api.openai.com", "http://host.docker.internal:8080", cp + "/api/other",
		"http://host.docker.internal:8080.example.test/api/llm/azure", "http://host.docker.internal:9999/api/llm/azure",
		// Parses, and shares the control plane's URL as a string prefix, but is another host: only the host
		// comparison refuses it.
		"http://host.docker.internal:80800/api/llm/azure", "http://host.docker.internal.example.test:8080/api/llm/azure",
		"http://u:p@host.docker.internal:8080/api/llm/azure", cp + "/api/llm/../../v1"}
	for _, base := range ok {
		if err := (Config{Provider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: base, ControlPlaneURL: cp}).Validate(); err != nil {
			t.Errorf("%q refused: %v", base, err)
		}
	}
	for _, base := range bad {
		if err := (Config{Provider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: base, ControlPlaneURL: cp}).Validate(); !errors.Is(err, ErrHostedNotProxied) {
			t.Errorf("%q = %v, want ErrHostedNotProxied", base, err)
		}
	}
	if err := (Config{Provider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: cp + "/api/llm/azure"}).Validate(); !errors.Is(err, ErrHostedNotProxied) {
		t.Errorf("no CONTROL_PLANE_URL: %v", err)
	}
	if err := (Config{Provider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: "https://api.openai.com", Standalone: true}).Validate(); err != nil {
		t.Errorf("standalone refused: %v", err)
	}
}
