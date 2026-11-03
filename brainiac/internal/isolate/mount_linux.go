//go:build linux

package isolate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// The mount view: inside its own user and mount namespaces the sandbox
// helper builds a root that holds only the policy's paths, read-only for
// Read and read-write for Write, and pivots into it. Whatever else is on the
// machine - a daemon's Unix socket in /run, another user's files, this
// user's home - is not in the process's view at all, so no rule has to
// name it. A pathname socket is a file, and Landlock has no right for
// connecting to one; a view without it is the cover.

const (
	capSysAdmin       = 21 // CAP_SYS_ADMIN, held in the helper's own user namespace only
	prCapAmbient      = 47 // PR_CAP_AMBIENT
	prCapAmbientClear = 4  // PR_CAP_AMBIENT_CLEAR_ALL
	prSetPdeathsig    = 1  // PR_SET_PDEATHSIG
)

// viewStep is one thing to put in the new root: a symlink as the host has
// it, or a bind of a real path.
type viewStep struct {
	path   string // where it appears, the same absolute path as on the host
	link   string // a symlink's target, or "" for a bind
	write  bool
	isFile bool
	tmpfs  bool // a fresh, private tmpfs (the view's own /dev/shm)
}

// viewDevices are the device nodes a view's /dev holds: the plain ones any
// program uses, and a GPU's, where this machine has them. Not the host's
// whole /dev: its /dev/shm and /dev/mqueue are shared with every host
// process, a channel out of the sandbox.
// WSL2 exposes its GPU as /dev/dxg (its user-space drivers are under
// /usr/lib/wsl, which the /usr read already binds, submounts included).
var viewDevices = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom",
	"/dev/dri", "/dev/kfd", "/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia-modeset", "/dev/dxg"}

// devSteps is the view's /dev: the devices above that exist, the NVIDIA
// GPUs (nvidia0, nvidia1...), and a private /dev/shm.
func devSteps() []viewStep { return devStepsWith(os.Stat, filepath.Glob) }

// devStepsWith is devSteps with the host's view passed in (tests).
func devStepsWith(stat func(string) (os.FileInfo, error), glob func(string) ([]string, error)) []viewStep {
	var out []viewStep
	paths := append([]string{}, viewDevices...)
	if gpus, err := glob("/dev/nvidia[0-9]*"); err == nil {
		paths = append(paths, gpus...)
	}
	for _, p := range paths {
		if fi, err := stat(p); err == nil {
			out = append(out, viewStep{path: p, write: true, isFile: !fi.IsDir()})
		}
	}
	return append(out, viewStep{path: "/dev/shm", tmpfs: true})
}

// planView lists what the new root needs for a policy, read from the host's
// view before anything is mounted. A path through a symlink keeps the
// symlink, and its real path is bound. A path the host does not have is
// left out, as the Landlock rules leave it out.
func planView(p Policy) []viewStep {
	seen := map[string]bool{}
	var steps []viewStep
	add := func(s viewStep) {
		key := s.path + "|" + s.link
		if !seen[key] {
			seen[key] = true
			steps = append(steps, s)
		}
	}
	walk := func(path string, write bool) {
		path = filepath.Clean(path)
		if !filepath.IsAbs(path) {
			return
		}
		// Follow the path one component at a time, keeping each symlink on
		// the way, until the real path is known.
		cur := "/"
		rest := strings.Split(strings.TrimPrefix(path, "/"), "/")
		for hops := 0; len(rest) > 0 && hops < 40; {
			next := filepath.Join(cur, rest[0])
			fi, err := os.Lstat(next)
			if err != nil {
				return
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(next)
				if err != nil {
					return
				}
				add(viewStep{path: next, link: target})
				hops++
				if !filepath.IsAbs(target) {
					target = filepath.Join(cur, target)
				}
				rest = append(strings.Split(strings.TrimPrefix(filepath.Clean(target), "/"), "/"), rest[1:]...)
				cur = "/"
				continue
			}
			cur, rest = next, rest[1:]
		}
		if len(rest) > 0 {
			return
		}
		fi, err := os.Stat(cur)
		if err != nil {
			return
		}
		add(viewStep{path: cur, write: write, isFile: !fi.IsDir()})
	}
	for _, r := range p.Read {
		walk(r, false)
	}
	for _, w := range p.Write {
		if filepath.Clean(w) == "/dev" {
			for _, s := range devSteps() {
				add(s)
			}
			continue
		}
		walk(w, true)
	}
	// Parents before children, so a bind inside a bound directory lands on
	// top of it rather than under it.
	sort.SliceStable(steps, func(i, j int) bool {
		return strings.Count(steps[i].path, "/") < strings.Count(steps[j].path, "/")
	})
	return steps
}

