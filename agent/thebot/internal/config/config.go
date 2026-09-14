// Package config loads TheBot's runtime configuration from the
// environment - no config files, no flags framework, just env vars with
// sane defaults so a VM provider can start the container with zero
// configuration and override only what they need to.
package config

import (
	"fmt"
	"os"
)

type Config struct {
	// WorkspaceRoot is where the AI always works - writes files here,
	// downloads here (including via Playwright later), and is the only
	// directory docling/RAG ever scope into. Defaults to the real path
	// decided for the embedded VM deployment; overridable for local dev.
	WorkspaceRoot string

	// Provider selects the chat brain: "ollama" (default, local,
	// zero-config), "anthropic", or "openai" - bring-your-own-key for the
	// latter two, opt-in, never baked into an image. Ollama itself is now
	// optional: only required when Provider is "ollama", OR as a fallback
	// embedder when Provider is "anthropic" (Anthropic has no embeddings
	// API at all - see internal/llm/anthropic.go).
	Provider string

	OllamaHost string // only required if Provider=ollama, or as an embeddings fallback for Provider=anthropic
	ChatModel  string // Ollama model name, only used if Provider=ollama

	// CodingModel routes the coding harness (internal/coding) to its own
	// model, leaving conversation, planning and failure-reasoning on
	// ChatModel. Empty means "use ChatModel", so nothing changes unless a
	// deployment configures it.
	//
	// The two workloads genuinely want different things. A build sends the
	// longest prompts the agent ever produces - the whole architecture,
	// every file written so far, the last compiler error - and asks for a
	// COMPLETE source file back, so it wants the largest context window
	// available. Chat wants a good conversationalist and short answers. On
	// a single GPU only one large model stays resident, so splitting the
	// roles is also what decides which model is loaded when.
	CodingModel string

	AnthropicAPIKey string
	AnthropicModel  string

	OpenAIAPIKey string
	OpenAIModel  string
	// OpenAIEmbedModel is separate from OpenAIModel (chat) - OpenAI's
	// embeddings and chat are different model families.
	OpenAIEmbedModel string

	EmbedModel string // Ollama's embed model name, only used if Ollama is the embedder

	// Port is loopback-only - "embedded, not an external API" means this
	// is how the VM's own local UI talks to the soul process, never a
	// network-facing contract for third parties.
	Port string
	// DoclingScript is the path to scripts/docling_convert.py - a Python
	// subprocess the Data RAG shells out to for real PDF/DOCX/HTML/image
	// parsing (docling is Python-only, so this dependency doesn't go away
	// just because the harness itself is Go). Default assumes the process
	// runs from the repo root (true for local `go run ./cmd/soul` and will
	// be true in the Docker image too); override for other layouts.
	DoclingScript string
}

func Load() Config {
	return Config{
		WorkspaceRoot: getenv("WORKSPACE_ROOT", "/home/headless/.flowera/ai"),

		Provider:   getenv("LLM_PROVIDER", "ollama"),
		OllamaHost: getenv("OLLAMA_HOST", "http://127.0.0.1:11434"),
		// qwen2.5-coder:14b, not devstral-small-2:24b - devstral repeatedly
		// hit a real VRAM cold-load crash during this session's testing
		// (~15.9GB, right at the edge of a 16GB card); qwen has been solid
		// through every test. Keep this and VMDockerfile's CHAT_MODEL in
		// sync - they silently diverged once already.
		ChatModel: getenv("CHAT_MODEL", "qwen2.5-coder:14b"),
		// No default: unset means "use CHAT_MODEL for coding too", which is
		// the right behaviour for a single-model deployment. Naming a model
		// here is an explicit choice to run two.
		CodingModel: getenv("CODING_MODEL", ""),

		AnthropicAPIKey: getenv("ANTHROPIC_API_KEY", ""),
		AnthropicModel:  getenv("ANTHROPIC_MODEL", "claude-sonnet-4-5"),

		OpenAIAPIKey:     getenv("OPENAI_API_KEY", ""),
		OpenAIModel:      getenv("OPENAI_MODEL", "gpt-4o"),
		OpenAIEmbedModel: getenv("OPENAI_EMBED_MODEL", "text-embedding-3-small"),

		EmbedModel: getenv("EMBED_MODEL", "nomic-embed-text"),

		Port:          getenv("SOUL_PORT", "4900"),
		DoclingScript: getenv("DOCLING_SCRIPT", "scripts/docling_convert.py"),
	}
}

// Validate checks the provider/key combination is actually usable - fails
// loudly and specifically at startup rather than letting a misconfigured
// provider surface as a confusing runtime error on the first chat request.
func (c Config) Validate() error {
	switch c.Provider {
	case "ollama":
		if c.OllamaHost == "" {
			return fmt.Errorf("LLM_PROVIDER=ollama requires OLLAMA_HOST to be set")
		}
	case "anthropic":
		if c.AnthropicAPIKey == "" {
			return fmt.Errorf("LLM_PROVIDER=anthropic requires ANTHROPIC_API_KEY to be set")
		}
	case "openai":
		if c.OpenAIAPIKey == "" {
			return fmt.Errorf("LLM_PROVIDER=openai requires OPENAI_API_KEY to be set")
		}
	default:
		return fmt.Errorf("unknown LLM_PROVIDER %q - must be one of: ollama, anthropic, openai", c.Provider)
	}
	return nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
