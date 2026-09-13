// Package conversation holds the in-memory chat transcript between a human and
// each desktop's agent: the finished messages both sides exchanged, plus the
// live step-by-step activity of whichever turn is running right now.
//
// It is the control plane's own record, not the agent's. A desktop's agent
// still keeps its own reasoning state inside the container (the Ollama loop's
// history list, Claude Code's session, TheBot's L1/L2); this is what the chat
// UI renders, shared by every browser pointed at the same control plane
// instead of living in one tab's localStorage.
package conversation

import (
	"fmt"
	"sync"
	"time"
)

// Roles a message can come from.
const (
	RoleUser  = "user"
	RoleAgent = "agent"
)

// Kinds an agent message can be: a normal answer, a request for human
// approval or help (the agent is blocked until a reply arrives), or a failure.
const (
	KindMessage  = "message"
	KindApproval = "approval"
	KindError    = "error"
)

// maxMessages and maxSteps bound how much of one desktop's chat is retained.
// The store is process-lifetime memory with no eviction of its own, so a
// long-lived desktop chatting all day must not grow without limit; the oldest
// entries are dropped once these are exceeded.
const (
	maxMessages = 200
	maxSteps    = 400
)

// lateStepWindow is how long after an agent's reply a step is still taken as
// part of the turn that reply closed. A desktop reports steps from a
// background queue but its reply directly, so the last line of a turn can
// land after the answer — measured on TheBot, under a millisecond after it.
// Opening a new turn for that line left the chat showing "Working…" forever
// over a finished task.
const lateStepWindow = 10 * time.Second

// Step is one line of live activity from the turn currently running: a shell
// command the agent chose, a file it wrote, a page it read. Every harness
// reports these through the same POST /api/agent/events endpoint, so the chat
// renders all three identically.
type Step struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// Message is one finished turn in the transcript. An agent message carries the
// Steps that produced it, so a completed turn can still be expanded to show
// the work behind the answer.
type Message struct {
	ID        string    `json:"id"`
	DesktopID string    `json:"desktop_id"`
	Role      string    `json:"role"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	Steps     []Step    `json:"steps,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Thread is a point-in-time snapshot of one desktop's chat, which is what both
// the JSON endpoint and each SSE frame send. Version increments on every
// change, letting a client cheaply tell whether anything actually moved.
type Thread struct {
	DesktopID string    `json:"desktop_id"`
	Messages  []Message `json:"messages"`
	Steps     []Step    `json:"steps"`
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Version   uint64    `json:"version"`
}

// thread is one desktop's mutable state. Its subscribers are notified (never
// blocked) whenever anything changes, so an SSE client can re-read a snapshot.
type thread struct {
	messages    []Message
	steps       []Step
	running     bool
	startedAt   time.Time
	version     uint64
	subscribers map[chan struct{}]bool
}

// Store is an in-memory, mutex-guarded set of per-desktop chat threads.
type Store struct {
	mu      sync.Mutex
	threads map[string]*thread
	seq     uint64
}

// NewStore creates an empty conversation Store.
func NewStore() *Store {
	return &Store{
		threads: make(map[string]*thread),
	}
}

// threadFor returns the thread for a desktop, creating it on first use. The
// caller must hold the lock.
func (s *Store) threadFor(desktopID string) *thread {
	existing, ok := s.threads[desktopID]
	if ok {
		return existing
	}

	created := &thread{
		subscribers: make(map[chan struct{}]bool),
	}

	s.threads[desktopID] = created

	return created
}

// nextID returns a unique identifier for a message or step. The caller must
// hold the lock. A plain timestamp is not enough on its own: several steps can
// land within the same nanosecond tick, and the UI keys on these.
func (s *Store) nextID() string {
	s.seq++

	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), s.seq)
}

// touch bumps the thread's version and wakes its subscribers. The caller must
// hold the lock.
func (t *thread) touch() {
	t.version++

	for subscriber := range t.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
			// Already has an unread wake-up pending; one is enough,
			// since a subscriber re-reads the whole snapshot anyway.
		}
	}
}

