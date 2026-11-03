package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/dag"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// maxDagRunsListed bounds how many past runs the run-history list returns.
const maxDagRunsListed = 50

// dagsPageHandler renders the DAGs list page.
func (s *Server) dagsPageHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/dags" {
		http.NotFound(w, r)
		return
	}

	list, err := s.Dags.List()
	if err != nil {
		http.Error(w, "could not read DAGs", http.StatusInternalServerError)
		return
	}

	if err := s.Web.RenderDags(w, webui.DagsPageData{Dags: list}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// dagPageHandler renders one DAG's editor page - the drag-and-drop canvas,
// its cron trigger, and its run history.
func (s *Server) dagPageHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/dags/"), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	d, err := s.Dags.Get(id)
	if errors.Is(err, dag.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not read the DAG", http.StatusInternalServerError)
		return
	}

	desktops, err := s.Desktops.List(r.Context())
	if err != nil {
		log.Printf("dag page: listing desktops: %v", err)
		http.Error(w, "could not list the desktops", http.StatusInternalServerError)
		return
	}

	if err := s.Web.RenderDag(w, webui.DagPageData{DAG: d, Desktops: desktops, Gated: isGated(r)}); err != nil {
		log.Printf("dag page: render: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// dagsAPIHandler lists DAG summaries as JSON.
func (s *Server) dagsAPIHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}

	list, err := s.Dags.List()
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, list)
}

// createDagHandler creates a new, empty DAG - no head, no nodes, no cron.
func (s *Server) createDagHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	d, err := s.Dags.Create(r.FormValue("name"))
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, d)
}

// dagAPIHandler returns one DAG's nodes, edges and recent runs as JSON.
func (s *Server) dagAPIHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}

	detail, err := s.Dags.Detail(r.URL.Query().Get("id"))
	dagRead(w, detail, err, "no such DAG")
}

// deleteDagHandler removes a DAG and, via cascade, everything under it.
func (s *Server) deleteDagHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	s.Dags.Delete(r.FormValue("id"))

	jsonResponse(w, map[string]string{"status": "deleted"})
}

// createDagNodeHandler adds a node to a DAG. The first node ever added
// becomes its head automatically.
func (s *Server) createDagNodeHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	desktopID, ok := s.dagNodeDesktop(w, r)
	if !ok {
		return
	}

	node, err := s.Dags.AddNode(r.FormValue("dag_id"), r.FormValue("name"), desktopID, r.FormValue("content"))
	dagReply(w, node, err, "no such DAG")
}

// updateDagNodeHandler changes a node's name, desktop or content.
func (s *Server) updateDagNodeHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	desktopID, ok := s.dagNodeDesktop(w, r)
	if !ok {
		return
	}

	node, err := s.Dags.UpdateNode(r.FormValue("id"), r.FormValue("name"), desktopID, r.FormValue("content"))
	dagReply(w, node, err, "no such node")
}

// dagNodePositionHandler records where the editor's canvas placed a node -
// called once, on drag release, not on every pointer move.
func (s *Server) dagNodePositionHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	x, errX := strconv.ParseFloat(r.FormValue("x"), 64)
	y, errY := strconv.ParseFloat(r.FormValue("y"), 64)

	if errX != nil || errY != nil {
		jsonStatusError(w, http.StatusBadRequest, "x and y must be numbers")
		return
	}

	err := s.Dags.SetNodePosition(r.FormValue("id"), x, y)
	dagRead(w, map[string]string{"status": "ok"}, err, "no such node")
}

// deleteDagNodeHandler removes a node from a DAG - refused while a run in
// progress still depends on it (see dag.Store.DeleteNode).
func (s *Server) deleteDagNodeHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	if err := s.Dags.DeleteNode(r.FormValue("id")); err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, map[string]string{"status": "deleted"})
}

// setDagHeadHandler promotes a node with no incoming edges to be the DAG's
// head - the previous head simply becomes an ordinary node.
func (s *Server) setDagHeadHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	d, err := s.Dags.SetHead(r.FormValue("dag_id"), r.FormValue("node_id"))
	dagReply(w, d, err, "no such node")
}

