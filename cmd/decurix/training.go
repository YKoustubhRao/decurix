// training.go is `decurix --export-training`: a thin client that asks a running control plane for its checked-done
// runs as a training set (GET /api/runs/training-export on the primary listener at --url, internal/sftexport) and
// writes the JSON lines to stdout; how many were written and what was skipped goes to stderr.

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// runExportTraining streams the export of the control plane at controlPlane to stdout and returns the exit code.
func runExportTraining(controlPlane, since, format string, stdout, stderr io.Writer) int {
	q := url.Values{"since": {since}, "format": {format}}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(controlPlane, "/")+"/api/runs/training-export?"+q.Encode(), nil)
	if err != nil {
		fmt.Fprintln(stderr, "export: bad --url")
		return 2
	}
	resp, err := (&http.Client{Timeout: 2 * time.Hour}).Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "export: no control plane answers at %s (it must be running; this talks to its own primary address)\n", controlPlane)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		fmt.Fprintf(stderr, "export: %s %s\n", resp.Status, strings.TrimSpace(string(raw)))
		return 1
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		fmt.Fprintln(stderr, "export: the export was cut short; what was written is whole lines only up to the cut")
		return 1
	}
	fmt.Fprintf(stderr, "export: %s runs written; skipped: %s\n", orNone(resp.Trailer.Get("X-Decurix-Exported")),
		orNone(resp.Trailer.Get("X-Decurix-Skipped")))
	return 0
}

// orNone is s, or "none" when it is empty.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}
