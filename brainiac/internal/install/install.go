// Package install gets what a plan chose onto this machine - Hugging Face
// files pinned to a commit and checked by sha256, GitHub release archives
// unpacked safely, Ollama pulls, the worker's Python packages, the pinned
// llama-server build - and writes the lock file that `brainiac serve` runs
// from. It is safe to run again: what is already in place is not fetched.
package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	if err := CheckPins(plan); err != nil {
		return nil, err
	}
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
			// CheckPins made sure of a 40-hex commit: the files are the
			// ones the catalog names, whatever "main" says today.
			commit := src.Revision
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
				if f.SHA256 != "" && f.SHA256 != want.SHA256 {
					return nil, fmt.Errorf("%s: the Hub says %s at %s has sha256 %s, the catalog pins %s; not downloading it",
						p.Model, want.Path, commit[:12], f.SHA256, want.SHA256)
				}
				f.SHA256 = want.SHA256 // the download is checked against the catalog's pin
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
						var changed *TemplateChanged
						if errors.As(err, &changed) && incoming != dest {
							// A file held for a person is not kept: it is gigabytes.
							_ = os.Remove(incoming)
						}
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
				sha, err := assetSHA(asset, src.SHA256)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", p.Model, err)
				}
				if err := hub.DownloadURL(ctx, gh, asset.URL, dest, asset.Size, sha, nil, hub.ProgressPrinter(opt.Log, src.Asset)); err != nil {
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
		f, err := WorkerLock(opt.WorkerDir, g, opt.Census.Python.Version, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return "", err
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

// assetSHA is the sha256 a GitHub asset must have: the catalog's pin.
// GitHub's digest, when it gives one, must agree. GitHub's digest alone is
// not enough: it says what the release holds today, and a release asset can
// be replaced; the catalog says what was chosen.
func assetSHA(a hub.Asset, pinned string) (string, error) {
	pinned = strings.ToLower(strings.TrimSpace(pinned))
	switch {
	case !catalog.IsSHA256(pinned):
		return "", fmt.Errorf("%s: the catalog pins no sha256 for it, so nothing vouches for its bytes; not downloading it (`brainiac catalog pin` fills it from GitHub's digest, or a signed catalog carries it)", a.Name)
	case a.SHA256 != "" && a.SHA256 != pinned:
		return "", fmt.Errorf("%s: GitHub's digest %s differs from the catalog's %s; not downloading it", a.Name, a.SHA256, pinned)
	}
	return pinned, nil
}

// CheckPins refuses a plan that names anything Brainiac would download
// without a pin: a Hugging Face source without its commit and every file's
// sha256, a GitHub asset or llama.cpp build without its sha256. It fails
// closed and names every gap at once, before anything is fetched.
func CheckPins(plan *selector.Plan) error {
	var gaps []string
	seen := map[string]bool{}
	for _, p := range plan.Picks {
		k := UnitKey(p.Model, p.Source)
		if seen[k] {
			continue
		}
		seen[k] = true
		if why := p.Source.Unpinned(); why != "" {
			gaps = append(gaps, fmt.Sprintf("%s (%s): %s", p.Model, p.Source.Label(), why))
		}
	}
	if l := plan.LlamaCpp; l != nil {
		rt := catalog.Runtime{LlamaCppTag: l.Tag, LlamaCppSHA256: l.SHA256}
		for _, name := range append([]string{l.Asset}, l.Extra...) {
			if why := rt.UnpinnedAsset(name); why != "" {
				gaps = append(gaps, why)
			}
		}
	}
	if len(gaps) == 0 {
		return nil
	}
	return fmt.Errorf("the catalog does not pin what this plan would download, so it is not installed (a name says where, only a hash says what):\n  %s\n"+
		"A signed catalog carries the pins; on a development machine `brainiac catalog pin <file>` fills them from the Hub's and GitHub's metadata",
		strings.Join(gaps, "\n  "))
}

// llamaManifest is where the sha256 of every file of an unpacked llama.cpp
// build is recorded, outside the build's own directory.
func llamaManifest(dir string) string { return dir + ".sha256.json" }

// treeSHAs is the sha256 of every regular file under dir, by relative path.
func treeSHAs(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return out, err
}

// llamaIntact reports whether an unpacked build is exactly what was
// recorded when it was installed: same files, same bytes. A build with no
// record, or one changed since, is installed again.
func llamaIntact(dir string) bool {
	data, err := os.ReadFile(llamaManifest(dir))
	if err != nil {
		return false
	}
	var want map[string]string
	if json.Unmarshal(data, &want) != nil || len(want) == 0 {
		return false
	}
	got, err := treeSHAs(dir)
	if err != nil || len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// ensureLlama downloads and unpacks the pinned llama-server build. A build
// already on disk is used only when its files match the record made at its
// install; otherwise it is downloaded and checked again.
func ensureLlama(ctx context.Context, l *selector.LlamaCpp, st *store.Store, hc *http.Client, log io.Writer) (string, error) {
	dir := st.LlamaCppDir(l.Tag, l.Variant)
	if bin := findExe(dir, "llama-server"); bin != "" {
		if llamaIntact(dir) {
			return bin, nil
		}
		fmt.Fprintf(log, "   %s does not match its install record; installing it again\n", dir)
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("llama.cpp: %w", err)
		}
	}
	fmt.Fprintf(log, "== llama.cpp %s (%s)\n", l.Tag, l.Asset)
	for _, name := range append([]string{l.Asset}, l.Extra...) {
		asset, err := hub.ReleaseAsset(ctx, hc, l.Repo, l.Tag, name)
		if err != nil {
			return "", fmt.Errorf("llama.cpp: %w", err)
		}
		sha, err := assetSHA(asset, l.SHA256[name])
		if err != nil {
			return "", fmt.Errorf("llama.cpp: %w", err)
		}
		archive := st.Path("bin", "llama.cpp", name)
		if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
			return "", err
		}
		if err := hub.DownloadURL(ctx, hc, asset.URL, archive, asset.Size, sha, nil, hub.ProgressPrinter(log, name)); err != nil {
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
	sums, err := treeSHAs(dir)
	if err != nil {
		return "", fmt.Errorf("llama.cpp: %w", err)
	}
	data, _ := json.MarshalIndent(sums, "", "  ")
	if err := os.WriteFile(llamaManifest(dir), data, 0o644); err != nil {
		return "", fmt.Errorf("llama.cpp: %w", err)
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

// WorkerLock is the hash-locked requirement file of a worker group for this
// machine: locks/<group>-cp3N-<target>.txt beside requirements-<group>.txt,
// made by scripts/lock_worker.py for CPython 3.N (from the interpreter's
// version, "3.12.4") on the target (win_amd64, linux_x86_64, linux_aarch64,
// macos_arm64). There is no unlocked fallback: with no lock for this
// Python and system, the worker is not installed (G-09, fail closed like
// the catalog's pins).
func WorkerLock(workerDir, group, pythonVersion, goos, goarch string) (string, error) {
	parts := strings.SplitN(strings.TrimSpace(pythonVersion), ".", 3)
	if len(parts) < 2 || parts[0] != "3" || parts[1] == "" {
		return "", fmt.Errorf("the worker's Python reports version %q; its packages are locked for CPython 3.11-3.14 only", pythonVersion)
	}
	targets := map[string]string{"windows/amd64": "win_amd64", "linux/amd64": "linux_x86_64", "linux/arm64": "linux_aarch64", "darwin/arm64": "macos_arm64"}
	target, ok := targets[goos+"/"+goarch]
	if !ok {
		return "", fmt.Errorf("the worker's packages are not locked for %s/%s; refusing an unlocked install", goos, goarch)
	}
	if _, err := os.Stat(filepath.Join(workerDir, "requirements-"+group+".txt")); err != nil {
		return "", fmt.Errorf("no requirement file for %s in %s", group, workerDir)
	}
	f := filepath.Join(workerDir, "locks", fmt.Sprintf("%s-cp3%s-%s.txt", group, parts[1], target))
	if _, err := os.Stat(f); err != nil {
		return "", fmt.Errorf("no hash-locked requirements for %s on CPython 3.%s %s (%s); refusing an unlocked install (brainiac/scripts/lock_worker.py makes them)", group, parts[1], target, f)
	}
	return f, nil
}
