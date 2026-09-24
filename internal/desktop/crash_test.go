package desktop

import "testing"

// A desktop Stop ended (SIGTERM, or SIGKILL after the timeout) is stopped,
// not crashed; anything else that ended it on its own is a crash.
func TestCrashReason(t *testing.T) {
	oom := func() bool { return true }
	notOOM := func() bool { return false }

	cases := []struct {
		state, status string
		oom           func() bool
		want          string
	}{
		{"running", "Up 5 minutes", nil, ""},
		{"paused", "Up 5 minutes (Paused)", nil, ""},
		{"created", "Created", nil, ""},
		{"exited", "Exited (0) 2 minutes ago", nil, ""},
		{"exited", "Exited (143) 2 minutes ago", nil, ""},
		{"exited", "Exited (137) 2 minutes ago", notOOM, ""},
		{"exited", "Exited (137) 2 minutes ago", oom, "it ran out of memory"},
		{"exited", "Exited (1) 2 minutes ago", nil, "it exited with code 1"},
		{"exited", "Exited (139) About an hour ago", nil, "it exited with code 139"},
		{"dead", "Dead", nil, "Docker could not stop it cleanly"},
		{"restarting", "Restarting (1) 3 seconds ago", nil, "it keeps restarting"},
		{"exited", "something unexpected", nil, ""},
	}

	for _, c := range cases {
		if got := crashReason(c.state, c.status, c.oom); got != c.want {
			t.Errorf("crashReason(%q, %q) = %q, want %q", c.state, c.status, got, c.want)
		}
	}
}

// The OOM question costs an inspect, so it is only asked of a 137.
func TestCrashReasonOnlyInspectsA137(t *testing.T) {
	asked := false
	crashReason("exited", "Exited (1) 1 minute ago", func() bool { asked = true; return false })
	crashReason("exited", "Exited (0) 1 minute ago", func() bool { asked = true; return false })
	if asked {
		t.Fatal("inspected a container whose exit code already answers it")
	}
}
