//go:build linux

package isolate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Landlock's system calls have one number on every architecture.
const (
	sysCreateRuleset = 444
	sysAddRule       = 445
	sysRestrictSelf  = 446

	rulePathBeneath = 1
	ruleNetPort     = 2

	createVersion = 1 // LANDLOCK_CREATE_RULESET_VERSION
	prNoNewPrivs  = 38
	oPath         = 0x200000 // O_PATH
)

// Filesystem rights, by the ABI version that added them.
const (
	fsExecute = 1 << iota
	fsWriteFile
	fsReadFile
	fsReadDir
	fsRemoveDir
	fsRemoveFile
	fsMakeChar
	fsMakeDir
	fsMakeReg
	fsMakeSock
	fsMakeFifo
	fsMakeBlock
	fsMakeSym
	fsRefer    // ABI 2
	fsTruncate // ABI 3
)

const (
	netBindTCP    = 1 // ABI 4
	netConnectTCP = 2 // ABI 4

	scopeAbstractUnix = 1 // ABI 6
	scopeSignal       = 2 // ABI 6
)

// fileRights are the only rights a rule on a file (not a directory) may hold.
const fileRights = fsExecute | fsWriteFile | fsReadFile | fsTruncate

// abi is the Landlock version the kernel offers, 0 when it offers none.
func abi() int {
	v, _, errno := syscall.Syscall(sysCreateRuleset, 0, 0, createVersion)
	if errno != 0 {
		return 0
	}
	return int(v)
}

// CanSandbox says this kernel can confine a process's files.
func CanSandbox() bool { return abi() > 0 }

// CanBlockTCP says a sandbox can also refuse TCP connections.
func CanBlockTCP() bool { return abi() >= 4 }

// Wrap makes cmd start through this program's own helper, which confines
// itself to the policy and then becomes the real program. The kernel keeps
// the confinement across that exec, and no child can ever shed it. When cmd
// already gets its own user namespace (NoNetwork), the helper also builds a
// mount view of the policy's paths alone: for that it holds CAP_SYS_ADMIN in
// that namespace, and gives it up before it becomes the program.
func Wrap(cmd *exec.Cmd, p Policy) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if a := cmd.SysProcAttr; a != nil && a.Cloneflags&syscall.CLONE_NEWUSER != 0 {
		a.Cloneflags |= syscall.CLONE_NEWNS
		a.AmbientCaps = []uintptr{capSysAdmin}
		p.Mount = true
	} else if !CanSandbox() {
		return errors.New("this kernel offers no Landlock")
	}
	policy, err := json.Marshal(p)
	if err != nil {
		return err
	}
	cmd.Args = append([]string{self, helperArg, string(policy), cmd.Path}, cmd.Args[1:]...)
	cmd.Path = self
	return nil
}

func init() {
	if len(os.Args) == 2 && os.Args[1] == probeArg {
		os.Exit(0) // CanConfine's helper got this far: confinement works here
	}
	if len(os.Args) < 4 || os.Args[1] != helperArg {
		return
	}
	// One thread: Landlock binds the thread that asks, and exec keeps that one.
	runtime.LockOSThread()
	var p Policy
	err := json.Unmarshal([]byte(os.Args[2]), &p)
	if err == nil && p.Mount {
		err = enterView(planView(p))
	}
	if err == nil {
		err = apply(p)
	}
	if err == nil {
		err = dropHelperPowers()
	}
	if err == nil {
		err = syscall.Exec(os.Args[3], append([]string{os.Args[3]}, os.Args[4:]...), os.Environ())
	}
	// Failing here must never become running unconfined.
	fmt.Fprintf(os.Stderr, "brainiac sandbox: %v\n", err)
	os.Exit(126)
}

