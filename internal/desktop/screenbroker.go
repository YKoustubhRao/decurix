package desktop

import (
	"os"
	"strings"
)

// ScreenBrokerEnv is the control plane's switch for the screen broker (docs/gate-boundary.md, "Next: the input
// broker"): with it "1", every desktop created from then on gets ScreenUserEnv. Off by default; a desktop keeps what it
// was created with (its environment is fixed at creation).
const ScreenBrokerEnv = "DECURIX_SCREEN_BROKER"

// ScreenUserEnv is set to "1" in a desktop created in broker mode: its screen is the screen user's (ScreenUser), and
// the worker drives it only through the broker (agent/decurix_agent/computer/broker.py). Not a secret.
const ScreenUserEnv = "DECURIX_SCREEN_USER"

// ScreenUser is the Linux user a broker-mode desktop's screen runs as (docker/desktop.basic.Dockerfile).
const ScreenUser = "decurixscreen"

// screenBrokerOn reports whether the control plane creates desktops in broker mode: ScreenBrokerEnv is exactly "1".
func screenBrokerOn() bool {
	return strings.TrimSpace(os.Getenv(ScreenBrokerEnv)) == "1"
}

// screenBrokerEnv is what a new desktop's environment gets for the screen broker: ScreenUserEnv=1 when it is on,
// nothing otherwise.
func screenBrokerEnv() []string {
	if !screenBrokerOn() {
		return nil
	}
	return []string{ScreenUserEnv + "=1"}
}
