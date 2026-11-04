package practice

import (
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A practice or eval case gives its desktop's agent a request (and, in an eval, an expert's reply) the way a person's
// message reaches a desktop in production: the same C14 decision on the text (tasks.RequestForm / tasks.ReplyForm,
// the rules tasks.Store.StoredForm applies), the same values withheld (withhold.HeldValues), the same notice
// (desktop.RequestNotice / ReplyNotice), told to the gate in the same order (desktop.TellGate, from
// PracticeAgentRequest). So what is measured here is the gate production runs.

// RequestGateNotice is what a practice desktop's gate is told before its agent has a case's request on taskID.
func RequestGateNotice(taskID, text string) desktop.GateNotice {
	stored, withheld := tasks.RequestForm(text, false)
	return desktop.RequestNotice(taskID, text, stored, withheld, withhold.HeldValues(text, stored))
}

// ReplyGateNotice is what a practice desktop's gate is told before its agent has a reply on taskID to question, a
// query (askedSecret: the question was asked for a secret).
func ReplyGateNotice(taskID, text, question string, askedSecret bool) desktop.GateNotice {
	stored, withheld := tasks.ReplyForm(text, false, question, askedSecret)
	return desktop.ReplyNotice(taskID, withheld, withhold.HeldValues(text, stored),
		&desktop.GateReply{Text: text, Asked: question, Kind: "query"})
}
