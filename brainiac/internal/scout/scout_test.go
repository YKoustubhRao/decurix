package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/hub"
	"brainiac/internal/web"
)

func TestNamesAndClasses(t *testing.T) {
	for in, want := range map[string]string{
		"unsloth/Qwen3.9-4B-GGUF": "qwen3.9-4b", "mradermacher/Foo-7B-i1-GGUF": "foo-7b",
		"bartowski/Org_Model-3B-GGUF": "org-model-3b", "Qwen/Qwen3.5-2B": "qwen3.5-2b",
	} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%s) = %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{"apache-2.0": catalog.LicensePermissive, "MIT": catalog.LicensePermissive,
		"cc-by-nc-4.0": catalog.LicenseNonCommercial, "qwen-research": catalog.LicenseNonCommercial, "gemma": catalog.LicenseRestricted, "": catalog.LicenseRestricted} {
		if got := LicenseClass(in); got != want {
			t.Errorf("LicenseClass(%q) = %s", in, got)
		}
	}
	for repo, want := range map[string]string{"x/SuperCoder-7B-GGUF": "code", "x/Med-Reason-8B": "medicine", "x/Mistral-Medium-GGUF": "brain",
		"x/OmniSQL-9B": "sql", "x/Indic-Translate-2B": "translate", "x/whisper-large-v4": "asr"} {
		if got := guessRole(repo, "", "brain"); got != want {
			t.Errorf("guessRole(%s) = %s, want %s", repo, got, want)
		}
	}
}

func TestPickGGUFPrefersQ4AndQ8Projector(t *testing.T) {
	files := []hub.File{{Path: "M-F16.gguf", Size: 8e9}, {Path: "M-Q8_0.gguf", Size: 4e9}, {Path: "M-Q4_K_M.gguf", Size: 2.4e9},
		{Path: "mmproj-F16.gguf", Size: 6e8}, {Path: "mmproj-Q8_0.gguf", Size: 4e8}, {Path: "README.md", Size: 10}}
	f, gb, mm, mmgb := pickGGUF(files)
	if f != "M-Q4_K_M.gguf" || gb != 2.4 || mm != "mmproj-Q8_0.gguf" || mmgb != 0.4 {
		t.Fatalf("%s %v %s %v", f, gb, mm, mmgb)
	}
}

