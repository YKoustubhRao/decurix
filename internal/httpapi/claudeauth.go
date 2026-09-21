package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// claudeAuthStatusHandler reports whether a shared Claude session exists -
// the dashboard uses this to show "Signed in as ..." or a "Log in to
// Claude" prompt next to the Claude Code harness option.
func (s *Server) claudeAuthStatusHandler(w http.ResponseWriter, r *http.Request) {
	session, ok := s.ClaudeAuth.Get()
	if !ok {
		jsonResponse(w, map[string]any{"loggedIn": false})
		return
	}

	jsonResponse(w, map[string]any{
		"loggedIn":  true,
		"email":     session.Email,
		"updatedAt": session.UpdatedAt,
	})
}

// claudeAuthLoginStartHandler begins a fresh "claude auth login" attempt and
// hands the dashboard's popup the sign-in URL to open.
func (s *Server) claudeAuthLoginStartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	loginID, signInURL, err := s.Desktops.StartClaudeLogin(r.Context())
	if err != nil {
		log.Println("claudeAuthLoginStart:", err)
		jsonStatusError(w, http.StatusInternalServerError, "Could not start the claude login flow. Check that the developer desktop image is built and try again.")
		return
	}

	jsonResponse(w, map[string]string{
		"loginId": loginID,
		"url":     signInURL,
	})
}

// claudeAuthLoginCodeHandler pastes the human-provided verification code
// into an in-progress login attempt, and on success stores the resulting
// session for every future claude-harness desktop to be seeded from.
func (s *Server) claudeAuthLoginCodeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	loginID := strings.TrimSpace(r.FormValue("login_id"))
	code := strings.TrimSpace(r.FormValue("code"))

	if loginID == "" || code == "" {
		jsonStatusError(w, http.StatusBadRequest, "missing login_id or code")
		return
	}

	credentialsJSON, email, err := s.Desktops.SubmitClaudeLoginCode(r.Context(), loginID, code)
	if err != nil {
		switch {
		case errors.Is(err, desktop.ErrClaudeLoginInvalidCode):
			jsonStatusError(w, http.StatusBadRequest, "That code didn't work - copy the whole thing from the sign-in page and try again.")
		case errors.Is(err, desktop.ErrClaudeLoginNotFound):
			jsonStatusError(w, http.StatusGone, "That login attempt expired. Start over.")
		default:
			log.Println("claudeAuthLoginCode:", err)
			jsonStatusError(w, http.StatusInternalServerError, "Claude login failed. Start over.")
		}
		return
	}

	if err := s.ClaudeAuth.Save(string(credentialsJSON), email); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"email": email,
	})
}

// claudeAuthLoginCancelHandler discards an in-progress login attempt - the
// dashboard calls this when the human closes the popup without finishing.
func (s *Server) claudeAuthLoginCancelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	loginID := strings.TrimSpace(r.FormValue("login_id"))
	if loginID != "" {
		s.Desktops.CancelClaudeLogin(loginID)
	}

	jsonResponse(w, map[string]string{"status": "cancelled"})
}
