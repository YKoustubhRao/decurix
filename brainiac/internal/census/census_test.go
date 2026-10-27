package census

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestParseMeminfo(t *testing.T) {
	text := "MemTotal:        5038080 kB\nMemFree:          136192 kB\nMemAvailable:    3479552 kB\nBuffers:            1234 kB\n"
	total, avail, ok := parseMeminfo(text)
	if !ok || total != 5038080*1024 || avail != 3479552*1024 {
		t.Fatalf("got %d %d %v", total, avail, ok)
	}

	old := "MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 250 kB\n"
	_, avail, ok = parseMeminfo(old)
	if !ok || avail != 400*1024 {
		t.Fatalf("old kernel: got %d %v", avail, ok)
	}

	if _, _, ok := parseMeminfo("nonsense"); ok {
		t.Fatal("parsed nonsense")
	}
}

func TestCgroupLimits(t *testing.T) {
	if _, ok := parseCgroupLimit("max\n"); ok {
		t.Fatal("v2 max is no limit")
	}
	if v, ok := parseCgroupLimit("4294967296\n"); !ok || v != 4<<30 {
		t.Fatalf("v2 limit: %d %v", v, ok)
	}

	stat := "anon 1000\nfile 5000\ninactive_file 2147483648\nactive_file 10\n"
	if v := memStatValue(stat, "inactive_file"); v != 2<<30 {
		t.Fatalf("inactive_file: %d", v)
	}

	// 16 GB machine, 4 GB cgroup with 3 GB used of which 1 GB is droppable cache.
	total, avail := applyCgroup(16<<30, 12<<30, 4<<30, 3<<30, 1<<30)
	if total != 4<<30 || avail != 2<<30 {
		t.Fatalf("capped: %d %d", total, avail)
	}
	// v1 "unlimited" is a huge number: no cap.
	total, avail = applyCgroup(16<<30, 12<<30, 9223372036854771712, 1<<30, 0)
	if total != 16<<30 || avail != 12<<30 {
		t.Fatalf("unlimited: %d %d", total, avail)
	}
	// Machine availability is the ceiling even inside a loose cgroup.
	_, avail = applyCgroup(16<<30, 1<<30, 8<<30, 1<<30, 0)
	if avail != 1<<30 {
		t.Fatalf("ceiling: %d", avail)
	}
}

const cpuinfoX86 = `processor	: 0
model name	: Intel(R) Core(TM) i5-10210U CPU @ 1.60GHz
physical id	: 0
core id		: 0
cpu cores	: 4

processor	: 1
model name	: Intel(R) Core(TM) i5-10210U CPU @ 1.60GHz
physical id	: 0
core id		: 0
cpu cores	: 4

processor	: 2
model name	: Intel(R) Core(TM) i5-10210U CPU @ 1.60GHz
physical id	: 0
core id		: 1
cpu cores	: 4

processor	: 3
model name	: Intel(R) Core(TM) i5-10210U CPU @ 1.60GHz
physical id	: 0
core id		: 1
cpu cores	: 4
`

const cpuinfoARM = `processor	: 0
BogoMIPS	: 2000.00
Features	: fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics fphp asimdhp cpuid asimdrdm lrcpc dcpop asimddp sve i8mm
CPU implementer	: 0x41
CPU part	: 0xd0c

processor	: 1
Features	: fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics fphp asimdhp cpuid asimdrdm lrcpc dcpop asimddp sve i8mm
CPU implementer	: 0x41
CPU part	: 0xd0c
`

