package ollama

import (
	"context"
	"log"
	"net"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Model selection is on once main calls EnableAutoSelect, and applies only
// while OLLAMA_MODEL is unset or "auto": a model somebody named is a choice,
// and is left alone (the best fit is still reported next to it).
//
// The pick is sticky. A desktop bakes OLLAMA_MODEL into its container when it
// is created, and Ollama here holds one model at a time, so a pick that moved
// with every change in load would leave desktops on different models, each
// evicting the other's. It is made when the control plane starts, and again
// only when a desktop is about to be created and there are none - see
// Reselect. Status always shows what fits *now*, next to what is in use.
var auto struct {
	mu      sync.Mutex
	enabled bool
	sel     *Selection
	at      time.Time
	// seed is the model existing desktops run (SeedCurrentModel), the "current" of a fresh process's first pick.
	seed string
}

// liveCacheTTL is how long Status reuses one measurement. Probing samples the
// CPU for a moment and runs nvidia-smi; the top bar polls.
const liveCacheTTL = 20 * time.Second

var live struct {
	mu sync.Mutex
	// refreshing is a measurement in flight (refreshLive): one at a time, and nobody waits on it.
	refreshing bool
	at         time.Time
	url        string
	sel        Selection
	res        Resources
	mode       string
	err        error
}

// EnableAutoSelect turns model selection on. It is off until main calls this,
// so a test that builds a desktop's environment never probes the machine.
func EnableAutoSelect() {
	auto.mu.Lock()
	auto.enabled = true
	auto.mu.Unlock()
}

// pinnedModel is the model OLLAMA_MODEL names, or "" when it is unset or "auto".
func pinnedModel() string {
	m := strings.TrimSpace(os.Getenv("OLLAMA_MODEL"))
	if strings.EqualFold(m, "auto") {
		return ""
	}

	return m
}

// autoModel is the selected model, if selection has run and applies.
func autoModel() (string, bool) {
	auto.mu.Lock()
	defer auto.mu.Unlock()

	if !auto.enabled || auto.sel == nil || auto.sel.Model.Name == "" {
		return "", false
	}

	return auto.sel.Model.Name, true
}

// evaluate measures this machine and chooses. mode is "remote" when the Ollama
// the desktops use is on another machine - this machine's memory says nothing
// about it, so nothing is chosen.
func evaluate(ctx context.Context, current string) (sel Selection, res Resources, mode string, err error) {
	raw := DesktopURL()
	bundled := raw == BundledURL

	if !bundled && !isLocalURL(raw) {
		return Selection{}, Resources{}, "remote", nil
	}

	if native := os.Getenv("DECURIX_WINDOWS_OLLAMA_URL"); native != "" && strings.TrimRight(raw, "/") == strings.TrimRight(native, "/") {
		result, err := windowsSelection(ctx, current)
		return result.Selection, result.Resources, "auto", err
	}

	base := raw
	if !bundled {
		base = dialableURL(ctx, raw)
	}

	// The bundled container is created without GPU access, so a GPU is
	// counted only for an Ollama running on the host itself.
	res, err = ProbeResources(ctx, base, !bundled)
	if err != nil {
		return Selection{}, Resources{}, "auto", err
	}

	installed, _ := listInstalled(ctx, base)
	canPull := bundled || os.Getenv("DECURIX_MODEL_AUTOPULL") == "1"

	return Choose(res, installed, canPull, current), res, "auto", nil
}

// SelectModel makes the pick for this run and records it; EnsureReady calls
// it once the Ollama the desktops use can be asked what it has installed.
func SelectModel(ctx context.Context) error {
	auto.mu.Lock()
	on := auto.enabled
	auto.mu.Unlock()

	if !on || pinnedModel() != "" {
		return nil
	}

	return apply(ctx)
}

// Reselect refreshes the pick. The caller says whether any desktop exists,
// because only with none can the model change without leaving a desktop on
// one the shared Ollama then has to swap against.
func Reselect(ctx context.Context, desktopsExist bool) {
	if desktopsExist {
		return
	}

	SelectModel(ctx)
}

func apply(ctx context.Context) error {
	cur, _ := autoModel()
	if cur == "" {
		auto.mu.Lock()
		cur = auto.seed
		auto.mu.Unlock()
	}

	sel, res, mode, err := evaluate(ctx, cur)
	if err != nil {
		log.Printf("Model selection: could not measure this machine: %v - keeping %s", err, DesktopModel())
		return err
	}

	if mode == "remote" {
		log.Printf("Model selection: the Ollama at %s is not on this machine - using %s as configured.", DesktopURL(), DesktopModel())
		return nil
	}

	if sel.Model.Name == "" {
		log.Printf("Model selection: no model to choose from - using %s.", DesktopModel())
		return nil
	}

	auto.mu.Lock()
	auto.sel, auto.at = &sel, time.Now()
	auto.mu.Unlock()

	log.Printf(
		"Model selection: %s (%.1f GB) - %s [RAM %.1f/%.1f GB free, %d cores %.0f%% busy%s]",
		sel.Model.Name, sel.Model.SizeGB, sel.Reason,
		res.RAMFreeGB, res.RAMTotalGB, res.Cores, res.CPUBusy*100, gpuNote(res),
	)

	if !sel.Model.Installed {
		log.Printf("Model selection: %s is not installed on that Ollama%s.", sel.Model.Name, pullHint())
	}

	if sel.Best.Name != "" && sel.Best.Name != sel.Model.Name {
		log.Printf("Model selection: %s would fit better but is not installed - `ollama pull %s` to use it.", sel.Best.Name, sel.Best.Name)
	}
	return nil
}

func gpuNote(r Resources) string {
	if !r.HasGPU() {
		return ", no GPU"
	}

	return ", " + r.GPUName + " " + strconv.FormatFloat(r.VRAMFreeGB, 'f', 1, 64) + " GB VRAM free"
}

func pullHint() string {
	if DesktopURL() == BundledURL {
		return " and will be pulled"
	}

	return ""
}

// Status is what the top bar shows.
type Status struct {
	// Mode is "auto" (chosen for this machine), "pinned" (OLLAMA_MODEL names
	// a model) or "remote" (the Ollama is on another machine).
	Mode string `json:"mode"`
	// Model is what new desktops are given.
	Model     string `json:"model"`
	Installed bool   `json:"installed"`
	// Best is what fits this machine best right now among the models it can
	// run, whether or not it is what is in use. Pull, when set, is a model
	// that would fit better still but is not installed.
	Best      string      `json:"best,omitempty"`
	BestSize  float64     `json:"best_size_gb,omitempty"`
	Pull      string      `json:"pull,omitempty"`
	Fits      bool        `json:"fits"`
	OnGPU     bool        `json:"on_gpu"`
	TPS       float64     `json:"est_tokens_per_second,omitempty"`
	Reason    string      `json:"reason"`
	AlsoFits  []Candidate `json:"also_fits,omitempty"`
	Budget    Budget      `json:"budget"`
	Resources Resources   `json:"resources"`
	ChosenAt  *time.Time  `json:"chosen_at,omitempty"`
}

// CurrentStatus reports the model in use and what fits the machine now, from what is already known: it never waits on a
// measurement (liveEvaluation). The dashboard's top bar asks on every page load and every minute, and a measurement
// waits on the model host - the Windows selector answers in up to 30-35 s, or not at all when local hosting is off -
// so every page load and every minute took 30 s (resource bench, 2026-11-05).
func CurrentStatus(ctx context.Context) Status {
	// An Ollama on another machine needs no measurement: said at once.
	if raw := DesktopURL(); raw != BundledURL && !isLocalURL(raw) {
		return Status{Mode: "remote", Model: DesktopModel(),
			Reason: "The Ollama server is on another machine, so its model is used as configured."}
	}
	sel, res, mode, err, known := liveEvaluation()

	st := Status{Mode: mode, Model: DesktopModel()}
	if !known {
		st.Mode = "auto"
		if pinnedModel() != "" {
			st.Mode = "pinned"
		}
		st.Reason = "Measuring this machine…"
		return st
	}
	if err != nil {
		st.Reason = "Could not measure this machine."
		return st
	}

	if mode == "remote" {
		st.Reason = "The Ollama server is on another machine, so its model is used as configured."
		return st
	}

	st.Budget, st.Resources = sel.Budget, res
	st.Best, st.BestSize = sel.Model.Name, sel.Model.SizeGB
	if sel.Best.Name != "" && sel.Best.Name != sel.Model.Name {
		st.Pull = sel.Best.Name
	}
	st.AlsoFits = sel.AlsoFits

	if pinnedModel() != "" {
		st.Mode = "pinned"
		st.Reason = "Set by OLLAMA_MODEL. Set it to \"auto\" to let Decurix choose."
		return st
	}

	auto.mu.Lock()
	chosen, at := auto.sel, auto.at
	auto.mu.Unlock()

	if chosen == nil {
		st.Reason = sel.Reason
		// The measurement describes what fits now; it says how well the model
		// in use fits only when that is the same model.
		if normName(sel.Model.Name) == normName(st.Model) {
			st.Fits, st.OnGPU, st.TPS, st.Installed = sel.Fits, sel.OnGPU, sel.TPS, sel.Model.Installed
		}
		return st
	}

	st.ChosenAt = &at
	st.Installed, st.Fits, st.OnGPU, st.TPS = chosen.Model.Installed, chosen.Fits, chosen.OnGPU, chosen.TPS
	st.Reason = chosen.Reason

	// When what fits now is the model in use, the fresh measurement says how
	// well it fits; the one made at startup may be hours old.
	if normName(sel.Model.Name) == normName(chosen.Model.Name) {
		st.Installed, st.Fits, st.OnGPU, st.TPS = sel.Model.Installed, sel.Fits, sel.OnGPU, sel.TPS
		st.Reason = sel.Reason
	}

	return st
}

// liveEvaluation is the latest measurement of what fits the machine now, and whether there is one for the Ollama the
// desktops use. It returns at once: a measurement older than liveCacheTTL, or none, starts one in the background
// (refreshLive), one at a time, and the caller gets what was known before it.
func liveEvaluation() (Selection, Resources, string, error, bool) {
	url := DesktopURL()
	live.mu.Lock()
	defer live.mu.Unlock()
	known := !live.at.IsZero() && live.url == url
	if (!known || time.Since(live.at) >= liveCacheTTL) && !live.refreshing {
		live.refreshing = true
		go refreshLive(url)
	}
	return live.sel, live.res, live.mode, live.err, known
}

// RefreshStatus measures now and waits for it, for a caller that wants the answer measured (a test, a refresh asked
// for by hand); CurrentStatus never waits.
func RefreshStatus(ctx context.Context) {
	url := DesktopURL()
	sel, res, mode, err := liveEvaluate(ctx, "")
	live.mu.Lock()
	live.sel, live.res, live.mode, live.err = sel, res, mode, err
	live.at, live.url = time.Now(), url
	live.mu.Unlock()
}

// PrimeStatus starts the first measurement in the background, so the dashboard's first look already has one.
func PrimeStatus() {
	_, _, _, _, _ = liveEvaluation()
}

// liveEvaluate measures what fits now (evaluate, with no current model: what fits, not what is in use); a test
// stands in a slow one.
var liveEvaluate = evaluate

// liveRefreshed is called after each background measurement; a test waits on it.
var liveRefreshed = func() {}

// liveRefreshTimeout bounds one background measurement, so a host that never answers frees the next one.
const liveRefreshTimeout = 45 * time.Second

// refreshLive measures in the background and keeps the result for liveEvaluation.
func refreshLive(url string) {
	ctx, cancel := context.WithTimeout(context.Background(), liveRefreshTimeout)
	defer cancel()
	sel, res, mode, err := liveEvaluate(ctx, "")
	live.mu.Lock()
	live.sel, live.res, live.mode, live.err = sel, res, mode, err
	live.at, live.url = time.Now(), url
	live.refreshing = false
	live.mu.Unlock()
	liveRefreshed()
}

// listInstalled lists the chat models the Ollama at baseURL has (GET
// /api/tags), leaving out embedding models.
func listInstalled(ctx context.Context, baseURL string) ([]Candidate, error) {
	// The timeout covers the per-model /api/show calls below too.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var body struct {
		Models []struct {
			Name    string `json:"name"`
			Size    int64  `json:"size"`
			Digest  string `json:"digest"`
			Details struct {
				Family        string   `json:"family"`
				Families      []string `json:"families"`
				ParameterSize string   `json:"parameter_size"`
			} `json:"details"`
		} `json:"models"`
	}
	if _, err := getJSON(ctx, baseURL+"/api/tags", 5*time.Second, 4<<20, &body); err != nil {
		return nil, err
	}

	var out []Candidate
	for _, m := range body.Models {
		if isEmbedding(m.Name, m.Details.Family, m.Details.Families) {
			continue
		}

		c := Candidate{Name: m.Name, SizeGB: float64(m.Size) / gb, Installed: true}
		c.ParamsB = parseParamSize(m.Details.ParameterSize)
		if c.ParamsB == 0 {
			c.ParamsB = c.SizeGB / 0.6 // ~0.6 GB per billion parameters at 4 bits
		}

		// The catalog knows which installed models are mixture-of-experts.
		for _, k := range catalog {
			if normName(k.Name) == normName(m.Name) {
				c.ActiveB = k.ActiveB
			}
		}
		c.KVPerToken = installedKVBytes(ctx, baseURL, m.Name, m.Digest)

		out = append(out, c)
	}

	return out, nil
}

