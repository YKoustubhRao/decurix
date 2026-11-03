// Package config is Brainiac's settings: where it listens, which mode it runs
// in, how to reach a remote brain, and what the selector may choose. They come
// from <home>/config.json and then the environment, which wins.
//
// A key is never stored here: the config names the environment variable that
// holds it (AZURE_KEY), so the file can be shown and copied safely.
//
// Depends on nothing internal.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The modes.
const (
	// ModeLocal: every model runs on this machine. Nothing leaves it except a
	// web search query the client itself wrote and sent ("web": true with
	// "web_query"), and then only when web is not "off".
	ModeLocal = "local"
	// ModeHybrid: pictures and audio are read here; the remote brain gets
	// text evidence (and the pictures only if ShareMedia), and answers.
	ModeHybrid = "hybrid"
	// ModeRemote: the remote brain answers and sees pictures itself;
	// speech and OCR still run here.
	ModeRemote = "remote"
)

// Remote kinds.
const (
	// RemoteDecurix is Decurix's control-plane proxy: the keys stay sealed
	// in Decurix, exactly as for TheBot's hosted brains.
	RemoteDecurix = "decurix"
	// RemoteAzure is an Azure AI endpoint called directly.
	RemoteAzure = "azure"
	// RemoteOpenAI is any OpenAI-compatible endpoint.
	RemoteOpenAI = "openai"
)

// Config is all of Brainiac's settings.
type Config struct {
	Listen string `json:"listen"`
	// APIKeyEnv names the variable holding the key clients must send; empty
	// means no key, which is allowed only on a loopback address.
	APIKeyEnv string  `json:"api_key_env,omitempty"`
	Mode      string  `json:"mode,omitempty"`
	OllamaURL string  `json:"ollama_url,omitempty"`
	Remote    *Remote `json:"remote,omitempty"`
	// ShareMedia lets a remote brain see pictures. Off by default: what is
	// on a screen or a scan stays on this machine unless allowed.
	ShareMedia bool `json:"share_media,omitempty"`
	// LocalFallback answers with the local brain when the remote one fails,
	// saying so in the answer's trace. Off: a weaker answer is not silently
	// passed off as the remote brain's.
	LocalFallback bool `json:"local_fallback,omitempty"`
	// Licenses allowed for models (permissive, restricted, noncommercial).
	Licenses []string `json:"licenses,omitempty"`
	// ReserveGB is memory kept free in every pool; 0 means the selector's default.
	ReserveGB float64 `json:"reserve_gb,omitempty"`
	// PlannedLoadGB is memory other work will need later (desktops to come).
	PlannedLoadGB float64 `json:"planned_load_gb,omitempty"`
	// Threads for llama-server; 0 means the physical cores.
	Threads int `json:"threads,omitempty"`
	// ImageMaxSide shrinks pictures shown to a local vision model; on a CPU
	// every pixel is prompt to read. 0 means 1280; -1 means never shrink.
	ImageMaxSide int `json:"image_max_side,omitempty"`
	// Web decides when an answer may read live web pages. The search query is
	// always one the client wrote and sent as "web_query"; Brainiac never
	// builds a query from the conversation, so with no web_query nothing is
	// looked up. "on-request" (the default): only when the request also sends
	// "web": true. "auto": a request that sends a web_query reads the web
	// without "web": true; refused in local mode. "off": never. What leaves
	// the machine is that query, scrubbed of identity numbers and secrets, and
	// the requests that fetch the result pages.
	Web string `json:"web,omitempty"`
	// SearxURL is a SearXNG instance to search with first.
	SearxURL string `json:"searx_url,omitempty"`
	// BraveKeyEnv names the variable holding a Brave Search API key.
	BraveKeyEnv string `json:"brave_key_env,omitempty"`
	// WebRender lets a client ask for a page to be read with a headless
	// browser (/v1/brainiac/fetch "render": true), for pages that need
	// JavaScript. Default on (off on an appliance); answers never use it on
	// their own. It costs a few hundred MB for a few seconds.
	WebRender *bool `json:"web_render,omitempty"`
	// UpdateEvery is how often `serve` looks for better open models ("off"
	// by default; "24h" for daily): it reads public evaluations, installs a
	// better plan beside the running one, checks it and stages it. It never
	// switches what is served: a person runs `brainiac update promote`.
	UpdateEvery string `json:"update_every,omitempty"`
	// Sandbox "off" lets model processes read and write what this user can.
	// It is for a GPU whose driver needs a path the sandbox does not allow.
	// "required" refuses to start a model process that cannot be fully
	// confined (no network, only its own files): the default on an appliance
	// (BRAINIAC_APPLIANCE set). Empty confines as far as the system allows
	// and reports the rest in status.unconfined.
	Sandbox string `json:"sandbox,omitempty"`
	// MaxLocalSeconds keeps a field with the remote brain when a local model
	// that earned it takes longer than this on average; 0 means no limit.
	MaxLocalSeconds float64 `json:"max_local_seconds,omitempty"`
}

