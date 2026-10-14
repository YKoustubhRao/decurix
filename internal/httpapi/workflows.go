package httpapi

import "net/http"

// Workflow mutations use the normal chat/task delivery path, preserving its
// task identity, busy checks and human conversation. This endpoint is read-only.
func (s *Server) workflowsAPIHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := requireDesktop(w, r, http.MethodGet)
	if !ok {
		return
	}
	s.callAgent(w, r, id, http.MethodGet, "/workflows", nil, true)
}
