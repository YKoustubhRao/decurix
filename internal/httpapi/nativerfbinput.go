package httpapi

import (
	"context"
	"encoding/binary"
	"log"
	"sync"
	"unicode"
)

// What a viewer's RFB client says about the keyboard and mouse, turned into
// what the worker's desktop is driven with (winhost.Session): presses and
// releases of mouse buttons, text, and keys by Windows virtual-key code with
// the modifiers held.

const (
	rfbKey = iota
	rfbPointer
	rfbCutText
)

type rfbInput struct {
	kind    int
	down    bool
	keysym  uint32
	buttons byte
	x, y    int
	text    string
}

// rfbInputs is the viewer's input waiting to be applied, in order. Reading it
// off the connection is not held up by applying it (a click can take the
// helper a moment), and of a run of mouse moves only the last is kept: where
// the pointer has been is of no use once it is somewhere else.
type rfbInputs struct {
	mu     sync.Mutex
	items  []rfbInput
	ready  chan struct{}
	closed bool
}

func newRFBInputs() *rfbInputs { return &rfbInputs{ready: make(chan struct{}, 1)} }

func (q *rfbInputs) push(in rfbInput) {
	q.mu.Lock()
	if n := len(q.items); in.kind == rfbPointer && n > 0 && q.items[n-1].kind == rfbPointer && q.items[n-1].buttons == in.buttons {
		q.items[n-1] = in
	} else {
		q.items = append(q.items, in)
	}
	q.mu.Unlock()

	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *rfbInputs) take() []rfbInput {
	q.mu.Lock()
	defer q.mu.Unlock()

	items := q.items
	q.items = nil

	return items
}

func (q *rfbInputs) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()

	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// apply carries out the viewer's input until the session ends.
func (c *rfbConn) apply(ctx context.Context, inputs *rfbInputs) {
	for {
		select {
		case <-inputs.ready:
		case <-ctx.Done():
			return
		}

		for _, in := range inputs.take() {
			switch in.kind {
			case rfbPointer:
				c.pointer(in)
			case rfbKey:
				c.key(in)
			case rfbCutText:
				c.clipboard = in.text
			}
		}

		inputs.mu.Lock()
		closed := inputs.closed
		inputs.mu.Unlock()
		if closed {
			return
		}
	}
}

// do records one input (while a demonstration is being recorded) and applies it.
func (c *rfbConn) do(req hostGUIRequest) string {
	if c.record != nil {
		c.record(req)
	}

	text, _, err := applyInput(c.sess, req)
	if err != nil {
		log.Printf("screen input %s: %v", req.Kind, err)
	}

	return text
}

// mods is the modifiers held, as the helper names them: c, s, a.
func (c *rfbConn) mods() string {
	mods := ""
	if c.ctrl {
		mods += "c"
	}
	if c.shift {
		mods += "s"
	}
	if c.alt {
		mods += "a"
	}

	return mods
}

// pointer turns RFB's "these buttons are down, here" into presses, releases,
// wheel turns and moves. Buttons 1 and 3 are left and right; 4 and 5 are the
// wheel, each press of which is one notch.
func (c *rfbConn) pointer(in rfbInput) {
	changed := in.buttons ^ c.buttons
	c.buttons = in.buttons
	mods := c.mods()

	if changed&0b11101 == 0 {
		c.do(hostGUIRequest{Kind: "move", X: in.x, Y: in.y, Right: in.buttons&0b100 != 0, Mods: mods})
		return
	}

	for _, button := range []struct {
		bit   byte
		right bool
	}{{0b001, false}, {0b100, true}} {
		if changed&button.bit == 0 {
			continue
		}
		kind := "up"
		if in.buttons&button.bit != 0 {
			kind = "down"
		}
		c.do(hostGUIRequest{Kind: kind, X: in.x, Y: in.y, Right: button.right, Mods: mods})
	}

	if changed&in.buttons&0b01000 != 0 {
		c.do(hostGUIRequest{Kind: "wheel", X: in.x, Y: in.y, Delta: 1})
	}
	if changed&in.buttons&0b10000 != 0 {
		c.do(hostGUIRequest{Kind: "wheel", X: in.x, Y: in.y, Delta: -1})
	}
}

