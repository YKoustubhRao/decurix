package ollama

import "testing"

func TestParseMeminfo(t *testing.T) {
	total, avail, err := parseMeminfo("MemTotal:       16384000 kB\nMemFree:  100 kB\nMemAvailable:    8192000 kB\n")
	if err != nil || total != 16384000*1024 || avail != 8192000*1024 {
		t.Fatalf("total=%d avail=%d err=%v", total, avail, err)
	}

	if _, _, err := parseMeminfo("MemTotal: 1 kB\n"); err == nil {
		t.Error("meminfo without MemAvailable parsed")
	}
}

func TestParseCPULine(t *testing.T) {
	idle, total, err := parseCPULine("cpu  100 10 50 800 40 0 0 0 0 0")
	if err != nil || idle != 840 || total != 1000 {
		t.Fatalf("idle=%d total=%d err=%v", idle, total, err)
	}
}

// The probe runs against this machine's real /proc.
func TestProbeResourcesOnThisMachine(t *testing.T) {
	r, err := ProbeResources(t.Context(), "http://127.0.0.1:1", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.RAMTotalGB <= 0 || r.RAMFreeGB <= 0 || r.RAMFreeGB > r.RAMTotalGB || r.Cores <= 0 || r.CPUBusy < 0 || r.CPUBusy > 1 {
		t.Errorf("implausible resources: %+v", r)
	}
}
