package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// runRestore puts a backup's data files (the database and the key files that
// open its sealed secrets - httpapi/backup.go) back into the data dir. It is
// the control plane's own records only: a desktop's skills and memory go back
// into a desktop from the dashboard (POST /api/desktops/restore), since a
// desktop is a container this command does not start.
//
// It never deletes: every file it replaces is moved into
// before-restore-<time>/ beside it. It refuses while a control plane answers
// on this machine, because one running would keep writing the database it
// replaces; force skips that check (a control plane in a container this one
// cannot see is the caller's to stop).
func runRestore(file, dataDir, controlPlane string, force bool) int {
	if file == "" {
		fmt.Fprintln(os.Stderr, "usage: decurix --restore-backup decurix-backup-YYYYMMDD-HHMM.tar.gz")
		return 2
	}

	if !force && answers(controlPlane) {
		fmt.Fprintf(os.Stderr, "A control plane is running at %s. Stop it first (docker compose stop, or end the decurix process), then restore.\n", controlPlane)
		return 1
	}

	stage, err := os.MkdirTemp(dataDir, ".restore-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}
	defer os.RemoveAll(stage)

	files, err := unpackData(file, stage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}

	aside := filepath.Join(dataDir, "before-restore-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(aside, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}

	// The live database's WAL and shared-memory files belong to it, not to
	// the snapshot: left in place, SQLite would replay the old WAL into the
	// restored database.
	replaced := append([]string{sqlitedb.FileName + "-wal", sqlitedb.FileName + "-shm"}, files...)
	for _, name := range replaced {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			if err := os.Rename(filepath.Join(dataDir, name), filepath.Join(aside, name)); err != nil {
				fmt.Fprintf(os.Stderr, "restore: set aside %s: %v\n", name, err)
				return 1
			}
		}
	}

	for _, name := range files {
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(dataDir, name)); err != nil {
			fmt.Fprintf(os.Stderr, "restore: put %s in place: %v (the files it replaced are in %s)\n", name, err, aside)
			return 1
		}
	}

	fmt.Printf("Restored %d file(s) into %s: %s\n", len(files), dataDir, strings.Join(files, ", "))
	fmt.Printf("What they replaced is in %s.\n", aside)
	fmt.Println("Start the control plane again. Skills and memory go back into a desktop from its card: Restore from a backup.")
	return 0
}

// unpackData checks the archive is a Decurix backup and writes its data/
// files, flat, into dir. It returns their names.
func unpackData(file, dir string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s is not a Decurix backup (not gzip): %w", file, err)
	}
	tr := tar.NewReader(gz)

	var names []string
	var manifest bool
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}

		if h.Name == "manifest.json" {
			var m struct {
				Version int `json:"version"`
			}
			if err := json.NewDecoder(tr).Decode(&m); err != nil || m.Version != 1 {
				return nil, fmt.Errorf("%s has a manifest this version cannot read", file)
			}
			manifest = true
			continue
		}

		// A backup this code wrote never climbs out of its folders; one that
		// does was made or changed by something else, and is not restored.
		for _, part := range strings.Split(h.Name, "/") {
			if part == ".." {
				return nil, fmt.Errorf("%s holds %q, which climbs out of the archive - not restored", file, h.Name)
			}
		}

		dirName, name := path.Split(path.Clean(h.Name))
		if dirName != "data/" || h.Typeflag != tar.TypeReg {
			continue
		}
		// Flat names only: nothing in a backup may write outside the data dir.
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			return nil, fmt.Errorf("%s holds a file named %q, which is not restored", file, h.Name)
		}

		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("unpack %s: %w", name, err)
		}
		names = append(names, name)
	}

	if !manifest {
		return nil, fmt.Errorf("%s is not a Decurix backup (no manifest.json)", file)
	}
	for _, n := range names {
		if n == sqlitedb.FileName {
			return names, nil
		}
	}
	return nil, fmt.Errorf("%s holds no database (data/%s)", file, sqlitedb.FileName)
}

// answers reports whether a control plane responds at url.
func answers(url string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(strings.TrimRight(url, "/") + "/api/model")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
