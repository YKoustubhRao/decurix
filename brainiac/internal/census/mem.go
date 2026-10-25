package census

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// parseMeminfo reads MemTotal and MemAvailable (bytes) from /proc/meminfo.
// An old kernel without MemAvailable gets MemFree + Buffers + Cached.
func parseMeminfo(text string) (total, avail uint64, ok bool) {
	fields := map[string]uint64{}
	for _, line := range strings.Split(text, "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		parts := strings.Fields(rest)
		if len(parts) == 0 {
			continue
		}
		v, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			continue
		}
		if len(parts) > 1 && strings.EqualFold(parts[1], "kB") {
			v *= 1024
		}
		fields[strings.TrimSpace(key)] = v
	}
	total, ok = fields["MemTotal"]
	if !ok || total == 0 {
		return 0, 0, false
	}
	if a, has := fields["MemAvailable"]; has {
		avail = a
	} else {
		avail = fields["MemFree"] + fields["Buffers"] + fields["Cached"]
	}
	if avail > total {
		avail = total
	}
	return total, avail, true
}

// parseCgroupLimit reads a cgroup memory limit file: "max" (v2) or a byte
// count; v1's "unlimited" is a huge number, which callers compare to RAM.
func parseCgroupLimit(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "max" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}

// memStatValue reads one key of a cgroup memory.stat file.
func memStatValue(text, key string) uint64 {
	for _, line := range strings.Split(text, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[0] == key {
			v, _ := strconv.ParseUint(parts[1], 10, 64)
			return v
		}
	}
	return 0
}

// applyCgroup caps a machine's memory by a cgroup limit. Page cache the
// cgroup can drop (inactive file pages) counts as available, as it does in
// MemAvailable.
func applyCgroup(total, avail, limit, usage, inactiveFile uint64) (uint64, uint64) {
	if limit == 0 || limit >= total {
		return total, avail
	}
	used := usage
	if inactiveFile < used {
		used -= inactiveFile
	} else {
		used = 0
	}
	free := uint64(0)
	if used < limit {
		free = limit - used
	}
	if free > avail {
		free = avail
	}
	return limit, free
}

var vmStatPage = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVMStat reads macOS vm_stat: free + inactive + speculative pages.
func parseVMStat(text string) (uint64, bool) {
	page := uint64(4096)
	if m := vmStatPage.FindStringSubmatch(text); m != nil {
		page, _ = strconv.ParseUint(m[1], 10, 64)
	}
	var pages uint64
	found := false
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Pages free", "Pages inactive", "Pages speculative":
			v, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
			if err == nil {
				pages += v
				found = true
			}
		}
	}
	return pages * page, found
}

// decodeConsole turns a Windows tool's output into text: wsl.exe writes
// UTF-16LE (with or without a BOM) unless WSL_UTF8 is set.
func decodeConsole(b []byte) string {
	if len(b) >= 2 && (b[0] == 0xFF && b[1] == 0xFE || looksUTF16(b)) {
		if b[0] == 0xFF && b[1] == 0xFE {
			b = b[2:]
		}
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return string(utf16.Decode(u))
	}
	return strings.TrimPrefix(string(b), string(rune(0xFEFF)))
}

// looksUTF16 guesses UTF-16LE: many zero bytes at odd positions.
func looksUTF16(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	zeros := 0
	for i := 1; i < len(b); i += 2 {
		if b[i] == 0 {
			zeros++
		}
	}
	return zeros*2 >= len(b)/2
}

// parseWSLRunning lists the running distributions from
// `wsl.exe --list --running --quiet`.
func parseWSLRunning(b []byte) []string {
	var names []string
	for _, line := range strings.Split(decodeConsole(b), "\n") {
		name := strings.TrimSpace(strings.Trim(line, "\x00\r"))
		if name == "" || strings.Contains(name, " ") && strings.Contains(strings.ToLower(name), "no running") {
			continue
		}
		names = append(names, name)
	}
	return names
}

// pickWSLDistro prefers a running distribution that is not Docker Desktop's
// own (all WSL2 distributions share one VM, so any shows its memory).
func pickWSLDistro(running []string) string {
	for _, d := range running {
		if !strings.HasPrefix(strings.ToLower(d), "docker-desktop") {
			return d
		}
	}
	if len(running) > 0 {
		return running[0]
	}
	return ""
}
