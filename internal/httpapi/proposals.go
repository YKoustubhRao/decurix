package httpapi

// What a worker's review pass proposes after a run, staged for a person (docs/learning-loop.md "The review pass",
// docs/skillhub.md "Proposed by workers"). A desktop posts its proposals for one of its own tasks on its callback
// route (primary listener, allowlisted in containerRoutes, acting only as itself); a person lists them and approves or
// rejects each on the Workforce page (primary listener, humansOnly, O-7). Nothing is given to any worker until a
// person approves it, and the approval is a judgement with that person as its author (plan rule 5).

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/knowledge"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// proposalBody is one proposal as a desktop posts it.
type proposalBody struct {
	Kind     string               `json:"kind"`
	Text     string               `json:"text"`
	Why      string               `json:"why"`
	Evidence []knowledge.Evidence `json:"evidence"`
	Replaces string               `json:"replaces"`
}

// proposalRefused is one proposal of a post that was not staged, and why.
type proposalRefused struct {
	Index int    `json:"index"`
	Why   string `json:"why"`
}

// desktopProposalsHandler stages the proposals a desktop's review pass made for one of its own tasks (POST
// /api/desktops/proposals {task_id, call_id?, model?, proposals: [...]}). Each is checked again here: a quote said to
// be the person's must stand in a person's own words on that task (or its request), and nothing holding a value the
// person marked secret, or anything shaped like a secret, is kept. Answers {kept: [{index, id}], refused: [{index,
// why}]}.
func (s *Server) desktopProposalsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	worker, fromDesktop := requestDesktop(r)
	if !fromDesktop {
		jsonStatusError(w, http.StatusForbidden, "only a desktop proposes what it learned")
		return
	}
	if s.Knowledge == nil || s.Tasks == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "proposals are not kept on this control plane")
		return
	}
	var body struct {
		TaskID    string         `json:"task_id"`
		CallID    string         `json:"call_id"`
		Model     string         `json:"model"`
		Proposals []proposalBody `json:"proposals"`
	}
	limitJSON(w, r)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		badJSON(w, err)
		return
	}
	if !tasks.ValidID(body.TaskID) || len(body.Proposals) == 0 || len(body.Proposals) > knowledge.MaxProposalsPerRun {
		jsonStatusError(w, http.StatusBadRequest, "say the task and up to 3 proposals")
		return
	}
	if owner, ok := s.Tasks.TaskWorker(body.TaskID); !ok || owner != worker {
		jsonStatusError(w, http.StatusForbidden, "a desktop proposes from its own tasks only")
		return
	}
	said, err := s.personWords(body.TaskID)
	if err != nil {
		internalError(w, "proposals: reading the person's words", err)
		return
	}
	held := s.heldFor(worker)
	roles := s.workerRoles(worker)

	kept := []map[string]any{}
	refused := []proposalRefused{}
	for i, p := range body.Proposals {
		if why := proposalProblem(p, said, held); why != "" {
			refused = append(refused, proposalRefused{Index: i, Why: why})
			continue
		}
		staged, err := s.Knowledge.Propose(knowledge.Proposal{Worker: worker, TaskID: body.TaskID, Kind: p.Kind,
			Text: p.Text, Why: p.Why, Evidence: p.Evidence, ReplacesText: p.Replaces, CallID: body.CallID,
			Model: body.Model}, roles)
		switch {
		case err == nil:
			kept = append(kept, map[string]any{"index": i, "id": staged.ID})
		case errors.Is(err, knowledge.ErrInvalid), errors.Is(err, knowledge.ErrDuplicate):
			// The store's own words for a person.
			refused = append(refused, proposalRefused{Index: i, Why: strings.TrimPrefix(refusal(err), knowledge.ErrInvalid.Error()+": ")})
		default:
			log.Printf("proposals: staging one of %s: %v", worker, err)
			refused = append(refused, proposalRefused{Index: i, Why: "it could not be kept"})
		}
	}
	jsonResponseStatus(w, http.StatusOK, map[string]any{"kept": kept, "refused": refused})
}

// personWords is what a person said on task: their own judgements' texts (not derived, not secret), and the task's
// request.
func (s *Server) personWords(task string) ([]string, error) {
	rows, err := s.Tasks.JudgementsForTask(task)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, j := range rows {
		if (j.Actor.Kind == tasks.ActorPerson || j.Actor.Kind == tasks.ActorScripted) && j.DerivedFrom == "" && !j.Secret {
			out = append(out, j.Text)
		}
	}
	if d, err := s.Tasks.Get(task); err == nil {
		out = append(out, d.Request)
	}
	return out, nil
}

