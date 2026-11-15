// privacy_purge.go serves POST /api/privacy/purge: a person purging a leaked secret, by value, from every store the
// control plane never deletes from (internal/purge). Primary listener only (RegisterLocalRoutes) and humansOnly: the
// gated listener never has it, and a desktop's agent, which can reach this listener, is refused. The value is read
// from the body and goes nowhere else: it is not logged, stored, or echoed; the answer is the counts per store and
// the judgement that records who purged it and why.

package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/access"
	"github.com/YKoustubhRao/decurix/internal/purge"
)

// maxPurgeBody bounds a purge request: a value and a reason.
const maxPurgeBody = 64 << 10

// purgeDoor is the door a purge's judgement names.
const purgeDoor = "privacy"

// purgeStores is the stores this server purges, as it has them open.
func (s *Server) purgeStores() purge.Stores {
	st := purge.Stores{DB: s.DB, Tasks: s.Tasks, Chat: s.Chat, Notes: s.QueryNotes, Runs: s.RunStore, Search: s.RunSearch}
	if s.LLMRec != nil {
		st.Calls = s.LLMRec
		st.Blobs = s.LLMRec.Blobs()
	}
	if s.Concierge != nil {
		st.Concierge = s.Concierge.Store
	}
	return st
}

// purgeRequest is a purge's body: JSON {"value", "reason"}, or the same as a form.
type purgeRequest struct {
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// readPurgeRequest reads r's body as JSON or as a form, within maxPurgeBody. The value is trimmed of the spaces and
// line ends a paste or a terminal adds.
func readPurgeRequest(w http.ResponseWriter, r *http.Request) (purgeRequest, bool) {
	var req purgeRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxPurgeBody)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		b, err := io.ReadAll(r.Body)
		if err != nil || json.Unmarshal(b, &req) != nil {
			return req, false
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return req, false
		}
		req.Value, req.Reason = r.PostFormValue("value"), r.PostFormValue("reason")
	}
	req.Value, req.Reason = strings.TrimSpace(req.Value), strings.TrimSpace(req.Reason)
	return req, true
}

// privacyPurgeHandler purges the value in the body from every store and answers with the counts per store (purge.Result).
// A refusal (too short, no reason, not a person, a purge already running) changes nothing and says why.
func (s *Server) privacyPurgeHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	// Registered on the primary listener alone; refused here too, should it ever be mounted on the gated one.
	if isGated(r) {
		jsonStatusError(w, http.StatusForbidden, "this can only be done on this computer, not through the phone or tunnel address")
		return
	}
	req, ok := readPurgeRequest(w, r)
	if !ok {
		jsonStatusError(w, http.StatusBadRequest, "send the value and a reason, as JSON or a form")
		return
	}
	// The owner's call: the person at this machine with access control off, or a signed-in owner.
	actor, ok := s.actorOf(r)
	if !ok || !slices.Contains(actor.Roles, access.RoleOwner) {
		jsonStatusError(w, http.StatusForbidden, "only the owner can purge a secret")
		return
	}
	res, err := s.purgeStores().Purge(r.Context(), req.Value, req.Reason, actor, purgeDoor)
	req.Value = ""
	// A refusal answers in its own words (purge's sentinel errors), which never hold the value.
	switch {
	case errors.Is(err, purge.ErrTooShort), errors.Is(err, purge.ErrNoReason), errors.Is(err, purge.ErrIsMark):
		jsonStatusError(w, http.StatusBadRequest, refusal(err))
		return
	case errors.Is(err, purge.ErrNoPerson):
		jsonStatusError(w, http.StatusForbidden, refusal(err))
		return
	case errors.Is(err, purge.ErrBusy):
		jsonStatusError(w, http.StatusConflict, refusal(err))
		return
	case errors.Is(err, purge.ErrNoRecord):
		jsonStatusError(w, http.StatusServiceUnavailable, refusal(err))
		return
	case err != nil:
		// The stores were purged; only the record of it failed. Counts only, never the value.
		log.Printf("privacy purge: ran (%d stored copies) but could not be recorded", res.Counts.Total())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	log.Printf("privacy purge %s by %s: %d stored copies rewritten", res.Judgement, actor.ID, res.Counts.Total())
	jsonResponse(w, res)
}
