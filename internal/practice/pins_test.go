package practice

import (
	"context"
	"errors"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/qualification"
)

// resolvingChat is the hub as the pins see it: chats, and names the deployment behind each label now.
type resolvingChat struct {
	*fakeChat
	deployments map[string]string // "provider/label" -> deployment
}

func (r *resolvingChat) Resolve(provider, asked string) (string, error) {
	d, ok := r.deployments[provider+"/"+asked]
	if !ok {
		return "", errors.New("not configured")
	}
	return d, nil
}

func resolving(s *Service, chat *fakeChat) *resolvingChat {
	r := &resolvingChat{fakeChat: chat, deployments: map[string]string{
		"azure/default": "gpt-big-1", "azure/fast": "gpt-small-1", "claude/default": "claude-big", "claude/fast": "claude-small"}}
	s.LLM = r
	return r
}

func TestPinsNameTheDeploymentsThatServeNow(t *testing.T) {
	s, chat, _, _, _ := service(t)
	resolving(s, chat)
	p, err := s.Pins(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if p.Deployment != "gpt-big-1" || p.FastDeployment != "gpt-small-1" {
		t.Fatalf("pins: %+v", p)
	}
	q := p
	if !samePins(p, q) {
		t.Fatal("equal pins differ")
	}
	q.Deployment = "gpt-big-2"
	if samePins(p, q) {
		t.Fatal("a changed deployment is the same worker")
	}
	q = p
	q.FastDeployment = "gpt-small-2"
	if samePins(p, q) {
		t.Fatal("a changed fast deployment is the same worker")
	}
}

func TestADeploymentChangeNeedsRetesting(t *testing.T) {
	s, chat, _, _, _ := service(t)
	hub := resolving(s, chat)
	su := approved(t, s)
	if su.Pins.Deployment != "gpt-big-1" {
		t.Fatalf("the approved suite pinned %+v", su.Pins)
	}
	if err := s.CheckCurrent(context.Background(), su, su.Source.SHA256); err != nil {
		t.Fatalf("unchanged: %v", err)
	}
	hub.deployments["azure/default"] = "gpt-big-2"
	if err := s.CheckCurrent(context.Background(), su, su.Source.SHA256); !errors.Is(err, ErrStale) {
		t.Fatalf("a changed deployment is not stale: %v", err)
	}
	r, err := s.Store.GetPractice(su.ResponsibilityID)
	if err != nil || r.Readiness != qualification.NeedsRetesting {
		t.Fatalf("responsibility: %+v %v", r, err)
	}
}

func TestBrainChangedInvalidatesOnlyThatProvidersSuites(t *testing.T) {
	s, chat, _, _, _ := service(t)
	resolving(s, chat)
	azure := approved(t, s)
	workers := s.Workers.(*fakeWorkers)
	workers.brain = "claude"
	src := officeSource()
	src.Name, src.Title = "total-other", "Total another file"
	src.Files = map[string][]byte{".decurix/skills/total-other.json": []byte(`{"name":"total-other"}`)}
	other, err := s.Draft(context.Background(), DraftRequest{WorkerID: "worker", Source: src, Count: 5}, nil)
	if err != nil || len(other.Problems) != 0 {
		t.Fatalf("draft: %v %v", err, other.Problems)
	}
	if _, err := s.Store.ApproveSuite(other.ID, other.GeneratorSHA, other.MockSHA, other.CasesSHA); err != nil {
		t.Fatal(err)
	}
	if other.Pins.Brain != "claude" {
		t.Fatalf("the second suite pinned %+v", other.Pins)
	}

	s.BrainChanged("azure")

	a, _ := s.Store.GetPractice(azure.ResponsibilityID)
	c, _ := s.Store.GetPractice(other.ResponsibilityID)
	if a.Readiness != qualification.NeedsRetesting || a.Reason != brainChangedReason {
		t.Fatalf("the azure suite was not invalidated: %+v", a)
	}
	if c.Readiness == qualification.NeedsRetesting {
		t.Fatalf("the claude suite was invalidated: %+v", c)
	}
	s.BrainChanged("") // nothing named, nothing done
	s.BrainChanged("claude")
	if c, _ = s.Store.GetPractice(other.ResponsibilityID); c.Readiness != qualification.NeedsRetesting {
		t.Fatalf("the claude suite was not invalidated: %+v", c)
	}
}

func TestPinsWithoutAResolverStayAsBefore(t *testing.T) {
	s, _, _, _, _ := service(t)
	p, err := s.Pins(context.Background(), "worker")
	if err != nil || p.Deployment != "" || p.FastDeployment != "" {
		t.Fatalf("pins: %+v %v", p, err)
	}
}
