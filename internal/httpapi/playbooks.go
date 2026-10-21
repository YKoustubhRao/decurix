package httpapi

import (
	"net/http"
	"regexp"
)

// What a desktop worked out for itself and kept (the agent's playbooks.py):
// the commands of a task whose result was checked, the script it wrote from
// them, and its own run on the screen that it replays. They live on the
// desktop, beside the skills a person taught, and are shown on the same page
// (/skills/<id>); the control plane only proxies them, as it does skills.

var playbookNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)

// requirePlaybook is requireDesktop plus a valid playbook name.
func requirePlaybook(w http.ResponseWriter, r *http.Request, method string) (string, string, bool) {
	return requireNamed(w, r, method, "name", playbookNamePattern, "not a playbook name")
}

// playbooksAPIHandler lists what a desktop worked out for itself.
func (s *Server) playbooksAPIHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := requireDesktop(w, r, http.MethodGet)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/playbooks", nil, true)
}

// playbookAPIHandler returns one of them with its commands and its script.
func (s *Server) playbookAPIHandler(w http.ResponseWriter, r *http.Request) {
	id, name, ok := requirePlaybook(w, r, http.MethodGet)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/playbooks/"+name, nil, true)
}

// playbookDeleteHandler makes a desktop forget one: the next request of that
// kind is worked out afresh.
func (s *Server) playbookDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, name, ok := requirePlaybook(w, r, http.MethodPost)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodPost, "/playbooks/"+name+"/delete", nil, true)
}
