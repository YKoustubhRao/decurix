// Package qualification keeps independent readiness evidence and scoped grants
// in the control plane. Agent experience and self-reported checks confer no grant.
package qualification

import (
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Untested       = "untested"
	Testing        = "testing"
	Qualified      = "qualified"
	NeedsRetesting = "needs_retesting"
	Supervised     = "supervised"
	Enabled        = "enabled_within_scope"
	Suspended      = "suspended"
	RuntimeVersion = "offline-artifact-v1"
	MaxFileBytes   = 8 << 20
	MaxTotalBytes  = 16 << 20
	// OwnerKeyFile is withheld from backup, host reads and desktop file copies.
	OwnerKeyFile     = "qualification-owner.token"
	MaxEvidenceBytes = 64 << 20
)

var (
	ErrInvalid  = errors.New("qualification: invalid definition")
	ErrNotFound = errors.New("qualification: not found")
	ErrConflict = errors.New("qualification: operation conflicts with current state")
	ErrDenied   = errors.New("qualification: supervised execution required")
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)
	filePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_. -]{0,119}$`)
)

// Definition freezes an executable, environment and owner-reviewed test suite.
// Only the offline artifact route is supported; a grant never enables a desktop.
type Definition struct {
	Name           string `json:"name"`
	WorkerID       string `json:"worker_id"`
	SourceName     string `json:"source_name"`
	Environment    string `json:"environment"`
	Scope          string `json:"scope"`
	Language       string `json:"language"`
	Script         string `json:"script"`
	Image          string `json:"image"`
	Runtime        string `json:"runtime"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Cases          []Case `json:"cases"`
}

// Case holds inputs and hidden expected results. Expectations never enter Docker.
type Case struct {
	ID       string            `json:"id"`
	Category string            `json:"category"`
	Args     []string          `json:"args"`
	Inputs   map[string][]byte `json:"inputs"`
	Expected string            `json:"expected"` // complete, escalate (exit 20), approval (exit 21)
	Checks   []Check           `json:"checks"`
}

// Check compares saved artifact content in the independent Go verifier.
type Check struct {
	File  string          `json:"file"`
	Kind  string          `json:"kind"` // text_equals, json_equals, csv_equals, xlsx_cells
	Text  string          `json:"text,omitempty"`
	JSON  json.RawMessage `json:"json,omitempty"`
	Rows  [][]string      `json:"rows,omitempty"`
	Cells []CellCheck     `json:"cells,omitempty"`
}

// CellCheck names an exact saved worksheet cell; Formula optionally requires one.
type CellCheck struct {
	Sheet   string `json:"sheet"`
	Cell    string `json:"cell"`
	Value   string `json:"value"`
	Formula *bool  `json:"formula,omitempty"`
	// FormulaText pins the exact formula too, so a fabricated cache cannot
	// hide a formula that will calculate a different answer when opened.
	FormulaText string `json:"formula_text,omitempty"`
}

