package runs

import (
	"context"
	"io"
	"time"
)

// Home is the as-worker file access desktop.Manager already has (homefs.go): paths are absolute, under the worker's
// home.
type Home interface {
	ReadTar(ctx context.Context, id, p string, limit int64) (io.ReadCloser, error)
	WriteTar(ctx context.Context, id, dir string, archive io.Reader) error
}

// DefaultLimit bounds what one run's bundle may hold when Puller.Limit is unset.
const DefaultLimit = 64 << 20

// Puller reads a run's files out of the worker's home and saves them as its bundle.
type Puller struct {
	Home  Home
	Store *Store
	Blobs Blobs
	// Limit bounds the bytes kept for one run, across all its parts (DefaultLimit when 0).
	Limit int64
}

// Pull reads ref's run off its desktop, stores every file once as a blob, saves the bundle and marks the run
// EXPORTED on the desktop.
func (p *Puller) Pull(ctx context.Context, ref ExecutionRef) (Bundle, error) {
	return Bundle{}, errNotYet
}

// FlushDesktop pulls each of refs in turn until timeout, and returns the execution ids it did not pull.
func (p *Puller) FlushDesktop(ctx context.Context, worker string, refs []ExecutionRef, timeout time.Duration) (notPulled []string) {
	for _, r := range refs {
		notPulled = append(notPulled, r.ExecID)
	}
	return notPulled
}
