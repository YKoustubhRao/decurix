package census

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const gib = 1 << 30

// Options says what Take measures.
type Options struct {
	// StoreDir is where models will live; disk space is measured there (or at
	// its nearest existing parent). Nothing is created. Empty means the home
	// directory.
	StoreDir string
	// OllamaURL is the Ollama to read. Empty means OLLAMA_URL, then
	// OLLAMA_HOST, then http://127.0.0.1:11434.
	OllamaURL string
	// Bench runs the short memory-bandwidth test (about a second).
	Bench bool
	// Network checks that Hugging Face, GitHub and PyPI answer.
	Network bool
}

// notes collects what could not be measured, safely from several goroutines.
type notes struct {
	mu   sync.Mutex
	list []string
}

// add records one note.
func (n *notes) add(format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, fmt.Sprintf(format, args...))
}

// Take measures this machine. Whatever cannot be measured is said in Notes;
// the only error is the context ending first.
func Take(ctx context.Context, opt Options) (Census, error) {
	n := &notes{}
	c := Census{Schema: Schema, TakenAt: time.Now().UTC()}

	c.Host = Host{OS: runtime.GOOS, Arch: runtime.GOARCH}
	c.Host.Hostname, _ = os.Hostname()
	hostFlags(&c.Host)

	c.CPU = probeCPU(n)

	hostTotal, hostAvail, kind, source, err := hostMemory()
	if err != nil {
		n.add("host memory could not be read: %v", err)
	} else {
		c.Pools = append(c.Pools, Pool{Name: "host", Kind: kind, TotalGB: toGB(hostTotal), AvailableGB: toGB(hostAvail), Source: source})
	}
	// The WSL VM and the GPU tools are each a second or more to ask.
	var extra []Pool
	var wgPools sync.WaitGroup
	wgPools.Add(2)
	go func() { defer wgPools.Done(); extra = extraPools(ctx, n) }()
	go func() { defer wgPools.Done(); c.GPUs = probeGPUs(ctx, n, hostTotal) }()
	wgPools.Wait()
	c.Pools = append(c.Pools, extra...)

	for _, g := range c.GPUs {
		if g.Pool != "host" && !g.Unified {
			c.Pools = append(c.Pools, Pool{Name: g.Pool, Kind: "vram", TotalGB: g.VRAMTotalGB, AvailableGB: g.VRAMFreeGB, Source: g.Vendor + " " + gpuSource(g)})
		}
	}

	c.Disk = probeDisk(opt.StoreDir, n)
	if c.Host.WSL {
		capWSLDisk(&c.Disk, n)
	}

	// The slow, independent probes run side by side.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, hasWSL := c.Pool("wsl")
		c.Ollama = probeOllama(ctx, ollamaURL(opt.OllamaURL), hasWSL, n)
	}()
	go func() {
		defer wg.Done()
		if opt.Network {
			c.Network = probeNetwork(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		c.Python = probePython(ctx, n)
	}()
	wg.Wait()

	// The bench runs last and alone, so nothing else competes for memory.
	if opt.Bench {
		size := 128 << 20
		if p, ok := c.Pool("host"); ok && p.AvailableGB < 1.5 {
			size = 32 << 20
		}
		b := memBench(size, runtime.NumCPU(), 600*time.Millisecond)
		c.Bench = &b
	}

	if err := ctx.Err(); err != nil {
		return c, err
	}
	c.Notes = n.list
	return c, nil
}

// gpuSource names the tool a GPU's figures came from.
func gpuSource(g GPU) string {
	switch g.Vendor {
	case "nvidia":
		return "nvidia-smi"
	case "amd":
		return "rocm-smi/sysfs"
	}
	return "probe"
}

// LiveAvailableGB re-measures how much memory one pool has free right now.
func LiveAvailableGB(ctx context.Context, pool string) (float64, error) {
	switch {
	case pool == "host":
		_, avail, _, _, err := hostMemory()
		if err != nil {
			return 0, err
		}
		return toGB(avail), nil
	case pool == "wsl":
		return liveWSL(ctx)
	case strings.HasPrefix(pool, "gpu"):
		for _, g := range probeGPUs(ctx, &notes{}, 0) {
			if g.Pool == pool {
				return g.VRAMFreeGB, nil
			}
		}
		return 0, fmt.Errorf("census: no GPU pool %q", pool)
	}
	return 0, fmt.Errorf("census: unknown pool %q", pool)
}

// run executes a command with a timeout and returns its standard output.
func run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out.Bytes(), fmt.Errorf("%s timed out after %s", name, timeout)
		}
		msg := strings.TrimSpace(errOut.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return out.Bytes(), fmt.Errorf("%s: %v: %s", name, err, msg)
		}
		return out.Bytes(), fmt.Errorf("%s: %v", name, err)
	}
	return out.Bytes(), nil
}

