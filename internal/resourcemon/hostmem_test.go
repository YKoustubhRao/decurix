package resourcemon

import (
	"context"
	"errors"
	"testing"
)

const sampleMeminfo = `MemTotal:        5048576 kB
MemFree:          812344 kB
MemAvailable:    1536000 kB
Buffers:           10240 kB
`

// TestParseMeminfoReadsTotalAndAvailable reads both in MB, and says when either is missing.
func TestParseMeminfoReadsTotalAndAvailable(t *testing.T) {
	total, avail, ok := ParseMeminfo([]byte(sampleMeminfo))
	if !ok || total != 4930 || avail != 1500 {
		t.Fatalf("got %d %d %v, want 4930 1500 true", total, avail, ok)
	}
	if _, _, ok := ParseMeminfo([]byte("MemTotal: 100 kB\n")); ok {
		t.Fatal("no MemAvailable read as known")
	}
}

// TestHostMemoryIsTrustedOnlyOnTheDockerHost: the local reading counts when its total is the daemon's, and is
// "not known" when it is another machine's (a control plane not on the Docker host).
func TestHostMemoryIsTrustedOnlyOnTheDockerHost(t *testing.T) {
	read := func() ([]byte, error) { return []byte(sampleMeminfo), nil }
	same := &HostMemory{read: read, DockerTotalMB: func(context.Context) (int64, error) { return 4900, nil }}
	if avail, ok := same.AvailableMB(context.Background()); !ok || avail != 1500 {
		t.Fatalf("on the Docker host: %d %v", avail, ok)
	}
	other := &HostMemory{read: read, DockerTotalMB: func(context.Context) (int64, error) { return 16000, nil }}
	if _, ok := other.AvailableMB(context.Background()); ok {
		t.Fatal("another machine's memory was taken for the Docker host's")
	}
}

// TestHostMemoryAsksDockerAgainUntilItAnswers: a daemon that is not answering yet leaves the verdict open.
func TestHostMemoryAsksDockerAgainUntilItAnswers(t *testing.T) {
	up := false
	h := &HostMemory{read: func() ([]byte, error) { return []byte(sampleMeminfo), nil },
		DockerTotalMB: func(context.Context) (int64, error) {
			if !up {
				return 0, errors.New("docker is starting")
			}
			return 4930, nil
		}}
	if _, ok := h.AvailableMB(context.Background()); ok {
		t.Fatal("known before Docker answered")
	}
	up = true
	if _, ok := h.AvailableMB(context.Background()); !ok {
		t.Fatal("not known once Docker answered")
	}
}

// TestHostMemoryWithoutProcIsNotKnown: a machine with no /proc/meminfo (macOS, Windows) knows nothing.
func TestHostMemoryWithoutProcIsNotKnown(t *testing.T) {
	h := &HostMemory{read: func() ([]byte, error) { return nil, errors.New("no such file") },
		DockerTotalMB: func(context.Context) (int64, error) { return 4930, nil }}
	if _, ok := h.AvailableMB(context.Background()); ok {
		t.Fatal("known without /proc/meminfo")
	}
}