func TestParseCPUInfo(t *testing.T) {
	x := parseCPUInfo(cpuinfoX86)
	if x.Model != "Intel(R) Core(TM) i5-10210U CPU @ 1.60GHz" || x.PhysicalCores != 2 {
		t.Fatalf("x86: %+v", x)
	}

	a := parseCPUInfo(cpuinfoARM)
	if !strings.Contains(a.Model, "0xd0c") {
		t.Fatalf("arm model: %q", a.Model)
	}
	flags := strings.Join(armFlags(a.Features), " ")
	for _, want := range []string{"neon", "dotprod", "sve", "i8mm"} {
		if !strings.Contains(flags, want) {
			t.Fatalf("arm flags %q lack %s", flags, want)
		}
	}

	// Only "cpu cores" known (no core ids): cores per package times packages.
	only := "processor : 0\nphysical id : 0\ncpu cores : 6\n\nprocessor : 1\nphysical id : 1\ncpu cores : 6\n"
	if n := parseCPUInfo(only).PhysicalCores; n != 12 {
		t.Fatalf("cpu cores fallback: %d", n)
	}
}

func TestProbeCPUOnThisMachine(t *testing.T) {
	c := probeCPU(&notes{})
	if c.LogicalCores != runtime.NumCPU() || c.PhysicalCores < 1 || c.PhysicalCores > c.LogicalCores || c.Model == "" {
		t.Fatalf("cpu: %+v", c)
	}
	if runtime.GOARCH == "amd64" {
		for _, f := range c.Flags {
			if f == "avx2" {
				return
			}
		}
		t.Logf("no avx2 on this CPU: %v", c.Flags)
	}
}

