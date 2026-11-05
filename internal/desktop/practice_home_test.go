package desktop

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// The practice home reads and writes only a practice desktop: a worker's own desktop and a mock app are refused
// before any exec or copy is made (O-6).
func TestPracticeHomeRefusesAnythingButAPracticeDesktop(t *testing.T) {
	d := &execDaemon{t: t, labels: map[string]map[string]string{
		"worker": {"decurix-desktop": "true"},
		"mock":   {PracticeLabel: "true", "decurix-practice-mock": "true"},
	}}
	h := PracticeHome{M: execManager(t, d)}
	for _, id := range []string{"worker", "mock"} {
		if _, err := h.ReadTar(context.Background(), id, "/home/headless/.decurix/runs/t1", 0); !errors.Is(err, ErrNotPractice) {
			t.Errorf("read %s: %v, want ErrNotPractice", id, err)
		}
		if err := h.WriteTar(context.Background(), id, "/home/headless", bytes.NewReader(nil)); !errors.Is(err, ErrNotPractice) {
			t.Errorf("write %s: %v, want ErrNotPractice", id, err)
		}
	}
	if _, err := h.ReadTar(context.Background(), "gone", "/home/headless/x", 0); err == nil {
		t.Error("a container that does not exist was read")
	}
	if len(d.execs) != 0 {
		t.Fatalf("an exec was made in a refused container: %v", d.execs)
	}
}
