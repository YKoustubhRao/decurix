package main

import (
	"testing"

	"thebot/internal/config"
)

func TestBuildProviderOpenAI(t *testing.T) {
	cfg := config.Config{Provider: "openai", OpenAIAPIKey: "sk-test", OpenAIModel: "gpt-4o", OpenAIEmbedModel: "text-embedding-3-small"}
	provider, models, embedder := buildProvider(cfg)

	if provider.Name() != "openai" {
		t.Errorf("provider.Name() = %q, want openai", provider.Name())
	}
	if models.Default != "gpt-4o" {
		t.Errorf("models.Default = %q, want gpt-4o", models.Default)
	}
	if embedder == nil {
		t.Fatal("embedder = nil, want the OpenAI provider itself (OpenAI implements Embedder)")
	}
	if any(embedder) != any(provider) {
		t.Error("embedder should be the SAME instance as the chat provider for OpenAI - one API key, one client, no need for two")
	}
}

func TestBuildProviderOllama(t *testing.T) {
	cfg := config.Config{Provider: "ollama", OllamaHost: "http://127.0.0.1:11434", ChatModel: "test-model", EmbedModel: "test-embed"}
	provider, models, embedder := buildProvider(cfg)

	if provider.Name() != "ollama" {
		t.Errorf("provider.Name() = %q, want ollama", provider.Name())
	}
	if models.Default != "test-model" {
		t.Errorf("models.Default = %q, want test-model", models.Default)
	}
	if embedder == nil || any(embedder) != any(provider) {
		t.Error("embedder should be the same Ollama instance as the chat provider")
	}
}

func TestBuildProviderAnthropicNoOllamaConfigured(t *testing.T) {
	cfg := config.Config{Provider: "anthropic", AnthropicAPIKey: "sk-test", AnthropicModel: "claude-test", OllamaHost: ""}
	provider, models, embedder := buildProvider(cfg)

	if provider.Name() != "anthropic" {
		t.Errorf("provider.Name() = %q, want anthropic", provider.Name())
	}
	if models.Default != "claude-test" {
		t.Errorf("models.Default = %q, want claude-test", models.Default)
	}
	if embedder != nil {
		t.Error("embedder should be nil when Anthropic is the chat provider and no OLLAMA_HOST is configured - Anthropic has no embeddings API")
	}
}

func TestBuildProviderAnthropicOllamaUnreachable(t *testing.T) {
	// A real, syntactically valid host that nothing is actually listening
	// on - IsReachable() must return false for real, not just assume it
	// works because a host string was provided.
	cfg := config.Config{Provider: "anthropic", AnthropicAPIKey: "sk-test", AnthropicModel: "claude-test", OllamaHost: "http://127.0.0.1:1"}
	_, _, embedder := buildProvider(cfg)

	if embedder != nil {
		t.Error("embedder should be nil when the configured Ollama host is unreachable, not silently assumed to work")
	}
}