// enterView builds the new root from steps and makes it the process's root.
// It runs in the helper, which holds CAP_SYS_ADMIN in its own user namespace;
// it fails, and the helper with it, rather than leave a partial view.
func enterView(steps []viewStep) error {
	cwd, _ := os.Getwd()
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	// A tmpfs becomes the root for a moment, with the old root under it, so
	// every bind is made from the host's real paths (/tmp included).
	base := "/tmp"
	if err := syscall.Mount("tmpfs", base, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0700"); err != nil {
		return fmt.Errorf("mount the new root: %w", err)
	}
	for _, d := range []string{"oldroot", "newroot"} {
		if err := os.Mkdir(filepath.Join(base, d), 0o700); err != nil {
			return err
		}
	}
	if err := syscall.PivotRoot(base, filepath.Join(base, "oldroot")); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", "/newroot", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mount the view: %w", err)
	}
	for _, s := range steps {
		dst := filepath.Join("/newroot", s.path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if s.link != "" {
			if err := os.Symlink(s.link, dst); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			continue
		}
		if s.tmpfs {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			if err := syscall.Mount("tmpfs", dst, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=1777"); err != nil {
				return fmt.Errorf("%s: %w", s.path, err)
			}
			continue
		}
		if err := bindInto(filepath.Join("/oldroot", s.path), dst, s.isFile, s.write); err != nil {
			return fmt.Errorf("%s: %w", s.path, err)
		}
	}
	// The second pivot leaves only the view; the old root is detached, so
	// nothing outside the policy can be reached by any path.
	if err := os.Mkdir("/newroot/.old", 0o700); err != nil {
		return err
	}
	if err := syscall.PivotRoot("/newroot", "/newroot/.old"); err != nil {
		return fmt.Errorf("pivot_root into the view: %w", err)
	}
	if err := syscall.Unmount("/.old", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach the old root: %w", err)
	}
	_ = os.Remove("/.old")
	if cwd == "" || os.Chdir(cwd) != nil {
		return os.Chdir("/")
	}
	return nil
}

// bindInto binds src at dst, read-only unless write. The flags the kernel
// locks on a mount from another namespace (nosuid, nodev, noexec, atime)
// are kept, or remounting read-only is refused.
func bindInto(src, dst string, isFile, write bool) error {
	if isFile {
		if _, err := os.Lstat(dst); err != nil {
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			f.Close()
		}
	} else if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	if write {
		return nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dst, &st); err != nil {
		return err
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY)
	for _, f := range []struct{ st, ms uintptr }{
		{0x2, syscall.MS_NOSUID}, {0x4, syscall.MS_NODEV}, {0x8, syscall.MS_NOEXEC},
		{0x400, syscall.MS_NOATIME}, {0x800, syscall.MS_NODIRATIME}, {0x1000, syscall.MS_RELATIME},
	} {
		if uintptr(st.Flags)&f.st != 0 {
			flags |= f.ms
		}
	}
	if err := syscall.Mount("", dst, "", flags, ""); err != nil {
		return fmt.Errorf("read-only: %w", err)
	}
	return nil
}

// dropHelperPowers gives up what the helper needed to build the view, so the
// program it becomes has no capability at all, and dies with Brainiac.
func dropHelperPowers() error {
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClear, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("clear ambient capabilities: %w", errno)
	}
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetPdeathsig, uintptr(syscall.SIGKILL), 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("parent death signal: %w", errno)
	}
	if os.Getppid() == 1 {
		return errors.New("Brainiac exited while the sandbox was being built")
	}
	return nil
}
