package httpapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func backupBytes(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(body)), Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return &buf
}

// Only a desktop's skills and memory are read - not recordings, not the
// data dir, not a name that climbs or nests.
func TestReadBackupFoldersTakesSkillsAndMemoryOnly(t *testing.T) {
	got, err := readBackupFolders(backupBytes(t, map[string]string{
		"manifest.json":                        `{"version":1}`,
		"data/decurix.db":                      "db",
		"desktops/old-box/skills/invoice.json": `{"name":"invoice"}`,
		"desktops/old-box/memory/notes.txt":    "the PO number is on page 2",
		"desktops/old-box/recordings/r/x.png":  "png",
		"desktops/old-box/skills/deep/x.json":  "nested",
		"desktops/../skills/x.json":            "climb",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got["old-box"]) != 2 {
		t.Fatalf("got %v", got)
	}
	if string(got["old-box"]["memory/notes.txt"]) != "the PO number is on page 2" {
		t.Errorf("memory = %q", got["old-box"]["memory/notes.txt"])
	}
}

func TestReadBackupFoldersRefusesWhatIsNotABackup(t *testing.T) {
	if _, err := readBackupFolders(strings.NewReader("not gzip")); err == nil {
		t.Error("read plain text as a backup")
	}
	if _, err := readBackupFolders(backupBytes(t, map[string]string{"desktops/a/skills/x.json": "{}"})); err == nil {
		t.Error("read an archive with no manifest as a backup")
	}
}

// With no source named: the target's own, else the only one, else ask.
func TestTheSourceDesktopIsPickedOnlyWhenItIsClear(t *testing.T) {
	one := map[string]map[string][]byte{"old": {}}
	if id, err := onlyDesktop(one, "new"); err != nil || id != "old" {
		t.Errorf("one desktop: %q, %v", id, err)
	}

	two := map[string]map[string][]byte{"a": {}, "b": {}}
	if id, err := onlyDesktop(two, "b"); err != nil || id != "b" {
		t.Errorf("its own: %q, %v", id, err)
	}
	if _, err := onlyDesktop(two, "c"); err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Errorf("two others: %v", err)
	}
}
