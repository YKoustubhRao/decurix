package httpapi

import (
	"context"
	"errors"
	"testing"
)

// An automated sender (a schedule, a DAG node, an email trigger) must never
// reach a desktop whose agent is running a task or waiting on a person: the
// watcher would take it as that person's reply. It is refused - without
// touching the desktop - as a *DesktopBusyError, which the senders read as
// "try again later".
func TestAutomatedSendsWaitForAFreeDesktop(t *testing.T) {
	s := chatServer(t)

	route := deliver(t, s, "bordu", "pay the invoice")

	for _, send := range []func() error{
		func() error { return s.Submit(context.Background(), "bordu", "check the mail") },
		func() error {
			_, err := s.SubmitTracked(context.Background(), "bordu", "an email arrived")
			return err
		},
	} {
		err := send()

		var busy *DesktopBusyError
		if !errors.As(err, &busy) || busy.DesktopBusy() == "" {
			t.Fatalf("err = %v, want a DesktopBusyError", err)
		}
	}

	if open, _ := s.Tasks.IsOpen("bordu", route.TaskID); !open {
		t.Error("the running task was disturbed")
	}
}
