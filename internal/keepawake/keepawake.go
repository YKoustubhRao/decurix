// Package keepawake keeps this machine from going to sleep on its own while
// Decurix has background work that needs it awake - a mailbox being watched
// for email triggers, a schedule waiting to fire. A sleeping machine runs
// nothing: no poll, no schedule, no desktop.
//
// What it can and cannot do, honestly:
//   - It stops *idle* sleep (the "sleep after N minutes" timer). It does not
//     stop a person putting the machine to sleep, closing a laptop's lid, or
//     a battery running flat - the operating system does not let an
//     application veto those, and it should not.
//   - It holds the request only while there is background work, and releases
//     it the moment there is none, so a machine with nothing scheduled
//     sleeps as it always did.
//   - What happens while asleep is not lost: internal/mailtrigger picks up the
//     email that arrived on the first poll after waking, and a schedule due
//     meanwhile fires once on waking.
//
// The mechanism is each system's own: SetThreadExecutionState on Windows
// (also reached from WSL through powershell.exe, since it is Windows that
// sleeps), caffeinate on macOS, a systemd-inhibit lock on Linux. Inside a
// container none of these reach the host; there the script that started
// Decurix holds it instead, asking GET /api/keep-awake whether to.
//
// Off with DECURIX_KEEP_AWAKE=0. Depends on nothing internal.
package keepawake

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// checkEvery is how often Run re-evaluates whether anything needs the
// machine awake. A variable only so a test need not wait for it.
var checkEvery = 30 * time.Second

// platformHolder is newHolder, replaced in tests.
var platformHolder = newHolder

// holder is one platform's way of holding a "do not idle-sleep" request.
type holder interface {
	// set takes (on) or releases (off) the request.
	set(on bool) error
	// alive reports whether the request is still actually held - a helper
	// process can die, and is then started again.
	alive() bool
	close()
}

// Enabled reports whether DECURIX_KEEP_AWAKE allows keeping the machine
// awake. On unless set to 0/false/off/no.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DECURIX_KEEP_AWAKE"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// Keeper keeps the machine awake while Wanted says there is a reason to.
type Keeper struct {
	// Wanted returns the reasons the machine should stay awake right now;
	// none means it may sleep.
	Wanted func() []string

	mu      sync.Mutex
	held    bool
	why     string
	running bool
}

// State is what GET /api/keep-awake reports.
type State struct {
	// Enabled is DECURIX_KEEP_AWAKE.
	Enabled bool `json:"enabled"`
	// Wanted says whether the machine should be kept awake now, and Reasons
	// why - what a host script outside a container acts on.
	Wanted  bool     `json:"wanted"`
	Reasons []string `json:"reasons"`
	// Held says this process itself holds the request; false inside a
	// container, where the host script does (Where says so).
	Held  bool   `json:"held"`
	Where string `json:"where"`
	// Script says a host script (scripts/*/lib/keep-awake) asked within
	// the last ScriptFresh - the one that holds it when Decurix runs in a
	// container. Filled in by the HTTP layer, which is what it asks.
	Script bool `json:"script"`
}

// ScriptFresh is how recently a host script must have asked for it to count
// as running; the scripts ask every 30 seconds.
const ScriptFresh = 90 * time.Second

// State reports whether the machine should be kept awake and whether this
// process is doing it.
func (k *Keeper) State() State {
	reasons := k.reasons()

	k.mu.Lock()
	defer k.mu.Unlock()

	return State{
		Enabled: Enabled(),
		Wanted:  Enabled() && len(reasons) > 0,
		Reasons: reasons,
		Held:    k.held,
		Where:   k.why,
	}
}

func (k *Keeper) reasons() []string {
	if k.Wanted == nil {
		return []string{}
	}

	reasons := k.Wanted()
	if reasons == nil {
		reasons = []string{}
	}

	return reasons
}

// Run holds the request whenever it is wanted until ctx is done, and always
// releases it on the way out.
func (k *Keeper) Run(ctx context.Context) {
	if !Enabled() {
		log.Println("Keep-awake: off (DECURIX_KEEP_AWAKE=0) - this machine may sleep, and nothing runs while it does.")
		return
	}

	h, where := platformHolder()

	k.mu.Lock()
	k.why = where
	k.running = true
	k.mu.Unlock()

	if h == nil {
		log.Printf("Keep-awake: %s", where)
		return
	}

	log.Printf("Keep-awake: while mailboxes or schedules are active, this machine will not idle-sleep (%s). Set DECURIX_KEEP_AWAKE=0 to allow it.", where)

	defer h.close()

	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()

	for {
		want := len(k.reasons()) > 0

		k.mu.Lock()
		held := k.held
		k.mu.Unlock()

		if want != held || (held && !h.alive()) {
			if err := h.set(want); err != nil {
				log.Printf("Keep-awake: %v", err)
				want = false
			} else if want {
				log.Printf("Keep-awake: holding (%s)", strings.Join(k.reasons(), ", "))
			} else {
				log.Println("Keep-awake: released - nothing needs this machine awake")
			}

			k.mu.Lock()
			k.held = want
			k.mu.Unlock()
		}

		select {
		case <-ctx.Done():
			_ = h.set(false)
			return
		case <-ticker.C:
		}
	}
}

// inContainer reports whether this process runs in a container, where no
// sleep request reaches the host.
func inContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}

	return false
}

const containerNote = "in a container, so the script that started Decurix keeps the host awake (scripts/*/lib/keep-awake), asking GET /api/keep-awake"
