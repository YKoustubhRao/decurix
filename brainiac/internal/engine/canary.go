package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"time"
	"unicode"

	"brainiac/internal/backend"
	"brainiac/internal/catalog"
	"brainiac/internal/evals"
)

// Canary checks that the models for some roles work before Brainiac switches
// to them: a few small requests per role with answers code can check. It is
// not a quality test - public evaluations chose the model - it catches the
// model that does not load, answers in the wrong template, cannot see, or
// returns nothing. Any failure keeps the models Brainiac runs now.
func (e *Engine) Canary(ctx context.Context, roles []string) error {
	img := digitsPNG("4417")
	for _, role := range roles {
		u := e.byRole[role]
		if u == nil {
			continue
		}
		limit := 10 * time.Minute
		if u.thinks {
			limit = 30 * time.Minute
		}
		cctx, cancel := context.WithTimeout(ctx, limit)
		err := e.canaryRole(cctx, role, u, img)
		cancel()
		if err != nil {
			return &CanaryError{Role: role, Model: u.model, Err: err}
		}
	}
	return nil
}

// CanaryError is the first role whose model failed its check. Err is an
// *ErrNoRoom when the model could not even be loaded for lack of memory.
type CanaryError struct {
	Role, Model string
	Err         error
}

func (e *CanaryError) Error() string { return fmt.Sprintf("%s (%s): %v", e.Role, e.Model, e.Err) }
func (e *CanaryError) Unwrap() error { return e.Err }

func (e *Engine) canaryRole(ctx context.Context, role string, u *unit, img []byte) error {
	ask := func(prompt string, image []byte) (string, error) {
		parts := []Part{{Kind: "text", Text: prompt}}
		if image != nil {
			parts = append(parts, Part{Kind: "image", Data: image, Mime: "image/png"})
		}
		no := false
		res, err := e.Chat(ctx, Request{Model: u.model, Web: &no, Messages: []InMessage{{Role: "user", Parts: parts}}})
		if err != nil {
			return "", err
		}
		return res.Text, nil
	}
	expect := func(answer, want string) error {
		if !strings.Contains(strings.ToLower(answer), strings.ToLower(want)) {
			return fmt.Errorf("asked for %q, answered %q", want, clip(answer))
		}
		return nil
	}
	switch role {
	case catalog.RoleBrain:
		a, err := ask("What is 17 + 25? Reply with the number only.", nil)
		if err != nil {
			return err
		}
		if err := expect(a, "42"); err != nil {
			return err
		}
		a, err = ask("Reply with exactly this one word: ready", nil)
		if err != nil {
			return err
		}
		return expect(a, "ready")
	case catalog.RoleVision, catalog.RoleGround:
		if !u.vision {
			return nil
		}
		// Straight to the model: through Chat, OCR evidence would answer for it.
		a, err := e.seeDirect(ctx, u, "What number is written in this picture? Reply with the number only.", img)
		if err != nil {
			return err
		}
		return expect(a, "4417")
	case catalog.RoleOCR:
		r, _, err := e.OCR(ctx, img)
		if err != nil {
			return err
		}
		for _, l := range r.Lines {
			if strings.Contains(l.Text, "4417") {
				return nil
			}
		}
		return fmt.Errorf("read %d lines, none with 4417", len(r.Lines))
	case catalog.RoleEmbed:
		v, _, err := e.Embed(ctx, []string{"a cat sat on the mat", "the invoice is due"})
		if err != nil {
			return err
		}
		if len(v) != 2 || len(v[0]) == 0 || len(v[0]) != len(v[1]) {
			return fmt.Errorf("vectors of the wrong shape")
		}
		return nil
	case catalog.RoleASR:
		_, _, err := e.Transcribe(ctx, silenceWAV(1), "wav", "")
		return err
	case "math":
		a, err := ask("What is 12 multiplied by 13? End with: The answer is <number>.", nil)
		if err != nil {
			return err
		}
		if !(evals.Check{Kind: "number", Value: "156"}).Correct(a) {
			return fmt.Errorf("12 x 13 answered %q", clip(a))
		}
		return nil
	case "code":
		a, err := ask("Write a Python function named add that returns the sum of two numbers.", nil)
		if err != nil {
			return err
		}
		return expect(a, "def add")
	case "sql":
		a, err := ask("Write one SQL query that counts the rows in the table t.", nil)
		if err != nil {
			return err
		}
		return expect(a, "count")
	case "translate":
		a, err := ask("Translate into Hindi: water", nil)
		if err != nil {
			return err
		}
		for _, r := range a {
			if unicode.Is(unicode.Devanagari, r) {
				return nil
			}
		}
		return fmt.Errorf("no Hindi in %q", clip(a))
	default:
		a, err := ask("Reply with exactly: ok", nil)
		if err != nil {
			return err
		}
		return expect(a, "ok")
	}
}

// seeDirect asks one model about one picture with nothing else in the prompt.
func (e *Engine) seeDirect(ctx context.Context, u *unit, prompt string, img []byte) (string, error) {
	release, _, err := e.res.Acquire(ctx, u)
	if err != nil {
		return "", err
	}
	defer release()
	zero, maxTok := 0.0, 64
	if u.thinks {
		maxTok = 4096
	}
	res, err := u.chat(ctx, backend.ChatRequest{MaxTokens: maxTok, Temperature: &zero, Think: u.thinks, Messages: []backend.Message{
		{Role: "user", Text: prompt, Images: []backend.Image{{Data: img, Mime: "image/png"}}},
	}})
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// digit5x7 is a 5x7 bitmap font for 0-9, enough to write a number a vision
// model and OCR must read back.
var digit5x7 = map[rune][7]string{
	'0': {"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "11110", "00001", "00001", "10001", "01110"},
	'6': {"00110", "01000", "10000", "11110", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00010", "01100"},
}

// digitsPNG draws a number in large black digits on white.
func digitsPNG(s string) []byte {
	const scale, pad = 12, 24
	w := pad*2 + len(s)*6*scale
	h := pad*2 + 7*scale
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.White)
		}
	}
	for i, ch := range s {
		g, ok := digit5x7[ch]
		if !ok {
			continue
		}
		for row, bits := range g {
			for col, b := range bits {
				if b != '1' {
					continue
				}
				x0, y0 := pad+i*6*scale+col*scale, pad+row*scale
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						img.Set(x0+dx, y0+dy, color.Black)
					}
				}
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// silenceWAV is a short 16 kHz mono 16-bit WAV of silence.
func silenceWAV(seconds int) []byte {
	n := 16000 * seconds
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+n*2))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint32(16000))
	binary.Write(&b, binary.LittleEndian, uint32(32000))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(n*2))
	b.Write(make([]byte, n*2))
	return b.Bytes()
}
