package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// waitingTask is a task on worker that stopped with a question (kind query or approval), and the route a reply to it
// takes.
func waitingTask(t *testing.T, s *Store, worker, kind string, secret bool) Route {
	t.Helper()
	w := Worker{ID: worker, Name: worker, Engine: "thebot"}
	route, err := s.Commit(NewRoute(), w, "file the bills", Origin{Kind: OriginPerson})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordOutcome(worker, route.TaskID, Outcome{Kind: kind, Reply: "Which ledger?", Secret: secret}); err != nil {
		t.Fatal(err)
	}
	reply, err := s.Plan(worker, "the purchases ledger")
	if err != nil || reply.Kind != RouteReply {
		t.Fatalf("plan = %+v, %v", reply, err)
	}
	return reply
}

func TestAReplyIsAJudgementByItsActor(t *testing.T) {
	s := openStore(t, newClock())
	reply := waitingTask(t, s, "box", OutcomeQuery, false)
	reply.JudgementID = NewJudgementID()
	operator := Actor{ID: "u_1", Name: "Asha", Roles: []string{"operator"}, Kind: ActorPerson}
	got, stored, err := s.CommitReply(reply, Worker{ID: "box", Name: "box"}, "the purchases ledger", false,
		ReplyBy{Actor: operator, Door: "chat"}, Origin{Kind: OriginPerson})
	if err != nil {
		t.Fatal(err)
	}
	if got.JudgementID != reply.JudgementID {
		t.Fatalf("judgement id %q, want the one chosen before delivery %q", got.JudgementID, reply.JudgementID)
	}
	j, ok, err := s.Judgement(got.JudgementID)
	if err != nil || !ok {
		t.Fatalf("judgement: %v %v", ok, err)
	}
	if j.Kind != JudgementAnswer || j.Actor.ID != "u_1" || j.Actor.Roles[0] != "operator" || j.Door != "chat" ||
		j.TaskID != reply.TaskID || j.WorkerID != "box" || j.Text != stored || j.InterventionID == "" {
		t.Fatalf("judgement = %+v", j)
	}
	d, _ := s.Get(reply.TaskID)
	iv := d.Executions[0].Interventions[0]
	if iv.ReplierID != "u_1" || iv.JudgementID != j.ID {
		t.Fatalf("intervention = %+v", iv)
	}
}

func TestASecretAnswersJudgementIsWithheldWhole(t *testing.T) {
	s := openStore(t, newClock())
	reply := waitingTask(t, s, "box", OutcomeQuery, true)
	got, _, err := s.CommitReply(reply, Worker{ID: "box", Name: "box"}, "Plum-Orbit-7731", false,
		ReplyBy{Actor: LocalOwner, Door: "card"}, Origin{Kind: OriginPerson})
	if err != nil {
		t.Fatal(err)
	}
	j, _, _ := s.Judgement(got.JudgementID)
	if j.Text != withhold.Secret || !j.Secret {
		t.Fatalf("a secret answer's judgement = %+v", j)
	}

	// A message the person marked secret is withheld whole too.
	reply = waitingTask(t, s, "eve", OutcomeQuery, false)
	got, _, _ = s.CommitReply(reply, Worker{ID: "eve", Name: "eve"}, "Zq9 plum", true, ReplyBy{Actor: LocalOwner}, Origin{Kind: OriginPerson})
	j, _, _ = s.Judgement(got.JudgementID)
	if j.Text != withhold.Secret || !j.Secret {
		t.Fatalf("a marked message's judgement = %+v", j)
	}
}

func TestAnApprovalReplyCarriesTheAccessDecision(t *testing.T) {
	s := openStore(t, newClock())
	reply := waitingTask(t, s, "box", OutcomeApproval, false)
	got, _, err := s.CommitReply(reply, Worker{ID: "box", Name: "box"}, "yes", false, ReplyBy{Actor: LocalOwner,
		Door: "concierge", Approval: map[string]any{"approval_id": "a_1", "outcome": "allowed", "decision": "yes"}}, Origin{Kind: OriginPerson})
	if err != nil {
		t.Fatal(err)
	}
	j, _, _ := s.Judgement(got.JudgementID)
	var payload map[string]string
	_ = json.Unmarshal(j.Payload, &payload)
	if j.Kind != JudgementApproval || payload["approval_id"] != "a_1" || payload["outcome"] != "allowed" {
		t.Fatalf("approval judgement = %+v (%s)", j, j.Payload)
	}
}

func TestNoJudgementWithoutAnActor(t *testing.T) {
	s := openStore(t, newClock())
	if _, err := s.AddJudgement(Judgement{Kind: JudgementLesson, Text: "x"}); err != ErrNoActor {
		t.Fatalf("AddJudgement without an actor = %v", err)
	}
	if _, err := s.AddJudgement(Judgement{Kind: "rumour", Actor: LocalOwner}); err == nil {
		t.Fatal("an unknown kind was stored")
	}
	// A reply recorded with no actor (no door sends one) keeps the reply and makes no judgement.
	reply := waitingTask(t, s, "box", OutcomeQuery, false)
	got, _, err := s.CommitReply(reply, Worker{ID: "box", Name: "box"}, "ok", false, ReplyBy{}, Origin{Kind: OriginPerson})
	if err != nil || got.JudgementID != "" {
		t.Fatalf("a reply without an actor = %+v, %v", got, err)
	}
	if list, _ := s.JudgementsForTask(reply.TaskID); len(list) != 0 {
		t.Fatalf("judgements = %+v", list)
	}
	if _, err := s.SetVerdict(reply.TaskID, VerdictCorrect, "", Actor{}); err != ErrNoActor {
		t.Fatalf("a verdict without an actor = %v", err)
	}
}

func TestAVerdictIsAJudgementAndOutlivesItsWorker(t *testing.T) {
	s := openStore(t, newClock())
	route, _ := s.Commit(NewRoute(), Worker{ID: "box", Name: "box"}, "count the bills", Origin{Kind: OriginPerson})
	_, _ = s.RecordOutcome("box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "12"})
	if _, err := s.SetVerdict(route.TaskID, VerdictWrong, "it is 13; password: Hunter2Hunter2", LocalOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorkerDeleted("box"); err != nil {
		t.Fatal(err)
	}
	list, err := s.JudgementsForWorker("box")
	if err != nil || len(list) != 1 {
		t.Fatalf("judgements of a deleted worker = %+v, %v", list, err)
	}
	j := list[0]
	if j.Kind != JudgementVerdict || j.Actor.ID != LocalOwner.ID || j.ExecutionID == "" || strings.Contains(j.Text, "Hunter2") ||
		!strings.Contains(string(j.Payload), `"verdict":"wrong"`) {
		t.Fatalf("verdict judgement = %+v", j)
	}
}

// Nothing anywhere in the control plane changes or removes a judgement: no UPDATE or DELETE names the table.
func TestJudgementsAreAppendOnly(t *testing.T) {
	bad := regexp.MustCompile(`(?i)(UPDATE\s+judgements|DELETE\s+FROM\s+judgements|DROP\s+TABLE\s+judgements|INSERT\s+OR\s+REPLACE\s+INTO\s+judgements|REPLACE\s+INTO\s+judgements)`)
	inserts := 0
	root := filepath.Join("..")
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if loc := bad.FindIndex(b); loc != nil {
			t.Errorf("%s changes or removes judgements: %q", p, b[loc[0]:loc[1]])
		}
		inserts += strings.Count(string(b), "INSERT INTO judgements")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserts != 1 {
		t.Fatalf("%d statements insert into judgements, want 1 (insertJudgement)", inserts)
	}
}
