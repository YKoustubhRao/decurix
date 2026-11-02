// Package install gets what a plan chose onto this machine - Hugging Face
// files pinned to a commit and checked by sha256, GitHub release archives
// unpacked safely, Ollama pulls, the worker's Python packages, the pinned
// llama-server build - and writes the lock file that `brainiac serve` runs
// from. It is safe to run again: what is already in place is not fetched.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"brainiac/internal/backend"
	"brainiac/internal/catalog"
	"brainiac/internal/census"
	"brainiac/internal/hub"
	"brainiac/internal/selector"
	"brainiac/internal/store"
	"brainiac/internal/worker"
)

// LockSchema is the version of the Lock JSON shape.
const LockSchema = 1

// Lock is what is installed for a plan, and where.
type Lock struct {
	Schema int             `json:"schema"`
	MadeAt time.Time       `json:"made_at"`
	Plan   *selector.Plan  `json:"plan"`
	Units  map[string]Unit `json:"units"` // by UnitKey(pick)
	// LlamaServer is the llama-server executable, when any pick needs it.
	LlamaServer string `json:"llama_server,omitempty"`
	// Python is the worker venv's interpreter, when any pick needs it.
	Python string `json:"python,omitempty"`
	// WorkerScript is brainiac_worker.py.
	WorkerScript string `json:"worker_script,omitempty"`
}

// Unit is one installed model source.
type Unit struct {
	Model  string            `json:"model"`
	Source catalog.Source    `json:"source"`
	Files  map[string]string `json:"files,omitempty"` // role -> absolute path
	Commit string            `json:"commit,omitempty"`
	// TemplateSHA is the chat template in the model's GGUF file; the file is
	// not served if it stops matching. See checkTemplate.
	TemplateSHA string `json:"template_sha,omitempty"`
}

// UnitKey names one model source.
func UnitKey(model string, src catalog.Source) string { return model + "|" + src.Label() }

// Options are what installing needs besides the plan.
type Options struct {
	Store *store.Store
	// Census gives the base Python for the venv and the Ollama URL.
	Census census.Census
	// WorkerDir is the directory holding brainiac_worker.py and requirements-*.txt.
	WorkerDir string
	Log       io.Writer
	// LockName is the lock file written (default lock.json). The updater
	// installs into lock.next.json and promotes it only after checking it.
	LockName string
	// Latest moves models already installed to their repo's newest commit.
	// Without it a model stays at the commit it was installed from: what runs
	// here does not change because someone pushed to a repo.
	Latest bool
}

// installedCommit is the commit the current lock has for a unit, or "".
func installedCommit(st *store.Store, key string) string {
	var l Lock
	if st.ReadJSON("lock.json", &l) != nil {
		return ""
	}
	return l.Units[key].Commit
}

