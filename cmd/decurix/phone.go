package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/YKoustubhRao/decurix/internal/phoneagent"
)

// runPhone is --phone: this binary as the phone link, run in Termux on an
// Android phone (internal/phoneagent). link is a pairing link from the
// Mobile menu, or "" to reconnect with what an earlier run saved. It returns
// the process's exit code.
func runPhone(link, name, serial string) int {
	logf := func(format string, args ...any) {
		fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	}

	useFallbackDNS()

	dir := phoneDir()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serial, err := phoneagent.PickSerial(ctx, serial)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	saved, loadErr := phoneagent.Load(dir)

	if link != "" {
		base, code, err := phoneagent.ParseLink(link)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}

		if loadErr == nil && phoneagent.SameControlPlane(saved, base) && phoneagent.Accepted(ctx, base, saved.Token) {
			// Paired already with this same address. A link naming another
			// address pairs again with its code: the saved token is never sent
			// to a host it was not made for (B-18).
			logf("already paired as %q with %s", saved.Name, base)
		} else {
			if name == "" {
				name = phoneModel(ctx, serial)
			}

			token, err := phoneagent.Pair(ctx, base, code, name)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}

			saved = phoneagent.Saved{Base: base, Token: token, Name: name}
			logf("paired as %q with %s", name, base)
		}

		if err := phoneagent.Save(dir, saved); err != nil {
			fmt.Fprintln(os.Stderr, "could not save the link:", err)
		}
	} else if loadErr != nil {
		fmt.Fprintln(os.Stderr, `this phone is not linked yet. On the Decurix dashboard open the Mobile menu, copy the pairing link, then run:
  decurix --phone "<pairing link>"`)
		return 1
	}

	// Keep the phone's CPU awake while linked, so a phone with its screen off
	// still answers. Termux's own command; without it Android may freeze Termux.
	if path, err := exec.LookPath("termux-wake-lock"); err == nil {
		exec.Command(path).Run()
		defer func() {
			if unlock, err := exec.LookPath("termux-wake-unlock"); err == nil {
				exec.Command(unlock).Run()
			}
		}()
	}

	logf("linking %s (adb %s) to %s - leave this running; Ctrl+C to stop", saved.Name, serial, saved.Base)

	err = phoneagent.Run(ctx, phoneagent.Config{
		Base:   saved.Base,
		Token:  saved.Token,
		Serial: serial,
		Dir:    dir,
		Logf:   logf,
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	return 0
}

// phoneDir is where the phone link keeps its state: the saved link and the
// scrcpy-server download.
func phoneDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "decurix-phone")
	}

	home, _ := os.UserHomeDir()

	return filepath.Join(home, ".decurix-phone")
}

// phoneModel names the phone after its model, for the paired-devices list.
func phoneModel(ctx context.Context, serial string) string {
	out, err := exec.CommandContext(ctx, "adb", "-s", serial, "shell", "getprop", "ro.product.model").Output()
	if model := string(out); err == nil && len(model) > 1 {
		return strings.TrimRight(model, "\n\r ")
	}

	return "Android phone"
}

// useFallbackDNS gives Go's resolver a server when the system has no
// /etc/resolv.conf to name one - which is Android: a Go binary built without
// cgo otherwise cannot look up the control plane's address at all.
func useFallbackDNS() {
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		return
	}

	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, network, "1.1.1.1:53")
			if err != nil {
				return d.DialContext(ctx, network, "8.8.8.8:53")
			}
			return conn, nil
		},
	}
}

// runPhoneSetup is --phone-setup BASE TOKEN, what the setup script a phone's
// browser handed to Termux runs: it claims this phone link's sign-in with the
// one-time token, connects ADB to the phone (asking only for what Android
// shows), arranges to start again after a restart, and then links.
func runPhoneSetup(base, setupToken, serial string) int {
	useFallbackDNS()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("Linking this phone to Decurix at " + base)

	token, name, err := phoneagent.Claim(ctx, base, setupToken)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	dir := phoneDir()
	if err := phoneagent.Save(dir, phoneagent.Saved{Base: base, Token: token, Name: name}); err != nil {
		fmt.Fprintln(os.Stderr, "could not save the link:", err)
		return 1
	}

	if _, err := phoneagent.SetupADB(ctx, bufio.NewReader(os.Stdin), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "Run  decurix --phone-setup  again from the phone link page once it is fixed.")
		return 1
	}

	writeBootScript()

	fmt.Println("\nDone. This phone's apps can now be picked when creating a desktop.\nKeep Termux running in the background (you can leave this screen).")

	stop()

	return runPhone("", "", serial)
}

// writeBootScript starts the phone link again when the phone restarts, if the
// Termux:Boot add-on is installed (it runs ~/.termux/boot/*); harmless if not.
func writeBootScript() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}

	dir := filepath.Join(home, ".termux", "boot")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}

	script := "#!/data/data/com.termux/files/usr/bin/sh\n" +
		"# Written by decurix --phone-setup: link this phone to Decurix after a restart.\n" +
		"termux-wake-lock\n" +
		"exec " + exe + " --phone >> " + filepath.Join(home, ".decurix-phone.log") + " 2>&1\n"

	_ = os.WriteFile(filepath.Join(dir, "decurix-phone"), []byte(script), 0o700)
}
