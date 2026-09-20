package ollama

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// readMemory returns total and available memory in bytes. "Available" is the
// kernel's own MemAvailable - free plus what can be reclaimed without
// swapping - i.e. total minus what other programs really hold. Inside a
// container with a memory limit, the limit is honoured.
func readMemory() (total, avail uint64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}

	total, avail, err = parseMeminfo(string(data))
	if err != nil {
		return 0, 0, err
	}

	if limit, used, ok := cgroupMemory(); ok && limit < total {
		total = limit
		if used < limit && limit-used < avail {
			avail = limit - used
		} else if used >= limit {
			avail = 0
		}
	}

	return total, avail, nil
}

// parseMeminfo reads MemTotal and MemAvailable (kB in the file) as bytes.
func parseMeminfo(s string) (total, avail uint64, err error) {
	var haveTotal, haveAvail bool

	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}

		kb, perr := strconv.ParseUint(f[1], 10, 64)
		if perr != nil {
			continue
		}

		switch f[0] {
		case "MemTotal:":
			total, haveTotal = kb*1024, true
		case "MemAvailable:":
			avail, haveAvail = kb*1024, true
		}
	}

	if !haveTotal || !haveAvail {
		return 0, 0, fmt.Errorf("meminfo: MemTotal/MemAvailable not found")
	}

	return total, avail, nil
}

// cgroupMemory reads a cgroup v2 memory limit and usage, if one is set.
func cgroupMemory() (limit, used uint64, ok bool) {
	rawMax, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0, 0, false
	}

	limit, err = strconv.ParseUint(strings.TrimSpace(string(rawMax)), 10, 64)
	if err != nil { // "max": no limit
		return 0, 0, false
	}

	rawCur, err := os.ReadFile("/sys/fs/cgroup/memory.current")
	if err != nil {
		return 0, 0, false
	}

	used, err = strconv.ParseUint(strings.TrimSpace(string(rawCur)), 10, 64)
	if err != nil {
		return 0, 0, false
	}

	return limit, used, true
}

// cpuBusy samples /proc/stat twice, window apart, and returns the fraction
// (0-1) of CPU time spent doing anything but idling.
func cpuBusy(ctx context.Context, window time.Duration) (float64, error) {
	idle1, total1, err := readCPUStat()
	if err != nil {
		return 0, err
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(window):
	}

	idle2, total2, err := readCPUStat()
	if err != nil {
		return 0, err
	}

	return busyFraction(idle1, total1, idle2, total2), nil
}

func readCPUStat() (idle, total uint64, err error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}

	line, _, _ := strings.Cut(string(data), "\n")
	return parseCPULine(line)
}

// parseCPULine reads the aggregate "cpu ..." line of /proc/stat. Idle is
// idle+iowait; total is every field the kernel reports (guest time is already
// inside user/nice, so it is left out).
func parseCPULine(line string) (idle, total uint64, err error) {
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, fmt.Errorf("stat: unexpected line %q", line)
	}

	for i, s := range f[1:] {
		if i >= 8 { // user nice system idle iowait irq softirq steal
			break
		}

		v, perr := strconv.ParseUint(s, 10, 64)
		if perr != nil {
			return 0, 0, perr
		}

		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}

	return idle, total, nil
}
