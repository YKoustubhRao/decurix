package desktop

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A base image the build pulls is shown as it comes: each layer's own events, and the bytes come so far while they
// keep coming, at most every pullProgressEvery; a pull that adds no bytes prints nothing more, so the app's stall
// watchdog sees it stop. Build output and errors are as before.
func TestABuildsPullIsShownWhileItMoves(t *testing.T) {
	// The clock moves 31 s each time it is read: once at the start, then once per progress message.
	at := time.Unix(0, 0)
	savedClock, savedEvery := buildClock, pullProgressEvery
	buildClock = func() time.Time { now := at; at = at.Add(31 * time.Second); return now }
	pullProgressEvery = 30 * time.Second
	t.Cleanup(func() { buildClock, pullProgressEvery = savedClock, savedEvery })

	stream := strings.Join([]string{
		`{"status":"Pulling from library/ubuntu","id":"24.04"}`,
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":1048576,"total":104857600}}`,
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":52428800,"total":104857600}}`,
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":52428800,"total":104857600}}`,
		`{"status":"Download complete","id":"aaa"}`,
		`{"stream":"Step 2/9 : RUN true\n"}`,
		`{"error":"boom","errorDetail":{"message":"the build failed"}}`,
	}, "\n")
	var out bytes.Buffer
	if err := streamBuildOutput(&out, strings.NewReader(stream)); err == nil || err.Error() != "the build failed" {
		t.Fatalf("error %v", err)
	}
	want := "24.04: Pulling from library/ubuntu\n" +
		"Pulling the base image: 1 of 100 MB\n" +
		"Pulling the base image: 50 of 100 MB\n" +
		"aaa: Download complete\n" +
		"Step 2/9 : RUN true\n"
	if got := out.String(); got != want {
		t.Fatalf("output %q, want %q", got, want)
	}

	// Within pullProgressEvery, bytes alone print nothing.
	at = time.Unix(0, 0)
	buildClock = func() time.Time { return at }
	out.Reset()
	if err := streamBuildOutput(&out, strings.NewReader(`{"status":"Downloading","id":"aaa","progressDetail":{"current":5,"total":10}}`)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("output %q", out.String())
	}
}
