package desktop

import (
	"os"
	"strings"
)

// ScreenBrokerEnv is the control plane's switch for the screen broker (docs/gate-boundary.md, "Next: the input
// broker"). On by default since 2026-11-08 (owner decision O-1): every desktop created while it is on gets
// ScreenUserEnv. "0" (or "false", "off", "no") turns it off for desktops created from then on; unset, empty or "1" is
// on. A desktop keeps what it was created with (its environment is fixed at creation), so changing it changes nothing
// for an existing desktop.
const ScreenBrokerEnv = "DECURIX_SCREEN_BROKER"

// ScreenUserEnv is set to "1" in a desktop created in broker mode: its screen is the screen user's (ScreenUser), and
// the worker drives it only through the broker (agent/decurix_agent/computer/broker.py). Not a secret.
const ScreenUserEnv = "DECURIX_SCREEN_USER"

// ScreenUser is the Linux user a broker-mode desktop's screen runs as (docker/desktop.basic.Dockerfile).
const ScreenUser = "decurixscreen"

// screenBrokerOn reports whether the control plane creates desktops in broker mode: on unless ScreenBrokerEnv says
// off ("0", "false", "off" or "no", any case). Any other value is on: a typo keeps the boundary rather than drop it.
func screenBrokerOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ScreenBrokerEnv))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// screenBrokerEnv is what a new desktop's environment gets for the screen broker: ScreenUserEnv=1 when it is on,
// nothing otherwise.
func screenBrokerEnv() []string {
	if !screenBrokerOn() {
		return nil
	}
	return []string{ScreenUserEnv + "=1"}
}
