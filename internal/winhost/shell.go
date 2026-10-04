package winhost

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A worker's desktop (a session opened with OpenDesktop) has no shell of its own, so the helper stands in for one: it
// paints a wallpaper with the worker's shortcuts behind the windows, says which windows a taskbar would list, and does
// to one what a taskbar button does. What a shortcut opens is not the helper's to know: a double-click on one arrives
// here as an event.

// unitSeparator divides the fields of one shortcut or window on the helper's line; TAB divides one from the next.
const unitSeparator = "\x1f"

// Shortcut is one icon on a worker's wallpaper.
type Shortcut struct {
	ID   string
	Name string
	// Icon is what Windows is asked for the picture of: a program, a shortcut file, a Start-menu address
	// (shell:AppsFolder\...) or a bare program name (notepad.exe). Empty draws the name's initial.
	Icon string
}

// TaskWindow is one window a taskbar would have a button for.
type TaskWindow struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Active    bool   `json:"active"`
	Minimized bool   `json:"minimized"`
}

// oneField makes a value safe to put in one field of the helper's line.
func oneField(v string) string {
	return strings.Join(strings.FieldsFunc(v, func(r rune) bool { return r == '\t' || r == '\r' || r == '\n' || r == 0x1f }), " ")
}

// SetShortcuts puts these icons on the worker's wallpaper, replacing the ones there; label is written in its corner.
func (s *Session) SetShortcuts(label string, shortcuts []Shortcut) error {
	items := make([]string, 0, len(shortcuts))
	for _, sc := range shortcuts {
		if id := oneField(sc.ID); id != "" {
			items = append(items, id+unitSeparator+oneField(sc.Name)+unitSeparator+oneField(sc.Icon))
		}
	}
	resp, err := s.Do("shortcuts " + oneField(label) + "\t" + strings.Join(items, "\t"))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "OK") {
		return errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
	}
	return nil
}

// OnEvent sets what is called with each thing the helper says on its own, other than a picture and "closed": so far
// "open <shortcut id>" (a double-click on an icon of the wallpaper, or an app picked in the Start panel) and "web <words>"
// (the Start panel's search asked to look something up). Each call is on a goroutine of its own, so f may
// send the helper commands.
func (s *Session) OnEvent(f func(event string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onEvent = f
}

func (s *Session) event(event string) {
	s.mu.Lock()
	f := s.onEvent
	s.mu.Unlock()
	if f != nil {
		go f(event)
	}
}

// Tasks lists the windows a taskbar would have a button for, the one in front first.
func (s *Session) Tasks() ([]TaskWindow, error) {
	resp, err := s.Do("tasks")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resp, "TASKS") {
		return nil, fmt.Errorf("the host helper answered %q", truncate(resp, 80))
	}
	windows := []TaskWindow{}
	for _, item := range strings.Split(strings.TrimPrefix(strings.TrimPrefix(resp, "TASKS"), " "), "\t") {
		f := strings.SplitN(item, unitSeparator, 3)
		if len(f) != 3 {
			continue
		}
		id, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		windows = append(windows, TaskWindow{ID: id, Title: f[2], Active: f[1] == "a", Minimized: f[1] == "m"})
	}
	return windows, nil
}

// Window does what a taskbar button does to its window: "raise" (restoring it if minimised), "min" or "close".
func (s *Session) Window(action string, id int64) error {
	switch action {
	case "raise", "min", "close":
	default:
		return fmt.Errorf("unknown window action %q", action)
	}
	resp, err := s.Do(fmt.Sprintf("win %s %d", action, id))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "OK") {
		return errors.New(strings.TrimSpace(strings.TrimPrefix(resp, "ERR")))
	}
	return nil
}

// ShellTyping reports whether the Start panel is open, so that what is typed goes to its search field and to no window.
func (s *Session) ShellTyping() bool {
	resp, err := s.Do("shell")
	return err == nil && strings.TrimSpace(resp) == "SHELL 1"
}

// OverWindow reports whether a window shows at that point of the picture; false is the wallpaper itself.
func (s *Session) OverWindow(x, y int) bool {
	resp, err := s.Do(fmt.Sprintf("over %d %d", x, y))
	return err == nil && strings.TrimSpace(resp) == "OVER window"
}
