package httpapi

import (
	"errors"
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.Web.RenderDag(w, webui.DagPageData{DAG: d, Desktops: desktops}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// dagsAPIHandler lists DAG summaries as JSON.
func (s *Server) dagsAPIHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
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
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
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
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	detail, err := s.Dags.Detail(r.URL.Query().Get("id"))
	if errors.Is(err, dag.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such DAG")
		return
	}
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, detail)
}

// deleteDagHandler removes a DAG and, via cascade, everything under it.
func (s *Server) deleteDagHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	s.Dags.Delete(r.FormValue("id"))

	jsonResponse(w, map[string]string{"status": "deleted"})
}

// createDagNodeHandler adds a node to a DAG. The first node ever added
// becomes its head automatically.
func (s *Server) createDagNodeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	desktopID := desktop.SanitizeID(strings.TrimSpace(r.FormValue("desktop_id")))

	if _, err := s.Desktops.Get(r.Context(), desktopID); err != nil {
		if errors.Is(err, desktop.ErrNotFound) {
			jsonStatusError(w, http.StatusBadRequest, "no such desktop")
			return
		}

		jsonError(w, err)
		return
	}

	node, err := s.Dags.AddNode(r.FormValue("dag_id"), r.FormValue("name"), desktopID, r.FormValue("content"))
	if errors.Is(err, dag.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such DAG")
		return
	}
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, node)
}

// updateDagNodeHandler changes a node's name, desktop or content.
func (s *Server) updateDagNodeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	desktopID := desktop.SanitizeID(strings.TrimSpace(r.FormValue("desktop_id")))

	if _, err := s.Desktops.Get(r.Context(), desktopID); err != nil {
		if errors.Is(err, desktop.ErrNotFound) {
			jsonStatusError(w, http.StatusBadRequest, "no such desktop")
			return
		}

		jsonError(w, err)
		return
	}

	node, err := s.Dags.UpdateNode(r.FormValue("id"), r.FormValue("name"), desktopID, r.FormValue("content"))
	if errors.Is(err, dag.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such node")
		return
	}
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, node)
}

// dagNodePositionHandler records where the editor's canvas placed a node -
// called once, on drag release, not on every pointer move.
func (s *Server) dagNodePositionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	x, errX := strconv.ParseFloat(r.FormValue("x"), 64)
	y, errY := strconv.ParseFloat(r.FormValue("y"), 64)

	if errX != nil || errY != nil {
		jsonStatusError(w, http.StatusBadRequest, "x and y must be numbers")
		return
	}

	err := s.Dags.SetNodePosition(r.FormValue("id"), x, y)
	if errors.Is(err, dag.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such node")
		return
	}
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{"status": "ok"})
}

// deleteDagNodeHandler removes a node from a DAG - refused while a run in
// progress still depends on it (see dag.Store.DeleteNode).
func (s *Server) deleteDagNodeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
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
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	d, err := s.Dags.SetHead(r.FormValue("dag_id"), r.FormValue("node_id"))
	if errors.Is(err, dag.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such node")
		return
	}
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, d)
}

// createDagEdgeHandler adds a directed edge between two of a DAG's nodes -
// rejected if it is a self-loop, an edge into the head, a duplicate, or
// would create a cycle.
func (s *Server) createDagEdgeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	edge, err := s.Dags.AddEdge(r.FormValue("dag_id"), r.FormValue("from"), r.FormValue("to"))
	if errors.Is(err, dag.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such node")
		return
	}
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, edge)
}

// deleteDagEdgeHandler removes an edge by ID.
func (s *Server) deleteDagEdgeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
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
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	d, err := s.Dags.SetCron(r.FormValue("dag_id"), r.FormValue("cron_expr"))
	if errors.Is(err, dag.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such DAG")
		return
	}
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, d)
}

// runDagHandler triggers a DAG's run immediately - the manual "ticket
// submit" door onto a run, alongside its own cron - and hands back the run
// with its head node already dispatched.
func (s *Server) runDagHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	run, err := s.DagRunner.Trigger(r.Context(), r.FormValue("id"), dag.TriggerManual)
	if errors.Is(err, dag.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such DAG")
		return
	}
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, run)
}

// dagRunsHandler lists a DAG's run history, most recent first.
func (s *Server) dagRunsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
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
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	detail, err := s.Dags.RunDetail(r.URL.Query().Get("id"))
	if errors.Is(err, dag.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such run")
		return
	}
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, detail)
}
