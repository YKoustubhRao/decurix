package httpapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// Restoring into a desktop takes a backup's (backup.go) skills and memory of
// one desktop - its own, or another's: a new desktop can take over what a
// deleted one was taught, which is how a worker's know-how outlives its VM.
// The control plane's own records are restored with `decurix
// --restore-backup`, with the control plane stopped.
//
//	POST /api/desktops/restore?id=<target>[&from=<desktop in the backup>]
//	body: the .tar.gz
//
// A skill or memory file the target already has is kept, never overwritten,
// and listed as such: what a desktop learned since the backup is newer than
// the backup. Recordings are not restored (a skill carries its own steps).

const (
	restoreMaxBody = 1 << 30  // a backup with recordings can be large
	restoreMaxFile = 16 << 20 // a skill or a memory file is small
	restoreMaxSum  = 256 << 20
)

// restoreParts maps a backup's folder to where it goes in a desktop.
var restoreParts = map[string]string{
	"skills":    "/home/headless/.decurix/skills",
	"memory":    "/home/headless/.decurix/memory",
	"playbooks": "/home/headless/.decurix/playbooks",
	"tools":     "/home/headless/.decurix/tools",
}

type restoreResult struct {
	From     string   `json:"from"`
	Restored []string `json:"restored"`
	Kept     []string `json:"kept_existing"`
}

// restoreDesktopHandler restores skills and memory into a desktop.
func (s *Server) restoreDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}

	target := r.URL.Query().Get("id")
	if target == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if _, err := s.Desktops.Get(r.Context(), target); err != nil {
		jsonError(w, err)
		return
	}

	byDesktop, err := readBackupFolders(http.MaxBytesReader(w, r.Body, restoreMaxBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	from := desktop.SanitizeID(r.URL.Query().Get("from"))
	if from == "" {
		from, err = onlyDesktop(byDesktop, desktop.SanitizeID(target))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	files, ok := byDesktop[from]
	if !ok {
		http.Error(w, fmt.Sprintf("the backup has no skills or memory of desktop %q", from), http.StatusBadRequest)
		return
	}

	res, err := s.restoreInto(r.Context(), target, from, files)
	if err != nil {
		jsonError(w, err)
		return
	}
	jsonResponse(w, res)
}

// restoreInto copies files ("skills/x.json" -> content) into the desktop,
// keeping any it already has.
func (s *Server) restoreInto(ctx context.Context, target, from string, files map[string][]byte) (restoreResult, error) {
	res := restoreResult{From: from, Restored: []string{}, Kept: []string{}}

	for part, dir := range restoreParts {
		have, err := s.namesIn(ctx, target, dir)
		if err != nil {
			return res, err
		}

		var put []desktop.File
		for rel, data := range files {
			folder, name := path.Split(rel)
			if folder != part+"/" {
				continue
			}
			if have[name] {
				res.Kept = append(res.Kept, rel)
				continue
			}
			put = append(put, desktop.File{Name: name, Data: data})
			res.Restored = append(res.Restored, rel)
		}
		if len(put) > 0 {
			if err := s.Desktops.CopyFiles(ctx, target, dir, put); err != nil {
				return res, err
			}
		}
	}

	sort.Strings(res.Restored)
	sort.Strings(res.Kept)
	return res, nil
}

// namesIn lists the file names directly inside a desktop folder; a folder
// that is not there yet has none.
func (s *Server) namesIn(ctx context.Context, id, dir string) (map[string]bool, error) {
	names := map[string]bool{}

	rc, err := s.Desktops.CopyOut(ctx, id, dir)
	if errors.Is(err, desktop.ErrNotFound) {
		return names, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		// Entries are "<folder>/<name>"; only direct children count.
		parts := strings.Split(strings.Trim(h.Name, "/"), "/")
		if len(parts) == 2 && h.Typeflag == tar.TypeReg {
			names[parts[1]] = true
		}
	}
}

// readBackupFolders reads a backup's desktops/<id>/{skills,memory}/<file>
// entries: desktop -> "skills/x.json" -> content.
func readBackupFolders(body io.Reader) (map[string]map[string][]byte, error) {
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, errors.New("that is not a Decurix backup (not a .tar.gz)")
	}
	// Everything is decompressed to be walked, also the entries skipped below: a small upload must not expand into
	// gigabytes of work (review 2026-11-02). A real backup's skills and memory are well inside this.
	tr := tar.NewReader(io.LimitReader(gz, 4*restoreMaxSum+restoreMaxBody))

	out := map[string]map[string][]byte{}
	var sum int64
	var manifest bool
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the backup could not be read: %v", err)
		}
		if h.Name == "manifest.json" {
			manifest = true
			continue
		}

		// desktops/<id>/<part>/<file>, flat below the part.
		p := strings.Split(h.Name, "/")
		if len(p) != 4 || p[0] != "desktops" || h.Typeflag != tar.TypeReg {
			continue
		}
		id, part, name := p[1], p[2], p[3]
		if _, ok := restoreParts[part]; !ok || name == "" || name == "." || name == ".." || id != desktop.SanitizeID(id) {
			continue
		}
		if h.Size > restoreMaxFile {
			return nil, fmt.Errorf("%s is %d MB, more than a skill or memory file can be", h.Name, h.Size>>20)
		}
		if sum += h.Size; sum > restoreMaxSum {
			return nil, fmt.Errorf("the backup's skills and memory are over %d MB", restoreMaxSum>>20)
		}

		data, err := io.ReadAll(io.LimitReader(tr, restoreMaxFile))
		if err != nil {
			return nil, fmt.Errorf("the backup could not be read: %v", err)
		}
		if out[id] == nil {
			out[id] = map[string][]byte{}
		}
		out[id][part+"/"+name] = data
	}

	if !manifest {
		return nil, errors.New("that is not a Decurix backup (no manifest.json)")
	}
	return out, nil
}

// onlyDesktop picks the source when none was named: the target's own, else
// the backup's one and only desktop.
func onlyDesktop(byDesktop map[string]map[string][]byte, target string) (string, error) {
	if _, ok := byDesktop[target]; ok {
		return target, nil
	}
	if len(byDesktop) == 1 {
		for id := range byDesktop {
			return id, nil
		}
	}

	var ids []string
	for id := range byDesktop {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "", errors.New("the backup holds no desktop's skills or memory")
	}
	return "", fmt.Errorf("the backup holds several desktops (%s): say which one with from=", strings.Join(ids, ", "))
}