// Run installs everything in the plan and returns the lock (also written to
// lock.json). A failure stops at the first part that could not be installed,
// naming it; what was installed before it stays for the next run.
func Run(ctx context.Context, plan *selector.Plan, opt Options) (*Lock, error) {
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	st := opt.Store
	lock := &Lock{Schema: LockSchema, MadeAt: time.Now().UTC(), Plan: plan, Units: map[string]Unit{}}
	hf := hub.New()
	gh := &http.Client{}

	// One unit per model source, in plan order.
	var picks []selector.Pick
	seen := map[string]bool{}
	for _, p := range plan.Picks {
		k := UnitKey(p.Model, p.Source)
		if !seen[k] {
			seen[k] = true
			picks = append(picks, p)
		}
	}
	for _, p := range picks {
		u := Unit{Model: p.Model, Source: p.Source}
		src := p.Source
		fmt.Fprintf(opt.Log, "== %s (%s)\n", p.Model, src.Label())
		switch src.Kind {
		case catalog.SourceHF:
			rev := src.Revision
			if rev == "" && !opt.Latest {
				rev = installedCommit(st, UnitKey(p.Model, src))
			}
			if rev == "" {
				rev = "main"
			}
			commit, err := hf.Revision(ctx, src.Repo, rev)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p.Model, err)
			}
			files, err := hf.Files(ctx, src.Repo, commit)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p.Model, err)
			}
			byPath := map[string]hub.File{}
			for _, f := range files {
				byPath[f.Path] = f
			}
			u.Commit = commit
			u.Files = map[string]string{}
			for _, want := range src.Files {
				f, ok := byPath[want.Path]
				if !ok {
					return nil, fmt.Errorf("%s: %s has no file %s at %s", p.Model, src.Repo, want.Path, commit[:12])
				}
				dest := st.HFFile(src.Repo, want.Path)
				if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
					return nil, err
				}
				// A file already in place may be running: a different one
				// arrives beside it, and replaces it only after its template
				// has been checked.
				incoming := dest
				if had := installedCommit(st, UnitKey(p.Model, src)); had != "" && had != commit {
					incoming = dest + ".incoming"
				}
				if err := hf.Download(ctx, src.Repo, commit, f, incoming, hub.ProgressPrinter(opt.Log, want.Path)); err != nil {
					return nil, fmt.Errorf("%s: %w", p.Model, err)
				}
				u.Files[want.Role] = dest
				if want.Role == "model" && src.Runtime == catalog.RuntimeLlamaCpp {
					if u.TemplateSHA, err = checkTemplate(st, p.Model, src.Repo, commit, incoming, time.Now()); err != nil {
						return nil, err
					}
					if u.TemplateSHA != "" {
						fmt.Fprintf(opt.Log, "   chat template %s\n", short(u.TemplateSHA))
					}
				}
				if incoming != dest {
					if err := os.Rename(incoming, dest); err != nil {
						return nil, fmt.Errorf("%s: %w", p.Model, err)
					}
				}
			}
		case catalog.SourceGitHub:
			asset, err := hub.ReleaseAsset(ctx, gh, src.Repo, src.Tag, src.Asset)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p.Model, err)
			}
			dest := st.GitHubAsset(src.Repo, src.Tag, src.Asset)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
			}
			files := st.SourceFiles(src)
			if !st.HasFiles(src) {
				if err := hub.DownloadURL(ctx, gh, asset.URL, dest, asset.Size, asset.SHA256, nil, hub.ProgressPrinter(opt.Log, src.Asset)); err != nil {
					return nil, fmt.Errorf("%s: %w", p.Model, err)
				}
				if src.Unpack {
					fmt.Fprintf(opt.Log, "   unpacking %s\n", src.Asset)
					if err := hub.Unpack(dest, st.GitHubUnpacked(src.Repo, src.Tag, src.Asset)); err != nil {
						return nil, fmt.Errorf("%s: %w", p.Model, err)
					}
					_ = os.Remove(dest) // the archive is no longer needed
				}
			}
			for role, path := range files {
				if _, err := os.Stat(path); err != nil {
					return nil, fmt.Errorf("%s: %s is missing %s after unpacking", p.Model, src.Asset, role)
				}
			}
			u.Files = files
		case catalog.SourceOllama:
			if p.Installed {
				break
			}
			if opt.Census.Ollama == nil {
				return nil, fmt.Errorf("%s: no Ollama to pull into", p.Model)
			}
			o := backend.NewOllama(opt.Census.Ollama.URL)
			last := time.Time{}
			err := o.Pull(ctx, src.Tag, func(status string, done, total int64) {
				if time.Since(last) > time.Second && total > 0 {
					last = time.Now()
					fmt.Fprintf(opt.Log, "   %s %.0f%%\n", status, 100*float64(done)/float64(total))
				}
			})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p.Model, err)
			}
		case catalog.SourcePip, catalog.SourceSystem:
			// The venv is set up once below, for every group together.
		}
		lock.Units[UnitKey(p.Model, src)] = u
	}

	if len(plan.Pip) > 0 {
		python, err := ensureVenv(ctx, plan.Pip, opt)
		if err != nil {
			return nil, err
		}
		lock.Python = python
	}
	if needsWorker(plan) {
		script := filepath.Join(opt.WorkerDir, "brainiac_worker.py")
		if _, err := os.Stat(script); err != nil {
			return nil, fmt.Errorf("the worker script is not at %s", script)
		}
		lock.WorkerScript, _ = filepath.Abs(script)
		if lock.Python == "" && opt.Census.Python != nil {
			lock.Python = opt.Census.Python.Path // tesseract alone needs no packages
		}
	}
	if plan.LlamaCpp != nil {
		bin, err := ensureLlama(ctx, plan.LlamaCpp, st, gh, opt.Log)
		if err != nil {
			return nil, err
		}
		lock.LlamaServer = bin
	}
	name := opt.LockName
	if name == "" {
		name = "lock.json"
	}
	if err := st.WriteJSON(name, lock); err != nil {
		return nil, err
	}
	return lock, nil
}

