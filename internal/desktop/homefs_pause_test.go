package desktop

import "testing"

// Two copies of one paused desktop overlap: only the last to finish pauses it again, and a person's Resume during a
// copy is never undone (RW1-C-04).
func TestOverlappingCopiesPauseOnceAndRespectAResume(t *testing.T) {
	c := &copyPauses{working: map[string]int{}, repause: map[string]bool{}}
	c.begin("d", true) // the first copy resumed it
	if !c.joining("d") {
		t.Fatal("a second copy did not join the first")
	}
	if c.end("d") {
		t.Fatal("the first copy to finish paused the desktop under the second")
	}
	if !c.end("d") {
		t.Fatal("the last copy did not pause it again")
	}

	c.begin("d", true)
	c.settled("d") // the person pressed Resume
	if c.end("d") {
		t.Fatal("a copy paused a desktop the person had resumed")
	}
	if c.joining("d") {
		t.Fatal("a running desktop nobody resumed for a copy was joined")
	}
}
