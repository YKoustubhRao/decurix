package phoneagent

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/YKoustubhRao/decurix/internal/phonehost"
)

// What an app's display shows is read from Android's own accessibility tree,
// the one TalkBack reads, with `uiautomator dump --windows` - a shell command
// every Android has, run over the same ADB as everything else. --windows
// (Android 11+, AOSP change 89f367d "Add a window dump for uiautomator") dumps
// the windows of every display, which is what reaches the virtual display an
// app runs on here: the plain dump only reads the phone's own screen. Its XML
// shape below was read from AccessibilityNodeInfoDumper.dumpWindowsToFile /
// dumpWindowRec / dumpNodeRec (frameworks/base, cmds/uiautomator):
//
//	<displays><display id="7">
//	  <window index="0" title=".." bounds="[0,0][720,1440]" layer=".." type="TYPE_APPLICATION" ..>
//	    <hierarchy rotation="0"><node text=".." resource-id=".." class=".." package=".."
//	      content-desc=".." checkable=".." checked=".." clickable=".." enabled=".." focusable=".."
//	      focused=".." scrollable=".." long-clickable=".." password=".." selected=".."
//	      bounds="[l,t][r,b]" hint=".."> ..child nodes.. </node></hierarchy>
//	    ..child windows..
//	  </window>
//	</display></displays>
//
// Bounds are the node's visible part, in the display's own pixels; invisible
// nodes are left out by the dumper itself.

const (
	// minTreeSDK is Android 11, the first with `uiautomator dump --windows`.
	minTreeSDK = 30
	// dumpOnPhone is where the dump is written before it is read back.
	dumpOnPhone = "/data/local/tmp/decurix-ui.xml"
	// maxTreeNodes bounds what one tree carries to the viewer.
	maxTreeNodes = 600
	// Pacing of the tree reader: soon after input, every treeActive while
	// someone is using an app, every treeIdle otherwise.
	treeAfterInput = 300 * time.Millisecond
	treeActive     = 1500 * time.Millisecond
	treeIdle       = 6 * time.Second
	treeActiveFor  = 30 * time.Second
)

type xmlDisplays struct {
	Displays []xmlDisplay `xml:"display"`
}

type xmlDisplay struct {
	ID      int         `xml:"id,attr"`
	Windows []xmlWindow `xml:"window"`
}

type xmlWindow struct {
	Layer     int           `xml:"layer,attr"`
	Type      string        `xml:"type,attr"`
	Windows   []xmlWindow   `xml:"window"`
	Hierarchy *xmlHierarchy `xml:"hierarchy"`
}

type xmlHierarchy struct {
	Nodes []xmlNode `xml:"node"`
}

type xmlNode struct {
	Text          string    `xml:"text,attr"`
	ResourceID    string    `xml:"resource-id,attr"`
	Class         string    `xml:"class,attr"`
	Package       string    `xml:"package,attr"`
	ContentDesc   string    `xml:"content-desc,attr"`
	Hint          string    `xml:"hint,attr"`
	Checkable     bool      `xml:"checkable,attr"`
	Checked       bool      `xml:"checked,attr"`
	Clickable     bool      `xml:"clickable,attr"`
	LongClickable bool      `xml:"long-clickable,attr"`
	Enabled       bool      `xml:"enabled,attr"`
	Focused       bool      `xml:"focused,attr"`
	Password      bool      `xml:"password,attr"`
	Selected      bool      `xml:"selected,attr"`
	Bounds        string    `xml:"bounds,attr"`
	Nodes         []xmlNode `xml:"node"`
}

// parseDump reads a `uiautomator dump --windows` file.
func parseDump(raw []byte) (*xmlDisplays, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("the phone wrote no accessibility dump")
	}

	var d xmlDisplays
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("reading the accessibility dump: %w", err)
	}

	return &d, nil
}

// displayFor picks the display an app is on: the id scrcpy reported when it
// has one, else the only display other than the phone's own screen with a
// window of pkg on it.
func (d *xmlDisplays) displayFor(id int, pkg string) *xmlDisplay {
	if id >= 0 {
		for i := range d.Displays {
			if d.Displays[i].ID == id {
				return &d.Displays[i]
			}
		}
		return nil
	}

	var found *xmlDisplay
	for i := range d.Displays {
		if d.Displays[i].ID != 0 && d.Displays[i].hasPackage(pkg) {
			if found != nil {
				return nil // two: not a guess to make
			}
			found = &d.Displays[i]
		}
	}

	return found
}

func (d *xmlDisplay) hasPackage(pkg string) bool {
	var in func(ws []xmlWindow) bool
	in = func(ws []xmlWindow) bool {
		for _, w := range ws {
			if w.Hierarchy != nil && len(w.Hierarchy.Nodes) > 0 && w.Hierarchy.Nodes[0].Package == pkg {
				return true
			}
			if in(w.Windows) {
				return true
			}
		}
		return false
	}

	return in(d.Windows)
}

// controls turns one display's windows into the controls a viewer lays over
// the picture, bottom window first, so a later control is drawn on top of an
// earlier one - a dialog over the app, a button over the row it sits in.
func (d *xmlDisplay) controls() []phonehost.Node {
	var windows []xmlWindow
	var flatten func(ws []xmlWindow)
	flatten = func(ws []xmlWindow) {
		for _, w := range ws {
			windows = append(windows, w)
			flatten(w.Windows)
		}
	}
	flatten(d.Windows)
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].Layer < windows[j].Layer })

	b := &treeBuilder{}
	for _, w := range windows {
		if w.Hierarchy == nil {
			continue
		}
		for _, n := range w.Hierarchy.Nodes {
			b.walk(n, nil)
		}
	}

	return b.out
}

