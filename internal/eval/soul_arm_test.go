package eval

import (
	"context"
	"testing"
)

// A +soul arm's practice desktop runs with soul's decisions on (desktop.PracticeSpec.SoulDecisions); the arm beside
// it without +soul runs without, and a calibration arm never has them.
func TestASoulArmTurnsOnSoulsDecisionsForItselfOnly(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"ask-first"},
		Arms: []Arm{{Name: "default+expert", Kind: ArmModel, Expert: true},
			{Name: "default+expert+soul", Kind: ArmModel, Expert: true, Soul: true},
			{Name: "oracle", Kind: ArmOracle, Soul: true}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if len(sb.specs) != 3 {
		t.Fatalf("%d practice desktops", len(sb.specs))
	}
	got := []bool{sb.specs[0].SoulDecisions, sb.specs[1].SoulDecisions, sb.specs[2].SoulDecisions}
	if got[0] || !got[1] || got[2] {
		t.Fatalf("soul decisions per arm (plain, +soul, oracle) = %v", got)
	}
}
