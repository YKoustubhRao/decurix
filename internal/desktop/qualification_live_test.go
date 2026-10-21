package desktop

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/docker/docker/client"
)

// TestQualificationLive proves the actual Docker boundary when explicitly
// enabled with an existing desktop image ID. It never builds or pulls images.
func TestQualificationLive(t *testing.T) {
	image := os.Getenv("DECURIX_QUALIFICATION_IMAGE")
	if image == "" {
		t.Skip("set DECURIX_QUALIFICATION_IMAGE to an existing local desktop image ID")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	m := NewManager(cli)
	d := qualification.Definition{Image: image, Runtime: qualification.RuntimeVersion, Language: "python", TimeoutSeconds: 10, Script: `import os, socket, json
from pathlib import Path
assert os.getuid() == 1001
assert not Path('/var/run/docker.sock').exists()
assert not list(Path('/home/headless').iterdir())
assert 'CONTROL_PLANE_URL' not in os.environ
assert 'OLLAMA_URL' not in os.environ
try:
    Path('/etc/readiness-write-test').write_text('bad')
    raise AssertionError('root filesystem writable')
except OSError:
    pass
try:
    socket.create_connection(('1.1.1.1', 443), timeout=1)
    raise AssertionError('external network reachable')
except OSError:
    pass
Path('result.json').write_text(json.dumps({'total': 30}))
`}
	c := qualification.Case{ID: "boundary", Expected: "complete", Checks: []qualification.Check{{File: "result.json", Kind: "json_equals", JSON: []byte(`{"total":30}`)}}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := m.RunQualification(ctx, d, c)
	finding := qualification.Verify(c, result, err)
	if !finding.Passed {
		t.Fatalf("real isolated run failed: error=%v finding=%+v", err, finding)
	}
	// A symlink cannot smuggle another file out through the artifact collector.
	d.Script = `import os; os.symlink('/etc/passwd', 'result.json')`
	if result, err = m.RunQualification(ctx, d, c); qualification.Verify(c, result, err).Passed {
		t.Fatal("symlink artifact escaped the collector")
	}
	// A detached child cannot modify a checked file after its parent exits.
	d.Script = `from pathlib import Path
import subprocess
Path('result.json').write_text('{"total":30}')
subprocess.Popen(['python3','-c',"import time;time.sleep(2);open('result.json','w').write('wrong')"],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
`
	result, err = m.RunQualification(ctx, d, c)
	if !qualification.Verify(c, result, err).Passed {
		t.Fatalf("subprocess freeze failed: %v %+v", err, result)
	}
}
