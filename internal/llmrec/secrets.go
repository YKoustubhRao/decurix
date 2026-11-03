package llmrec

import (
	"strings"
	"sync"
	"time"
)

// What SecretSet keeps per desktop, and for how long: enough for the values a person marks secret in one working
// day, never kept past it, and never written anywhere.
const (
	secretsPerDesktop = 64
	secretsTTL        = 24 * time.Hour
)

// SecretSet holds, in memory only, the values a person marked secret per desktop, so the call recorder withholds
// them from what that desktop's model calls carry afterwards. The zero value is ready to use.
type SecretSet struct {
	mu  sync.Mutex
	by  map[string][]heldSecret
	now func() time.Time
}

type heldSecret struct {
	value string
	at    time.Time
}

// Remember keeps values (of three characters or more) for desktop.
func (s *SecretSet) Remember(desktop string, values ...string) {
	if desktop == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string][]heldSecret{}
	}
	now := s.clock()
	held := s.fresh(desktop, now)
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < 3 {
			continue
		}
		kept := held[:0]
		for _, h := range held {
			if h.value != v {
				kept = append(kept, h)
			}
		}
		held = append(kept, heldSecret{value: v, at: now})
	}
	if len(held) > secretsPerDesktop {
		held = held[len(held)-secretsPerDesktop:]
	}
	s.by[desktop] = held
}

// For is the values held for desktop now.
func (s *SecretSet) For(desktop string) []string {
	if s == nil || desktop == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.fresh(desktop, s.clock())
	out := make([]string, len(held))
	for i, h := range held {
		out[i] = h.value
	}
	return out
}

// fresh is desktop's values younger than secretsTTL, with the older ones dropped. The caller holds mu.
func (s *SecretSet) fresh(desktop string, now time.Time) []heldSecret {
	held := s.by[desktop]
	kept := make([]heldSecret, 0, len(held))
	for _, h := range held {
		if now.Sub(h.at) < secretsTTL {
			kept = append(kept, h)
		}
	}
	if s.by != nil {
		s.by[desktop] = kept
	}
	return kept
}

func (s *SecretSet) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
