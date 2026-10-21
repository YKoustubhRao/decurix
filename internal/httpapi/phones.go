package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/phonehost"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/YKoustubhRao/decurix/internal/winhost"
	"github.com/YKoustubhRao/decurix/internal/wsconn"
)

// A linked phone (internal/phonehost, the phone side is internal/phoneagent)
// connects out to /api/phones/connect as a paired device; its apps join the
// create dialog's app list, and a VM shows one through /phone-app/view.

const (
	// phoneDeviceCookie is the paired-device cookie (internal/lanaccess) a phone links with.
	phoneDeviceCookie = "decurix_lan_device"
	// phoneAppsWait bounds how long the app list waits for linked phones to answer.
	phoneAppsWait = 20 * time.Second
	// phoneOpenWait bounds starting an app on a phone: ADB, the server, the display.
	phoneOpenWait = 45 * time.Second
	// phoneTargetPrefix is the "target" a phone app's launch token is signed over.
	phoneTargetPrefix = "android:"
)

// SetPhones turns phone links on: phones can connect, and their apps join
// the create dialog's app list.
func (s *Server) SetPhones(hub *phonehost.Hub) {
	s.Phones = hub
	desktop.SetExtraHostApps(s.phoneAppItems)
}

// phoneConnectHandler is a linked phone's one connection: it proves itself
// with its paired-device token, then stays open for as long as the phone is
// linked. On the gated listener the gate has already checked the same cookie.
func (s *Server) phoneConnectHandler(w http.ResponseWriter, r *http.Request) {
	if s.Phones == nil || s.Devices == nil {
		http.Error(w, "phone links are not available", http.StatusNotFound)
		return
	}

	cookie, err := r.Cookie(phoneDeviceCookie)
	if err != nil {
		http.Error(w, "pair this phone first", http.StatusUnauthorized)
		return
	}

	device, ok := s.Devices.Lookup(cookie.Value)
	if !ok {
		http.Error(w, "this phone's pairing is not known here", http.StatusForbidden)
		return
	}

	c, err := wsconn.Upgrade(w, r)
	if err != nil {
		return
	}

	// The link outlives the request that opened it only as long as the process does.
	s.Phones.Serve(context.WithoutCancel(r.Context()), device.ID, device.Name, c)
}

// phonesHandler lists the phones linked right now.
func (s *Server) phonesHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}

	if s.Phones == nil {
		jsonResponse(w, map[string]any{"phones": []phonehost.Info{}})
		return
	}

	jsonResponse(w, map[string]any{"phones": s.Phones.Phones()})
}

// refreshPhoneApps asks every linked phone for its apps (in parallel, up to
// phoneAppsWait), so the scan that follows lists them.
func (s *Server) refreshPhoneApps(ctx context.Context, force bool) {
	if s.Phones == nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, phoneAppsWait)
	defer cancel()

	var wg sync.WaitGroup
	for _, p := range s.Phones.Phones() {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, _ = s.Phones.Apps(ctx, id, force)
		}(p.ID)
	}
	wg.Wait()
}

// phoneAppItems is every linked phone's apps as entries of the app list,
// from what the phones last said (desktop.SetExtraHostApps calls this).
func (s *Server) phoneAppItems() []desktop.HostAppItem {
	if s.Phones == nil {
		return nil
	}

	var items []desktop.HostAppItem
	for _, p := range s.Phones.Phones() {
		for _, app := range s.Phones.Cached(p.ID) {
			items = append(items, desktop.HostAppItem{
				ID:          "phone-" + p.ID + "-" + app.Package,
				Name:        app.Name,
				Category:    "Phone: " + p.Name,
				Description: "Runs on " + p.Name + " and shows in the VM. The phone has to be linked (decurix --phone) when you open it.",
				Detected:    true,
				AppType:     "phone_app",
				Mode:        desktop.ModePhone,
				HostCmd:     desktop.PhoneCmd(p.ID, app.Package),
			})
		}
	}

	return items
}

