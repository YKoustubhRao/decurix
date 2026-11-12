// privacy_canary_test.go sends a unique string through the engine, typed
// into a request and read off a picture, then searches every file the run
// left: what holds the string must be owner-only (0600 inside 0700 folders)
// and inside the store, and no trace may hold it (traces promise no content).
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/store"
)

// canaryOllama is a fake Ollama: the vision model "reads" the canary off any
// picture, and the brain repeats it.
type canaryOllama struct{ canary string }

func (f canaryOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/ps":
		fmt.Fprint(w, `{"models":[]}`)
	case "/api/chat":
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		text := "The answer mentions " + f.canary + "."
		if strings.HasPrefix(body.Model, "vlm") {
			text = "A login screen. The password field shows " + f.canary + "."
		}
		out, _ := json.Marshal(map[string]any{"message": map[string]string{"content": text}, "done": false})
		fmt.Fprintln(w, string(out))
		fmt.Fprintln(w, `{"message":{"content":""},"done":true,"prompt_eval_count":20,"eval_count":5}`)
	default:
		http.NotFound(w, r)
	}
}

func TestPrivacyCanaryLandsOnlyInOwnerOnlyStoreFiles(t *testing.T) {
	canary := "CANARY-q7Zx-0x5eed-" + fmt.Sprint(os.Getpid())
	base := t.TempDir()
	// Whatever the run writes to a temp folder or a home lands under base.
	for _, k := range []string{"TMPDIR", "TMP", "TEMP", "HOME", "USERPROFILE"} {
		d := filepath.Join(base, "env-"+strings.ToLower(k))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(k, d)
	}
	t.Setenv("BRAINIAC_HOME", "")

	srv := httptest.NewServer(canaryOllama{canary})
	defer srv.Close()
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = config.ModeLocal, srv.URL
	root := filepath.Join(base, "home")
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Options{Config: cfg, Store: st, Lock: lockWith(
		ollamaPick(catalog.RoleBrain, "tiny", "tiny:1b"), ollamaPick(catalog.RoleVision, "vlm", "vlm:2b"))})
	if err != nil {
		t.Fatal(err)
	}
	e.res.live = nil
	defer e.Close()

	ask := Request{Model: "brainiac", Messages: []InMessage{{Role: "user", Parts: []Part{
		{Kind: "text", Text: "My password is " + canary + "; what does this screen say?"},
		{Kind: "image", Data: pngBytes, Name: "screen.png"}}}}}
	for i := 0; i < 2; i++ { // the second read comes from the cache
		res, err := e.Chat(context.Background(), ask)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Text, canary) {
			t.Fatalf("the fake brain did not answer with the canary: %q", res.Text)
		}
		if i == 1 && !cachedDescribe(res) {
			t.Fatalf("the second picture was not read from the cache: %+v", res.Trace.Atoms)
		}
	}

	traces, holders := 0, 0
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		inStore := rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		isTrace := inStore && strings.HasPrefix(rel, "traces"+string(filepath.Separator))
		if isTrace {
			traces++
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.Contains(data, []byte(canary)) {
			return nil
		}
		holders++
		switch {
		case !inStore:
			t.Errorf("the canary was written outside the store: %s", p)
		case isTrace:
			t.Errorf("a trace holds the canary: %s", rel)
		default:
			ownerOnly(t, root, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if traces == 0 {
		t.Fatal("no trace file was written, so the trace check proved nothing")
	}
	if holders == 0 {
		t.Fatal("no file held the canary, so the mode check proved nothing (the picture's reading should be cached)")
	}
}

// cachedDescribe reports whether a response's picture description came from the cache.
func cachedDescribe(res *Response) bool {
	for _, a := range res.Trace.Atoms {
		if a.Op == "describe" && a.Cached {
			return true
		}
	}
	return false
}

// ownerOnly fails unless p is 0600 and every folder from it up to root is 0700.
// Windows ignores Unix modes, so there it only checks the file is in the store.
func ownerOnly(t *testing.T, root, p string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("%s holds the canary with mode %v, want 0600", p, modeOf(st))
	}
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		if st, err := os.Stat(d); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("folder %s above a canary file has mode %v, want 0700", d, modeOf(st))
		}
		if d == root || d == filepath.Dir(d) {
			return
		}
	}
}

func modeOf(st os.FileInfo) any {
	if st == nil {
		return "missing"
	}
	return st.Mode().Perm()
}
