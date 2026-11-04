package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/httpapi"
	"github.com/YKoustubhRao/decurix/internal/idlepause"
	"github.com/YKoustubhRao/decurix/internal/ollama"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// dockerStartup is the start-up that needs Docker, run by Manager.WatchDocker once Docker answers (and again on a
// later answer if it failed): the network desktops live on, the sweeps of what an earlier run left behind, and the
// background repairs of existing desktops. Only the network is required; everything else is best effort and logs.
func dockerStartup(ctx context.Context, desktops *desktop.Manager, server *httpapi.Server, tasksStore *tasks.Store) error {
	if err := desktops.EnsureNetwork(ctx); err != nil {
		return fmt.Errorf("create the Decurix network: %w", err)
	}
	// Each desktop has a network of its own; a control plane container made again (an update) is on none of them
	// until it joins, and neither reaches the other till then. Networks whose desktop is gone go first, so none is
	// joined. A network that cannot be joined is said here, and that desktop's start fails saying so.
	desktops.SweepDesktopNetworks(ctx)
	if err := desktops.JoinDesktopNetworks(ctx); err != nil {
		log.Printf("warning: desktop networks: %v", err)
	}
	// Every desktop's screen password is held again: a worker can read its own (desktop.HoldScreenPasswords).
	desktops.HoldScreenPasswords(ctx)
	// Host hops (privileged containers that run something on the host) left by an earlier run go first: their owner is
	// gone, and the Windows helper a hop runs keeps its app and its memory for as long as the hop lives.
	desktop.SweepHopsAtStart()
	// Home-copy helpers this instance left mid-copy hold a desktop's home volume (RW1-C-05).
	desktops.SweepHomeHelpers(ctx)
	// And the host-app helpers (with the apps they started) that this instance's earlier run left: a Windows program
	// started through WSL outlives the hop that started it. Only helpers carrying this instance's tag (W-04).
	server.SweepHostHelpersAtStart()

	// Non-blocking: the dashboard is still useful (and says what is wrong) when the images have not been built yet.
	if err := desktops.EnsureImages(ctx); err != nil {
		log.Printf("warning: %v", err)
		log.Printf("run `%s --build-images` to build them", exeName())
	}

	// Pre-warm the shared tools volume in the background so the first VM creation does not wait for it.
	go func() {
		if err := desktops.EnsureToolsVolume(context.Background()); err != nil {
			log.Printf("background tools volume: %v", err)
		}
	}()
	// Desktops created before the control plane moved off the LAN-facing address keep calling it (see
	// Manager.StaleControlPlane): say so once, with what to do, rather than leaving them to fail silently.
	go func() {
		if stale := desktops.StaleControlPlane(context.Background()); len(stale) > 0 {
			log.Printf("warning: desktops %s call the control plane at an address it no longer serves them on; "+
				"recreate them (their environment is fixed when they are created)", strings.Join(stale, ", "))
		}
	}()
	// Desktops made when half their chosen memory was reserved get the idle floor instead
	// (Manager.ReleaseReservations): the rest is taken only when used.
	go func() {
		if changed := desktops.ReleaseReservations(context.Background()); len(changed) > 0 {
			log.Printf("desktops %s now reserve only their idle memory; the size chosen stays their ceiling",
				strings.Join(changed, ", "))
		}
	}()

	// Qualification work is disposable. Never resume a process whose evidence transaction was interrupted; startup
	// revoked its grant in NewStore. Practice desktops are disposable the same way (docs/simulation.md).
	cleanup, cleanupDone := context.WithTimeout(ctx, 30*time.Second)
	if err := desktops.CleanupQualification(cleanup); err != nil {
		log.Printf("warning: cleanup interrupted readiness containers: %v", err)
	}
	if err := desktops.CleanupPractice(cleanup); err != nil {
		log.Printf("warning: cleanup practice containers: %v", err)
	}
	cleanupDone()

	// Model selection keeps to the model existing desktops run (C-11): a fresh process has no pick of its own yet, and
	// a different one would make the one-model Ollama swap between the two on every turn.
	if all, err := desktops.List(ctx); err == nil {
		for _, d := range all {
			if d.Brain == desktop.BrainOllama && d.Model != "" {
				ollama.SeedCurrentModel(d.Model)
				break
			}
		}
	}

	// Tasks of desktops whose container went away while the control plane was down (or crashed without its agent
	// reporting) were left running for good, keeping the PC awake and the worker busy (C-17).
	idlepause.CloseGoneAtStart(ctx, desktops, tasksStore)

	// Notes of desktops deleted before deleting resolved them sat in "Needs you" for good, with a Reply that had
	// nowhere to go. Only when the list is known: a failed list is not evidence that anything was deleted.
	go func() {
		all, err := desktops.List(context.Background())
		if err != nil {
			return
		}
		ids := make([]string, 0, len(all))
		for _, d := range all {
			ids = append(ids, d.ID)
		}
		if n := server.QueryNotes.ResolveOrphans(ids); n > 0 {
			log.Printf("Resolved %d open note(s) of desktops that no longer exist.", n)
		}
	}()
	// Requested apps a desktop was created without (and the retry of ones the host could not be asked about yet) live
	// in memory; read them back from the desktops, which keep them for their agents.
	go desktops.RestoreMissingApps(context.Background())
	// The Windows apps copied into VMs are kept running on the host, ready to show the moment one is opened.
	go server.RestoreHostApps(context.Background())
	return nil
}