// phoneAppAuthorised reports whether token is the launch token for app pkg
// on phone device under session id, as the VM's launcher was written with.
func (s *Server) phoneAppAuthorised(id, device, pkg, token string) bool {
	if token == "" || !hostGUIID.MatchString(id) {
		return false
	}

	want := winhost.Token(s.hostGUISecretKey(), id, phoneTargetPrefix+device, pkg)

	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

// phoneAppViewHandler is the window a VM opens to show an app on a phone.
func (s *Server) phoneAppViewHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !s.phoneAppAuthorised(q.Get("id"), q.Get("device"), q.Get("package"), q.Get("token")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	title := strings.TrimSpace(q.Get("title"))
	if title == "" {
		title = "Phone app"
	}

	var out bytes.Buffer
	err := s.Web.RenderPhoneApp(&out, webui.PhoneAppPageData{
		ID: q.Get("id"), Token: q.Get("token"), Desktop: q.Get("desktop"),
		Title: title, Device: q.Get("device"), Package: q.Get("package"),
	})
	if err != nil {
		jsonError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Write(out.Bytes())
}

// phoneAppWSHandler is the viewer's one connection to an app on a phone:
// the app is started on a display of its own when it connects and closed
// when it goes. Out, binary: each of the phone's video messages as scrcpy
// frames it (a 12-byte header and its payload); text JSON {"opened":true,
// "codec":...} once streaming, {"tree":phonehost.Tree} whenever the app's
// controls change, and {"closed":true,"error":...} at the end.
// In, text JSON: one phonehost.Input each.
func (s *Server) phoneAppWSHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	device, pkg := q.Get("device"), q.Get("package")
	if !s.phoneAppAuthorised(q.Get("id"), device, pkg, q.Get("token")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	c, err := wsconn.Upgrade(w, r)
	if err != nil {
		return
	}
	defer c.Close()

	say := func(v any) error {
		raw, _ := json.Marshal(v)
		return c.Write(wsconn.OpText, raw)
	}

	if s.Phones == nil {
		say(map[string]any{"closed": true, "error": "Phone links are not available on this control plane."})
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.markDesktopViewed(ctx, q.Get("desktop"))

	if _, linked := s.Phones.Name(device); !linked {
		say(map[string]any{"closed": true, "error": "The phone is not linked right now. On the phone, open Termux and run: decurix --phone"})
		return
	}

	openCtx, openCancel := context.WithTimeout(ctx, phoneOpenWait)
	sess, err := s.Phones.Open(openCtx, device, pkg, atoiIn(q.Get("w"), 720, 240, 2160), atoiIn(q.Get("h"), 1440, 320, 3840), atoiIn(q.Get("dpi"), 280, 120, 640))
	openCancel()
	if err != nil {
		msg := err.Error()
		if errors.Is(err, phonehost.ErrNoPhone) {
			msg = "The phone disconnected. On the phone, open Termux and run: decurix --phone"
		}
		say(map[string]any{"closed": true, "error": msg})
		return
	}
	defer sess.Close()

	if say(map[string]any{"opened": true, "codec": sess.Codec()}) != nil {
		return
	}

	go func() {
		defer cancel()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case f := <-sess.Frames():
				if c.Write(wsconn.OpBinary, f) != nil {
					return
				}
			case t := <-sess.Trees():
				if say(map[string]any{"tree": t}) != nil {
					return
				}
			case <-sess.Done():
				say(map[string]any{"closed": true, "error": sess.Err()})
				return
			case <-ping.C:
				if c.Write(wsconn.OpPing, nil) != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		<-ctx.Done()
		c.Close()
	}()

	for {
		op, msg, err := c.Read(70 * time.Second)
		if err != nil {
			return
		}
		if op != wsconn.OpText {
			continue
		}

		var in phonehost.Input
		if json.Unmarshal(msg, &in) != nil {
			continue
		}
		if sess.Input(in) != nil {
			return
		}
	}
}

// atoiIn parses s as an int in [lo, hi], or returns def.
func atoiIn(s string, def, lo, hi int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return def
	}

	return n
}