// Responsibility separates demonstrated readiness from the owner's authorization.
type Responsibility struct {
	ID            string     `json:"id"`
	Definition    Definition `json:"definition"`
	Revision      string     `json:"revision"`
	Readiness     string     `json:"readiness"`
	Authorization string     `json:"authorization"`
	Reason        string     `json:"reason"`
	EvaluationID  string     `json:"evaluation_id,omitempty"`
	ActiveRunID   string     `json:"active_run_id,omitempty"`
	QualifiedAt   time.Time  `json:"qualified_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Result is supplied by the Docker adapter, never by an agent's callback.
type Result struct {
	ExitCode   int               `json:"exit_code"`
	Output     string            `json:"output"`
	Artifacts  map[string][]byte `json:"artifacts"`
	DurationMS int64             `json:"duration_ms"`
}

// Finding records the independent decision and artifact hashes used to make it.
type Finding struct {
	CaseID     string            `json:"case_id"`
	Category   string            `json:"category"`
	Expected   string            `json:"expected"`
	Observed   string            `json:"observed"`
	Passed     bool              `json:"passed"`
	Problems   []string          `json:"problems"`
	Hashes     map[string]string `json:"hashes"`
	ExitCode   int               `json:"exit_code"`
	DurationMS int64             `json:"duration_ms"`
	Output     string            `json:"output"`
}

// Run is immutable evidence once finished; practice is never production autonomy.
type Run struct {
	ID               string     `json:"id"`
	ResponsibilityID string     `json:"responsibility_id"`
	TaskID           string     `json:"task_id"`
	Mode             string     `json:"mode"` // practice or production
	Revision         string     `json:"revision"`
	Definition       Definition `json:"definition"`
	// ProductionCase freezes the real inputs and independent check contract.
	ProductionCase *Case             `json:"production_case,omitempty"`
	Status         string            `json:"status"`
	Findings       []Finding         `json:"findings"`
	Artifacts      map[string][]byte `json:"artifacts,omitempty"`
	// CaseArtifacts is only what a run begun before qualification_files kept in its JSON; a case's files are stored
	// in rows now (Store.CaseArtifact). CaseFiles names them by case, filled on read for the evidence page.
	CaseArtifacts map[string]map[string][]byte `json:"case_artifacts,omitempty"`
	CaseFiles     map[string][]string          `json:"case_files,omitempty"`
	StartedAt     time.Time                    `json:"started_at"`
	EndedAt       time.Time                    `json:"ended_at,omitempty"`
}

// Fingerprint includes every executable, environment, scope and suite input.
func Fingerprint(d Definition) string {
	b, _ := json.Marshal(d)
	return SHA256Hex(b)
}

// FileName permits only flat regular artifacts, never paths or reserved programs.
func FileName(s string) bool {
	return filePattern.MatchString(s) && s != "." && !strings.HasPrefix(s, ".") &&
		path.Base(s) == s && !strings.ContainsAny(s, "\\:/\x00\r\n") && s != "main.py" && s != "main.sh"
}

// ValidateCase bounds inputs and requires explicit evidence for successful work.
func ValidateCase(c Case) error {
	if !namePattern.MatchString(c.ID) || len(c.Args) > 50 || len(c.Inputs) > 20 || len(c.Checks) > 30 {
		return ErrInvalid
	}
	total := 0
	for _, a := range c.Args {
		if len(a) > 4096 || strings.ContainsRune(a, 0) {
			return ErrInvalid
		}
	}
	for n, b := range c.Inputs {
		total += len(b)
		if !FileName(n) || len(b) > MaxFileBytes {
			return ErrInvalid
		}
	}
	if total > MaxTotalBytes {
		return ErrInvalid
	}
	if c.Expected != "complete" && c.Expected != "escalate" && c.Expected != "approval" {
		return ErrInvalid
	}
	if c.Expected == "complete" && len(c.Checks) == 0 {
		return ErrInvalid
	}
	if c.Expected != "complete" && len(c.Checks) != 0 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, check := range c.Checks {
		_, inputExists := c.Inputs[check.File]
		if !FileName(check.File) || seen[check.File] || inputExists {
			return ErrInvalid
		}
		seen[check.File] = true
		switch check.Kind {
		case "text_equals":
			if len(check.Text) > MaxFileBytes {
				return ErrInvalid
			}
		case "json_equals":
			if _, err := decodeJSON(check.JSON); err != nil {
				return ErrInvalid
			}
		case "csv_equals":
			if len(check.Rows) == 0 || len(check.Rows) > 10000 {
				return ErrInvalid
			}
		case "xlsx_cells":
			if len(check.Cells) == 0 || len(check.Cells) > 2000 {
				return ErrInvalid
			}
			for _, cell := range check.Cells {
				if cell.Sheet == "" || !cellPattern.MatchString(cell.Cell) {
					return ErrInvalid
				}
				if cell.Formula != nil && *cell.Formula && cell.FormulaText == "" {
					return ErrInvalid
				}
				if cell.Formula != nil && !*cell.Formula && cell.FormulaText != "" {
					return ErrInvalid
				}
			}
		default:
			return ErrInvalid
		}
	}
	return nil
}

// Validate requires varied and adversarial holdouts before a suite can qualify.
func Validate(d Definition) error {
	if strings.TrimSpace(d.Name) == "" || len(d.Name) > 120 || !namePattern.MatchString(d.WorkerID) ||
		!namePattern.MatchString(d.SourceName) || strings.TrimSpace(d.Scope) == "" || len(d.Scope) > 2000 ||
		strings.TrimSpace(d.Environment) == "" || len(d.Environment) > 200 ||
		(d.Language != "python" && d.Language != "bash") || len(d.Script) == 0 || len(d.Script) > 128<<10 || !utf8.ValidString(d.Script) ||
		!regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(d.Image) || d.Runtime != RuntimeVersion ||
		d.TimeoutSeconds < 1 || d.TimeoutSeconds > 300 || len(d.Cases) < 5 || len(d.Cases) > 30 {
		return ErrInvalid
	}
	ids, categories := map[string]bool{}, map[string]bool{}
	suiteBytes := 0
	for _, c := range d.Cases {
		if ValidateCase(c) != nil || ids[c.ID] {
			return ErrInvalid
		}
		ids[c.ID], categories[c.Category] = true, true
		encoded, _ := json.Marshal(c)
		suiteBytes += len(encoded)
		if suiteBytes > 20<<20 {
			return ErrInvalid
		}
		if (c.Category == "ordinary" || c.Category == "varied" || c.Category == "injection") && c.Expected != "complete" {
			return ErrInvalid
		}
		if (c.Category == "missing_input" || c.Category == "invalid_input") && c.Expected != "escalate" {
			return ErrInvalid
		}
	}
	for _, category := range []string{"ordinary", "varied", "missing_input", "invalid_input", "injection"} {
		if !categories[category] {
			return ErrInvalid
		}
	}
	return nil
}
