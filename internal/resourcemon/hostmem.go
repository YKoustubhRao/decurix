package resourcemon

import (
	"bufio"
	"bytes"
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
)

// HostMemory reads how much memory the Docker host has available (MemAvailable of its /proc/meminfo), what making
// room for a desktop's start (idlepause.Checker.MakeRoom) measures against. It is the same number the resource bench
// reads on the docker host (bench/resources/run.py, host_memory).
//
// The control plane reads its own /proc/meminfo: in a container on the Docker host (compose, the Windows app's WSL
// distro) that is the host's, since containers do not virtualise it, and a binary on a Linux host is on the Docker
// host itself. It is trusted only when its MemTotal matches the total the Docker daemon reports (within 5%): a
// control plane that is not on the Docker host (a binary on macOS, whose Docker runs in a VM), or a /proc virtualised
// to the container's own limit, reads as "not known", and desktops then start as they always did.
type HostMemory struct {
	// DockerTotalMB is the Docker host's total memory as its daemon reports it (desktop.Manager.HostMemTotalMB).
	DockerTotalMB func(ctx context.Context) (int64, error)

	// read is the test seam for /proc/meminfo.
	read func() ([]byte, error)

	mu      sync.Mutex
	checked bool // the local reading was compared with the daemon's total once
	same    bool // and it is the Docker host's
}

// AvailableMB is the Docker host's MemAvailable in MB, and whether it is known.
func (h *HostMemory) AvailableMB(ctx context.Context) (int64, bool) {
	if h == nil {
		return 0, false
	}
	read := h.read
	if read == nil {
		read = func() ([]byte, error) { return os.ReadFile("/proc/meminfo") }
	}
	raw, err := read()
	if err != nil {
		h.settle(false, "this machine has no /proc/meminfo")
		return 0, false
	}
	total, avail, ok := ParseMeminfo(raw)
	if !ok {
		h.settle(false, "/proc/meminfo has no MemTotal or MemAvailable")
		return 0, false
	}
	h.mu.Lock()
	checked, same := h.checked, h.same
	h.mu.Unlock()
	if !checked {
		if h.DockerTotalMB == nil {
			return 0, false
		}
		docker, err := h.DockerTotalMB(ctx)
		if err != nil || docker <= 0 {
			return 0, false // Docker not answering yet: asked again next time
		}
		same = sameTotal(total, docker)
		why := ""
		if !same {
			why = "its memory (" + strconv.FormatInt(total, 10) + " MB) is not the Docker host's (" + strconv.FormatInt(docker, 10) + " MB)"
		}
		h.settle(same, why)
	}
	if !same {
		return 0, false
	}
	return avail, true
}

// settle keeps the verdict on whether the local reading is the Docker host's, logging once when it is not.
func (h *HostMemory) settle(same bool, why string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.checked {
		return
	}
	h.checked, h.same = true, same
	if !same {
		log.Printf("resourcemon: the Docker host's free memory cannot be read here (%s); desktops start without making room first", why)
	}
}

// sameTotal says whether two totals in MB are the same machine's: within 5%, since the kernel's MemTotal and the
// daemon's differ by what each leaves out.
func sameTotal(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d*20 <= b
}

// ParseMeminfo reads MemTotal and MemAvailable, in MB, from /proc/meminfo's text.
func ParseMeminfo(raw []byte) (totalMB, availMB int64, ok bool) {
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		key, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			totalMB, haveTotal = kb/1024, true
		case "MemAvailable":
			availMB, haveAvail = kb/1024, true
		}
	}
	return totalMB, availMB, haveTotal && haveAvail
}
