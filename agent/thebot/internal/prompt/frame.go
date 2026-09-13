// Package prompt builds the one system message every model call in a job
// starts with.
//
// "Every call" is the point. On a CPU-only host the expensive part of a
// model call is not writing the answer, it is reading the prompt - a 3B
// model evaluates roughly 14 tokens a second, so a 600-token preamble costs
// the better part of a minute before the first word comes back. Ollama keeps
// the evaluated prefix of the last prompt it served and reuses whatever part
// of the next prompt matches it byte for byte. A job that plans, runs a
// command loop, and then writes a reply used to open each of those calls
// with a different preamble, so every one of them paid full price. Starting
// all of them with the same frame means only the first call of a job reads
// it; the rest start from the cache.
//
// The sections are ordered from most stable to least: the persona never
// changes, the host's context changes when a human edits worker memory,
// recalled facts change per request, and the clock changes every minute.
// Anything that varies sits at the end so it only ever invalidates the tail.
//
// "The tail" is still everything after it, though - including a skill's
// own instructions and the task. So the volatile sections are only put in
// where they are needed: live counters and the time of day for a
// conversational job (the one kind of job that gets asked "how much have
// you indexed?" or "what time is it?"), and just the date otherwise. With
// the counters in every job, the job log's count alone - which goes up by
// one per job - forced every operator job to re-read its whole contract.
package prompt

import (
	"strings"
	"time"

	"thebot/internal/llm"
)

// NeedsToolsMarker is what a conversational answer says instead of an
// answer when it would have to look at the machine to give one. The
// scheduler reroutes such a step to the operator, which can.
const NeedsToolsMarker = "NEEDS_TOOLS"

// Persona is TheBot's standing identity and the few rules that apply to
// every kind of call. Kept deliberately short: it is re-read whenever the
// prompt cache is cold, and every rule here is one a small model has to
// weigh against all the others.
const Persona = `You are TheBot, an AI agent that lives on this computer and does real work on it. That is your name. The language model generating this text is only your brain - an interchangeable part, not who you are. If asked which model you run on, you may say so, but you are TheBot.

What you can really do: run shell commands on this machine, search the web and read pages, download files, write files, build and test software, and search documents you have indexed. You remember this conversation turn by turn, and you keep long-term memory across conversations - anything recalled is listed below as fact.

How to answer:
- Be direct and brief. Answer what was asked, using the facts below when they apply.
- Never invent results, files, or numbers you did not actually see.`

// ChatRule is appended to the frame by the conversational skill only. It
// used to sit in Persona, which every call shares - and the operator, told
// to reply with COMMAND/DONE/HELP, answered "ping - just reply pong" with
// HELP: NEEDS_TOOLS, which reached a human as the whole of a help request.
// Appended after the frame, it still reuses the frame's cached prefix.
//
// "Clearly" and the unclear-message sentence are measured, too: in a real
// desktop "browser test message one" came back NEEDS_TOOLS, and the operator
// it was handed to invented a clipboard command and asked the human to
// install xclip.
const ChatRule = `You are answering in conversation, with no tools in this reply. Only if the message clearly asks about the current state of this computer that you have not been shown (its files, folders, processes, installed software, hardware, settings), reply with exactly ` + NeedsToolsMarker + ` and nothing else, and you will be given tools. If you cannot tell what the human wants, ask them in one short sentence.`

// Frame is the per-job standing context that goes into the system message.
type Frame struct {
	// Host is what the process embedding TheBot says about where it runs:
	// the desktop's name, its working directory, persistent worker memory,
	// the approval rules it enforces. Supplied per request, never mixed
	// into the task text - see scheduler.Request.Context for why.
	Host string
	// Facts are long-term memories recalled as relevant to this request.
	Facts []string
	// Stats are TheBot's own live counters (indexed files, stored facts).
	Stats string
	// Now is stamped once per job so every call in the job renders the same
	// clock line. Zero means "leave the clock out".
	Now time.Time
	// WithTime renders Now with the time of day; otherwise only the date,
	// which stays the same for every job that day.
	WithTime bool
}

// Message renders the frame as a single system message.
func (f Frame) Message() llm.Message {
	var b strings.Builder
	b.WriteString(Persona)

	if host := strings.TrimSpace(f.Host); host != "" {
		b.WriteString("\n\n")
		b.WriteString(host)
	}

	if len(f.Facts) > 0 {
		b.WriteString("\n\nThings you already know about the user, recalled from long-term memory. Treat them as true:\n- ")
		b.WriteString(strings.Join(f.Facts, "\n- "))
	}

	if stats := strings.TrimSpace(f.Stats); stats != "" {
		b.WriteString("\n\n")
		b.WriteString(stats)
	}

	if !f.Now.IsZero() {
		if f.WithTime {
			b.WriteString("\n\nCurrent date and time: ")
			b.WriteString(f.Now.Format("Monday 2 January 2006, 15:04 MST"))
		} else {
			b.WriteString("\n\nToday is ")
			b.WriteString(f.Now.Format("Monday 2 January 2006") + ".")
		}
	}

	return llm.Message{Role: "system", Content: b.String()}
}

// IsNeedsTools reports whether a conversational reply is the
// NEEDS_TOOLS signal rather than an answer. Small models decorate it
// ("**NEEDS_TOOLS**", "NEEDS_TOOLS."), so it is matched at the start with
// punctuation and emphasis ignored, and only when that is essentially the
// whole reply - a real answer that happens to mention the word is an answer.
func IsNeedsTools(reply string) bool {
	trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(reply), "*_`\"'."))
	if !strings.HasPrefix(strings.ToUpper(trimmed), NeedsToolsMarker) {
		return false
	}
	return len(trimmed) <= len(NeedsToolsMarker)+40
}