// toGB turns bytes into GiB, rounded to two decimals.
func toGB(b uint64) float64 {
	return round2(float64(b) / gib)
}

// round2 rounds to two decimals.
func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// probeDisk measures free space where models will live.
func probeDisk(storeDir string, n *notes) Disk {
	dir := storeDir
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = home
		} else {
			dir = "."
		}
	}
	dir, _ = filepath.Abs(dir)
	existing := nearestExisting(dir)
	free, total, err := diskUsage(existing)
	if err != nil {
		n.add("disk space at %s could not be read: %v", existing, err)
		return Disk{Path: dir}
	}
	if memoryBacked(existing) {
		n.add("%s is a RAM filesystem (tmpfs): models stored there use memory and vanish on reboot; set BRAINIAC_HOME to a real disk", existing)
	}
	return Disk{Path: dir, FreeGB: toGB(free), TotalGB: toGB(total)}
}

// capWSLDisk limits a WSL virtual disk's free space to the Windows drive
// behind it: the virtual disk can promise more than C: has, and a download
// that fills C: fails (or worse) while the virtual disk still looks roomy.
func capWSLDisk(d *Disk, n *notes) {
	free, _, err := diskUsage("/mnt/c")
	if err != nil {
		return
	}
	if gb := toGB(free); gb < d.FreeGB {
		n.add("WSL's virtual disk reports %.1f GB free but the Windows drive behind it (/mnt/c) has %.1f GB; using the smaller", d.FreeGB, gb)
		d.FreeGB = gb
	}
}

// nearestExisting walks up from dir to the first directory that exists.
func nearestExisting(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// probeNetwork checks the download sources in parallel.
func probeNetwork(ctx context.Context) Network {
	targets := []string{
		"https://huggingface.co/api/models?limit=1",
		"https://api.github.com",
		"https://pypi.org/simple/pip/",
	}
	ok := make([]bool, len(targets))
	client := &http.Client{Timeout: 4 * time.Second}
	var wg sync.WaitGroup
	for i, u := range targets {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", "brainiac-census/1")
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
			ok[i] = resp.StatusCode < 500
		}(i, u)
	}
	wg.Wait()
	return Network{HuggingFace: ok[0], GitHub: ok[1], PyPI: ok[2]}
}

// pythonCandidates are the interpreters tried, in order.
func pythonCandidates() [][]string {
	if runtime.GOOS == "windows" {
		// "python3" on Windows is usually the Store stub, which never runs anything.
		return [][]string{{"python"}, {"py", "-3"}}
	}
	return [][]string{{"python3"}, {"python"}}
}

// probePython finds an interpreter of at least 3.9 for workers.
func probePython(ctx context.Context, n *notes) *Python {
	const script = "import sys; print(sys.executable); print('%d.%d.%d' % tuple(sys.version_info[:3]))"
	for _, cand := range pythonCandidates() {
		args := append(append([]string{}, cand[1:]...), "-c", script)
		out, err := run(ctx, 5*time.Second, cand[0], args...)
		if err != nil {
			continue
		}
		path, version, ok := parsePythonProbe(string(out))
		if !ok || !versionAtLeast(version, 3, 9) {
			continue
		}
		p := &Python{Path: path, Version: version}
		if _, err := run(ctx, 5*time.Second, path, "-c", "import venv, ensurepip"); err == nil {
			p.Venv = true
		}
		return p
	}
	n.add("no Python 3.9+ found (OCR and speech workers need one)")
	return nil
}

