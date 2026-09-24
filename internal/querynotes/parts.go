package querynotes

import "strings"

// The sections of the note a desktop agent writes (decurix_agent/messaging.py).
// A note opens with "DECURIX QUERY REQUEST / Desktop: / Desktop ID:"
// boilerplate, which is all a card showing the note's first lines ever said;
// the task and the agent's own words are what a person reads.
const (
	// HeadingFinal starts what a finished task replied.
	HeadingFinal = "\nFinal reply:\n"
	// HeadingProblem starts what a blocked task asks.
	HeadingProblem = "\nProblem:\n"

	headingTask = "\nTask:\n"
)

// trailers are the boilerplate sections after the message, which end it.
var trailers = []string{
	"\n\nCompleted at:",
	"\n\nQuery ID:",
}

// ParseMessage returns what the agent said in a note and the heading it said
// it under, or "" and "" for a note in no known shape.
func ParseMessage(content string) (message, heading string) {
	for _, heading := range []string{HeadingFinal, HeadingProblem} {
		start := strings.Index(content, heading)
		if start == -1 {
			continue
		}

		rest := content[start+len(heading):]
		for _, trailer := range trailers {
			if end := strings.Index(rest, trailer); end != -1 {
				rest = rest[:end]
			}
		}

		return strings.TrimSpace(rest), heading
	}

	return "", ""
}

// ParseTask returns the request a note is about, or "" when it names none.
func ParseTask(content string) string {
	start := strings.Index(content, headingTask)
	if start == -1 {
		return ""
	}

	task := content[start+len(headingTask):]
	if end := strings.Index(task, "\n\n"); end != -1 {
		task = task[:end]
	}

	if task = strings.TrimSpace(task); task == "unknown" {
		return ""
	}
	return task
}

// Message is what the agent said in the note, or "" for a note in no known
// shape (show its Content whole then).
func (n QueryNote) Message() string {
	message, _ := ParseMessage(n.Content)
	return message
}

// Task is the request the note is about, or "".
func (n QueryNote) Task() string {
	return ParseTask(n.Content)
}