// namedKeys are X keysyms for keys that are not characters, as Windows
// virtual-key codes.
var namedKeys = map[uint32]int{
	0xff08: 8, 0xff09: 9, 0xff0d: 13, 0xff8d: 13, 0xff1b: 27, 0xffff: 46, 0xff63: 45,
	0xff50: 36, 0xff51: 37, 0xff52: 38, 0xff53: 39, 0xff54: 40, 0xff55: 33, 0xff56: 34, 0xff57: 35,
	0xff61: 44, 0xff13: 19, 0xff14: 145,
}

// key turns one RFB key event into what the desktop takes: a held modifier is
// remembered, a character is typed, and a named key or a shortcut is a
// virtual-key code with the modifiers held. Only presses act - the helper
// sends a key's release with its press.
func (c *rfbConn) key(in rfbInput) {
	switch in.keysym {
	case 0xffe1, 0xffe2:
		c.shift = in.down
		return
	case 0xffe3, 0xffe4:
		c.ctrl = in.down
		return
	case 0xffe9, 0xffea:
		c.alt = in.down
		return
	case 0xffeb, 0xffec: // the Start key
		if in.down {
			c.sess.Do("start")
		}
		return
	}
	if !in.down {
		return
	}

	mods := c.mods()

	vk, named := namedKeys[in.keysym]
	if in.keysym >= 0xffbe && in.keysym <= 0xffc9 { // F1 to F12
		vk, named = int(in.keysym-0xffbe)+112, true
	}
	if named {
		switch {
		case c.alt && vk == 9: // Alt+Tab: the next window, as a window manager would
			if windows, err := c.sess.Tasks(); err == nil && len(windows) > 1 {
				c.sess.Window("raise", windows[1].ID)
			}
		case c.alt && vk == 115: // Alt+F4
			if windows, err := c.sess.Tasks(); err == nil && len(windows) > 0 && windows[0].Active {
				c.sess.Window("close", windows[0].ID)
			}
		default:
			c.do(hostGUIRequest{Kind: "key", VK: vk, Mods: mods})
		}
		return
	}

	var r rune
	switch {
	case in.keysym >= 0x20 && in.keysym <= 0xff:
		r = rune(in.keysym)
	case in.keysym&0xff000000 == 0x01000000:
		r = rune(in.keysym & 0x00ffffff)
	case in.keysym >= 0xffaa && in.keysym <= 0xffb9: // the number pad
		r = rune(in.keysym - 0xff80)
	default:
		return
	}

	// With Ctrl or Alt held a character is a shortcut, sent as its key; Ctrl
	// with Alt is AltGr, which makes a character.
	if (c.ctrl || c.alt) && !(c.ctrl && c.alt) {
		upper := unicode.ToUpper(r)
		if !(upper >= 'A' && upper <= 'Z') && !(upper >= '0' && upper <= '9') {
			return
		}

		if c.ctrl && !c.alt && !c.shift {
			switch upper {
			case 'V':
				// What the viewer put on the clipboard is typed in; with
				// nothing from the viewer, the app pastes this computer's own.
				if c.clipboard != "" {
					c.do(hostGUIRequest{Kind: "paste", Text: c.clipboard})
					return
				}
			case 'C', 'X':
				if text := c.do(hostGUIRequest{Kind: "copy", VK: int(upper)}); text != "" {
					cut := toLatin1(text)
					c.send(append(binary.BigEndian.AppendUint32([]byte{3, 0, 0, 0}, uint32(len(cut))), cut...))
				}
				return
			}
		}

		c.do(hostGUIRequest{Kind: "key", VK: int(upper), Mods: mods})
		return
	}

	c.do(hostGUIRequest{Kind: "type", Text: string(r)})
}