type treeBuilder struct {
	out []phonehost.Node
}

// walk adds n and what is beneath it. inside is the text of the control n
// sits in, when there is one: plain text inside a button is that button's
// label, not a separate thing on the screen (Android does the same when a
// screen reader lands on a list row).
func (b *treeBuilder) walk(n xmlNode, inside *[]string) {
	if len(b.out) >= maxTreeNodes {
		return
	}

	box, visible := parseBounds(n.Bounds)
	label := clean(firstOf(n.ContentDesc, n.Text))

	if role := controlRole(n); role != "" && visible {
		i := len(b.out)
		node := phonehost.Node{Role: role, Box: box, Disabled: !n.Enabled, Focused: n.Focused, Selected: n.Selected}

		if n.Checkable {
			checked := n.Checked
			node.Checked = &checked
		}

		var texts []string
		if role == "textbox" {
			node.Password = n.Password
			node.Name = clean(firstOf(n.Hint, n.ContentDesc))
			if n.Text != n.Hint {
				node.Value = limit(n.Text, 500)
			}
		} else {
			node.Name = label
		}

		b.out = append(b.out, node)
		for _, c := range n.Nodes {
			b.walk(c, &texts)
		}

		if b.out[i].Name == "" && len(texts) > 0 {
			b.out[i].Name = limit(strings.Join(texts, " "), 200)
		}
		if b.out[i].Name == "" {
			// A control with no label at all is still usually named by its
			// developer ("com.whatsapp:id/send"), which is better than nothing.
			b.out[i].Name = idName(n.ResourceID)
		}
		if inside != nil && b.out[i].Name != "" && role != "textbox" {
			*inside = append(*inside, b.out[i].Name)
		}
		return
	}

	if label != "" {
		if inside != nil {
			*inside = append(*inside, label)
		} else if visible {
			role := "text"
			if n.Text == "" && strings.Contains(n.Class, "Image") {
				role = "image"
			}
			b.out = append(b.out, phonehost.Node{Role: role, Name: limit(label, 500), Box: box, Selected: n.Selected})
		}
	}

	for _, c := range n.Nodes {
		b.walk(c, inside)
	}
}

// controlRole says what kind of control n is, or "" when it is not one.
func controlRole(n xmlNode) string {
	class := n.Class

	switch {
	case strings.Contains(class, "EditText") || strings.Contains(class, "AutoCompleteTextView"):
		return "textbox"
	case n.Checkable && (strings.Contains(class, "Switch") || strings.Contains(class, "ToggleButton")):
		return "switch"
	case n.Checkable && strings.Contains(class, "RadioButton"):
		return "radio"
	case n.Checkable:
		return "checkbox"
	case strings.Contains(class, "SeekBar") || strings.Contains(class, "RatingBar"):
		return "slider"
	case n.Clickable || n.LongClickable:
		return "button"
	}

	return ""
}

var boundsRe = regexp.MustCompile(`^\[(-?\d+),(-?\d+)\]\[(-?\d+),(-?\d+)\]$`)

// parseBounds reads Rect.toShortString ("[l,t][r,b]") as left, top, width,
// height, and whether it has any area.
func parseBounds(s string) ([4]int, bool) {
	m := boundsRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [4]int{}, false
	}

	var v [4]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}

	box := [4]int{v[0], v[1], v[2] - v[0], v[3] - v[1]}

	return box, box[2] > 0 && box[3] > 0
}

// idName turns a resource id ("com.whatsapp:id/send_button") into words ("send button").
func idName(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if id == "" {
		return ""
	}

	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	prev := rune(0)
	for _, r := range id {
		switch {
		case r == '_' || r == '-' || r == '.':
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()

	return strings.Join(words, " ")
}

func firstOf(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func clean(s string) string { return limit(strings.Join(strings.Fields(s), " "), 200) }

// limit cuts s to at most n bytes without splitting a character.
func limit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

// treeFor is the tree of the app on display id (or of pkg, when id is not
// known), or nil when that display is not in the dump.
func treeFor(d *xmlDisplays, id int, pkg string, w, h int) *phonehost.Tree {
	disp := d.displayFor(id, pkg)
	if disp == nil {
		return nil
	}

	nodes := disp.controls()
	if nodes == nil {
		nodes = []phonehost.Node{}
	}

	return &phonehost.Tree{W: w, H: h, Nodes: nodes}
}

// dumpUI reads every display's accessibility tree from the phone.
func dumpUI(ctx context.Context, serial string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	// A dump that fails (an app that never goes idle) leaves no file, rather
	// than the previous one to be read as if it were now.
	script := "rm -f " + dumpOnPhone + "; uiautomator dump --windows " + dumpOnPhone + " >/dev/null 2>&1; cat " + dumpOnPhone + " 2>/dev/null"

	out, err := adbCommand(ctx, serial, "shell", script).Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("uiautomator dump: %w", err)
	}

	return out, nil
}

// newDisplayRe is the line scrcpy's server logs once it has made an app's
// display (NewDisplayCapture, v4.1): "New display: 720x1440/280 (id=12)".
var newDisplayRe = regexp.MustCompile(`New display: (\d+)x(\d+)/\d+ \(id=(\d+)\)`)

// parseNewDisplay reads the display's size and id off one line of the server's log.
func parseNewDisplay(line string) (w, h, id int, ok bool) {
	m := newDisplayRe.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, 0, false
	}

	w, _ = strconv.Atoi(m[1])
	h, _ = strconv.Atoi(m[2])
	id, _ = strconv.Atoi(m[3])

	return w, h, id, true
}
