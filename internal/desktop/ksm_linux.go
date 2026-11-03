//go:build linux

package desktop

import (
	"log"
	"os"
	"strings"
)

const (
	ksmRunPath       = "/sys/kernel/mm/ksm/run"
	ksmPagesPath     = "/sys/kernel/mm/ksm/pages_to_scan"
	ksmSleepPath     = "/sys/kernel/mm/ksm/sleep_millisecs"
	ksmZeroPagesPath = "/sys/kernel/mm/ksm/use_zero_pages"
)

// OptimizeHostKSM enables Linux Kernel Samepage Merging (KSM), which merges identical memory pages across container
// processes, only when DECURIX_ENABLE_KSM=1 (or true). It is off by default (L-01): merging pages across desktops
// that distrust each other opens a known page-deduplication timing side channel between them, and memory saved is not
// worth that unless the owner chooses it. DECURIX_DISABLE_KSM=1, or DECURIX_ENABLE_KSM=0, turns it off explicitly.
func OptimizeHostKSM() {
	if !ksmWanted(os.Getenv("DECURIX_ENABLE_KSM"), os.Getenv("DECURIX_DISABLE_KSM")) {
		// Not wanted: turned off when an earlier build left it on (it stays on until the kernel restarts otherwise,
		// RW1-D-03). DisableHostKSM writes only when it is on, and only where this process may.
		DisableHostKSM()
		return
	}

	data, err := os.ReadFile(ksmRunPath)
	if err != nil {
		// KSM not present or kernel doesn't support CONFIG_KSM
		return
	}

	val := strings.TrimSpace(string(data))
	if val == "1" {
		_ = os.WriteFile(ksmZeroPagesPath, []byte("1"), 0644)
		log.Println("[ksm] Linux Kernel Samepage Merging (KSM) is active (deduplicating RAM across containers).")
		return
	}

	// Try enabling KSM
	if err := os.WriteFile(ksmRunPath, []byte("1"), 0644); err != nil {
		log.Printf("[ksm] Linux KSM is supported but inactive. For 25-40%% RAM deduplication across desktops, enable with: sudo sh -c 'echo 1 > %s'", ksmRunPath)
		return
	}

	// Configure gentle scan parameters if writable
	_ = os.WriteFile(ksmPagesPath, []byte("100"), 0644)
	_ = os.WriteFile(ksmSleepPath, []byte("20"), 0644)
	_ = os.WriteFile(ksmZeroPagesPath, []byte("1"), 0644)

	log.Println("[ksm] Enabled Linux Kernel Samepage Merging (KSM) for cross-container RAM deduplication.")
}

// DisableHostKSM disables Linux Kernel Samepage Merging (KSM) scanning to eliminate
// all kernel memory scanning CPU overhead and copy-on-write page fault latencies.
func DisableHostKSM() {
	data, err := os.ReadFile(ksmRunPath)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(data)) != "0" {
		if err := os.WriteFile(ksmRunPath, []byte("0"), 0644); err == nil {
			log.Println("[ksm] Disabled Linux Kernel Samepage Merging (KSM) scanning.")
		}
	}
}

// ksmWanted reports whether KSM is to be turned on: only on an explicit DECURIX_ENABLE_KSM=1 (or true), and never
// when DECURIX_DISABLE_KSM says otherwise.
func ksmWanted(enable, disable string) bool {
	if disable == "1" || disable == "true" {
		return false
	}
	return enable == "1" || enable == "true"
}
