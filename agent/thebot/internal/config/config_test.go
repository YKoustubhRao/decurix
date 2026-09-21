package config

import "testing"

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
	if err := (Config{Provider: "anthropic", AnthropicAPIKey: "sk-test"}).Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil for a valid anthropic config", err)
	}
	if err := (Config{Provider: "anthropic"}).Validate(); err == nil {
		t.Error("Validate() = nil for anthropic with no API key, want an error")
	}
}

func TestValidateOpenAI(t *testing.T) {
	if err := (Config{Provider: "openai", OpenAIAPIKey: "sk-test"}).Validate(); err != nil {
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