func needsWorker(plan *selector.Plan) bool {
	for _, p := range plan.Picks {
		if p.Source.Runtime == catalog.RuntimeWorker {
			return true
		}
	}
	return false
}

// ensureVenv installs the requirement groups into the store's venv.
func ensureVenv(ctx context.Context, groups []string, opt Options) (string, error) {
	if opt.Census.Python == nil || !opt.Census.Python.Venv {
		return "", errors.New("the worker needs Python 3.9+ with venv, and none was found")
	}
	var reqs []string
	for _, g := range groups {
		f := filepath.Join(opt.WorkerDir, "requirements-"+g+".txt")
		if _, err := os.Stat(f); err != nil {
			return "", fmt.Errorf("no requirement file for %s at %s", g, f)
		}
		reqs = append(reqs, f)
	}
	sort.Strings(reqs)
	fmt.Fprintf(opt.Log, "== worker Python packages (%s)\n", strings.Join(groups, ", "))
	logf := func(f string, a ...any) { fmt.Fprintf(opt.Log, "   "+f+"\n", a...) }
	python, err := worker.EnsureVenv(ctx, opt.Census.Python.Path, opt.Store.Venv(), reqs, logf)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		_ = os.WriteFile(filepath.Join(opt.Store.Venv(), "brainiac-"+g+".ok"), []byte("ok\n"), 0o644)
	}
	return python, nil
}

// ensureLlama downloads and unpacks the pinned llama-server build.
func ensureLlama(ctx context.Context, l *selector.LlamaCpp, st *store.Store, hc *http.Client, log io.Writer) (string, error) {
	dir := st.LlamaCppDir(l.Tag, l.Variant)
	if bin := findExe(dir, "llama-server"); bin != "" {
		return bin, nil
	}
	fmt.Fprintf(log, "== llama.cpp %s (%s)\n", l.Tag, l.Asset)
	for _, name := range append([]string{l.Asset}, l.Extra...) {
		asset, err := hub.ReleaseAsset(ctx, hc, l.Repo, l.Tag, name)
		if err != nil {
			return "", fmt.Errorf("llama.cpp: %w", err)
		}
		archive := st.Path("bin", "llama.cpp", name)
		if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
			return "", err
		}
		if err := hub.DownloadURL(ctx, hc, asset.URL, archive, asset.Size, asset.SHA256, nil, hub.ProgressPrinter(log, name)); err != nil {
			return "", fmt.Errorf("llama.cpp: %w", err)
		}
		target := dir
		if name != l.Asset {
			target = dir + "-extra-" + store.ArchiveStem(name)
		}
		if err := hub.Unpack(archive, target); err != nil {
			return "", fmt.Errorf("llama.cpp: %w", err)
		}
		_ = os.Remove(archive)
	}
	// CUDA runtime DLLs go beside the server on Windows.
	for _, name := range l.Extra {
		extra := dir + "-extra-" + store.ArchiveStem(name)
		bin := findExe(dir, "llama-server")
		if bin != "" {
			_ = copyTree(extra, filepath.Dir(bin))
		}
	}
	bin := findExe(dir, "llama-server")
	if bin == "" {
		return "", fmt.Errorf("llama.cpp: no llama-server in %s", l.Asset)
	}
	return bin, nil
}

// findExe finds an executable by name under dir.
func findExe(dir, name string) string {
	want := name
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	var found string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && d.Name() == want {
			found = p
		}
		return nil
	})
	return found
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, d.Name()), data, 0o755)
	})
}

// ReadLock reads the store's lock file.
func ReadLock(st *store.Store) (*Lock, error) {
	var l Lock
	if err := st.ReadJSON("lock.json", &l); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("nothing is installed yet: run `brainiac up` (or `brainiac pull`) first")
		}
		return nil, err
	}
	if l.Plan == nil {
		return nil, errors.New("lock.json has no plan: run `brainiac pull` again")
	}
	return &l, nil
}
