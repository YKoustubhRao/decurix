// Tests for turn times: estimated before anything is measured, then taken
// from brainiac bench's measurements of the installed file.
package selector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/speed"
	"brainiac/internal/store"
)

func TestTurnTimesAreEstimatedThenMeasured(t *testing.T) {
	st := emptyStore(t)
	p := Select(thisLaptop(), cat(t), st, Policy{HaveProgram: noPrograms})
	brain, _ := p.Pick(catalog.RoleBrain)
	if brain.SpeedBasis != "estimated" || brain.WarmTurnSec <= 0 || brain.ColdTurnSec <= brain.WarmTurnSec {
		t.Fatalf("estimate: basis %q warm %.0f s cold %.0f s", brain.SpeedBasis, brain.WarmTurnSec, brain.ColdTurnSec)
	}
	// Install the brain's files (stand-ins) and measure them.
	installFiles(t, st, brain.Source)
	model := st.SourceFiles(brain.Source)["model"]
	tab := &speed.Table{}
	tab.Put(speed.Result{Model: brain.Model, Source: brain.Source.Label(), File: speed.FileSig(model), At: time.Now(),
		PrefillShortTPS: 60, PrefillLongTPS: 45, LongTokens: 6000, DecodeTPS: 9, DecodeLongTPS: 7})
	if err := st.WriteJSON(speed.File, tab); err != nil {
		t.Fatal(err)
	}
	p = Select(thisLaptop(), cat(t), st, Policy{HaveProgram: noPrograms})
	brain, _ = p.Pick(catalog.RoleBrain)
	if brain.SpeedBasis != "measured" || brain.Speed != 9 || brain.PrefillTPS != 60 {
		t.Fatalf("measured: %+v\n%s", brain, render(p))
	}
	if want := 1000.0/60 + 200.0/9; brain.WarmTurnSec < want-0.1 || brain.WarmTurnSec > want+0.1 {
		t.Fatalf("warm turn %.1f s, want %.1f", brain.WarmTurnSec, want)
	}
	if want := 6000.0/45 + 200.0/7; brain.ColdTurnSec < want-0.1 || brain.ColdTurnSec > want+0.1 {
		t.Fatalf("cold turn %.1f s, want %.1f", brain.ColdTurnSec, want)
	}
	if !strings.HasPrefix(p.DecodeBasis, "measured here") {
		t.Fatalf("decode basis %q", p.DecodeBasis)
	}
	// A replaced file is a new one: its old measurement no longer counts.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(model, later, later); err != nil {
		t.Fatal(err)
	}
	p = Select(thisLaptop(), cat(t), st, Policy{HaveProgram: noPrograms})
	if b, _ := p.Pick(catalog.RoleBrain); b.SpeedBasis != "estimated" {
		t.Fatalf("a stale measurement was used: %+v", b)
	}
}

// installFiles puts a non-empty stand-in at every file a source needs.
func installFiles(t *testing.T, st *store.Store, src catalog.Source) {
	t.Helper()
	for _, path := range st.SourceFiles(src) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not a real model"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