// apply confines the calling thread, and everything it starts, to the
// policy with Landlock, and forbids gaining privileges. With no Landlock
// only a mount view (p.Mount) confines the files; without either it fails.
func apply(p Policy) error {
	version := abi()
	if version == 0 {
		if !p.Mount {
			return errors.New("this kernel offers no Landlock")
		}
		if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
			return fmt.Errorf("no_new_privs: %w", errno)
		}
		return nil
	}
	handled := uint64(fsExecute | fsWriteFile | fsReadFile | fsReadDir | fsRemoveDir | fsRemoveFile | fsMakeChar |
		fsMakeDir | fsMakeReg | fsMakeSock | fsMakeFifo | fsMakeBlock | fsMakeSym)
	if version >= 2 {
		handled |= fsRefer
	}
	if version >= 3 {
		handled |= fsTruncate
	}
	// handled_access_fs, handled_access_net, scoped: each newer field makes the
	// struct longer, and an older kernel refuses a longer one.
	attr := [3]uint64{handled}
	size := 8
	if version >= 4 && p.BlockTCP {
		attr[1] = netBindTCP | netConnectTCP
		size = 16
	}
	if version >= 6 {
		attr[2] = scopeAbstractUnix | scopeSignal
		size = 24
	}
	fd, _, errno := syscall.Syscall(sysCreateRuleset, uintptr(unsafe.Pointer(&attr[0])), uintptr(size), 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	defer syscall.Close(int(fd))
	read := uint64(fsExecute | fsReadFile | fsReadDir)
	write := handled
	for _, path := range p.Read {
		if err := allowPath(int(fd), path, read); err != nil {
			return err
		}
	}
	for _, path := range p.Write {
		if err := allowPath(int(fd), path, write); err != nil {
			return err
		}
	}
	if attr[1] != 0 {
		for _, port := range p.BindTCP {
			rule := [2]uint64{netBindTCP, uint64(port)}
			if _, _, errno := syscall.Syscall6(sysAddRule, fd, ruleNetPort, uintptr(unsafe.Pointer(&rule[0])), 0, 0, 0); errno != 0 {
				return fmt.Errorf("landlock_add_rule port %d: %w", port, errno)
			}
		}
	}
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("no_new_privs: %w", errno)
	}
	if _, _, errno := syscall.RawSyscall(sysRestrictSelf, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// allowPath grants rights beneath one path. A path this system does not have
// is skipped: the policy lists what any Linux might use.
func allowPath(ruleset int, path string, rights uint64) error {
	fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		rights &= fileRights
	}
	// struct landlock_path_beneath_attr is packed: u64 rights, then s32 fd.
	var rule [12]byte
	*(*uint64)(unsafe.Pointer(&rule[0])) = rights
	*(*int32)(unsafe.Pointer(&rule[8])) = int32(fd)
	if _, _, errno := syscall.Syscall6(sysAddRule, uintptr(ruleset), rulePathBeneath, uintptr(unsafe.Pointer(&rule[0])), 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_add_rule %s: %w", path, errno)
	}
	return nil
}

var (
	probeOnce sync.Once
	probeOK   bool
	probeErr  error
)

// CanConfine reports whether a model process can be started with no network
// and a view of only its own files, by starting this program that way once.
// A host that refuses unprivileged user namespaces, or the capabilities in
// them (an AppArmor rule, a container), says no here, before any model.
func CanConfine() (bool, error) {
	probeOnce.Do(func() {
		self, err := os.Executable()
		if err != nil {
			probeErr = err
			return
		}
		cmd := exec.Command(self, probeArg)
		cmd.Env = Env()
		NoNetwork(cmd)
		if probeErr = Wrap(cmd, ForProgram(self, nil, nil)); probeErr != nil {
			return
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			probeErr = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
			return
		}
		probeOK = true
	})
	return probeOK, probeErr
}

// resolved is what running a program needs from where it lives: the program
// file, the real one behind a symlink (a venv's python points at the
// system's or a managed install's), and the shared libraries beside each
// (llama.cpp ships its libggml*.so next to llama-server). Not the whole
// directory: a program in a shared directory (a download folder, /tmp) must
// not be handed everything else in it.
func resolved(program string) []string {
	files := []string{program}
	if real, err := filepath.EvalSymlinks(program); err == nil && real != program {
		files = append(files, real)
	}
	out := append([]string{}, files...)
	seen := map[string]bool{}
	for _, f := range files {
		dir := filepath.Dir(f)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if n := e.Name(); !e.IsDir() && (strings.HasSuffix(n, ".so") || strings.Contains(n, ".so.")) {
				out = append(out, filepath.Join(dir, n))
			}
		}
	}
	return out
}
