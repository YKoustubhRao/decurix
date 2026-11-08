package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brainiac/internal/scout"
	"brainiac/internal/store"
)

// A quantiser's copy is no longer adopted on its own (O-3.4), but a person
// can still adopt it, and what they adopt is pinned to the commit and the
// sha256 they looked at, so it can be installed.
func TestAPersonCanAdoptAModelTheUpdaterWouldNot(t *testing.T) {
	const repo = "unsloth/Nova-2B-GGUF"
	if scout.TrustedUploader(repo) {
		t.Fatalf("%s is trusted for automatic adoption", repo)
	}
	commit, sum := strings.Repeat("c", 40), strings.Repeat("d", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/api/models/"+repo+"/revision/main":
			fmt.Fprintf(w, `{"sha":%q}`, commit)
		case p == "/api/models/"+repo+"/tree/"+commit:
			fmt.Fprintf(w, `[{"type":"file","path":"Nova-2B-Q4_K_M.gguf","size":1300000000,"lfs":{"oid":%q,"size":1300000000}},
				{"type":"file","path":"README.md","size":10,"oid":"0123"}]`, sum)
		case p == "/api/models/"+repo:
			fmt.Fprint(w, `{"id":"`+repo+`","cardData":{"license":"apache-2.0"},"gguf":{"total":2000000000,"architecture":"nova"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	t.Setenv("BRAINIAC_APPLIANCE", "")
	home := t.TempDir()
	if err := runAdopt([]string{repo, "--roles", "brain=40", "--file", "Nova-2B-Q4_K_M.gguf", "--mmproj", "none", "--home", home}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := loadCatalog(st)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := cat.Model("nova-2b")
	if !ok {
		t.Fatal("the adopted model is not in the catalog")
	}
	s := m.Sources[0]
	if s.Repo != repo || s.Revision != commit || s.Files[0].SHA256 != sum || s.Unpinned() != "" {
		t.Fatalf("adopted unpinned: %+v (%s)", s, s.Unpinned())
	}
}

// On an appliance the catalog changes only by a signed release.
func TestAnApplianceRefusesALocalAdoption(t *testing.T) {
	t.Setenv("BRAINIAC_APPLIANCE", "1")
	t.Setenv("HF_ENDPOINT", "http://127.0.0.1:1")
	err := runAdopt([]string{"Qwen/Qwen3.9-4B-GGUF", "--roles", "brain=40", "--home", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "signed release") {
		t.Fatalf("%v", err)
	}
	if err := runPin([]string{"b99999", "--home", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "signed release") {
		t.Fatalf("scout pin: %v", err)
	}
}
