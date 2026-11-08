// Package isolate limits what a process running a downloaded model can reach.
// A model file is input to a large native parser and carries a template the
// server executes, so the process gets none of Brainiac's secrets and, where
// the system allows, no network.
//
// Depends on nothing internal.
package isolate

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// allowed are the variables a model process may see: what the system needs to
// start a program, and the settings that choose a GPU or a thread count. A
// list of what to keep, because no list of secrets' names is ever complete.
var allowed = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`PATH HOME USER LOGNAME LANG LC_ALL LC_CTYPE TZ TMPDIR TMP TEMP
		LD_LIBRARY_PATH DYLD_LIBRARY_PATH
		SYSTEMROOT WINDIR SYSTEMDRIVE PROGRAMDATA PROGRAMFILES PROGRAMFILES(X86) LOCALAPPDATA APPDATA USERPROFILE
		COMPUTERNAME NUMBER_OF_PROCESSORS PROCESSOR_ARCHITECTURE PATHEXT COMSPEC
		CUDA_VISIBLE_DEVICES CUDA_DEVICE_ORDER NVIDIA_VISIBLE_DEVICES ROCR_VISIBLE_DEVICES GPU_DEVICE_ORDINAL
		HSA_OVERRIDE_GFX_VERSION VK_ICD_FILENAMES OMP_NUM_THREADS OPENCV_IO_MAX_IMAGE_PIXELS`) {
		allowed[k] = true
	}
}

// helperArg marks this program being run as the sandbox helper (see Wrap);
// probeArg is what the helper becomes when CanConfine tries it out.
const (
	helperArg = "__brainiac_sandbox"
	probeArg  = "__brainiac_sandbox_probe"
)

// Policy is what a sandboxed process may touch; everything else is refused.
type Policy struct {
	Read  []string `json:"read,omitempty"`  // read and execute, beneath each path
	Write []string `json:"write,omitempty"` // and create, change and delete
	// List are directories whose entries it may list, not read: a program
	// that finds its own plugins by listing its directory (ggml). The right
	// holds beneath each, so subdirectories are listed too. In a mount view
	// such a directory holds only what Read grants; without one the names
	// of everything beneath it are visible, never the contents.
	List []string `json:"list,omitempty"`
	// LibraryPath is the program's own $ORIGIN search path, expanded. A
	// mount view has no /proc/self/exe, from which the loader expands
	// $ORIGIN for the program, so the helper passes it as LD_LIBRARY_PATH
	// there, after whatever that already holds. LD_LIBRARY_PATH is searched
	// before DT_RUNPATH and the loader's cache, and for every object, the
	// system's libraries' own dependencies too. That is safe only because
	// these directories are inside the build and, in a view, hold nothing
	// but granted files; it is never set without a view.
	LibraryPath []string `json:"library_path,omitempty"`
	BlockTCP    bool     `json:"block_tcp,omitempty"`
	// BindTCP are the ports it may listen on when TCP is blocked. It may
	// connect to none.
	BindTCP []int `json:"bind_tcp,omitempty"`
	// Mount builds a view of the filesystem holding only Read and Write
	// (Linux, set by Wrap when the process gets its own namespaces).
	Mount bool `json:"mount,omitempty"`
}

// systemRead is what a program needs from the system to run, on any Linux.
// /proc is only the machine's own facts: /proc/<pid>/environ would hand over
// the secrets of every process this user runs, Brainiac's first of all.
// /proc/driver/nvidia is what NVIDIA's user-space driver reads about its GPUs.
var systemRead = []string{"/usr", "/lib", "/lib32", "/lib64", "/bin", "/sbin", "/opt", "/etc", "/sys",
	"/proc/cpuinfo", "/proc/meminfo", "/proc/stat", "/proc/filesystems", "/proc/version", "/proc/sys",
	"/proc/driver/nvidia", "/nix/store", "/run/opengl-driver"}

// systemWrite is the devices: null and random, and the GPU's nodes, which a
// server opens read-write.
var systemWrite = []string{"/dev"}

// logf reports what a policy grants beyond its usual shape (tests replace it).
var logf = log.Printf

var (
	rootsMu      sync.RWMutex
	installRoots []string
)

// SetInstallRoots names the directories Brainiac installs programs into
// (its llama.cpp builds, its Python environment). A program beneath one
// whose ELF Brainiac cannot read gets its directory, read-only, instead of
// the libraries it would name; anywhere else it gets only itself.
func SetInstallRoots(dirs ...string) {
	var roots []string
	for _, d := range dirs {
		if d != "" {
			roots = append(roots, realPath(d))
		}
	}
	rootsMu.Lock()
	installRoots = roots
	rootsMu.Unlock()
}

