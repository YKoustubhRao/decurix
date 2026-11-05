package practice

import (
	"context"
	"errors"
	"testing"
)

func TestSimulateRefusesWhileAnEvalHoldsTheSlot(t *testing.T) {
	s, _, _, _, _ := service(t)
	release, err := s.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(); !errors.Is(err, ErrBusy) {
		t.Fatalf("the slot was taken twice: %v", err)
	}
	if _, err := s.StartRun(context.Background(), "any-suite", "sha"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Simulate started during an eval: %v", err)
	}
	release()
	release()
	again, err := s.Reserve()
	if err != nil {
		t.Fatalf("the slot was not given back: %v", err)
	}
	again()
}
