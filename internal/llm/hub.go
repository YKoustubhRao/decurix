package llm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sealed"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// The hosted providers a desktop's brain can be. "ollama" (the local model)
// is not one of them: it needs no key and never goes through this package.
const (
	ProviderAzure  = "azure"
	ProviderClaude = "claude"
	// ProviderOpenAI is any OpenAI-compatible server: OpenAI itself by default, or another named by its base address
	// (OpenRouter; a Brainiac box on the network, which may run on plain http).
	ProviderOpenAI = "openai"
)

// KeyFile is the data-dir file sealing the saved API keys (sealed.LoadKey).
const KeyFile = "llm.key"

// Providers lists the hosted providers, in the order the dashboard shows them.
var Providers = []string{ProviderClaude, ProviderAzure, ProviderOpenAI}

// ValidProvider reports whether p names a hosted provider.
func ValidProvider(p string) bool {
	return p == ProviderAzure || p == ProviderClaude || p == ProviderOpenAI
}

// Settings is what a person types in to set up one provider. Key is never
// read back out of the Hub; an empty Key on a save keeps the saved one.
type Settings struct {
	Endpoint  string `json:"endpoint,omitempty"` // Azure: required; OpenAI-compatible: optional (OpenAI by default)
	Key       string `json:"-"`
	Model     string `json:"model"`
	FastModel string `json:"fast_model"`
	// Vision is whether the model reads images, found by Test (Claude's
	// current models all do; an Azure deployment is shown a picture).
	Vision bool `json:"vision"`
}

