package phoneagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// The setup a phone's browser starts (the control plane's /phone/link page)
// ends up here: Termux downloads this binary and runs --phone-setup, which
// claims a sign-in for the phone link and pairs ADB with the phone itself.
// Android lets no app turn on Wireless debugging or read its pairing code,
// so those two things the person does; everything else happens here.

// Claim trades the one-time setup token the phone's browser was given for
// this phone link's own sign-in (the same paired device as the browser).
func Claim(ctx context.Context, base, setupToken string) (token, name string, err error) {
	form := url.Values{"t": {setupToken}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/api/phones/claim", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("reaching %s: %w", base, err)
	}
	defer resp.Body.Close()

	var ans struct {
		Token string `json:"token"`
		Name  string `json:"name"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&ans)

	if resp.StatusCode != http.StatusOK || ans.Token == "" {
		if ans.Error == "" {
			ans.Error = resp.Status
		}
		return "", "", fmt.Errorf("the setup link was not accepted (%s); open the phone link page again for a fresh one", ans.Error)
	}

	return ans.Token, ans.Name, nil
}

// mdnsService matches a line of `adb mdns services`:
// "adb-2A1B3C-xyz	_adb-tls-pairing._tcp.	192.168.1.7:37101".
var mdnsService = regexp.MustCompile(`(_adb-tls-(?:pairing|connect)\._tcp)\.?\s+([0-9.]+):(\d+)`)

// findService returns the address (ip:port) of the first Wireless debugging
// service of kind ("pairing" or "connect") ADB sees, trying until wait ends.
// Not every phone lets Termux hear multicast, so "" is a normal answer.
func findService(ctx context.Context, kind string, wait time.Duration) string {
	deadline := time.Now().Add(wait)

	for time.Now().Before(deadline) && ctx.Err() == nil {
		out, _ := exec.CommandContext(ctx, "adb", "mdns", "services").Output()
		for _, m := range mdnsService.FindAllStringSubmatch(string(out), -1) {
			if m[1] == "_adb-tls-"+kind+"._tcp" {
				return m[2] + ":" + m[3]
			}
		}

		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}

	return ""
}

// SetupADB connects ADB to this phone, pairing it first if it never was:
// the only things asked of the person are the switch in Settings and the
// numbers Android shows. It ends connected through Wireless debugging itself,
// which is paired and encrypted. It never opens adb's legacy port 5555 (adb
// tcpip): that listens on every network the phone joins, unencrypted, until
// the phone restarts (review 2026-11-03, B-26). The cost is that Wireless
// debugging's port changes with the network; Reconnect finds the new one.
func SetupADB(ctx context.Context, in *bufio.Reader, out io.Writer) (string, error) {
	if serial, err := PickSerial(ctx, ""); err == nil {
		return serial, nil
	}
	if serial, err := Reconnect(ctx); err == nil {
		return serial, nil
	}

	fmt.Fprint(out, `
The last step needs Android's Wireless debugging, which only you can turn on:
  1. Settings is opening at Developer options. (No Developer options?
     Settings > About phone > tap "Build number" 7 times, then come back.)
  2. Turn on "Wireless debugging", tap it, then tap "Pair device with pairing code".
  3. Come back here (split screen is easiest) and type the code it shows.

`)
	_ = exec.CommandContext(ctx, "am", "start", "-a", "android.settings.APPLICATION_DEVELOPMENT_SETTINGS").Run()

	ask := func(question string) (string, error) {
		fmt.Fprint(out, question)
		line, err := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" && err != nil {
			return "", errors.New("no answer (run the setup in Termux itself, not piped from elsewhere)")
		}
		return line, nil
	}

	pairAt := findService(ctx, "pairing", 25*time.Second)
	if pairAt == "" {
		port, err := ask(`Port of the pairing screen (the number after ":" under "IP address & Port"): `)
		if err != nil {
			return "", err
		}
		pairAt = "localhost:" + strings.TrimSpace(port)
	}

	code, err := ask("Wi-Fi pairing code (6 digits): ")
	if err != nil {
		return "", err
	}

	pairOut, err := exec.CommandContext(ctx, "adb", "pair", pairAt, code).CombinedOutput()
	if err != nil || !strings.Contains(strings.ToLower(string(pairOut)), "success") {
		return "", fmt.Errorf("pairing failed: %s", strings.TrimSpace(string(pairOut)))
	}
	fmt.Fprintln(out, "Paired.")

	connectAt := findService(ctx, "connect", 15*time.Second)
	if connectAt == "" {
		port, err := ask(`Port on the Wireless debugging screen (after ":" under "IP address & Port"): `)
		if err != nil {
			return "", err
		}
		connectAt = "localhost:" + strings.TrimSpace(port)
	}

	return connectWireless(ctx, connectAt)
}

// Reconnect brings ADB back to this phone without asking anything: the
// device ADB already has, else Wireless debugging when it is on (its port
// changes, so it is looked up).
func Reconnect(ctx context.Context) (string, error) {
	if serial, err := PickSerial(ctx, ""); err == nil {
		return serial, nil
	}

	connectAt := findService(ctx, "connect", 6*time.Second)
	if connectAt == "" {
		return "", errors.New("adb cannot reach this phone: turn on Wireless debugging (Settings > Developer options); the link retries on its own")
	}

	return connectWireless(ctx, connectAt)
}

// runADB runs adb with args; a variable so a test can see what is asked of adb.
var runADB = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "adb", args...).CombinedOutput()
}

// connectWireless connects ADB to Wireless debugging at addr and returns the
// device's serial there.
func connectWireless(ctx context.Context, addr string) (string, error) {
	if out, err := runADB(ctx, "connect", addr); err != nil || strings.Contains(string(out), "failed") {
		return "", fmt.Errorf("adb connect %s: %s", addr, strings.TrimSpace(string(out)))
	}

	return pickSerial(ctx, addr)
}

// pickSerial is PickSerial, a variable for tests.
var pickSerial = PickSerial