// The sandbox settings: SandboxOff turns the model processes' file sandbox
// off; SandboxRequired starts none that cannot be fully confined.
const (
	SandboxOff      = "off"
	SandboxRequired = "required"
)

// Appliance reports whether this Brainiac runs as an office appliance
// (BRAINIAC_APPLIANCE set), where the safe settings are the defaults.
func Appliance() bool { return strings.TrimSpace(os.Getenv("BRAINIAC_APPLIANCE")) != "" }

// The web settings.
const (
	WebAuto      = "auto"
	WebOnRequest = "on-request"
	WebOff       = "off"
)

// Render reports whether the headless browser may be used.
func (c Config) Render() bool { return c.WebRender == nil || *c.WebRender }

// Remote is how to reach a remote brain.
type Remote struct {
	Kind string `json:"kind"`
	// URL: the control plane (http://127.0.0.1:8080) for decurix, the
	// endpoint for azure, the base URL for openai.
	URL string `json:"url"`
	// Provider is the Decurix hosted brain: "azure" or "claude".
	Provider string `json:"provider,omitempty"`
	// Model is the deployment or model name ("default" through Decurix).
	Model string `json:"model,omitempty"`
	// KeyEnv names the variable holding the key (none for decurix).
	KeyEnv string `json:"key_env,omitempty"`
	// APIVersion for azure (default 2024-05-01-preview).
	APIVersion string `json:"api_version,omitempty"`
	// Vision says the remote model reads pictures.
	Vision bool `json:"vision,omitempty"`
	// USDPerMTokIn and USDPerMTokOut are its price per million tokens, for
	// reporting what answers cost; 0 reports none.
	USDPerMTokIn  float64 `json:"usd_per_mtok_in,omitempty"`
	USDPerMTokOut float64 `json:"usd_per_mtok_out,omitempty"`
}

// Default is the configuration with nothing set.
func Default() Config {
	return Config{Listen: "127.0.0.1:8484", OllamaURL: "http://127.0.0.1:11434"}
}

// Load reads <home>/config.json if present, then the environment.
func Load(home string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &c); err != nil {
			return c, fmt.Errorf("config.json: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return c, err
	}
	c.fromEnv()
	if c.Mode == "" {
		c.Mode = ModeLocal
		if c.Remote != nil {
			c.Mode = ModeHybrid
		}
	}
	if c.Web == "" {
		c.Web = WebOnRequest
	}
	if c.UpdateEvery == "" {
		c.UpdateEvery = "off"
	}
	if c.Sandbox == "" && Appliance() {
		c.Sandbox = SandboxRequired
	}
	if c.WebRender == nil && Appliance() {
		// A headless browser is a large program running a stranger's
		// JavaScript; an office box runs it only when its operator says so.
		off := false
		c.WebRender = &off
	}
	return c, c.Validate()
}

func (c *Config) fromEnv() {
	set := func(key string, into *string) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			*into = v
		}
	}
	set("BRAINIAC_LISTEN", &c.Listen)
	set("BRAINIAC_API_KEY_ENV", &c.APIKeyEnv)
	set("BRAINIAC_MODE", &c.Mode)
	set("OLLAMA_URL", &c.OllamaURL)
	set("BRAINIAC_WEB", &c.Web)
	set("BRAINIAC_SEARX_URL", &c.SearxURL)
	set("BRAINIAC_BRAVE_KEY_ENV", &c.BraveKeyEnv)
	set("BRAINIAC_UPDATE_EVERY", &c.UpdateEvery)
	set("BRAINIAC_SANDBOX", &c.Sandbox)
	if v := os.Getenv("BRAINIAC_MAX_LOCAL_SECONDS"); v != "" {
		c.MaxLocalSeconds, _ = strconv.ParseFloat(v, 64)
	}
	if v := os.Getenv("BRAINIAC_SHARE_MEDIA"); v != "" {
		c.ShareMedia, _ = strconv.ParseBool(v)
	}
	// BRAINIAC_REMOTE=decurix:azure | decurix:claude | azure | openai
	if v := strings.TrimSpace(os.Getenv("BRAINIAC_REMOTE")); v != "" {
		kind, provider, _ := strings.Cut(v, ":")
		r := &Remote{Kind: kind, Provider: provider}
		if c.Remote != nil && c.Remote.Kind == kind {
			*r = *c.Remote
			if provider != "" {
				r.Provider = provider
			}
		}
		c.Remote = r
	}
	if c.Remote != nil {
		set("BRAINIAC_REMOTE_URL", &c.Remote.URL)
		set("BRAINIAC_REMOTE_MODEL", &c.Remote.Model)
		set("BRAINIAC_REMOTE_KEY_ENV", &c.Remote.KeyEnv)
		if v := os.Getenv("BRAINIAC_REMOTE_VISION"); v != "" {
			c.Remote.Vision, _ = strconv.ParseBool(v)
		}
		c.Remote.fill()
	}
}

