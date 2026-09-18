package desktop

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectHostApps(t *testing.T) {
	items := DetectHostApps()
	if len(items) == 0 {
		t.Fatal("expected non-empty host app list")
	}

	foundBridge := false
	for _, item := range items {
		if item.ID == "host_bridge" {
			foundBridge = true
			if !item.Detected {
				t.Errorf("host_bridge should always be marked detected")
			}
		}
	}
	if !foundBridge {
		t.Errorf("expected host_bridge in DetectHostApps list")
	}
}

func TestCacheExclusion(t *testing.T) {
	testCases := []struct {
		name     string
		path     string
		isDir    bool
		expected bool
	}{
		{"Normal file", "/home/user/.config/gh/hosts.yml", false, false},
		{"Chrome Cache dir", "/home/user/.config/google-chrome/Default/Cache", true, true},
		{"GPUCache", "/home/user/.config/google-chrome/Default/GPUCache", true, true},
		{"Code Cache", "/home/user/.config/google-chrome/Default/Code Cache", true, true},
		{"Crashpad", "/home/user/.config/google-chrome/Crashpad", true, true},
		{"Temporary file", "/home/user/.config/google-chrome/Default/file.tmp", false, true},
		{"Log file", "/home/user/.config/google-chrome/Default/debug.log", false, true},
		{"Flowera repo recursion", "/home/user/flowera/cmd/flowera", true, true},
		{"Migration toolkit recursion", "/home/user/migration-toolkit", true, true},
		{"Node modules", "/home/user/.config/Code/node_modules", true, true},
		{"Cookies DB", "/home/user/.config/google-chrome/Default/Cookies", false, false},
		{"Login Data DB", "/home/user/.config/google-chrome/Default/Login Data", false, false},
	}

	for _, tc := range testCases {
		res := isCacheOrTransient(filepath.Base(tc.path), tc.path, tc.isDir)
		if res != tc.expected {
			t.Errorf("isCacheOrTransient(%q, %q, %v) = %v; want %v", filepath.Base(tc.path), tc.path, tc.isDir, res, tc.expected)
		}
	}
}

func TestPackHostBridgeLaunchers(t *testing.T) {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	if err := packHostBridgeLaunchers(tw); err != nil {
		t.Fatalf("packHostBridgeLaunchers failed: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	tr := tar.NewReader(buf)
	foundFiles := make(map[string]bool)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		foundFiles[hdr.Name] = true

		if strings.HasSuffix(hdr.Name, ".desktop") || strings.HasPrefix(hdr.Name, ".local/bin/") {
			if hdr.Mode&0111 == 0 {
				t.Errorf("file %s expected executable bit set, got mode %o", hdr.Name, hdr.Mode)
			}
		}
	}

	expected := []string{
		".local/bin/host-run",
		".local/bin/host-open",
		".local/bin/host-clip",
		".local/bin/chrome-host",
		".local/bin/code-host",
		".local/bin/terminal-host",
		"Desktop/chrome-host.desktop",
		"Desktop/code-host.desktop",
		"Desktop/terminal-host.desktop",
		".local/share/applications/chrome-host.desktop",
	}

	for _, exp := range expected {
		if !foundFiles[exp] {
			t.Errorf("expected %s in host bridge tar archive", exp)
		}
	}
}

func TestWriteDirToTar(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hostapps_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create dummy files: cookies, cache, config
	_ = os.WriteFile(filepath.Join(tmpDir, "Cookies"), []byte("auth_cookie_token=abc123xyz"), 0600)
	_ = os.MkdirAll(filepath.Join(tmpDir, "Cache"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "Cache", "data_0"), []byte("cache_blob_should_be_stripped"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "Preferences"), []byte(`{"logged_in": true}`), 0644)

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	err = writeDirToTar(tw, tmpDir, ".config/app", isCacheOrTransient)
	if err != nil {
		t.Fatalf("writeDirToTar: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(buf)
	foundFiles := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		foundFiles[hdr.Name] = true
	}

	if !foundFiles[".config/app/Cookies"] {
		t.Errorf("expected Cookies to be preserved")
	}
	if !foundFiles[".config/app/Preferences"] {
		t.Errorf("expected Preferences to be preserved")
	}
	if foundFiles[".config/app/Cache/data_0"] {
		t.Errorf("Cache/data_0 should have been stripped!")
	}
}
