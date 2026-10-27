package engine

import "fmt"

// Part is one piece of a message: text, a picture or a recording.
type Part struct {
	Kind   string // text | image | audio
	Text   string
	Data   []byte
	Mime   string // image/png, audio/wav ...
	Format string // audio container: wav, mp3, m4a, ogg, webm, flac
	Name   string // a label for it, when the client gave one
}

// InMessage is one turn as a client sent it.
type InMessage struct {
	Role  string
	Parts []Part
}

// Caps is what the chosen brain and the installed specialists can do.
type Caps struct {
	BrainSees bool // the brain is shown pictures itself
	OCR       bool
	Vision    bool // a local vision model can describe a picture the brain cannot see
	ASR       bool
}

// Atom is one small job a request is broken into.
type Atom struct {
	ID   string // image-1, audio-2: how evidence names what it came from
	Op   string // ocr | describe | asr
	Msg  int    // index into the request's messages
	Part int    // index into that message's parts
}

// Atomize breaks a request into the perception jobs it needs, by rule, never
// by asking a model (a model judging its own task's granularity was a
// hard-learned mistake in Decurix's atomizer). Text needs nothing: a text
// request goes straight to the brain, so "hello" costs one model call.
//
//   - A picture is always read by OCR when OCR is installed: exact text with
//     positions is cheap and is what small vision models get wrong most.
//   - A picture the brain cannot see itself is described by a local vision
//     model, so a text-only or privacy-limited brain still knows what it shows.
//   - A recording is transcribed (with voice detection inside the worker);
//     no brain here takes raw audio.
func Atomize(msgs []InMessage, caps Caps) []Atom {
	var atoms []Atom
	images, audios := 0, 0
	for mi, m := range msgs {
		for pi, p := range m.Parts {
			switch p.Kind {
			case "image":
				images++
				id := fmt.Sprintf("image-%d", images)
				if caps.OCR {
					atoms = append(atoms, Atom{ID: id, Op: "ocr", Msg: mi, Part: pi})
				}
				if !caps.BrainSees && caps.Vision {
					atoms = append(atoms, Atom{ID: id, Op: "describe", Msg: mi, Part: pi})
				}
			case "audio":
				audios++
				if caps.ASR {
					atoms = append(atoms, Atom{ID: fmt.Sprintf("audio-%d", audios), Op: "asr", Msg: mi, Part: pi})
				}
			}
		}
	}
	return atoms
}

// mediaIDs names every picture and recording in order, the way Atomize does.
func mediaIDs(msgs []InMessage) map[[2]int]string {
	out := map[[2]int]string{}
	images, audios := 0, 0
	for mi, m := range msgs {
		for pi, p := range m.Parts {
			switch p.Kind {
			case "image":
				images++
				out[[2]int{mi, pi}] = fmt.Sprintf("image-%d", images)
			case "audio":
				audios++
				out[[2]int{mi, pi}] = fmt.Sprintf("audio-%d", audios)
			}
		}
	}
	return out
}
