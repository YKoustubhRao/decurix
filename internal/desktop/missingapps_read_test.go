package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// The missing-apps file is read back as the worker, through the home copy layer, never by Docker as root: its name
// sits in a folder the worker writes (REVIEW-FINAL).
func TestMissingAppsAreReadBackAsTheWorker(t *testing.T) {
	fake, _ := homeDesktop(true, false)
	fake.ex.onExec = func(_ string, opts container.ExecOptions, _ []byte) ([]byte, int) {
		cmd := strings.Join(opts.Cmd, " ")
		switch {
		case strings.Contains(cmd, `[ -e "$1" ]`):
			if strings.HasSuffix(cmd, homeDir+"/"+MissingAppsFile) {
				return nil, 0
			}
			return nil, 1
		case strings.Contains(cmd, "tar -c"):
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			body := []byte(`[{"id":"tally","name":"TallyPrime","reason":"not on this computer","pending":true}]` + "\n")
			_ = tw.WriteHeader(&tar.Header{Name: MissingAppsFile, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
			_, _ = tw.Write(body)
			_ = tw.Close()
			return buf.Bytes(), 0
		}
		return nil, 0
	}
	m := newManagerWith(fake)
	apps, err := m.missingAppsOf(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].ID != "tally" || !apps[0].Pending {
		t.Fatalf("read back %+v", apps)
	}
	assertAsWorker(t, fake)
}
