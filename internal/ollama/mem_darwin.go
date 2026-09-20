package ollama

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// readMemory returns total and available memory in bytes. Available is what
// vm_stat calls free, inactive and speculative: memory the OS hands back on
// demand, which is what Activity Monitor leaves out of "used".
func readMemory() (total, avail uint64, err error) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, 0, err
	}

	total, err = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, 0, err
	}

	vm, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, 0, err
	}

	avail, err = parseVMStat(string(vm))
	return total, avail, err
}

var (
	vmPageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)
	vmPagesRe    = regexp.MustCompile(`(?m)^Pages (free|inactive|speculative):\s+(\d+)\.`)
)

// parseVMStat sums the reclaimable pages in vm_stat's output, as bytes.
func parseVMStat(s string) (uint64, error) {
	m := vmPageSizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("vm_stat: page size not found")
	}

	pageSize, _ := strconv.ParseUint(m[1], 10, 64)

	var pages uint64
	for _, p := range vmPagesRe.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.ParseUint(p[2], 10, 64)
		pages += n
	}

	return pages * pageSize, nil
}

var topIdleRe = regexp.MustCompile(`([\d.]+)% idle`)

// cpuBusy asks top for two samples a second apart and reads the second's idle
// share. window is not honoured exactly: top's own interval is one second.
func cpuBusy(ctx context.Context, _ time.Duration) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "top", "-l", "2", "-n", "0", "-s", "1").Output()
	if err != nil {
		return 0, err
	}

	var idle string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "CPU usage:") {
			if m := topIdleRe.FindStringSubmatch(line); m != nil {
				idle = m[1]
			}
		}
	}

	pct, err := strconv.ParseFloat(idle, 64)
	if err != nil {
		return 0, fmt.Errorf("top: no CPU usage line")
	}

	return 1 - pct/100, nil
}
