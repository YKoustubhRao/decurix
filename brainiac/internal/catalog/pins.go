package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// IsCommit reports whether s is a full 40-hex commit id.
func IsCommit(s string) bool { return commitRe.MatchString(s) }

// IsSHA256 reports whether s is a 64-hex sha256 in lower case.
func IsSHA256(s string) bool { return sha256Re.MatchString(s) }

// Unpinned says what a source lacks before Brainiac may download it, or ""
// when nothing is missing. A Hugging Face source needs its commit and the
// sha256 of every file; a GitHub asset its sha256. The pins are what makes
// a catalog worth signing: a name and a tag say where, only a hash says
// what. Ollama tags, pip groups and system programs are fetched by other
// programs (Ollama, pip, the system's package manager) and have no pin here.
func (s Source) Unpinned() string {
	var missing []string
	switch s.Kind {
	case SourceHF:
		if !IsCommit(s.Revision) {
			missing = append(missing, "no revision (a 40-hex commit)")
		}
		for _, f := range s.Files {
			if !IsSHA256(f.SHA256) {
				missing = append(missing, "no sha256 for "+f.Path)
			}
		}
	case SourceGitHub:
		if !IsSHA256(s.SHA256) {
			missing = append(missing, "no sha256 for "+s.Asset)
		}
	}
	return strings.Join(missing, "; ")
}

// UnpinnedAsset says what the llama.cpp asset named lacks, or "".
func (r Runtime) UnpinnedAsset(name string) string {
	if IsSHA256(r.LlamaCppSHA256[name]) {
		return ""
	}
	return fmt.Sprintf("llama.cpp %s: no sha256 for %s", r.LlamaCppTag, name)
}

// Unpinned lists every model source the catalog cannot install because a
// pin is missing, one line each ("model (source): what is missing").
func (c *Catalog) Unpinned() []string {
	var out []string
	for _, m := range c.Models {
		for _, s := range m.Sources {
			if why := s.Unpinned(); why != "" {
				out = append(out, fmt.Sprintf("%s (%s): %s", m.ID, s.Label(), why))
			}
		}
	}
	sort.Strings(out)
	return out
}