// A whole run against stand-ins for the Hub, GitHub and the llama.cpp source.
func TestRunFindsVerifiesAndSkipsWhatTheCatalogHas(t *testing.T) {
	created := time.Now().AddDate(0, 0, -10).Format(time.RFC3339)
	old := time.Now().AddDate(0, 0, -200).Format(time.RFC3339)
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models":
			fmt.Fprint(w, `[{"id":"unsloth/Qwen3.5-2B-GGUF","likes":900},
				{"id":"allenai/Nova-4B-GGUF","likes":500,"pipeline_tag":"text-generation","tags":["gguf"]},
				{"id":"mradermacher/Nova-4B-GGUF","likes":40},
				{"id":"oddlab/Weird-3B-GGUF","likes":300},
				{"id":"nclab/Closed-2B-GGUF","likes":200},
				{"id":"oldlab/Ancient-1B-GGUF","likes":100},
				{"id":"someone/Qwen3.8-27B-Uncensored-GGUF","likes":5000},
				{"id":"remixer/Qwen3.5-2B-Turbo-GGUF","likes":800}]`)
		case r.URL.Path == "/api/models/allenai/Nova-4B-GGUF":
			fmt.Fprintf(w, `{"id":"allenai/Nova-4B-GGUF","createdAt":%q,"downloads":2000,"cardData":{"license":"apache-2.0","model-index":[{"results":[{"dataset":{"name":"AIME26"},"metrics":[{"type":"acc","value":88.5}]}]}]},"gguf":{"architecture":"qwen35","total":4.2e9}}`, created)
		case r.URL.Path == "/api/models/oddlab/Weird-3B-GGUF":
			fmt.Fprintf(w, `{"id":"oddlab/Weird-3B-GGUF","createdAt":%q,"downloads":900,"cardData":{"license":"mit","model-index":[{"results":[{"dataset":{"name":"X"},"metrics":[{"type":"acc","value":70}]}]}]},"gguf":{"architecture":"brandnewarch","total":3e9}}`, created)
		case r.URL.Path == "/api/models/nclab/Closed-2B-GGUF":
			fmt.Fprintf(w, `{"id":"nclab/Closed-2B-GGUF","createdAt":%q,"downloads":900,"cardData":{"license":"cc-by-nc-4.0","model-index":[{"results":[{"dataset":{"name":"X"},"metrics":[{"type":"acc","value":70}]}]}]},"gguf":{"architecture":"qwen35","total":2e9}}`, created)
		case r.URL.Path == "/api/models/oldlab/Ancient-1B-GGUF":
			fmt.Fprintf(w, `{"id":"oldlab/Ancient-1B-GGUF","createdAt":%q,"cardData":{"license":"mit"},"gguf":{"architecture":"qwen35","total":1e9}}`, old)
		case r.URL.Path == "/api/models/remixer/Qwen3.5-2B-Turbo-GGUF":
			fmt.Fprintf(w, `{"id":"remixer/Qwen3.5-2B-Turbo-GGUF","createdAt":%q,"downloads":9000,"cardData":{"license":"apache-2.0","base_model":"Qwen/Qwen3.5-2B","model-index":[{"results":[{"dataset":{"name":"X"},"metrics":[{"type":"acc","value":70}]}]}]},"gguf":{"architecture":"qwen35","total":2e9}}`, created)
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
			fmt.Fprint(w, `[{"type":"file","path":"Nova-4B-Q4_K_M.gguf","size":2500000000},{"type":"file","path":"Nova-4B-Q8_0.gguf","size":4300000000}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hubSrv.Close()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases") {
			assets := strings.Repeat(`{},`, 12)
			fmt.Fprintf(w, `[{"tag_name":"b99999","published_at":%q,"assets":[%s{}]},{"tag_name":"b88888","published_at":%q,"assets":[%s{}]}]`,
				time.Now().Format(time.RFC3339), assets, time.Now().AddDate(0, 0, -20).Format(time.RFC3339), assets)
			return
		}
		if strings.Contains(r.URL.Path, "/b88888/") {
			fmt.Fprint(w, `{ LLM_ARCH_QWEN35, "qwen35" }, { LLM_ARCH_BRANDNEW, "brandnewarch" },`)
			return
		}
		fmt.Fprint(w, `{ LLM_ARCH_QWEN35, "qwen35" },`)
	}))
	defer gh.Close()
	cat, _ := catalog.Default()
	h := hub.New()
	h.Endpoint = hubSrv.URL
	w := web.New()
	w.AllowPrivate = true
	w.HTTP = nil
	rep, err := Run(context.Background(), Options{Hub: h, Web: w, Catalog: cat, BudgetGB: 2.8, GitHubAPI: gh.URL, RawBase: gh.URL})
	if err != nil {
		t.Fatal(err)
	}
	if rep.LlamaCpp.NewestSafe != "b88888" || len(rep.LlamaCpp.NewArchs) != 1 || rep.LlamaCpp.NewArchs[0] != "brandnewarch" {
		t.Fatalf("llama.cpp %+v", rep.LlamaCpp)
	}
	by := map[string]Candidate{}
	for _, c := range rep.Candidates {
		by[c.Base] = c
	}
	if _, ok := by["qwen3.5-2b"]; ok {
		t.Error("a model the catalog has came back as new")
	}
	nova, ok := by["nova-4b"]
	if !ok || nova.Repo != "allenai/Nova-4B-GGUF" || !nova.Usable || !nova.ArchOK || nova.File != "Nova-4B-Q4_K_M.gguf" {
		t.Fatalf("nova %+v", nova)
	}
	if nova.FitsHere == nil || *nova.FitsHere || len(nova.Claims) != 1 {
		t.Errorf("nova fits %v claims %v (2.5 GB + overhead on a 2.8 GB budget)", nova.FitsHere, nova.Claims)
	}
	if weird := by["weird-3b"]; weird.Usable || !strings.Contains(weird.ArchNote, "newer llama.cpp") {
		t.Errorf("weird %+v", weird)
	}
	if c := by["closed-2b"]; c.Usable || !strings.Contains(strings.Join(c.Problems, " "), "non-commercial") {
		t.Errorf("a non-commercial model was not flagged: %+v", c)
	}
	for _, gone := range []string{"qwen3.8-27b-uncensored", "qwen3.5-2b-turbo"} {
		if _, ok := by[gone]; ok {
			t.Errorf("%s (a remix of a catalogued model) was reported", gone)
		}
	}
	if _, ok := by["ancient-1b"]; ok {
		t.Error("a model older than the window was reported")
	}
	b, _ := json.Marshal(rep)
	if !strings.Contains(string(b), `"candidates"`) {
		t.Fatal("report does not serialise")
	}
}

func TestOnlyListedUploadersAreTrusted(t *testing.T) {
	// First parties only: a quantiser's or a mirror's copy is a person's choice.
	for repo, want := range map[string]bool{"Qwen/Qwen3.9-4B-GGUF": true, "google/gemma-5-4b-it-GGUF": true, "tencent/Hy-MT3-GGUF": true,
		"unsloth/Nova-GGUF": false, "ggml-org/Nova-GGUF": false, "bartowski/Nova-GGUF": false, "mradermacher/Nova-GGUF": false,
		"lmstudio-community/Nova-GGUF": false, "openai-community/gpt2": false,
		"someone/Qwen3.9-4B-GGUF": false, "qwen-official/Qwen3.9-4B-GGUF": false, "unsloth2/Nova-GGUF": false} {
		if got := TrustedUploader(repo); got != want {
			t.Errorf("TrustedUploader(%s) = %v", repo, got)
		}
	}
}

// A stranger's "-GGUF" repo must not beat a trusted copy of the same model:
// only one repo per model goes on to be checked.
func TestAStrangerNeverOutranksATrustedCopy(t *testing.T) {
	stranger := uploaderRank("someone/Qwen3.9-4B-GGUF")
	for _, repo := range []string{"Qwen/Qwen3.9-4B-GGUF", "ggml-org/Qwen3.9-4B-GGUF", "unsloth/Qwen3.9-4B-GGUF", "bartowski/Qwen3.9-4B-GGUF",
		"mradermacher/Qwen3.9-4B-GGUF", "lmstudio-community/Qwen3.9-4B-GGUF"} {
		if uploaderRank(repo) >= stranger {
			t.Errorf("%s ranks %d, a stranger %d", repo, uploaderRank(repo), stranger)
		}
	}
}

func TestARenamedDerivativeIsCaughtByItsBaseModelOrTags(t *testing.T) {
	for name, d := range map[string]hub.Details{
		"base model": {BaseModels: []string{"someone/Qwen3.8-27B-abliterated"}},
		"tag":        {Tags: []string{"gguf", "uncensored"}},
		"tag's base": {Tags: []string{"base_model:someone/Qwen3.8-27B-Heretic"}},
	} {
		if !derivedFromJunk(d) {
			t.Errorf("%s: not caught", name)
		}
	}
	for name, d := range map[string]hub.Details{
		"plain":            {BaseModels: []string{"Qwen/Qwen3.8-27B"}, Tags: []string{"gguf", "text-generation"}},
		"trained on data":  {Tags: []string{"dataset:toxic-comments-for-safety"}},
		"nothing declared": {},
	} {
		if derivedFromJunk(d) {
			t.Errorf("%s: dropped", name)
		}
	}
}

func TestLiveScout(t *testing.T) {
	if os.Getenv("BRAINIAC_LIVE") == "" {
		t.Skip("set BRAINIAC_LIVE=1 to scout the real Hub")
	}
	cat, _ := catalog.Default()
	rep, err := Run(context.Background(), Options{Catalog: cat, BudgetGB: 2.8, MaxVerify: 12, Log: os.Stdout})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("llama.cpp %+v", rep.LlamaCpp)
	for _, c := range rep.Candidates {
		fits := "?"
		if c.FitsHere != nil {
			fits = fmt.Sprint(*c.FitsHere)
		}
		t.Logf("%-9s usable=%-5v fits=%-5s %-48s %-8s %5.2f GB %-14s %v", c.Role, c.Usable, fits, c.Repo, c.LicenseClass, c.SizeGB, c.Arch, c.Problems)
	}
	for _, n := range rep.Notes {
		t.Log("note:", n)
	}
}

func TestSplitFilesAreSummed(t *testing.T) {
	files := []hub.File{{Path: "Q4_K_M/Big-Q4_K_M-00001-of-00003.gguf", Size: 10e6}, {Path: "Q4_K_M/Big-Q4_K_M-00002-of-00003.gguf", Size: 50e9},
		{Path: "Q4_K_M/Big-Q4_K_M-00003-of-00003.gguf", Size: 40e9}, {Path: "Big-Q8_0.gguf", Size: 1e9}}
	f, gb, _, _ := pickGGUF(files)
	if f != "Q4_K_M/Big-Q4_K_M-00001-of-00003.gguf" || gb != 90.01 {
		t.Fatalf("%s %v", f, gb)
	}
	if baseName("x/Tiel-Coder-35B-A3B-GGUF-MTP") != baseName("x/Tiel-Coder-35B-A3B-GGUF") {
		t.Fatal("an -MTP repo is a separate model")
	}
}