// proposalProblem is why a proposal is not staged, "" when nothing here stands against it: a quote said to be the
// person's that is not in their words, or any part holding a secret.
func proposalProblem(p proposalBody, said, held []string) string {
	parts := []string{p.Text, p.Why, p.Replaces}
	for _, e := range p.Evidence {
		parts = append(parts, e.Quote)
		if e.Where != knowledge.EvidencePerson {
			continue
		}
		quote := strings.Join(strings.Fields(e.Quote), " ")
		found := false
		for _, words := range said {
			if quote != "" && strings.Contains(strings.Join(strings.Fields(words), " "), quote) {
				found = true
				break
			}
		}
		if !found {
			return "a quote given as the person's is not in their words on this task"
		}
	}
	for _, part := range parts {
		if part != "" && (llmrec.WithholdValues(part, held) != part || withhold.Text(part) != part) {
			return "it holds a secret, so it is not kept"
		}
	}
	return ""
}

// proposalsListHandler lists the proposals (GET /api/knowledge/proposals?state=proposed|approved|rejected|all; the
// waiting ones by default), newest first, each with the request of the run it came from.
func (s *Server) proposalsListHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	if s.Knowledge == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "knowledge is not kept on this control plane")
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	switch state {
	case "":
		state = knowledge.StateProposed
	case "all":
		state = ""
	case knowledge.StateProposed, knowledge.StateApproved, knowledge.StateRejected:
	default:
		jsonStatusError(w, http.StatusBadRequest, "state is proposed, approved, rejected or all")
		return
	}
	list, err := s.Knowledge.Proposals(state, 200)
	if err != nil {
		internalError(w, "proposals: listing", err)
		return
	}
	requests := map[string]string{}
	if s.Tasks != nil {
		for _, p := range list {
			if _, seen := requests[p.TaskID]; seen {
				continue
			}
			requests[p.TaskID] = ""
			if d, err := s.Tasks.Get(p.TaskID); err == nil {
				requests[p.TaskID] = cutRunes(d.Request, 300)
			}
		}
	}
	jsonResponseStatus(w, http.StatusOK, map[string]any{"proposals": list, "requests": requests})
}

// cutRunes is s cut to at most n characters, with an ellipsis when cut.
func cutRunes(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// proposalsDecideHandler approves (POST /api/knowledge/proposals/approve {id, scope?, scope_key?}) or rejects (POST
// /api/knowledge/proposals/reject {id, note?}) a waiting proposal, as the person making the request, and keeps the
// decision as a judgement of theirs on the run's task.
func (s *Server) proposalsDecideHandler(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		if s.Knowledge == nil {
			jsonStatusError(w, http.StatusServiceUnavailable, "knowledge is not kept on this control plane")
			return
		}
		actor, ok := s.actorOf(r)
		if !ok || actor.Kind != tasks.ActorPerson {
			jsonStatusError(w, http.StatusForbidden, "only a person decides what a worker learns")
			return
		}
		var body struct {
			ID       string `json:"id"`
			Scope    string `json:"scope"`
			ScopeKey string `json:"scope_key"`
			Note     string `json:"note"`
		}
		limitJSON(w, r)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			badJSON(w, err)
			return
		}
		by := knowledge.Decider{ID: actor.ID, Name: actor.Name}
		var (
			p    knowledge.Proposal
			item knowledge.Item
			err  error
		)
		if approve {
			item, p, err = s.Knowledge.Approve(strings.TrimSpace(body.ID), by, body.Scope, body.ScopeKey)
		} else {
			p, err = s.Knowledge.Reject(strings.TrimSpace(body.ID), by, body.Note)
		}
		switch {
		case err == nil:
		case errors.Is(err, knowledge.ErrChanged):
			jsonStatusError(w, http.StatusConflict, "it changed since this was proposed")
			return
		case errors.Is(err, knowledge.ErrNotFound):
			jsonStatusError(w, http.StatusNotFound, "no such proposal")
			return
		case errors.Is(err, knowledge.ErrInvalid):
			jsonStatusError(w, http.StatusBadRequest, strings.TrimPrefix(refusal(err), knowledge.ErrInvalid.Error()+": "))
			return
		default:
			internalError(w, "proposals: deciding", err)
			return
		}
		s.keepProposalDecision(actor, p, item)
		answer := map[string]any{"proposal": p}
		if approve {
			answer["item"] = item
		}
		jsonResponseStatus(w, http.StatusOK, answer)
	}
}

// keepProposalDecision records a person's decision on a proposal as their judgement on the run's task (plan R5). The
// decision itself is already kept with the proposal and, for an approval, as the item's source; a failure here is
// logged, never undone.
func (s *Server) keepProposalDecision(actor tasks.Actor, p knowledge.Proposal, item knowledge.Item) {
	if s.Tasks == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"proposal": p.ID, "decision": p.State, "item": item.ID,
		"scope": item.Scope, "scope_key": item.ScopeKey, "note": p.Note})
	if _, err := s.Tasks.AddJudgement(tasks.Judgement{Actor: actor, Door: "workforce", Kind: tasks.JudgementKnowledge,
		WorkerID: p.Worker, TaskID: p.TaskID, Text: p.Text, Payload: payload}); err != nil {
		log.Printf("proposals: keeping the decision on %s as a judgement: %v", p.ID, err)
	}
}
