package safety

import (
	"os"
	"strings"
	"testing"
)

// TestMain runs the package's tests in the environment a desktop's soul has: no variable that could point curl or wget
// at a proxy or another config file. onlyLocalTargets rightly reads any such variable (XDG_CONFIG_HOME, CURL_HOME, a
// proxy) as "not local", and CI's Linux runners set XDG_CONFIG_HOME, so every localhost example failed there and
// nowhere else (CI on main, 2026-11-05). A test about such a variable sets it itself with t.Setenv.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if proxyRe.MatchString(name) && !strings.EqualFold(name, "no_proxy") {
			os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}