// AppendUserMessage records something the human sent to a desktop and opens a
// new turn: the agent is now considered busy, and the steps it reports from
// here on belong to this turn.
func (s *Store) AppendUserMessage(desktopID, content string) Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	message := Message{
		ID:        s.nextID(),
		DesktopID: desktopID,
		Role:      RoleUser,
		Kind:      KindMessage,
		Content:   content,
		CreatedAt: time.Now().UTC(),
	}

	current.messages = appendCapped(current.messages, message, maxMessages)
	current.steps = nil
	current.running = true
	current.startedAt = message.CreatedAt

	current.touch()

	return message
}

// AppendAgentMessage records the agent's reply and closes the current turn,
// attaching the steps it took along the way to the message itself.
func (s *Store) AppendAgentMessage(desktopID, kind, content string) Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	switch kind {
	case KindMessage, KindApproval, KindError:
	default:
		kind = KindMessage
	}

	message := Message{
		ID:        s.nextID(),
		DesktopID: desktopID,
		Role:      RoleAgent,
		Kind:      kind,
		Content:   content,
		Steps:     current.steps,
		CreatedAt: time.Now().UTC(),
	}

	current.messages = appendCapped(current.messages, message, maxMessages)
	current.steps = nil
	current.running = false
	current.startedAt = time.Time{}

	current.touch()

	return message
}

// AppendStep records one line of live activity for the running turn. A step
// arriving while nothing is marked running still opens a turn, so work a
// desktop started on its own is visible rather than silently dropped — unless
// it trails a turn that only just closed, in which case it belongs to that one.
func (s *Store) AppendStep(desktopID, text string) Step {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	step := Step{
		ID:        s.nextID(),
		Text:      text,
		CreatedAt: time.Now().UTC(),
	}

	if last := current.lastMessage(); !current.running && last != nil &&
		last.Role == RoleAgent && step.CreatedAt.Sub(last.CreatedAt) < lateStepWindow {
		last.Steps = appendCapped(last.Steps, step, maxSteps)

		current.touch()

		return step
	}

	if !current.running {
		current.running = true
		current.startedAt = step.CreatedAt
	}

	current.steps = appendCapped(current.steps, step, maxSteps)

	current.touch()

	return step
}

// Snapshot returns a copy of one desktop's thread, safe for a caller to hold
// and serialize after the lock is released.
func (s *Store) Snapshot(desktopID string) Thread {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	messages := make([]Message, len(current.messages))
	copy(messages, current.messages)

	steps := make([]Step, len(current.steps))
	copy(steps, current.steps)

	return Thread{
		DesktopID: desktopID,
		Messages:  messages,
		Steps:     steps,
		Running:   current.running,
		StartedAt: current.startedAt,
		Version:   current.version,
	}
}

// Clear discards one desktop's transcript, including any turn it currently
// believes is running.
func (s *Store) Clear(desktopID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	current.messages = nil
	current.steps = nil
	current.running = false
	current.startedAt = time.Time{}

	current.touch()
}

// Subscribe returns a channel that receives a wake-up whenever a desktop's
// thread changes, plus the function to call to stop listening. The channel is
// buffered and sent to non-blocking, so a slow reader never stalls a writer.
func (s *Store) Subscribe(desktopID string) (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	updates := make(chan struct{}, 1)
	current.subscribers[updates] = true

	return updates, func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		delete(current.subscribers, updates)
	}
}

// lastMessage returns the thread's most recent message, or nil for an empty
// thread. The caller must hold the lock.
func (t *thread) lastMessage() *Message {
	if len(t.messages) == 0 {
		return nil
	}

	return &t.messages[len(t.messages)-1]
}

// appendCapped appends an item, dropping the oldest entries once the slice
// would exceed max.
func appendCapped[T any](items []T, item T, max int) []T {
	items = append(items, item)

	if len(items) > max {
		items = items[len(items)-max:]
	}

	return items
}
