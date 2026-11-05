package phonehost

import (
	"context"
	"testing"
	"time"
)

// Drop, called when a device is unpaired, ends its live link and its
// sessions at once (review 2026-11-03, B-18).
func TestDropEndsAnUnpairedPhonesLink(t *testing.T) {
	hub, phone := link(t)

	go func() {
		m := phone.recv()
		phone.send(Message{Op: OpOpened, Session: m.Session, Codec: "h264"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := hub.Open(ctx, "dev1", "com.example.app", 720, 1440, 280)
	if err != nil {
		t.Fatal(err)
	}

	hub.Drop("dev1")

	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session outlived its phone's pairing")
	}
	if len(hub.Phones()) != 0 {
		t.Fatal("an unpaired phone is still listed")
	}
	if _, err := hub.Open(ctx, "dev1", "com.example.app", 720, 1440, 280); err == nil {
		t.Fatal("an unpaired phone could still be opened")
	}

	hub.Drop("dev1") // dropping twice, or a phone that is not linked, is harmless
}
