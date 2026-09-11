package workspace

import (
	"os"
	"path/filepath"
)

// FileState is the (size, mtime) fingerprint used to detect whether a
// file is new or changed - the same fingerprint lib/ingestor.js's Node
// prototype used, ported here for the scheduler's real-time P0 ingestion
// trigger (see internal/scheduler: every subtask's real file effects are
// diffed against a pre-run snapshot).
type FileState struct {
	Size    int64
	ModTime int64
}

var ignoreDirs = map[string]bool{
	".git": true, ".rag": true, ".feedback": true, ".memory": true, "node_modules": true, "__pycache__": true,
	".venv": true, "venv": true, "env": true, ".env": true, ".tox": true,
}

// Snapshot walks every real file under the workspace root right now -
// ground truth from disk, not a cached listing.
//
// A live run created a Python venv named "myenv" (an agent-chosen name,
// not any fixed convention) to install real dependencies, and every file
// under it - including the venv's own compiled python3 binary, 3 times
// over via its symlinks - got auto-ingested into the Data RAG as "text":
// a multi-megabyte ELF executable read raw and chunked into 6133 pieces,
// three times, each chunk needing its own real embedding call. A
// name-based skip list can never keep up with whatever the model decides
// to call a venv, so this also detects one structurally: any directory
// containing pyvenv.cfg IS a venv by definition (that marker file is
// exactly how `pip`/`venv` itself identify one), whatever it's named.
func (w *Workspace) Snapshot() (map[string]FileState, error) {
	out := map[string]FileState{}
	err := filepath.WalkDir(w.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a vanished file mid-walk isn't a hard failure
		}
		if d.IsDir() {
			if ignoreDirs[d.Name()] {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(path, "pyvenv.cfg")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(w.root, path)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = FileState{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		return nil
	})
	return out, err
}

// DiffNewOrChanged returns every relPath present in `after` that's either
// missing from `before` or has a different (size, mtime) - real files
// that were genuinely created or modified between two snapshots.
func DiffNewOrChanged(before, after map[string]FileState) []string {
	var changed []string
	for relPath, state := range after {
		prev, existed := before[relPath]
		if !existed || prev != state {
			changed = append(changed, relPath)
		}
	}
	return changed
}
