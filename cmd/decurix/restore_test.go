package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// nobody is an address where no control plane answers.
const nobody = "http://127.0.0.1:1"

func writeArchive(t *testing.T, files map[string]string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "backup.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(body)), Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The backup's files take the old ones' places, which are kept - WAL too.
func TestRestorePutsTheBackupInPlaceAndKeepsWhatItReplaced(t *testing.T) {
	data := t.TempDir()
	db := filepath.Join(data, sqlitedb.FileName)
	os.WriteFile(db, []byte("old db"), 0o600)
	os.WriteFile(db+"-wal", []byte("old wal"), 0o600)
	os.WriteFile(filepath.Join(data, "llm.key"), []byte("old key"), 0o600)

	backup := writeArchive(t, map[string]string{
		"manifest.json":              `{"version": 1}`,
		"data/" + sqlitedb.FileName:  "new db",
		"data/llm.key":               "new key",
		"desktops/box/skills/x.json": "{}",
	})

	if code := runRestore(backup, data, nobody, false); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := read(t, db); got != "new db" {
		t.Errorf("database = %q", got)
	}
	if got := read(t, filepath.Join(data, "llm.key")); got != "new key" {
		t.Errorf("key = %q", got)
	}
	if _, err := os.Stat(db + "-wal"); !os.IsNotExist(err) {
		t.Error("the old WAL was left beside the restored database")
	}

	aside, _ := filepath.Glob(filepath.Join(data, "before-restore-*"))
	if len(aside) != 1 {
		t.Fatalf("set-aside folders: %v", aside)
	}
	for name, want := range map[string]string{sqlitedb.FileName: "old db", sqlitedb.FileName + "-wal": "old wal", "llm.key": "old key"} {
		if got := read(t, filepath.Join(aside[0], name)); got != want {
			t.Errorf("kept %s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "x.json")); !os.IsNotExist(err) {
		t.Error("a desktop's file was written into the data dir")
	}
}

// What is not a backup, or cannot be restored safely, changes nothing.
func TestRestoreRefusesWhatIsNotABackup(t *testing.T) {
	cases := map[string]map[string]string{
		"no manifest": {"data/" + sqlitedb.FileName: "x"},
		"no database": {"manifest.json": `{"version": 1}`, "data/llm.key": "k"},
		"escapes":     {"manifest.json": `{"version": 1}`, "data/" + sqlitedb.FileName: "x", "data/../../etc/evil": "x"},
		"future":      {"manifest.json": `{"version": 2}`, "data/" + sqlitedb.FileName: "x"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			os.WriteFile(filepath.Join(data, sqlitedb.FileName), []byte("old db"), 0o600)

			if code := runRestore(writeArchive(t, files), data, nobody, false); code == 0 {
				t.Fatal("restored")
			}
			if got := read(t, filepath.Join(data, sqlitedb.FileName)); got != "old db" {
				t.Errorf("database changed to %q", got)
			}
		})
	}
}
