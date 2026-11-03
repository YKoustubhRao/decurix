// Package isolate limits what a process running a downloaded model can reach.
// A model file is input to a large native parser and carries a template the
// server executes, so the process gets none of Brainiac's secrets and, where
// the system allows, no network.
//
// Depends on nothing internal.
package isolate

import (
	"os"
	"runtime"
	"strings"
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
	Read     []string `json:"read,omitempty"`  // read and execute, beneath each path
	Write    []string `json:"write,omitempty"` // and create, change and delete
	BlockTCP bool     `json:"block_tcp,omitempty"`
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
var systemRead = []string{"/usr", "/lib", "/lib32", "/lib64", "/bin", "/sbin", "/opt", "/etc", "/sys",
	"/proc/cpuinfo", "/proc/meminfo", "/proc/stat", "/proc/filesystems", "/proc/version", "/proc/sys",
	"/nix/store", "/run/opengl-driver"}

// systemWrite is the devices: null and random, and the GPU's nodes, which a
// server opens read-write.
var systemWrite = []string{"/dev"}

// ForProgram is the policy for a program that reads the given paths and
// writes the given ones, with what the system needs added.
func ForProgram(program string, read, write []string) Policy {
	p := Policy{Read: append([]string{}, systemRead...), Write: append(append([]string{}, systemWrite...), write...)}
	p.Read = append(p.Read, resolved(program)...)
	p.Read = append(p.Read, read...)
	return p
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