// parsePythonProbe reads the two lines the probe script prints.
func parsePythonProbe(out string) (path, version string, ok bool) {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(out, "\r\n", "\n")), "\n")
	if len(lines) < 2 {
		return "", "", false
	}
	path = strings.TrimSpace(lines[len(lines)-2])
	version = strings.TrimSpace(lines[len(lines)-1])
	if path == "" || version == "" {
		return "", "", false
	}
	return path, version, true
}

// versionAtLeast compares a dotted version with major.minor.
func versionAtLeast(version string, major, minor int) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || (ma == major && mi >= minor)
}

// Summary is the census as a few lines a person can read.
func (c Census) Summary() string {
	var b strings.Builder
	line := func(label, format string, args ...any) {
		fmt.Fprintf(&b, "%-7s %s\n", label, fmt.Sprintf(format, args...))
	}

	host := c.Host.OS + "/" + c.Host.Arch
	if c.Host.WSL {
		host += " (WSL)"
	}
	if c.Host.Container {
		host += " (container)"
	}
	line("Host", "%s %s", c.Host.Hostname, host)

	flags := strings.Join(c.CPU.Flags, " ")
	if flags == "" {
		flags = "no notable flags"
	}
	line("CPU", "%s - %d cores / %d threads - %s", c.CPU.Model, c.CPU.PhysicalCores, c.CPU.LogicalCores, flags)

	for i, p := range c.Pools {
		label := ""
		if i == 0 {
			label = "Memory"
		}
		line(label, "%-5s %.1f GB total, %.1f GB available (%s, %s)", p.Name, p.TotalGB, p.AvailableGB, p.Kind, p.Source)
	}
	if len(c.Pools) == 0 {
		line("Memory", "not measured")
	}

	if len(c.GPUs) == 0 {
		line("GPU", "none usable")
	}
	for _, g := range c.GPUs {
		extra := ""
		if g.ComputeCap != "" {
			extra = ", compute " + g.ComputeCap
		}
		line("GPU", "%s %s - %.1f GB, %.1f GB free, pool %s, %s%s", g.Vendor, g.Name, g.VRAMTotalGB, g.VRAMFreeGB, g.Pool, strings.Join(g.Backends, "/"), extra)
	}

	line("Disk", "%.1f GB free of %.1f GB at %s", c.Disk.FreeGB, c.Disk.TotalGB, c.Disk.Path)

	if c.Bench != nil {
		line("Bench", "memory copy %.1f GB/s (%d threads, %.2f s)", c.Bench.MemCopyGBps, c.Bench.Threads, c.Bench.Seconds)
	}

	if c.Ollama == nil {
		line("Ollama", "not found")
	} else {
		o := c.Ollama
		line("Ollama", "%s v%s in pool %s - %d models, %.1f GB loaded now", o.URL, o.Version, o.Pool, len(o.Models), o.LoadedGB)
		for _, m := range o.Models {
			var bits []string
			if m.ParamsB > 0 {
				bits = append(bits, strconv.FormatFloat(m.ParamsB, 'f', -1, 64)+"B")
			}
			if m.Quant != "" {
				bits = append(bits, m.Quant)
			}
			if m.Vision {
				bits = append(bits, "vision")
			}
			if m.ContextLength > 0 {
				bits = append(bits, "ctx "+strconv.Itoa(m.ContextLength))
			}
			line("", "  %-28s %5.1f GB  %s", m.Name, m.SizeGB, strings.Join(bits, " "))
		}
	}

	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	line("Net", "huggingface %s, github %s, pypi %s", yes(c.Network.HuggingFace), yes(c.Network.GitHub), yes(c.Network.PyPI))

	if c.Python == nil {
		line("Python", "none")
	} else {
		venv := "venv ok"
		if !c.Python.Venv {
			venv = "no venv"
		}
		line("Python", "%s %s (%s)", c.Python.Path, c.Python.Version, venv)
	}

	notes := append([]string{}, c.Notes...)
	sort.Strings(notes)
	for i, note := range notes {
		label := ""
		if i == 0 {
			label = "Notes"
		}
		line(label, "- %s", note)
	}
	return b.String()
}

var (
	// errUnsupported is returned by probes that do not exist on this OS.
	errUnsupported = errors.New("not supported on " + runtime.GOOS)
	// errNoWSL is returned when no WSL distribution is running.
	errNoWSL = errors.New("no WSL distribution is running")
)