// ProviderStatus is one provider as the dashboard sees it: never the key.
type ProviderStatus struct {
	Configured bool   `json:"configured"`
	Endpoint   string `json:"endpoint,omitempty"`
	Model      string `json:"model,omitempty"`
	FastModel  string `json:"fast_model,omitempty"`
	Vision     bool   `json:"vision"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

var schema = []string{
	`CREATE TABLE llm_providers (
		provider   TEXT PRIMARY KEY,
		endpoint   TEXT NOT NULL DEFAULT '',
		api_key    TEXT NOT NULL,
		model      TEXT NOT NULL DEFAULT '',
		fast_model TEXT NOT NULL DEFAULT '',
		updated_ns INTEGER NOT NULL
	)`,
	`ALTER TABLE llm_providers ADD COLUMN vision INTEGER NOT NULL DEFAULT 0`,
}

// Hub holds the configured hosted providers and forwards the desktops'
// chats to them. Settings changed through it apply to the next request: a
// desktop names a provider, never a key, so nothing needs recreating.
type Hub struct {
	db  *sql.DB // nil keeps settings in memory only (tests)
	key []byte  // nil stores keys unsealed (a key file that could not be written)

	mu      sync.RWMutex
	azure   *Client
	claude  *Claude
	openai  *Client
	updated map[string]time.Time
	vision  map[string]bool

	// ClaudeBaseURL points Claude at another address, and HTTP is the client
	// every provider call uses (tests: a TLS fake's own client).
	ClaudeBaseURL string
	HTTP          *http.Client
}

// NewHub opens the Hub on db (nil for memory only), sealing keys with key,
// and loads what was saved.
func NewHub(db *sql.DB, key []byte) (*Hub, error) {
	h := &Hub{db: db, key: key, updated: map[string]time.Time{}, vision: map[string]bool{}}

	if db == nil {
		return h, nil
	}

	if err := sqlitedb.Migrate(db, "llm", schema); err != nil {
		return nil, err
	}

	type saved struct {
		provider string
		s        Settings
		updated  int64
	}
	all, err := sqlitedb.QueryAll(db, func(row sqlitedb.Scanner) (saved, error) {
		var r saved
		err := row.Scan(&r.provider, &r.s.Endpoint, &r.s.Key, &r.s.Model, &r.s.FastModel, &r.s.Vision, &r.updated)
		return r, err
	}, `SELECT provider, endpoint, api_key, model, fast_model, vision, updated_ns FROM llm_providers`)
	if err != nil {
		return nil, err
	}

	for _, r := range all {
		plain, err := sealed.Open(key, r.s.Key)
		if err != nil {
			// A key file that no longer opens it: the provider shows as
			// not set up, and a person types the key in again.
			continue
		}
		r.s.Key = plain
		h.apply(r.provider, r.s, sqlitedb.FromNS(r.updated))
	}

	return h, nil
}

// apply installs one provider's settings in memory. Callers hold no lock.
func (h *Hub) apply(provider string, s Settings, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	switch provider {
	case ProviderAzure:
		h.azure = h.newAzure(s)
	case ProviderClaude:
		h.claude = h.newClaude(s)
	case ProviderOpenAI:
		h.openai = h.newOpenAI(s)
	}
	h.updated[provider] = at
	h.vision[provider] = s.Vision || provider == ProviderClaude
}

// newAzure, newOpenAI and newClaude build a provider client with the Hub's test seams.
func (h *Hub) newAzure(s Settings) *Client {
	c := NewAzure(s.Endpoint, s.Key, s.Model, s.FastModel)
	if h.HTTP != nil {
		c.HTTP = h.HTTP
	}
	return c
}

func (h *Hub) newOpenAI(s Settings) *Client {
	c := NewOpenAI(s.Endpoint, s.Key, s.Model, s.FastModel)
	if h.HTTP != nil {
		c.HTTP = h.HTTP
	}
	return c
}

func (h *Hub) newClaude(s Settings) *Claude {
	c := NewClaude(s.Key, s.Model, s.FastModel)
	c.BaseURL = h.ClaudeBaseURL
	if h.HTTP != nil {
		c.HTTP = h.HTTP
	}
	return c
}

// Configured reports whether a provider can answer.
func (h *Hub) Configured(provider string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	switch provider {
	case ProviderAzure:
		return h.azure.Configured()
	case ProviderClaude:
		return h.claude.Configured()
	case ProviderOpenAI:
		return h.openai.Configured()
	}

	return false
}

// AnyConfigured reports whether any hosted provider is set up.
func (h *Hub) AnyConfigured() bool {
	return h.Configured(ProviderAzure) || h.Configured(ProviderClaude) || h.Configured(ProviderOpenAI)
}

// Model is the main model of a configured provider ("" otherwise): what a
// desktop with that brain lists as its model.
func (h *Hub) Model(provider string) string {
	return h.Status()[provider].Model
}

// Status describes every provider, keys left out.
func (h *Hub) Status() map[string]ProviderStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := map[string]ProviderStatus{}

	az := ProviderStatus{}
	if h.azure.Configured() {
		az = ProviderStatus{Configured: true, Endpoint: h.azure.Endpoint, Model: h.azure.Model, FastModel: h.azure.FastModel}
	}
	cl := ProviderStatus{}
	if h.claude.Configured() {
		cl = ProviderStatus{Configured: true, Model: h.claude.Model, FastModel: h.claude.FastModel}
	}
	op := ProviderStatus{}
	if h.openai.Configured() {
		op = ProviderStatus{Configured: true, Endpoint: h.openai.Endpoint, Model: h.openai.Model, FastModel: h.openai.FastModel}
	}

	for p, st := range map[string]*ProviderStatus{ProviderAzure: &az, ProviderClaude: &cl, ProviderOpenAI: &op} {
		if t, ok := h.updated[p]; ok && st.Configured {
			st.UpdatedAt = t.UTC().Format(time.RFC3339)
			st.Vision = h.vision[p]
		}
		out[p] = *st
	}

	return out
}

// savedKey is the key currently in use for a provider ("" when none).
func (h *Hub) savedKey(provider string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	switch provider {
	case ProviderAzure:
		if h.azure != nil {
			return h.azure.Key
		}
	case ProviderClaude:
		if h.claude != nil {
			return h.claude.Key
		}
	case ProviderOpenAI:
		if h.openai != nil {
			return h.openai.Key
		}
	}

	return ""
}

// privateHost reports whether an endpoint's host is this machine's own or on its network: a server there may run on
// plain http (a Brainiac box, a model served in WSL); anything else must be https, or the key goes out in the clear.
func privateHost(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "host.docker.internal" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// Complete fills in what a save left out (the saved key when Key is empty)
// and checks the settings, without saving them.
func (h *Hub) Complete(provider string, s Settings) (Settings, error) {
	if !ValidProvider(provider) {
		return s, fmt.Errorf("unknown provider %q", provider)
	}

	s.Endpoint = strings.TrimRight(strings.TrimSpace(s.Endpoint), "/")
	s.Key = strings.TrimSpace(s.Key)
	s.Model = strings.TrimSpace(s.Model)
	s.FastModel = strings.TrimSpace(s.FastModel)

	if s.Key == "" {
		s.Key = h.savedKey(provider)
	}
	if s.Endpoint == "" && provider == ProviderAzure {
		h.mu.RLock()
		if h.azure != nil {
			s.Endpoint = h.azure.Endpoint
		}
		h.mu.RUnlock()
	}
	if s.Key == "" {
		return s, errors.New("an API key is needed")
	}

	switch provider {
	case ProviderAzure:
		u, err := url.Parse(s.Endpoint)
		if s.Endpoint == "" || err != nil || u.Scheme != "https" || u.Host == "" {
			return s, errors.New("the endpoint must be the https:// address of your Azure AI resource")
		}
	case ProviderOpenAI:
		if s.Endpoint == "" {
			s.Endpoint = DefaultOpenAIEndpoint
		}
		u, err := url.Parse(s.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && privateHost(u))) {
			return s, errors.New("the endpoint must be the https:// address of an OpenAI-compatible server " +
				"(http:// only for one on this machine or your own network)")
		}
	default:
		s.Endpoint = ""
	}

	return s, nil
}

// Save stores a provider's settings (Complete first) and puts them in use.
func (h *Hub) Save(provider string, s Settings) error {
	s, err := h.Complete(provider, s)
	if err != nil {
		return err
	}

	now := time.Now()

	if h.db != nil {
		stored := s.Key
		if h.key != nil {
			if stored, err = sealed.Seal(h.key, s.Key); err != nil {
				return err
			}
		}

		if _, err := h.db.Exec(
			`INSERT INTO llm_providers (provider, endpoint, api_key, model, fast_model, vision, updated_ns)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(provider) DO UPDATE SET endpoint = excluded.endpoint, api_key = excluded.api_key,
			   model = excluded.model, fast_model = excluded.fast_model, vision = excluded.vision, updated_ns = excluded.updated_ns`,
			provider, s.Endpoint, stored, s.Model, s.FastModel, s.Vision, sqlitedb.ToNS(now),
		); err != nil {
			return err
		}
	}

	h.apply(provider, s, now)

	return nil
}

// Forget removes a provider's settings. Desktops with that brain fail their
// next request with "not set up" until a person sets it up again.
func (h *Hub) Forget(provider string) error {
	if !ValidProvider(provider) {
		return fmt.Errorf("unknown provider %q", provider)
	}

	if h.db != nil {
		if _, err := h.db.Exec(`DELETE FROM llm_providers WHERE provider = ?`, provider); err != nil {
			return err
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	switch provider {
	case ProviderAzure:
		h.azure = nil
	case ProviderClaude:
		h.claude = nil
	case ProviderOpenAI:
		h.openai = nil
	}
	delete(h.updated, provider)
	delete(h.vision, provider)

	return nil
}

// Chat forwards one OpenAI-shaped chat body to a provider (see Client.Chat
// and Claude.Chat for what comes back).
func (h *Hub) Chat(ctx context.Context, provider string, body []byte) (int, []byte, http.Header, error) {
	h.mu.RLock()
	az, cl, op := h.azure, h.claude, h.openai
	h.mu.RUnlock()

	// Each Chat answers ErrNotConfigured itself, a nil client included.
	switch provider {
	case ProviderAzure:
		return az.Chat(ctx, body)
	case ProviderClaude:
		return cl.Chat(ctx, body)
	case ProviderOpenAI:
		return op.Chat(ctx, body)
	}

	return 0, nil, nil, fmt.Errorf("llm: unknown provider %q", provider)
}

// Test asks a provider, with the given settings (Complete first), for a
// one-word answer - what the dashboard's "Check & save" runs, so a mistyped
// key is found while the person is still looking at the form - and then
// whether it reads images: Claude's current models all do; an Azure
// deployment is whatever the person named, so it is shown a one-pixel
// picture, and a refusal means text only. The answer decides whether TheBot
// is shown the screen and scanned pages as pictures.
func (h *Hub) Test(ctx context.Context, provider string, s Settings) (vision bool, err error) {
	s, err = h.Complete(provider, s)
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	var chat func(context.Context, []byte) (int, []byte, http.Header, error)
	switch provider {
	case ProviderAzure:
		chat = h.newAzure(s).Chat
	case ProviderClaude:
		chat = h.newClaude(s).Chat
	case ProviderOpenAI:
		chat = h.newOpenAI(s).Chat
	}

	body := []byte(`{"model":"default","max_completion_tokens":64,"messages":[{"role":"user","content":"Reply with the word OK."}]}`)
	status, answer, _, err := chat(ctx, body)
	if err != nil {
		return false, fmt.Errorf("could not reach it: %w", err)
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("it answered %d: %s", status, upstreamMessage(answer))
	}

	if provider == ProviderClaude {
		return true, nil
	}

	picture, _ := json.Marshal(map[string]any{
		"model":                 "default",
		"max_completion_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "What colour is this picture? One word."},
			map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + onePixelPNG}},
		}}},
	})
	status, _, _, err = chat(ctx, picture)

	return err == nil && status == http.StatusOK, nil
}

// onePixelPNG is a 1x1 red PNG.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

// upstreamMessage is the error message inside an answer body, or the body.
func upstreamMessage(body []byte) string {
	if message := errorMessage(body); message != "" {
		return message
	}

	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "..."
	}

	return s
}
