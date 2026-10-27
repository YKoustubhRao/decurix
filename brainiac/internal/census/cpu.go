package census

import (
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// probeCPU reads the processor: model, cores and inference-relevant flags.
func probeCPU(n *notes) CPU {
	c := CPU{LogicalCores: runtime.NumCPU()}

	c.Model = strings.TrimSpace(x86Brand())
	if c.Model == "" {
		c.Model = strings.TrimSpace(cpuModelOS())
	}
	if c.Model == "" {
		c.Model = "unknown " + runtime.GOARCH + " CPU"
	}

	c.PhysicalCores = physicalCores()
	if c.PhysicalCores <= 0 || c.PhysicalCores > c.LogicalCores {
		if c.PhysicalCores <= 0 {
			n.add("physical core count unknown; using the %d logical cores", c.LogicalCores)
		}
		c.PhysicalCores = c.LogicalCores
	}

	seen := map[string]bool{}
	for _, f := range append(x86Flags(), osFlags()...) {
		seen[strings.ToLower(f)] = true
	}
	for f := range seen {
		c.Flags = append(c.Flags, f)
	}
	sort.Strings(c.Flags)
	return c
}

// cpuInfo is what /proc/cpuinfo says.
type cpuInfo struct {
	Model         string
	PhysicalCores int
	Features      []string // arm64 "Features" words
}

// parseCPUInfo reads /proc/cpuinfo text (x86 or arm64).
func parseCPUInfo(text string) cpuInfo {
	var info cpuInfo
	cores := map[string]bool{}
	packages := map[string]bool{}
	coresPerPackage := 0
	var physID, coreID string
	var cpuPart, implementer string

	flush := func() {
		if physID != "" && coreID != "" {
			cores[physID+"/"+coreID] = true
		}
		physID, coreID = "", ""
	}

	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "model name":
			if info.Model == "" {
				info.Model = value
			}
		case "Model", "Hardware":
			if info.Model == "" && value != "" {
				info.Model = value
			}
		case "physical id":
			physID = value
			packages[value] = true
		case "core id":
			coreID = value
		case "cpu cores":
			if n, err := strconv.Atoi(value); err == nil && coresPerPackage == 0 {
				coresPerPackage = n
			}
		case "Features":
			if len(info.Features) == 0 {
				info.Features = strings.Fields(value)
			}
		case "CPU part":
			if cpuPart == "" {
				cpuPart = value
			}
		case "CPU implementer":
			if implementer == "" {
				implementer = value
			}
		}
	}
	flush()

	switch {
	case len(cores) > 0:
		info.PhysicalCores = len(cores)
	case coresPerPackage > 0:
		info.PhysicalCores = coresPerPackage * max(1, len(packages))
	}
	if info.Model == "" && cpuPart != "" {
		info.Model = "ARM CPU (implementer " + implementer + ", part " + cpuPart + ")"
	}
	return info
}

// armFlags turns arm64 /proc/cpuinfo Features into Brainiac's flag names.
func armFlags(features []string) []string {
	flags := []string{"neon"}
	for _, f := range features {
		switch f {
		case "asimddp":
			flags = append(flags, "dotprod")
		case "sve":
			flags = append(flags, "sve")
		case "sve2":
			flags = append(flags, "sve2")
		case "i8mm":
			flags = append(flags, "i8mm")
		case "bf16":
			flags = append(flags, "bf16")
		}
	}
	return flags
}
