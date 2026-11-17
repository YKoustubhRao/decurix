// Package desktop: screensession_blank_test.go checks that a screen being resized is never taken as a thumbnail.
package desktop

import (
	"image"
	"testing"
)

// After a resize the session's frame is empty until the whole screen is painted again at its new size; a thumbnail
// taken then was black (a card went black while a window was fitting the desktop to itself). snapshot gives no frame
// meanwhile, and one again, as a new version, once the screen is whole.
func TestAScreenBeingResizedIsNotTakenAsAThumbnail(t *testing.T) {
	s := &shotSession{frame: image.NewRGBA(image.Rect(0, 0, 4, 4)), version: 3}

	if frame, version := s.snapshot(0, false); frame == nil || version != 3 {
		t.Fatalf("a whole screen gave frame %v version %d", frame, version)
	}

	s.blank = true // what paint sets on a DesktopSize rectangle
	if frame, _ := s.snapshot(0, false); frame != nil {
		t.Fatal("a screen being repainted after a resize was taken as a thumbnail")
	}

	s.blank, s.version = false, s.version+1 // what run does once the full update is painted
	if frame, version := s.snapshot(3, true); frame == nil || version != 4 {
		t.Fatalf("the repainted screen gave frame %v version %d", frame, version)
	}
}
