// purge.go is `decurix --purge-secret`: the terminal's way to purge a leaked secret, by value, from a running control
// plane's stores (internal/purge). Like --cli it is a thin client: it reads the value from stdin (never argv, which
// other users of the machine and the shell's history can read), posts it with the reason to POST /api/privacy/purge
// on the primary listener at --url, and prints the counts per store. The value is never printed.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/purge"
)

// runPurgeSecret reads one line from stdin as the value, purges it at controlPlane with reason, and prints the counts
// to stdout. It returns the process's exit code.
func runPurgeSecret(controlPlane, reason string, stdin io.Reader, stdout, stderr io.Writer) int {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		fmt.Fprintln(stderr, `usage: decurix --purge-secret --reason "why it is purged" < file-holding-the-value`)
		fmt.Fprintln(stderr, "The value is read from stdin, never from the command line.")
		return 2
	}
	fmt.Fprintln(stderr, "Paste the leaked value and press Enter (or pipe it in). It is sent to the control plane only.")
	line, err := bufio.NewReader(io.LimitReader(stdin, 32<<10)).ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintln(stderr, "purge: could not read the value from stdin")
		return 1
	}
	value := strings.TrimSpace(line)
	if err := purge.Check(value); err != nil {
		fmt.Fprintln(stderr, "purge: "+err.Error())
		return 2
	}
	body, _ := json.Marshal(map[string]string{"value": value, "reason": reason})
	value = ""
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(controlPlane, "/")+"/api/privacy/purge", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(stderr, "purge: bad --url")
		return 2
	}
	req.Header.Set("Content-Type", "application/json")
	// A purge rebuilds the database at the end: give it time.
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "purge: no control plane answers at %s (it must be running; this talks to its own primary address)\n", controlPlane)
		return 1
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		fmt.Fprintln(stderr, "purge: "+e.Error)
		return 1
	}
	var res purge.Result
	if err := json.Unmarshal(raw, &res); err != nil {
		fmt.Fprintln(stderr, "purge: the control plane's answer could not be read")
		return 1
	}
	c := res.Counts
	fmt.Fprintf(stdout, "Purged. Recorded as %s.\n", res.Judgement)
	for _, row := range []struct {
		label string
		n     int64
	}{{"stored bodies and files", c.Blobs}, {"model calls", c.LLMCalls}, {"judgements", c.Judgements},
		{"task records", c.TaskRecords}, {"chat", c.Chat}, {"queries", c.QueryNotes}, {"run bundles", c.RunBundles}} {
		fmt.Fprintf(stdout, "  %-24s %d\n", row.label, row.n)
	}
	if c.BlobsFailed > 0 {
		fmt.Fprintf(stdout, "  %-24s %d\n", "bodies left (in use)", c.BlobsFailed)
	}
	for _, p := range res.Problems {
		fmt.Fprintln(stdout, "  "+p)
	}
	return 0
}
