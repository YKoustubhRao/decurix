package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"thebot/internal/llm"
)

// SkillLibrary is acquired procedural memory: reusable recipes for
// problems TheBot has already solved once, so the same research or
// failure-recovery doesn't have to happen again from scratch. This is the
// positive counterpart to internal/feedback's negative-feedback store -
// "here's what worked", not just "here's what to avoid".
//
// A skill is NOT a new compiled capability - a running agent can't add a
// new Go type to itself. It's a stored PROCEDURE: the concrete steps that
// solved a specific class of problem, matched against future requests by
// meaning and reused directly instead of re-derived by the planner.
//
// Real skill files, not a hidden database blob - the user explicitly
// asked for something they (or the agent) can read and follow later.
// Markdown with a small frontmatter block lives at:
//
//	<workspaceRoot>/.skills/pending/<slug>.md    (proposed, not yet approved)
//	<workspaceRoot>/.skills/confirmed/<slug>.md  (approved, matched against requests)
//
// Every acquisition - success-path research or failure-path decomposed
// recovery - goes through pending first. Nothing is matched against
// future requests, and nothing runs automatically, until a human
// confirms it. A skill "working once" is not the same as a skill being
// SAFE to auto-reapply forever with no one ever looking at it again - a
// bad or malicious fix a web search turned up would look exactly like a
// good one at acquisition time.
type SkillLibrary struct {
	dir      string // <workspaceRoot>/.skills
	embedder llm.Embedder
	model    string

	mu    sync.Mutex
	cache map[string][]float32 // slug -> trigger embedding, confirmed only
}

type AcquiredSkill struct {
	Slug       string
	Trigger    string
	Steps      []string
	AcquiredAt time.Time
	Source     string // "research" | "decomposed-recovery"
	Score      float64
}