func (r *Remote) fill() {
	switch r.Kind {
	case RemoteDecurix:
		if r.URL == "" {
			r.URL = "http://127.0.0.1:8080"
		}
		if r.Model == "" {
			r.Model = "default"
		}
	case RemoteAzure:
		if r.APIVersion == "" {
			r.APIVersion = "2024-05-01-preview"
		}
		if r.KeyEnv == "" {
			r.KeyEnv = "AZURE_KEY"
		}
		if r.URL == "" {
			r.URL = os.Getenv("AZURE_ENDPOINT")
		}
		if r.Model == "" {
			r.Model = "gpt-5.4"
		}
	case RemoteOpenAI:
		if r.KeyEnv == "" {
			r.KeyEnv = "OPENAI_API_KEY"
		}
	}
}

// Validate checks the settings make sense and are safe.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeLocal, ModeHybrid, ModeRemote:
	default:
		return fmt.Errorf("mode %q is not local, hybrid or remote", c.Mode)
	}
	switch c.Web {
	case "", WebAuto, WebOnRequest, WebOff:
	default:
		return fmt.Errorf("web %q is not auto, on-request or off", c.Web)
	}
	switch c.Sandbox {
	case "", SandboxOff, SandboxRequired:
	default:
		return fmt.Errorf("sandbox %q is not off or required", c.Sandbox)
	}
	if c.Sandbox == SandboxOff && Appliance() {
		return fmt.Errorf("sandbox off is refused on an appliance: a model process there must not reach what this user can")
	}
	if c.Mode == ModeLocal && c.Web == WebAuto {
		return fmt.Errorf("web auto is refused in local mode, which keeps everything on this machine: use on-request (a request sends \"web\": true and its own \"web_query\") or off")
	}
	if (c.Mode == ModeHybrid || c.Mode == ModeRemote) && c.Remote == nil {
		return fmt.Errorf("mode %s needs a remote brain (BRAINIAC_REMOTE=decurix:azure, azure or openai)", c.Mode)
	}
	if c.Remote != nil {
		switch c.Remote.Kind {
		case RemoteDecurix:
			if c.Remote.Provider != "azure" && c.Remote.Provider != "claude" {
				return fmt.Errorf("a decurix remote names its hosted brain: decurix:azure or decurix:claude")
			}
		case RemoteAzure, RemoteOpenAI:
			if c.Remote.URL == "" {
				return fmt.Errorf("the %s remote needs a URL (BRAINIAC_REMOTE_URL)", c.Remote.Kind)
			}
		default:
			return fmt.Errorf("remote kind %q is not decurix, azure or openai", c.Remote.Kind)
		}
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	if Appliance() && c.APIKey() == "" {
		return fmt.Errorf("an appliance is a shared office machine, so its Brainiac needs an API key (set BRAINIAC_API_KEY_ENV to a variable holding one), even on loopback")
	}
	if !Loopback(host) && c.APIKey() == "" {
		return fmt.Errorf("listening on %s needs an API key (set BRAINIAC_API_KEY_ENV to a variable holding one): Brainiac can use this machine's memory and disk, so it is never open beyond it without one", host)
	}
	return nil
}

// APIKey is the client key, read from the environment variable APIKeyEnv names.
func (c Config) APIKey() string {
	if c.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.APIKeyEnv)
}

// BraveKey is the Brave Search key, read from its environment variable.
func (c Config) BraveKey() string {
	if c.BraveKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.BraveKeyEnv)
}

// RemoteKey is the remote brain's key, read from its environment variable.
func (c Config) RemoteKey() string {
	if c.Remote == nil || c.Remote.KeyEnv == "" {
		return ""
	}
	return os.Getenv(c.Remote.KeyEnv)
}

// Loopback reports whether host is this machine only.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Save writes the config to <home>/config.json.
func (c Config) Save(home string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "config.json"), append(data, '\n'), 0o644)
}
