// Package speed keeps what Brainiac measured about how fast each installed
// model runs on this machine: how fast it reads a prompt (prefill), short
// and long, how fast it writes (decode), what a turn costs when the prompt's
// start is cached and when it is not, which thread count was best, and
// whether the speed held up when the machine warmed. The selector and the
// router use these numbers instead of estimates once they exist.
//
// A measurement belongs to the exact model file, llama.cpp build and
// settings it was taken with; any change makes it stale (Find skips it).
//
// Depends on nothing internal.
package speed

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// File is the measurements' file under Brainiac's home.
const File = "speed.json"

// Result is one model's measurement.
type Result struct {
	Model  string `json:"model"`  // catalog id
	Source string `json:"source"` // the catalog source's label
	// File and Build are the model file's and the server program's size and
	// time (a replaced file is a new one); Settings is everything else that
	// changes speed: window, threads, KV type, speculative decoding.
	File     string    `json:"file"`
	Build    string    `json:"build"`
	Settings string    `json:"settings"`
	Threads  int       `json:"threads"`
	OnGPU    bool      `json:"on_gpu,omitempty"`
	Power    string    `json:"power"` // ac | battery | unknown
	At       time.Time `json:"at"`

	// PrefillShortTPS and PrefillLongTPS are prompt tokens read per second
	// for a short prompt and for LongTokens of one (attention costs more as
	// the context grows).
	PrefillShortTPS float64 `json:"prefill_short_tps"`
	PrefillLongTPS  float64 `json:"prefill_long_tps"`
	LongTokens      int     `json:"long_tokens"`
	// DecodeTPS and DecodeLongTPS are answer tokens per second after the
	// short and the long prompt.
	DecodeTPS     float64 `json:"decode_tps"`
	DecodeLongTPS float64 `json:"decode_long_tps"`
	// ColdTurnSec is the long prompt read from its start plus a short
	// answer; WarmTurnSec is the same prompt again with a new question at
	// its end, its start taken from the prompt cache (WarmCachedShare of it).
	ColdTurnSec     float64 `json:"cold_turn_sec"`
	WarmTurnSec     float64 `json:"warm_turn_sec"`
	WarmCachedShare float64 `json:"warm_cached_share"`
	// Sustained is the short prefill speed measured again at the end over
	// the first time: under 1 means the machine slowed as it warmed.
	Sustained float64 `json:"sustained"`
	// ThreadsTried are the thread counts compared and the turn time each
	// gave (a short prompt plus a short answer), when more than one was.
	ThreadsTried map[int]float64 `json:"threads_tried,omitempty"`
}

// Table is every measurement kept.
type Table struct {
	Results []Result `json:"results"`
}

// Load reads the table; a missing file is an empty table.
func Load(path string) (*Table, error) {
	t := &Table{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, t); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return t, nil
}

// Put adds a measurement, replacing the earlier one for the same model,
// source and file.
func (t *Table) Put(r Result) {
	out := t.Results[:0]
	for _, old := range t.Results {
		if old.Model != r.Model || old.Source != r.Source || old.File != r.File {
			out = append(out, old)
		}
	}
	t.Results = append(out, r)
	sort.Slice(t.Results, func(i, j int) bool {
		if t.Results[i].Model != t.Results[j].Model {
			return t.Results[i].Model < t.Results[j].Model
		}
		return t.Results[i].At.Before(t.Results[j].At)
	})
}

// Find is the newest measurement of a model's source; file, when not
// empty, must match (a measurement of a replaced file is stale).
func (t *Table) Find(model, source, file string) (Result, bool) {
	var best Result
	ok := false
	if t == nil {
		return best, false
	}
	for _, r := range t.Results {
		if r.Model != model || r.Source != source || (file != "" && r.File != file) {
			continue
		}
		if !ok || r.At.After(best.At) {
			best, ok = r, true
		}
	}
	return best, ok
}

// TurnSec estimates a turn's seconds from a measurement: newTokens of prompt
// to read (what the cache does not hold) and outTokens to write.
func (r Result) TurnSec(newTokens, outTokens int) float64 {
	pre := r.PrefillLongTPS
	if newTokens < r.LongTokens/2 || pre <= 0 {
		pre = r.PrefillShortTPS
	}
	dec := r.DecodeTPS
	if newTokens > r.LongTokens/2 && r.DecodeLongTPS > 0 {
		dec = r.DecodeLongTPS
	}
	if pre <= 0 || dec <= 0 {
		return 0
	}
	return float64(newTokens)/pre + float64(outTokens)/dec
}

// FileSig is a file's size and modification time ("" when it has none): a
// file replaced in place is a new file to measure.
func FileSig(path string) string {
	if path == "" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d@%d", fi.Size(), fi.ModTime().UnixNano())
}

// Power is whether this machine runs on mains or battery now ("unknown"
// where it cannot tell): a laptop on battery runs slower, so a measurement
// says which it was.
func Power() string {
	dir := "/sys/class/power_supply"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "unknown"
	}
	sawMains := false
	for _, e := range entries {
		kind, _ := os.ReadFile(filepath.Join(dir, e.Name(), "type"))
		if strings.TrimSpace(string(kind)) != "Mains" {
			continue
		}
		sawMains = true
		if on, _ := os.ReadFile(filepath.Join(dir, e.Name(), "online")); strings.TrimSpace(string(on)) == "1" {
			return "ac"
		}
	}
	if sawMains {
		return "battery"
	}
	return "unknown"
}
