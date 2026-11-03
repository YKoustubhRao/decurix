package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// ============================================================
// Requested apps a desktop does not have
//
// Found live (2026-10-20): right after a control-plane start, the host helper checking which Windows apps can run died
// after 56 of 119 checks; every app it never got to - Word among them - was marked unavailable, the desktop was
// created without it, and nothing said so. Asked for a letter in Word, the worker opened another word processor.
//
// So a requested app that is not added is recorded with the reason, in plain words: in the create response, on the
// desktop's card, and in a file the desktop's own agent reads (MissingAppsFile), so a request naming it is answered
// "that app isn't on this desktop" instead of being done in something else. And "the host could not be asked" is
// kept apart from "the host said no": the first is tried again in the background, and the app's launcher is added to
// the running desktop when the check passes.
// ============================================================

// MissingAppsFile is where, under the desktop's home, its agent finds the requested apps it does not have
// (decurix_agent reads it: identity.py and `gui open`). A JSON list of MissingApp.
const MissingAppsFile = ".decurix/missing-apps.json"

// UnverifiedAppReason is what a requested app the host could not be asked about yet is missing for.
const UnverifiedAppReason = "Not added yet: Decurix could not check it on this computer, because the check did not answer. " +
	"It keeps trying for a few minutes and adds the app when the check passes."

// gaveUpReason replaces UnverifiedAppReason once the background retries are used up.
const gaveUpReason = "Not added: Decurix tried for several minutes but could not check it on this computer. " +
	"Add it to the desktop again once the computer has settled."

// hostAppRetryDelays are the waits before each background retry of a desktop's pending apps: about six minutes in
// all, long enough for a host that was busy starting up. A variable so tests can shorten it.
var hostAppRetryDelays = []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute}

// Seams for tests: what a retry runs, how it tells a deleted desktop, and how the agent's file is written.
var (
	// retryMigrate, when set, replaces MigrateHostApps in a retry (nil: the real one; a func value here would be an
	// initialisation cycle).
	retryMigrate func(m *Manager, ctx context.Context, id string, apps []string) error
	desktopGone  = func(m *Manager, ctx context.Context, id string) bool {
		_, err := m.client.ContainerInspect(ctx, "desktop_"+id)
		return err != nil && client.IsErrNotFound(err)
	}
	writeMissingFileFn = func(m *Manager, ctx context.Context, containerID string, apps []MissingApp) error {
		return m.copyMissingFile(ctx, containerID, apps)
	}
)

// MissingApps is what the desktop with this ID was asked to have and does not, sorted by name; nil when nothing.
func (m *Manager) MissingApps(id string) []MissingApp {
	m.missingMu.Lock()
	defer m.missingMu.Unlock()
	if len(m.missing[id]) == 0 {
		return nil
	}
	return append([]MissingApp(nil), m.missing[id]...)
}

// noteMissingApps records the outcome of one migration: every app in requested is now either added (dropped from the
// list) or in missing. Entries for apps this migration was not asked about are kept. It reports whether the desktop's
// agent needs its file (re)written - whenever there is or was anything in it.
func (m *Manager) noteMissingApps(id string, requested map[string]bool, missing []MissingApp) bool {
	m.missingMu.Lock()
	defer m.missingMu.Unlock()
	before := len(m.missing[id])
	var kept []MissingApp
	for _, a := range m.missing[id] {
		if !requested[strings.ToLower(a.ID)] {
			kept = append(kept, a)
		}
	}
	kept = append(kept, missing...)
	sort.Slice(kept, func(i, j int) bool { return strings.ToLower(kept[i].Name) < strings.ToLower(kept[j].Name) })
	if len(kept) == 0 {
		delete(m.missing, id)
	} else {
		m.missing[id] = kept
	}
	if before > 0 || len(kept) > 0 {
		go m.invalidateList()
	}
	return before > 0 || len(kept) > 0
}

