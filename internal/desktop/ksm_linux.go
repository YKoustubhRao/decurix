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

// OptimizeHostKSM inspects and enables Linux Kernel Samepage Merging (KSM)
// if supported by the host kernel, deduplicating identical physical memory
// pages across container processes.
// pages across container processes. If FLOWERA_DISABLE_KSM is set, it explicitly disables KSM.
func OptimizeHostKSM() {
	if os.Getenv("FLOWERA_DISABLE_KSM") == "1" ||
		os.Getenv("FLOWERA_DISABLE_KSM") == "true" ||
		os.Getenv("FLOWERA_ENABLE_KSM") == "0" ||
		os.Getenv("FLOWERA_ENABLE_KSM") == "false" {
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