// InstallRoots are the directories SetInstallRoots named, by their real
// paths.
func InstallRoots() []string {
	rootsMu.RLock()
	defer rootsMu.RUnlock()
	return append([]string{}, installRoots...)
}

// realPath is p with its symlinks resolved, or p cleaned when it cannot be.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// directoryFallback says whether dir, the directory of a program whose ELF
// cannot be read, may be granted read-only in its place, and why not. Never
// a filesystem root, the home directory or the temporary directory, and
// only beneath an install root: the fallback grants a directory and all
// beneath it, which nothing else in a policy does.
func directoryFallback(dir string) (bool, string) {
	dir = realPath(dir)
	if filepath.Dir(dir) == dir {
		return false, "a filesystem root"
	}
	if home, err := os.UserHomeDir(); err == nil && dir == realPath(home) {
		return false, "the home directory"
	}
	if dir == realPath(os.TempDir()) {
		return false, "the temporary directory"
	}
	rootsMu.RLock()
	defer rootsMu.RUnlock()
	for _, r := range installRoots {
		if within(r, dir) {
			return true, ""
		}
	}
	return false, "not inside Brainiac's install store"
}

// ForProgram is the policy for a program that reads the given paths and
// writes the given ones, with what the system needs added: the program, and
// the shared libraries it and its plugins (libraries it opens itself at run
// time) load from its own build, read-only.
func ForProgram(program string, read, write []string, plugins ...string) Policy {
	p := Policy{Read: append([]string{}, systemRead...), Write: append(append([]string{}, systemWrite...), write...)}
	files, libPath := resolved(program, plugins)
	p.Read = append(p.Read, files...)
	p.Read = append(p.Read, pythonStdlib(program)...)
	p.Read = append(p.Read, read...)
	p.LibraryPath = libPath
	return p
}

// withLibraryPath is environ with dirs added at the end of LD_LIBRARY_PATH.
func withLibraryPath(environ, dirs []string) []string {
	if len(dirs) == 0 {
		return environ
	}
	add := strings.Join(dirs, ":")
	out := make([]string, 0, len(environ)+1)
	done := false
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "LD_LIBRARY_PATH="); ok && !done {
			done = true
			if v != "" {
				add = v + ":" + add
			}
			kv = "LD_LIBRARY_PATH=" + add
		}
		out = append(out, kv)
	}
	if !done {
		out = append(out, "LD_LIBRARY_PATH="+add)
	}
	return out
}

// Env is this process's environment reduced to the allowed variables, plus
// extra ("NAME=value") as given.
func Env(extra ...string) []string {
	return append(filter(os.Environ(), runtime.GOOS == "windows"), extra...)
}

func filter(environ []string, foldCase bool) []string {
	out := make([]string, 0, len(allowed))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if foldCase {
			name = strings.ToUpper(name) // Windows spells these several ways
		}
		if allowed[name] {
			out = append(out, kv)
		}
	}
	return out
}

// pythonStdlib is the standard library of a managed (non-system) Python
// interpreter, which the interpreter needs to start and which no ELF names:
// a build with its own prefix (uv's, pyenv's, a relocatable one) keeps it in
// lib/python3.N beside its bin directory. It is found structurally, the way
// the interpreter finds its own prefix: program's real path is <prefix>/bin/<name>,
// and <prefix>/lib/python3.N holds os.py (the landmark). Only that directory
// is granted, read-only, never the prefix; when the program's name says its
// version (python3.12) only that version's directory. A prefix the policy
// already reads (/usr, /usr/local, /opt...) adds nothing, and a directory
// whose real path leaves the prefix is not granted. A venv's python is a
// symlink to its base interpreter, so the base's library is the one found.
func pythonStdlib(program string) []string {
	real, err := filepath.EvalSymlinks(program)
	if err != nil {
		return nil
	}
	bin := filepath.Dir(real)
	if filepath.Base(bin) != "bin" {
		return nil
	}
	prefix := filepath.Dir(bin)
	if filepath.Dir(prefix) == prefix || systemPath(filepath.ToSlash(real)) {
		return nil
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(real)), ".exe")
	if !strings.HasPrefix(name, "python") {
		return nil
	}
	want := ""
	if v := strings.TrimPrefix(name, "python"); strings.HasPrefix(v, "3.") {
		want = "python" + v
	}
	entries, err := os.ReadDir(filepath.Join(prefix, "lib"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || !isPythonLibDir(n) || (want != "" && n != want) {
			continue
		}
		dir := filepath.Join(prefix, "lib", n)
		r, err := filepath.EvalSymlinks(dir)
		if err != nil || !within(prefix, r) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(r, "os.py")); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, r)
	}
	return out
}

// isPythonLibDir says n is "python3.N" with N digits.
func isPythonLibDir(n string) bool {
	v, ok := strings.CutPrefix(n, "python3.")
	if !ok || v == "" {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
