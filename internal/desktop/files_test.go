package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComputeDefaultVMDest(t *testing.T) {
	userHome, _ := os.UserHomeDir()
	if userHome != "" {
		hostDoc := filepath.Join(userHome, "Documents", "report.pdf")
		got := ComputeDefaultVMDest(hostDoc)
		want := "/home/headless/Documents/report.pdf"
		if got != want {
			t.Errorf("ComputeDefaultVMDest(%q) = %q, want %q", hostDoc, got, want)
		}
	}

	// Test anchor folder detection for paths outside home
	got := ComputeDefaultVMDest("D:/data/projects/my-app")
	want := "/home/headless/projects/my-app"
	if got != want {
		t.Errorf("ComputeDefaultVMDest anchor got %q, want %q", got, want)
	}

	gotBase := ComputeDefaultVMDest("D:/random/unknown/file.txt")
	wantBase := "/home/headless/file.txt"
	if gotBase != wantBase {
		t.Errorf("ComputeDefaultVMDest fallback got %q, want %q", gotBase, wantBase)
	}
}
