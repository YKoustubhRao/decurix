//go:build linux

package desktop

import (
	"log"
	"os"
	"strings"
)

const (
	ksmRunPath   = "/sys/kernel/mm/ksm/run"
	ksmPagesPath = "/sys/kernel/mm/ksm/pages_to_scan"
	ksmSleepPath = "/sys/kernel/mm/ksm/sleep_millisecs"
)

// OptimizeHostKSM inspects and enables Linux Kernel Samepage Merging (KSM)
// if supported by the host kernel, deduplicating identical physical memory
// pages across container processes.
func OptimizeHostKSM() {
	data, err := os.ReadFile(ksmRunPath)
	if err != nil {
		// KSM not present or kernel doesn't support CONFIG_KSM
		return
	}

	val := strings.TrimSpace(string(data))
	if val == "1" {
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

	log.Println("[ksm] Enabled Linux Kernel Samepage Merging (KSM) for cross-container RAM deduplication.")
}