func TestParseVMStat(t *testing.T) {
	text := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               12345.
Pages active:                            500000.
Pages inactive:                          100000.
Pages speculative:                         5000.
`
	avail, ok := parseVMStat(text)
	if !ok || avail != (12345+100000+5000)*16384 {
		t.Fatalf("vm_stat: %d %v", avail, ok)
	}
}

func utf16le(s string, bom bool) []byte {
	var b []byte
	if bom {
		b = append(b, 0xFF, 0xFE)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

func TestParseWSLRunning(t *testing.T) {
	out := utf16le("Ubuntu\r\ndocker-desktop\r\nDecurix\r\n", false)
	got := parseWSLRunning(out)
	if strings.Join(got, ",") != "Ubuntu,docker-desktop,Decurix" {
		t.Fatalf("utf16: %q", got)
	}
	if d := pickWSLDistro([]string{"docker-desktop", "Decurix"}); d != "Decurix" {
		t.Fatalf("pick: %q", d)
	}
	if got := parseWSLRunning(utf16le("Ubuntu\r\n", true)); len(got) != 1 || got[0] != "Ubuntu" {
		t.Fatalf("bom: %q", got)
	}
	if got := parseWSLRunning([]byte("Ubuntu\n")); len(got) != 1 {
		t.Fatalf("utf8: %q", got)
	}
	if got := parseWSLRunning(utf16le("There are no running distributions.\r\n", false)); len(got) != 0 {
		t.Fatalf("none running: %q", got)
	}
	if !strings.Contains(decodeConsole(utf16le("MemTotal: 1 kB", false)), "MemTotal") {
		t.Fatal("decode")
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	text := "NVIDIA GeForce RTX 4090, 24564, 23001, 550.54.14, 8.9\nNVIDIA RTX PRO 6000, 97887, 97000, 580.1, 12.0\n"
	gpus := parseNvidiaSMI(text, 0)
	if len(gpus) != 2 || gpus[0].Name != "NVIDIA GeForce RTX 4090" || gpus[0].VRAMTotalGB != 23.99 || gpus[0].ComputeCap != "8.9" || gpus[1].VRAMTotalGB < 95 {
		t.Fatalf("nvidia: %+v", gpus)
	}
	spark := parseNvidiaSMI("NVIDIA GB10, [N/A], [N/A], 580.95.05, 12.1\n", 128<<30)
	if len(spark) != 1 || !spark[0].Unified || spark[0].VRAMTotalGB != 128 {
		t.Fatalf("gb10: %+v", spark)
	}
}

func TestParseAMD(t *testing.T) {
	mem := []byte(`{"card0": {"VRAM Total Memory (B)": "25753026560", "VRAM Total Used Memory (B)": "1073741824"}}`)
	names := []byte(`{"card0": {"Card series": "Radeon RX 7900 XTX", "Card model": "0x744c"}}`)
	gpus := parseRocmSMI(mem, names)
	if len(gpus) != 1 || gpus[0].Name != "Radeon RX 7900 XTX" || gpus[0].VRAMTotalGB != 23.98 || gpus[0].VRAMFreeGB != 22.98 {
		t.Fatalf("rocm: %+v", gpus)
	}

	amd := []byte(`[{"gpu": 0, "mem_usage": {"total_vram": {"value": 196592, "unit": "MB"}, "used_vram": {"value": 292, "unit": "MB"}, "free_vram": {"value": 196300, "unit": "MB"}}}]`)
	if g := parseAmdSMI(amd); len(g) != 1 || g[0].VRAMTotalGB != 191.98 {
		t.Fatalf("amd-smi: %+v", g)
	}
	wrapped := []byte(`{"gpu_data": [{"gpu": 1, "mem_usage": {"total_vram": {"value": 16, "unit": "GB"}, "used_vram": {"value": 1, "unit": "GB"}}}]}`)
	if g := parseAmdSMI(wrapped); len(g) != 1 || g[0].VRAMFreeGB != 15 {
		t.Fatalf("amd-smi wrapped: %+v", g)
	}

	apu := amdFromSysfs(512<<20, 100<<20, 64<<30, 2<<30)
	if !apu.Unified || apu.VRAMTotalGB != 64 || apu.VRAMFreeGB != 62 {
		t.Fatalf("apu: %+v", apu)
	}
	card := amdFromSysfs(16<<30, 1<<30, 32<<30, 0)
	if card.Unified || card.VRAMTotalGB != 16 || card.VRAMFreeGB != 15 {
		t.Fatalf("discrete: %+v", card)
	}
}

func TestOllamaURLAndPool(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:11434":            "http://127.0.0.1:11434",
		"http://localhost:11434/":  "http://localhost:11434",
		"192.168.1.5":              "http://192.168.1.5:11434",
		"https://ollama.lan:443":   "https://ollama.lan:443",
		"http://decurix-ollama:80": "http://decurix-ollama:80",
	}
	for in, want := range cases {
		if got := normalizeOllamaURL(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}

	if p := ollamaPool("linux", "http://127.0.0.1:11434", false, false); p != "host" {
		t.Errorf("linux loopback: %s", p)
	}
	if p := ollamaPool("linux", "http://192.168.1.5:11434", false, false); p != "remote" {
		t.Errorf("remote: %s", p)
	}
	if p := ollamaPool("windows", "http://127.0.0.1:11434", false, true); p != "wsl" {
		t.Errorf("windows wsl: %s", p)
	}
	if p := ollamaPool("windows", "http://localhost:11434", true, true); p != "host" {
		t.Errorf("windows native: %s", p)
	}
	if p := ollamaPool("windows", "http://localhost:11434", false, false); p != "host" {
		t.Errorf("windows no wsl: %s", p)
	}
}

func TestParseParamSize(t *testing.T) {
	cases := map[string]float64{"3.1B": 3.1, "596.05M": 0.6, "8.2B": 8.2, "1T": 1000, "": 0, "x": 0}
	for in, want := range cases {
		if got := parseParamSize(in); got != want {
			t.Errorf("parseParamSize(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestProbeOllamaReadsWithoutLoading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			w.Write([]byte(`{"version":"0.12.6"}`))
		case "/api/tags":
			w.Write([]byte(`{"models":[
				{"name":"qwen3-vl:2b-instruct","size":1889519783,"details":{"family":"qwen3vl","parameter_size":"2.1B","quantization_level":"Q4_K_M"}},
				{"name":"nomic-embed-text:latest","size":274302450,"details":{"family":"nomic-bert","parameter_size":"137M","quantization_level":"F16"}}]}`))
		case "/api/show":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["model"] == "qwen3-vl:2b-instruct" {
				w.Write([]byte(`{"capabilities":["completion","vision"],"model_info":{"qwen3vl.context_length":262144}}`))
				return
			}
			w.Write([]byte(`{"capabilities":["embedding"],"model_info":{"nomic-bert.context_length":2048}}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest","size":536870912}]}`))
		default:
			t.Errorf("probe sent %s %s: it must never chat, generate or load", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	n := &notes{}
	o := probeOllama(context.Background(), srv.URL, false, n)
	if o == nil || o.Version != "0.12.6" || len(o.Models) != 2 || o.Pool != "host" {
		t.Fatalf("ollama: %+v notes %v", o, n.list)
	}
	vl := o.Models[0]
	if !vl.Vision || vl.ContextLength != 262144 || vl.ParamsB != 2.1 || vl.Quant != "Q4_K_M" || vl.SizeGB != 1.76 {
		t.Fatalf("vl: %+v", vl)
	}
	if o.Models[1].Vision || o.Models[1].ContextLength != 2048 {
		t.Fatalf("embed: %+v", o.Models[1])
	}
	if o.LoadedGB != 0.5 {
		t.Fatalf("loaded: %v", o.LoadedGB)
	}

	if missing := probeOllama(context.Background(), "http://127.0.0.1:1", false, n); missing != nil {
		t.Fatal("found an Ollama on port 1")
	}
}

func TestMemBench(t *testing.T) {
	b := memBench(8<<20, 2, 100*time.Millisecond)
	if b.MemCopyGBps <= 0 || b.Threads != 2 || b.Seconds <= 0 {
		t.Fatalf("bench: %+v", b)
	}
}

func TestPythonProbeParsing(t *testing.T) {
	path, version, ok := parsePythonProbe("C:\\Python311\\python.exe\r\n3.11.3\r\n")
	if !ok || path != `C:\Python311\python.exe` || version != "3.11.3" {
		t.Fatalf("%q %q %v", path, version, ok)
	}
	if !versionAtLeast("3.14.4", 3, 9) || versionAtLeast("3.8.10", 3, 9) || !versionAtLeast("4.0", 3, 9) {
		t.Fatal("versionAtLeast")
	}
}

func TestNearestExistingAndSummary(t *testing.T) {
	dir := t.TempDir()
	if got := nearestExisting(dir + "/a/b/c"); got != dir {
		t.Fatalf("nearest: %q", got)
	}

	c := Census{
		Host:   Host{OS: "linux", Arch: "amd64", Hostname: "box", WSL: true},
		CPU:    CPU{Model: "Test CPU", PhysicalCores: 4, LogicalCores: 8, Flags: []string{"avx2"}},
		Pools:  []Pool{{Name: "host", Kind: "ram", TotalGB: 4.8, AvailableGB: 3.3, Source: "/proc/meminfo"}},
		Ollama: &Ollama{URL: "http://127.0.0.1:11434", Version: "0.12.6", Pool: "host", Models: []OllamaModel{{Name: "qwen3:0.6b", SizeGB: 0.49, ParamsB: 0.6}}},
		Notes:  []string{"Intel GPU present; not used"},
	}
	s := c.Summary()
	for _, want := range []string{"Test CPU", "4 cores / 8 threads", "3.3 GB available", "qwen3:0.6b", "(WSL)", "Intel GPU"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary lacks %q:\n%s", want, s)
		}
	}
}

func TestLiveAvailableHost(t *testing.T) {
	start := time.Now()
	gb, err := LiveAvailableGB(context.Background(), "host")
	if err != nil || gb <= 0 {
		t.Skipf("host memory not measurable here: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("host re-measure took %s", time.Since(start))
	}
	if _, err := LiveAvailableGB(context.Background(), "nowhere"); err == nil {
		t.Fatal("unknown pool accepted")
	}
}

// TestTakeLive measures this machine for real. Opt-in: BRAINIAC_LIVE=1.
func TestTakeLive(t *testing.T) {
	if os.Getenv("BRAINIAC_LIVE") != "1" {
		t.Skip("set BRAINIAC_LIVE=1 to measure this machine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	c, err := Take(ctx, Options{Bench: true, Network: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("census took %s\n%s", time.Since(start).Round(time.Millisecond), c.Summary())
	if len(c.Pools) == 0 {
		t.Fatal("no memory pool measured")
	}
}
