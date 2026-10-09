package httpapi

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestTheRealHostPicker opens the system's chooser for real, so it runs only
// when asked for: DECURIX_TEST_PICKER=1 (cancel the window it opens).
func TestTheRealHostPicker(t *testing.T) {
	if os.Getenv("DECURIX_TEST_PICKER") == "" {
		t.Skip("opens a window on this computer's screen; set DECURIX_TEST_PICKER=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	picked, err := pickHostPathsOS(ctx, os.Getenv("DECURIX_TEST_PICKER") == "folder", os.Getenv("DECURIX_TEST_PICKER") == "any")
	if err != nil {
		t.Fatalf("the chooser failed: %v", err)
	}
	t.Logf("picked: %q", picked)
}