// isEmbedding reports whether an installed model only embeds text.
func isEmbedding(name, family string, families []string) bool {
	n := strings.ToLower(name)
	for _, w := range []string{"embed", "minilm", "bge-", "rerank"} {
		if strings.Contains(n, w) {
			return true
		}
	}

	for _, f := range append([]string{family}, families...) {
		if strings.Contains(strings.ToLower(f), "bert") {
			return true
		}
	}

	return false
}

// parseParamSize reads Ollama's "7.6B" / "494.03M" into billions.
func parseParamSize(s string) float64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}

	unit := s[len(s)-1]
	v, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0
	}

	switch unit {
	case 'B':
		return v
	case 'M':
		return v / 1000
	case 'T':
		return v * 1000
	}

	return 0
}

// isLocalURL reports whether the Ollama at raw runs on this machine: loopback,
// the name a container uses for its host, or an address of one of this
// machine's own interfaces.
func isLocalURL(raw string) bool {
	// The Windows launcher resolves the WSL gateway on each start. Its
	// explicitly marked native server uses a separate Windows RAM budget.
	if native := os.Getenv("DECURIX_WINDOWS_OLLAMA_URL"); native != "" && strings.TrimRight(raw, "/") == strings.TrimRight(native, "/") {
		return true
	}

	u, err := neturl.Parse(raw)
	if err != nil {
		return false
	}

	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "host.docker.internal" || host == "decurix-ollama" {
		return true
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}

	return false
}
