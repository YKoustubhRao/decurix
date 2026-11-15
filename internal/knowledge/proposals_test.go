package knowledge

import (
	"errors"
	"testing"
)

var owner = Decider{ID: "local-owner", Name: "Owner (this computer)"}

// proposal is a well-formed proposal of worker w from task t.
func proposal(text string) Proposal {
	return Proposal{Worker: "w1", TaskID: "t_1", Kind: ProposalLesson, Text: text, Why: "the person said so",
		Evidence: []Evidence{{Quote: "always put the region first", Where: EvidencePerson}}, Model: "m"}
}

func TestAProposalIsStagedAndNeverGivenOut(t *testing.T) {
	s := newStore(t)
	p, err := s.Propose(proposal("The region column comes first in a summary."), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateProposed || p.ID == "" || len(p.Evidence) != 1 {
		t.Fatalf("proposal = %+v", p)
	}
	given, err := s.ForWorker("w1", nil)
	if err != nil || len(given) != 0 {
		t.Fatalf("a proposal was given to the worker: %v %v", given, err)
	}
	list, err := s.Proposals(StateProposed, 0)
	if err != nil || len(list) != 1 || list[0].ID != p.ID || list[0].Evidence[0].Where != EvidencePerson {
		t.Fatalf("listed = %+v %v", list, err)
	}
}

func TestApprovalMakesAnItemAuthoredByTheApprover(t *testing.T) {
	s := newStore(t)
	p, _ := s.Propose(proposal("The region column comes first in a summary."), nil)

	if _, _, err := s.Approve(p.ID, Decider{}, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an approval by nobody: %v", err)
	}
	it, done, err := s.Approve(p.ID, owner, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if it.Scope != ScopeWorker || it.ScopeKey != "w1" || it.Kind != KindLesson || it.Source.Kind != SourcePerson ||
		it.Source.Ref != "local-owner" || it.Source.Worker != "w1" || it.Source.TaskID != "t_1" {
		t.Fatalf("item = %+v", it)
	}
	if done.State != StateApproved || done.DecidedBy != "local-owner" || done.ItemID != it.ID || done.DecidedAt == nil {
		t.Fatalf("proposal after approval = %+v", done)
	}
	given, _ := s.ForWorker("w1", nil)
	if len(given) != 1 || given[0].ID != it.ID {
		t.Fatalf("the approved item is not given: %+v", given)
	}
	if _, _, err := s.Approve(p.ID, owner, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a second approval: %v", err)
	}
	if _, err := s.Reject(p.ID, owner, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rejecting an approved proposal: %v", err)
	}
}

func TestApprovalScopes(t *testing.T) {
	s := newStore(t)
	note := proposal("Invoices are saved before they are printed.")
	note.Kind = ProposalAppNote
	p, _ := s.Propose(note, nil)
	if _, _, err := s.Approve(p.ID, owner, ScopeWorker, "w2"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("approved for another worker: %v", err)
	}
	if _, _, err := s.Approve(p.ID, owner, ScopeTask, "t_1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("approved for a task: %v", err)
	}
	it, _, err := s.Approve(p.ID, owner, ScopeRole, "clerk")
	if err != nil || it.Scope != ScopeRole || it.ScopeKey != "clerk" || it.Kind != KindFact {
		t.Fatalf("role approval = %+v %v", it, err)
	}
}

func TestRejectionKeepsTheRowAndRefusesTheSameTextAgain(t *testing.T) {
	s := newStore(t)
	p, _ := s.Propose(proposal("Reports are sent before noon."), nil)
	got, err := s.Reject(p.ID, owner, "a one-off")
	if err != nil || got.State != StateRejected || got.Note != "a one-off" || got.DecidedBy != "local-owner" {
		t.Fatalf("rejected = %+v %v", got, err)
	}
	if all, _ := s.Proposals("", 0); len(all) != 1 {
		t.Fatalf("a rejected proposal is gone: %+v", all)
	}
	again := proposal("reports are sent  before noon.")
	again.TaskID = "t_2"
	if _, err := s.Propose(again, nil); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a rejected text proposed again: %v", err)
	}
}

func TestProposeRefusesDuplicatesBadShapesAndTooMany(t *testing.T) {
	s := newStore(t)
	mustAdd(t, s, ScopeOrg, "", KindRule, "Never pay without a purchase order.")
	if _, err := s.Propose(proposal("never pay without a purchase order."), nil); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a duplicate of a given item: %v", err)
	}
	if _, err := s.Propose(proposal("One."), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(proposal("One."), nil); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a duplicate of a waiting proposal: %v", err)
	}
	for _, text := range []string{"Two.", "Three."} {
		if _, err := s.Propose(proposal(text), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Propose(proposal("Four."), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a fourth proposal of one run: %v", err)
	}

	bad := []Proposal{
		{Worker: "w1", TaskID: "t_9", Kind: "rule", Text: "x", Evidence: []Evidence{{Quote: "q", Where: EvidenceRun}}},
		{Worker: "w1", TaskID: "t_9", Kind: ProposalLesson, Text: "x"},
		{Worker: "w1", TaskID: "t_9", Kind: ProposalLesson, Text: "x", Evidence: []Evidence{{Quote: "q", Where: "model"}}},
		{Worker: "", TaskID: "t_9", Kind: ProposalLesson, Text: "x", Evidence: []Evidence{{Quote: "q", Where: EvidenceRun}}},
		{Worker: "w1", TaskID: "t_9", Kind: ProposalLesson, Text: "x\x01", Evidence: []Evidence{{Quote: "q", Where: EvidenceRun}}},
	}
	for i, p := range bad {
		if _, err := s.Propose(p, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad proposal %d: %v", i, err)
		}
	}
}

func TestAReplacingProposalQuotesALiveItemExactly(t *testing.T) {
	s := newStore(t)
	old := mustAdd(t, s, ScopeRole, "clerk", KindRule, "Totals are shown with two decimals.")

	wrong := proposal("Totals are shown as whole units.")
	wrong.ReplacesText = "Totals are shown with 2 decimals."
	if _, err := s.Propose(wrong, []string{"clerk"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a replaces that quotes nothing: %v", err)
	}
	// Not one of this worker's lines: another role's item is not found either.
	right := proposal("Totals are shown as whole units.")
	right.ReplacesText = "Totals are  shown with two decimals."
	if _, err := s.Propose(right, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a replaces of a line the worker is not given: %v", err)
	}
	p, err := s.Propose(right, []string{"clerk"})
	if err != nil || p.ReplacesID != old.ID {
		t.Fatalf("proposal = %+v %v", p, err)
	}
	it, _, err := s.Approve(p.ID, owner, ScopeOrg, "") // the scope asked for gives way to the replaced item's
	if err != nil || it.Scope != ScopeRole || it.ScopeKey != "clerk" {
		t.Fatalf("approved = %+v %v", it, err)
	}
	now, chain, err := s.Why(it.ID)
	if err != nil || now.ID != it.ID || len(chain) != 1 || chain[0].ID != old.ID {
		t.Fatalf("why = %+v %+v %v", now, chain, err)
	}
}

func TestAnApprovalIsRefusedWhenTheReplacedItemChanged(t *testing.T) {
	s := newStore(t)
	old := mustAdd(t, s, ScopeWorker, "w1", KindLesson, "Reports go out on Fridays.")
	p := proposal("Reports go out on Mondays.")
	p.ReplacesText = "Reports go out on Fridays."
	staged, err := s.Propose(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A person replaced the item meanwhile.
	if _, err := s.Supersede(old.ID, Item{Text: "Reports go out on Thursdays.", Source: person}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Approve(staged.ID, owner, "", ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("approval after the item changed: %v", err)
	}
	still, _ := s.GetProposal(staged.ID)
	if still.State != StateProposed {
		t.Fatalf("a refused approval changed the proposal: %+v", still)
	}
	given, _ := s.ForWorker("w1", nil)
	if len(given) != 1 || given[0].Text != "Reports go out on Thursdays." {
		t.Fatalf("given = %+v", given)
	}

	// Retired meanwhile: changed too.
	old2 := mustAdd(t, s, ScopeWorker, "w1", KindLesson, "Drafts are kept for a week.")
	p2 := proposal("Drafts are kept for a month.")
	p2.TaskID, p2.ReplacesText = "t_2", "Drafts are kept for a week."
	staged2, err := s.Propose(p2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Retire(old2.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Approve(staged2.ID, owner, "", ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("approval after the item was retired: %v", err)
	}
}
