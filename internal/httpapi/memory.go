package httpapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/memory"
)

// desktopMemoryHandler stores a new worker memory entry or returns all remembered workflows.
func (s *Server) desktopMemoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// A desktop reads its own memory; every worker's is a person's to see.
		own, _ := requestDesktop(r)
		jsonResponse(w, s.Memory.List(own))
		return
	}

	if !requirePost(w, r) {
		return
	}

	if err := r.ParseForm(); err != nil {
		badForm(w, err)
		return
	}

	desktopID, ok := actingAs(w, r, strings.TrimSpace(r.FormValue("desktop_id")))
	if !ok {
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	summary := strings.TrimSpace(r.FormValue("summary"))
	category := strings.TrimSpace(r.FormValue("category"))

	entry, err := s.Memory.Record(desktopID, title, summary, category)
	if err != nil {
		jsonError(w, err)
		return
	}

	filename, content := memory.FormatForDesktopFile(entry)

	persistErr := s.Desktops.PersistMemoryFile(r.Context(), entry.DesktopID, filename, content)
	if persistErr != nil {
		log.Printf("desktopMemoryHandler: persist entry %s to %s: %v", entry.ID, entry.DesktopID, persistErr)
	}

	// Fan the same entry out to every worker this one shares L1 memory
	// with, so "share memory between two workers" means a lesson learned
	// on either one reaches the other from then on, not just at the
	// moment they were linked. Best-effort per desktop, same posture as
	// the primary write above returning an error to the client but never
	// rolling back entry - a desktop that is stopped or gone shouldn't
	// stop the entry from being recorded, and shouldn't stop it reaching
	// the *other* linked desktops either.
	for _, linkedID := range s.Memory.LinkedWorkers(entry.DesktopID) {
		if err := s.Desktops.PersistMemoryFile(r.Context(), linkedID, filename, content); err != nil {
			log.Printf("desktopMemoryHandler: share entry %s with %s: %v", entry.ID, linkedID, err)
		}
	}

	if persistErr != nil {
		jsonError(w, fmt.Errorf("persist memory: %w", persistErr))
		return
	}

	jsonResponse(w, entry)
}

// linkDesktopMemoryHandler links two workers so a memory entry recorded for
// either one is shared with the other going forward, and backfills every
// entry either one already has into the other's container.
func (s *Server) linkDesktopMemoryHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	if err := r.ParseForm(); err != nil {
		badForm(w, err)
		return
	}

	desktopID := strings.TrimSpace(r.FormValue("desktop_id"))
	linkedID := strings.TrimSpace(r.FormValue("linked_id"))

	if desktopID == "" || linkedID == "" {
		http.Error(w, "desktop_id and linked_id are required", http.StatusBadRequest)
		return
	}

	if _, ok := s.desktopFound(w, r, desktopID, http.StatusBadRequest, "desktop_id not found"); !ok {
		return
	}

	if _, ok := s.desktopFound(w, r, linkedID, http.StatusBadRequest, "linked_id not found"); !ok {
		return
	}

	if err := s.Memory.Link(desktopID, linkedID); err != nil {
		jsonError(w, err)
		return
	}

	s.backfillSharedMemory(r.Context(), desktopID, linkedID)

	jsonResponse(w, map[string]any{"linked": s.Memory.LinkedWorkers(desktopID)})
}

// backfillSharedMemory copies every existing entry each of the two desktops
// already has into the other's container, so linking two workers shares
// what they already learned, not just what they learn from then on.
// Best-effort: a copy that fails is logged, never surfaced to the caller,
// since the link itself (the part that matters for future entries) already
// succeeded.
func (s *Server) backfillSharedMemory(ctx context.Context, desktopA, desktopB string) {
	for _, pair := range [][2]string{{desktopA, desktopB}, {desktopB, desktopA}} {
		for _, entry := range s.Memory.List(pair[0]) {
			filename, content := memory.FormatForDesktopFile(entry)
			if err := s.Desktops.PersistMemoryFile(ctx, pair[1], filename, content); err != nil {
				log.Printf("backfillSharedMemory: copy %s to %s: %v", entry.ID, pair[1], err)
			}
		}
	}
}

// unlinkDesktopMemoryHandler stops sharing L1 memory between two workers.
func (s *Server) unlinkDesktopMemoryHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	if err := r.ParseForm(); err != nil {
		badForm(w, err)
		return
	}

	desktopID := strings.TrimSpace(r.FormValue("desktop_id"))
	linkedID := strings.TrimSpace(r.FormValue("linked_id"))

	if desktopID == "" || linkedID == "" {
		http.Error(w, "desktop_id and linked_id are required", http.StatusBadRequest)
		return
	}

	if err := s.Memory.Unlink(desktopID, linkedID); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]any{"linked": s.Memory.LinkedWorkers(desktopID)})
}
