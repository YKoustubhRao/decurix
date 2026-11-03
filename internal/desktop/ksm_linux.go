//go:build linux

package desktop

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
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
		explicit := os.Getenv("DECURIX_ENABLE_KSM") == "0" || os.Getenv("DECURIX_ENABLE_KSM") == "false" || os.Getenv("DECURIX_DISABLE_KSM") != ""
		on := false
		if data, err := os.ReadFile(ksmRunPath); err == nil {
			on = strings.TrimSpace(string(data)) == "1"
		}
		switch ksmOffDecision(explicit, on, ksmMarked()) {
		case ksmTurnOff:
			// Decurix turned it on (its marker says so), or the person asked: it goes off. KSM an owner enabled for
			// other work is never touched unasked (REVIEW-3 follow-up 4).
			DisableHostKSM()
			clearKSMMark()
		case ksmSayOn:
			log.Printf("[ksm] Kernel Samepage Merging is on, and Decurix did not turn it on, so it is left as it is. It merges memory across desktops that distrust each other; set DECURIX_DISABLE_KSM=1 to turn it off.")
		}
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

	markKSM() // so a later start that does not want it turns off only what Decurix turned on

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

// ksmMarkFile is the marker in the data directory that says this control plane turned KSM on.
func ksmMarkFile() string { return filepath.Join(sqlitedb.DataDir(), "ksm-enabled-by-decurix") }

func ksmMarked() bool { _, err := os.Stat(ksmMarkFile()); return err == nil }

func markKSM() { _ = os.WriteFile(ksmMarkFile(), []byte("1\n"), 0o600) }

func clearKSMMark() { _ = os.Remove(ksmMarkFile()) }

// ksmWanted reports whether KSM is to be turned on: only on an explicit DECURIX_ENABLE_KSM=1 (or true), and never
// when DECURIX_DISABLE_KSM says otherwise.
func ksmWanted(enable, disable string) bool {
	if disable == "1" || disable == "true" {
		return false
	}
	return enable == "1" || enable == "true"
}
