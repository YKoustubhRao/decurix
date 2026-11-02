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
// the confinement across that exec, and no child can ever shed it.
func Wrap(cmd *exec.Cmd, p Policy) error {
	self, err := os.Executable()
	if err != nil {
		return err
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
	if len(os.Args) < 4 || os.Args[1] != helperArg {
		return
	}
	// One thread: Landlock binds the thread that asks, and exec keeps that one.
	runtime.LockOSThread()
	var p Policy
	err := json.Unmarshal([]byte(os.Args[2]), &p)
	if err == nil {
		err = apply(p)
	}
	if err == nil {
		err = syscall.Exec(os.Args[3], append([]string{os.Args[3]}, os.Args[4:]...), os.Environ())
	}
	// Failing here must never become running unconfined.
	fmt.Fprintf(os.Stderr, "brainiac sandbox: %v\n", err)
	os.Exit(126)
}

// apply confines the calling thread, and everything it starts, to the policy.
func apply(p Policy) error {
	version := abi()
	if version == 0 {
		return errors.New("this kernel offers no Landlock")
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

// resolved is a program's directory, and the real one behind a symlink (a
// venv's python points at the system's or a managed install's).
func resolved(program string) []string {
	out := []string{filepath.Dir(program)}
	if real, err := filepath.EvalSymlinks(program); err == nil && filepath.Dir(real) != out[0] {
		out = append(out, filepath.Dir(real))
	}
	return out
}