// createDagEdgeHandler adds a directed edge between two of a DAG's nodes -
// rejected if it is a self-loop, an edge into the head, a duplicate, or
// would create a cycle.
func (s *Server) createDagEdgeHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	edge, err := s.Dags.AddEdge(r.FormValue("dag_id"), r.FormValue("from"), r.FormValue("to"))
	dagReply(w, edge, err, "no such node")
}

// deleteDagEdgeHandler removes an edge by ID.
func (s *Server) deleteDagEdgeHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	if err := s.Dags.DeleteEdge(r.FormValue("id")); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{"status": "deleted"})
}

// setDagCronHandler sets or clears a DAG's cron trigger - an empty
// cron_expr clears it, since a DAG has no separate enabled/disabled flag.
func (s *Server) setDagCronHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	d, err := s.Dags.SetCron(r.FormValue("dag_id"), r.FormValue("cron_expr"))
	dagReply(w, d, err, "no such DAG")
}

// runDagHandler triggers a DAG's run immediately - the manual "ticket
// submit" door onto a run, alongside its own cron - and hands back the run
// with its head node already dispatched.
func (s *Server) runDagHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	run, err := s.DagRunner.Trigger(r.Context(), r.FormValue("id"), dag.TriggerManual)
	dagReply(w, run, err, "no such DAG")
}

// cancelDagRunHandler ends a run a person gave up on (POST id=<run>): nothing more of it is sent, and what was in
// flight is marked cancelled. A person's, on the primary listener only (RegisterLocalRoutes).
func (s *Server) cancelDagRunHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if s.DagRunner == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "DAG runs are not available")
		return
	}

	cancelled, err := s.DagRunner.Cancel(strings.TrimSpace(r.FormValue("id")))
	if err != nil {
		jsonError(w, err)
		return
	}
	if !cancelled {
		jsonStatusError(w, http.StatusConflict, "That run is not running.")
		return
	}
	jsonResponse(w, map[string]string{"status": "cancelled"})
}

// dagRunsHandler lists a DAG's run history, most recent first.
func (s *Server) dagRunsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}

	runs, err := s.Dags.Runs(r.URL.Query().Get("dag_id"), maxDagRunsListed)
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, runs)
}

// dagRunAPIHandler returns one run's per-node status, for the run-detail
// graph view.
func (s *Server) dagRunAPIHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}

	detail, err := s.Dags.RunDetail(r.URL.Query().Get("id"))
	dagRead(w, detail, err, "no such run")
}

// dagNodeDesktop reads a node form's desktop_id and checks the desktop
// exists, answering the request itself (and returning false) when it does not.
func (s *Server) dagNodeDesktop(w http.ResponseWriter, r *http.Request) (string, bool) {
	desktopID := desktop.SanitizeID(strings.TrimSpace(r.FormValue("desktop_id")))

	if _, err := s.Desktops.Get(r.Context(), desktopID); err != nil {
		if errors.Is(err, desktop.ErrNotFound) {
			jsonStatusError(w, http.StatusBadRequest, "no such desktop")
			return "", false
		}

		jsonError(w, err)
		return "", false
	}

	return desktopID, true
}

// dagReply answers a DAG edit with v, or with what err calls for:
// dag.ErrNotFound is a 404 saying missing, any other error a 400 with the
// store's reason.
func dagReply(w http.ResponseWriter, v any, err error, missing string) {
	switch {
	case errors.Is(err, dag.ErrNotFound):
		jsonStatusError(w, http.StatusNotFound, missing)
	case err != nil:
		jsonStatusError(w, http.StatusBadRequest, err.Error())
	default:
		jsonResponse(w, v)
	}
}

// dagRead is dagReply for a call whose other errors are internal ones
// (jsonError) rather than the store refusing the edit.
func dagRead(w http.ResponseWriter, v any, err error, missing string) {
	switch {
	case errors.Is(err, dag.ErrNotFound):
		jsonStatusError(w, http.StatusNotFound, missing)
	case err != nil:
		jsonError(w, err)
	default:
		jsonResponse(w, v)
	}
}