// pendingApps are the IDs of this desktop's apps still waiting for the host to be asked.
func (m *Manager) pendingApps(id string) []string {
	m.missingMu.Lock()
	defer m.missingMu.Unlock()
	var ids []string
	for _, a := range m.missing[id] {
		if a.Pending {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

// forgetMissingApps drops everything recorded for a desktop that no longer exists.
func (m *Manager) forgetMissingApps(id string) {
	m.missingMu.Lock()
	delete(m.missing, id)
	m.missingMu.Unlock()
}

// giveUpPending turns this desktop's still-pending apps into plain "not added" ones and reports whether there were any.
func (m *Manager) giveUpPending(id string) bool {
	m.missingMu.Lock()
	defer m.missingMu.Unlock()
	changed := false
	for i, a := range m.missing[id] {
		if a.Pending {
			m.missing[id][i].Pending = false
			m.missing[id][i].Reason = gaveUpReason
			changed = true
		}
	}
	return changed
}

// retryPendingApps starts, unless one is already running, the background retry of a desktop's apps the host could not
// be asked about: after each of hostAppRetryDelays it migrates them again (which asks the host again and adds what
// checks out to the running desktop), stops once none is pending or the desktop is gone, and after the last says they
// were not added.
func (m *Manager) retryPendingApps(id string) {
	if len(m.pendingApps(id)) == 0 {
		return
	}
	m.missingMu.Lock()
	if m.retrying[id] {
		m.missingMu.Unlock()
		return
	}
	m.retrying[id] = true
	m.missingMu.Unlock()

	go func() {
		defer func() {
			m.missingMu.Lock()
			delete(m.retrying, id)
			m.missingMu.Unlock()
		}()
		for _, wait := range hostAppRetryDelays {
			time.Sleep(wait)
			apps := m.pendingApps(id)
			if len(apps) == 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if desktopGone(m, ctx, id) {
				cancel()
				m.forgetMissingApps(id)
				return
			}
			log.Printf("hostapps: asking the host again about %s for %s", strings.Join(apps, ", "), id)
			migrate := m.MigrateHostApps
			if retryMigrate != nil {
				migrate = func(ctx context.Context, id string, apps []string) error { return retryMigrate(m, ctx, id, apps) }
			}
			if err := migrate(ctx, id, apps); err != nil {
				log.Printf("hostapps: retrying apps for %s: %v", id, err)
			}
			cancel()
		}
		if m.giveUpPending(id) {
			log.Printf("hostapps: gave up on unchecked apps for %s", id)
			m.rewriteMissingFile(id)
			m.invalidateList()
		}
	}()
}

// missingFileTar is the tar that puts apps, as JSON, at MissingAppsFile under the desktop's home.
func missingFileTar(apps []MissingApp) ([]byte, error) {
	if apps == nil {
		apps = []MissingApp{}
	}
	data, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dir := strings.SplitN(MissingAppsFile, "/", 2)[0] + "/"
	if err := tw.WriteHeader(&tar.Header{Name: dir, Mode: 0o755, Typeflag: tar.TypeDir, ModTime: time.Now(),
		Uid: 1000, Gid: 1000, Uname: "headless", Gname: "headless"}); err != nil {
		return nil, err
	}
	if err := writeStringToTar(tw, MissingAppsFile, string(data)+"\n", 0o644); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// copyMissingFile copies MissingAppsFile into the container (created or running). A root-side copy, kept on
// purpose: the file is a direct child of /home/headless, whose parent cannot be a link, Docker does not follow the last
// component, and extracting replaces whatever entry is there (the live test in homefs_live_test.go covers it).
func (m *Manager) copyMissingFile(ctx context.Context, containerID string, apps []MissingApp) error {
	data, err := missingFileTar(apps)
	if err != nil {
		return err
	}
	return m.client.CopyToContainer(ctx, containerID, "/home/headless", bytes.NewReader(data), container.CopyToContainerOptions{})
}

// RestoreMissingApps reads back, from every desktop, the requested apps it does not have - what a control-plane
// restart would otherwise forget - and resumes the retry of the ones still pending. Run it once, in the background,
// at start.
func (m *Manager) RestoreMissingApps(ctx context.Context) {
	desktops, err := m.List(ctx)
	if err != nil {
		log.Printf("hostapps: restoring missing apps: %v", err)
		return
	}
	for _, d := range desktops {
		// Root-side read of a direct child of /home/headless: see copyMissingFile.
		rc, _, err := m.client.CopyFromContainer(ctx, "desktop_"+d.ID, "/home/headless/"+MissingAppsFile)
		if err != nil {
			continue
		}
		apps, err := readMissingTar(rc)
		rc.Close()
		if err != nil || len(apps) == 0 {
			continue
		}
		m.missingMu.Lock()
		if len(m.missing[d.ID]) == 0 {
			m.missing[d.ID] = apps
		}
		m.missingMu.Unlock()
		m.retryPendingApps(d.ID)
	}
	m.invalidateList()
}

// readMissingTar reads the one MissingAppsFile out of the tar CopyFromContainer returns.
func readMissingTar(r io.Reader) ([]MissingApp, error) {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			break
		}
	}
	var apps []MissingApp
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// recheckHostApps asks the host again about apps it could not be asked about before, and keeps the answers in the
// cached app list, so the next desktop to ask for them does not start from the stale "not checked".
func recheckHostApps(apps []HostAppItem) []HostAppItem {
	if len(apps) == 0 {
		return nil
	}
	got := vetHostApps(apps)
	hostAppsCacheMu.Lock()
	for _, app := range got {
		for i := range cachedHostApps {
			if cachedHostApps[i].ID == app.ID {
				cachedHostApps[i] = app
			}
		}
	}
	hostAppsCacheMu.Unlock()
	return got
}

// PrecheckHostApps says, from the app list already in memory and without asking the host, which of the requested
// apps are known not to be addable - for the immediate answer to a create request. The copy itself has the last word.
func PrecheckHostApps(selected []string) []MissingApp {
	if len(selected) == 0 {
		return nil
	}
	hostAppsCacheMu.RLock()
	defer hostAppsCacheMu.RUnlock()
	byID := make(map[string]HostAppItem, len(cachedHostApps))
	for _, app := range cachedHostApps {
		byID[strings.ToLower(app.ID)] = app
	}
	var out []MissingApp
	for _, id := range selected {
		app, ok := byID[strings.ToLower(strings.TrimSpace(id))]
		if !ok || app.Mode != ModeUnavailable {
			continue
		}
		reason := app.UnavailableReason
		if app.Unverified {
			reason = "Being checked again on this computer before it is added."
		}
		out = append(out, MissingApp{ID: strings.ToLower(app.ID), Name: app.Name, Reason: reason, Pending: app.Unverified})
	}
	return out
}

// keptAppPrefix marks a MissingApp recorded because the host could not keep the app running (MarkAppUnavailable), as
// against one the host check refused: its ID is this prefix and the app's launcher slug.
const keptAppPrefix = "kept:"

// MarkAppUnavailable records that an app copied to a desktop cannot run on the host, with the reason in plain words,
// where the desktop's card and its agent see what it lacks (MissingAppsFile). slug is the app's launcher slug.
func (m *Manager) MarkAppUnavailable(id, slug, name, reason string) {
	appID := keptAppPrefix + strings.ToLower(slug)
	if m.noteMissingApps(id, map[string]bool{appID: true}, []MissingApp{{ID: appID, Name: name, Reason: reason}}) {
		go m.rewriteMissingFile(id)
	}
}

// ClearAppUnavailable drops what MarkAppUnavailable recorded for an app that is being kept again (copied again, or its
// desktop started again).
func (m *Manager) ClearAppUnavailable(id, slug string) {
	appID := keptAppPrefix + strings.ToLower(slug)
	m.missingMu.Lock()
	found := false
	for _, a := range m.missing[id] {
		found = found || a.ID == appID
	}
	m.missingMu.Unlock()
	if found && m.noteMissingApps(id, map[string]bool{appID: true}, nil) {
		go m.rewriteMissingFile(id)
	}
}

// rewriteMissingFile writes a desktop's MissingAppsFile again, best-effort: a stopped desktop gets it on its next change.
func (m *Manager) rewriteMissingFile(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := writeMissingFileFn(m, ctx, "desktop_"+id, m.MissingApps(id)); err != nil {
		log.Printf("hostapps: telling %s which apps are missing: %v", id, err)
	}
}