func NewSkillLibrary(workspaceRoot string, embedder llm.Embedder, model string) (*SkillLibrary, error) {
	dir := filepath.Join(workspaceRoot, ".skills")
	if err := os.MkdirAll(filepath.Join(dir, "pending"), 0o755); err != nil {
		return nil, fmt.Errorf("skills: create pending dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "confirmed"), 0o755); err != nil {
		return nil, fmt.Errorf("skills: create confirmed dir: %w", err)
	}
	return &SkillLibrary{dir: dir, embedder: embedder, model: model, cache: map[string][]float32{}}, nil
}

var slugSanitizeRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string, max int) string {
	slug := strings.Trim(slugSanitizeRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(slug) > max {
		slug = strings.Trim(slug[:max], "-")
	}
	if slug == "" {
		slug = "skill"
	}
	return slug
}

// Propose writes a new pending skill file - the honest, real proposal a
// user reviews before anything is trusted. Never overwrites a real match;
// a name collision gets a numeric suffix so two distinct proposals don't
// clobber each other.
func (s *SkillLibrary) Propose(trigger string, steps []string, source string) (*AcquiredSkill, error) {
	if trigger == "" || len(steps) == 0 {
		return nil, fmt.Errorf("skills: a proposal needs both a trigger and at least one real step")
	}
	base := slugify(trigger, 50)
	slug := base
	for i := 2; ; i++ {
		if _, err := os.Stat(s.pendingPath(slug)); os.IsNotExist(err) {
			break
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}

	skill := &AcquiredSkill{Slug: slug, Trigger: trigger, Steps: steps, AcquiredAt: time.Now().UTC(), Source: source}
	if err := writeSkillFile(s.pendingPath(slug), skill); err != nil {
		return nil, err
	}
	return skill, nil
}

// ListPending returns every real proposal sitting on disk awaiting a
// decision - nothing here has been trusted yet.
func (s *SkillLibrary) ListPending() ([]AcquiredSkill, error) {
	return listSkillFiles(filepath.Join(s.dir, "pending"))
}

// ListConfirmed returns every skill actually trusted and in use.
func (s *SkillLibrary) ListConfirmed() ([]AcquiredSkill, error) {
	return listSkillFiles(filepath.Join(s.dir, "confirmed"))
}

// Confirm moves a real pending proposal into confirmed use - the one
// point where a human's approval turns a proposal into something that
// gets automatically reapplied in the future.
func (s *SkillLibrary) Confirm(ctx context.Context, slug string) (*AcquiredSkill, error) {
	skill, err := readSkillFile(s.pendingPath(slug))
	if err != nil {
		return nil, fmt.Errorf("skills: no such pending skill %q: %w", slug, err)
	}
	if err := writeSkillFile(s.confirmedPath(slug), skill); err != nil {
		return nil, err
	}
	if err := os.Remove(s.pendingPath(slug)); err != nil {
		return nil, fmt.Errorf("skills: confirmed but could not clear the pending copy: %w", err)
	}
	if s.embedder != nil {
		if vec, err := s.embedder.Embed(ctx, "", skill.Trigger); err == nil {
			s.mu.Lock()
			s.cache[slug] = vec
			s.mu.Unlock()
		}
	}
	return skill, nil
}

// Reject discards a real pending proposal permanently - never partially
// trusted, never silently reapplied.
func (s *SkillLibrary) Reject(slug string) error {
	if err := os.Remove(s.pendingPath(slug)); err != nil {
		return fmt.Errorf("skills: no such pending skill %q: %w", slug, err)
	}
	return nil
}

// Match returns confirmed skills relevant to a request, most relevant
// first, above a real similarity floor - a weak, unrelated match reused
// as if it were a proven recipe would be worse than finding nothing.
const skillMatchThreshold = 0.55

func (s *SkillLibrary) Match(ctx context.Context, request string, topK int) ([]AcquiredSkill, error) {
	if s.embedder == nil {
		return nil, nil
	}
	confirmed, err := s.ListConfirmed()
	if err != nil {
		return nil, err
	}
	if len(confirmed) == 0 {
		return nil, nil
	}

	queryVec, err := s.embedder.Embed(ctx, "", request)
	if err != nil {
		return nil, fmt.Errorf("skills: embed request: %w", err)
	}

	var scored []AcquiredSkill
	for _, sk := range confirmed {
		vec := s.triggerEmbedding(ctx, sk)
		if vec == nil {
			continue
		}
		score := llm.CosineSimilarity(queryVec, vec)
		if score < skillMatchThreshold {
			continue
		}
		sk.Score = score
		scored = append(scored, sk)
	}

	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored, nil
}

// triggerEmbedding is cached per slug for the life of the process - a
// confirmed skill's trigger text doesn't change, so re-embedding it on
// every single match call would be pure waste.
func (s *SkillLibrary) triggerEmbedding(ctx context.Context, sk AcquiredSkill) []float32 {
	s.mu.Lock()
	if vec, ok := s.cache[sk.Slug]; ok {
		s.mu.Unlock()
		return vec
	}
	s.mu.Unlock()

	vec, err := s.embedder.Embed(ctx, "", sk.Trigger)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	s.cache[sk.Slug] = vec
	s.mu.Unlock()
	return vec
}

func (s *SkillLibrary) pendingPath(slug string) string {
	return filepath.Join(s.dir, "pending", slug+".md")
}
func (s *SkillLibrary) confirmedPath(slug string) string {
	return filepath.Join(s.dir, "confirmed", slug+".md")
}

// --- real file format: small frontmatter + a numbered recipe ---

var (
	frontmatterFieldRe = regexp.MustCompile(`(?m)^(\w+):\s*(.+)$`)
	skillStepLineRe    = regexp.MustCompile(`^\s*\d+[.)]\s*(.+)$`)
)

func writeSkillFile(path string, sk *AcquiredSkill) error {
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntrigger: %s\nacquired: %s\nsource: %s\n---\n\n",
		strings.ReplaceAll(sk.Trigger, "\n", " "), sk.AcquiredAt.Format(time.RFC3339), sk.Source)
	for i, step := range sk.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("skills: create dir: %w", err)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func readSkillFile(path string) (*AcquiredSkill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(raw)

	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		return nil, fmt.Errorf("skills: %s is not a valid skill file (missing frontmatter)", path)
	}
	front, body := parts[1], parts[2]

	sk := &AcquiredSkill{Slug: strings.TrimSuffix(filepath.Base(path), ".md")}
	for _, m := range frontmatterFieldRe.FindAllStringSubmatch(front, -1) {
		switch m[1] {
		case "trigger":
			sk.Trigger = strings.TrimSpace(m[2])
		case "source":
			sk.Source = strings.TrimSpace(m[2])
		case "acquired":
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(m[2])); err == nil {
				sk.AcquiredAt = t
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if m := skillStepLineRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			sk.Steps = append(sk.Steps, strings.TrimSpace(m[1]))
		}
	}
	if sk.Trigger == "" || len(sk.Steps) == 0 {
		return nil, fmt.Errorf("skills: %s has no usable trigger/steps", path)
	}
	return sk, nil
}

func listSkillFiles(dir string) ([]AcquiredSkill, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []AcquiredSkill
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		sk, err := readSkillFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // a hand-edited or corrupted file is skipped, not fatal
		}
		out = append(out, *sk)
	}
	return out, nil
}
