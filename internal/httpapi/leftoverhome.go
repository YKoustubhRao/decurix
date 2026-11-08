package httpapi

// A home folder an earlier desktop left behind. A delete whose volume removal failed keeps the old desktop's home
// volume, and a new desktop of that name is refused rather than handed the old one's memory and sign-ins
// (desktop.homeVolumeFor). The create form learns of it before anything starts (refuseLeftoverHome), and offers to
// delete it; the delete is a person's act at this computer (POST /api/desktops/home-volume/delete, primary listener,
// humans only: RegisterLocalRoutes).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// leftoverCheckTimeout bounds the create form's look for a leftover home: a Docker slow to answer delays no create.
const leftoverCheckTimeout = 5 * time.Second

// refuseLeftoverHome answers a create of name with 409 when an earlier desktop of that name left its home volume,
// and reports whether it did. leftover_home tells the form it may offer the delete: only where that route is served
// (never through the phone or tunnel address). A check that could not be made in leftoverCheckTimeout refuses
// nothing - the creation checks again and says why.
func (s *Server) refuseLeftoverHome(w http.ResponseWriter, r *http.Request, name string) bool {
	ctx, cancel := context.WithTimeout(r.Context(), leftoverCheckTimeout)
	defer cancel()
	leftover, err := s.Desktops.LeftoverHome(ctx, name)
	if err != nil {
		log.Printf("createDesktop: leftover home check for %s: %v", name, err)
		return false
	}
	if !leftover {
		return false
	}
	id := desktop.SanitizeID(name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": fmt.Sprintf("The home folder of an earlier desktop called %s is still there (its files, memory and "+
			"sign-ins). A new desktop does not take it over: delete it, or choose another name.", id),
		"leftover_home": !isGated(r),
		"id":            id,
	})
	return true
}

// deleteLeftoverHomeHandler answers POST /api/desktops/home-volume/delete (name): removes the home volume an earlier
// desktop of that name left, for good, only when no desktop of the name exists or is being made and nothing mounts
// it (desktop.Manager.RemoveLeftoverHome). Never a shared volume.
func (s *Server) deleteLeftoverHomeHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || !validDesktopNameRe.MatchString(name) {
		jsonStatusError(w, http.StatusBadRequest, "name the desktop whose old home folder to delete")
		return
	}

	// Under the desktop's lifecycle lock, like every other change to it.
	unlock := s.Desktops.LockDesktop(name)
	err := s.Desktops.RemoveLeftoverHome(r.Context(), name)
	unlock()

	switch {
	case err == nil:
		jsonResponse(w, map[string]string{"status": "deleted", "id": desktop.SanitizeID(name)})
	case errors.Is(err, desktop.ErrNoLeftoverHome):
		jsonStatusError(w, http.StatusNotFound, "No home folder of an earlier desktop is left under that name.")
	case errors.Is(err, desktop.ErrHomeInUse):
		log.Printf("deleteLeftoverHome: %s: %v", name, err)
		jsonStatusError(w, http.StatusConflict, "That home folder is still in use: a desktop of that name exists or "+
			"is being made, or a container still has it. Nothing was deleted.")
	default:
		internalError(w, "deleteLeftoverHome "+name, err)
	}
}
