package llm

import (
	"encoding/json"
	"fmt"
	"time"
)

// Spend is metered where it is spent (review 2026-11-03, C-08): every hosted call passes through this proxy, which
// knows the calling desktop and reads the usage the provider reports. The spend limits read this, not only what a
// desktop says it spent when a turn ends - a figure that is empty until then, and one the worker writes itself.

// Usage is what one answer says it used.
type Usage struct {
	Input  int64
	Output int64
}

// Tokens is the answer's tokens in all.
func (u Usage) Tokens() int64 { return u.Input + u.Output }

// ParseUsage reads an OpenAI-shaped answer's usage (every provider here answers in that shape); an answer without one
// used nothing this can count.
func ParseUsage(answer []byte) Usage {
	var a struct {
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			Total      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(answer, &a) != nil {
		return Usage{}
	}
	u := Usage{Input: max(a.Usage.Prompt, 0), Output: max(a.Usage.Completion, 0)}
	if u.Tokens() == 0 && a.Usage.Total > 0 {
		u.Input = a.Usage.Total
	}
	return u
}

// usageDay is the day a call is counted under: the local calendar day, the one a daily spend limit starts at.
func usageDay(at time.Time) string { return at.In(time.Local).Format("2006-01-02") }

// Meter adds one call's usage to worker's count for the day of at. Without a database it keeps nothing.
func (h *Hub) Meter(worker string, at time.Time, u Usage) error {
	if h.db == nil || worker == "" || u.Tokens() == 0 {
		return nil
	}
	_, err := h.db.Exec(
		`INSERT INTO llm_usage (worker, day, input_tokens, output_tokens) VALUES (?, ?, ?, ?)
		 ON CONFLICT(worker, day) DO UPDATE SET input_tokens = input_tokens + excluded.input_tokens,
		   output_tokens = output_tokens + excluded.output_tokens`,
		worker, usageDay(at), u.Input, u.Output)
	if err != nil {
		return fmt.Errorf("llm: metering %s: %w", worker, err)
	}
	return nil
}

// Metered is the tokens worker's hosted calls used on the days from since's day on.
func (h *Hub) Metered(worker string, since time.Time) (int64, error) {
	if h.db == nil {
		return 0, nil
	}
	var tokens int64
	err := h.db.QueryRow(
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM llm_usage WHERE worker = ? AND day >= ?`,
		worker, usageDay(since)).Scan(&tokens)
	if err != nil {
		return 0, fmt.Errorf("llm: reading metered use of %s: %w", worker, err)
	}
	return tokens, nil
}
