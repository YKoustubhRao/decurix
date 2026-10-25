package selector

import (
	"strings"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/census"
)

const domainCatalog = `{
 "version": "test", "runtime": {"llamacpp_repo": "ggml-org/llama.cpp", "llamacpp_tag": "b1"},
 "roles": [
  {"id": "brain", "weight": 10, "min_quality": 20, "speed_floor": 3, "speed_unit": "tok/s"},
  {"id": "math", "kind": "domain", "weight": 3, "min_quality": 30, "speed_floor": 3, "speed_unit": "tok/s"},
  {"id": "law", "kind": "domain", "weight": 3, "min_quality": 30, "speed_floor": 3, "speed_unit": "tok/s"},
  {"id": "medicine", "kind": "domain", "weight": 3, "min_quality": 30, "speed_floor": 3, "speed_unit": "tok/s"}
 ],
 "models": [
  {"id": "brain-2b", "roles": {"brain": 34, "medicine": 36}, "params_b": 2, "license_class": "permissive",
   "sources": [{"kind": "ollama", "runtime": "ollama", "tag": "brain:2b", "size_gb": 1.2}]},
  {"id": "math-1.5b", "roles": {"math": 55}, "params_b": 1.5, "thinks": true, "license_class": "permissive",
   "sources": [{"kind": "ollama", "runtime": "ollama", "tag": "math:1.5b", "size_gb": 1.0}]},
  {"id": "law-7b-old", "roles": {"law": 36}, "params_b": 7, "license_class": "permissive",
   "sources": [{"kind": "ollama", "runtime": "ollama", "tag": "law:7b", "size_gb": 1.0}]},
  {"id": "med-27b", "roles": {"medicine": 70}, "params_b": 27, "license_class": "permissive",
   "sources": [{"kind": "ollama", "runtime": "ollama", "tag": "med:27b", "size_gb": 17}]}
 ]}`

func smallMachine() census.Census {
	return census.Census{
		Host:    census.Host{OS: "linux", Arch: "amd64"},
		CPU:     census.CPU{PhysicalCores: 4, LogicalCores: 8},
		Pools:   []census.Pool{{Name: "host", Kind: "ram", TotalGB: 8, AvailableGB: 4}},
		Disk:    census.Disk{FreeGB: 100},
		Bench:   &census.Bench{MemCopyGBps: 20},
		Ollama:  &census.Ollama{URL: "http://x", Pool: "host"},
		Network: online, Python: python,
	}
}

func TestSpecialistOnlyWhereItBeatsTheBrainAndFits(t *testing.T) {
	cat, err := catalog.Parse([]byte(domainCatalog))
	if err != nil {
		t.Fatal(err)
	}
	p := Select(smallMachine(), cat, emptyStore(t), Policy{HaveProgram: noPrograms})
	math, ok := p.Pick("math")
	if !ok || math.Model != "math-1.5b" || !strings.Contains(math.Why, "beats the brain's 34") || !strings.Contains(math.Why, "reasoning model") {
		t.Fatalf("math %+v\n%s", math, render(p))
	}
	by := map[string]string{}
	for _, m := range p.ByBrain {
		by[m.Role] = m.Why
	}
	if !strings.Contains(by["law"], "does not beat the brain") {
		t.Errorf("law: %q", by["law"])
	}
	if !strings.Contains(by["medicine"], "needs") {
		t.Errorf("medicine (27B on 3.2 GB): %q", by["medicine"])
	}
	if out := render(p); !strings.Contains(out, "law: the brain answers it") {
		t.Errorf("render:\n%s", out)
	}
	none := Select(smallMachine(), cat, emptyStore(t), Policy{HaveProgram: noPrograms, Domains: []string{"none"}})
	if _, ok := none.Pick("math"); ok || len(none.ByBrain) != 0 {
		t.Fatalf("--domains none still chose specialists")
	}
	only := Select(smallMachine(), cat, emptyStore(t), Policy{HaveProgram: noPrograms, Domains: []string{"law"}})
	if _, ok := only.Pick("math"); ok {
		t.Fatal("math chosen though only law was asked for")
	}
}
