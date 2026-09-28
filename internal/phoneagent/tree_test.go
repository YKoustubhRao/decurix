package phoneagent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/phonehost"
)

// sampleDump is shaped exactly as AccessibilityNodeInfoDumper.dumpWindowsToFile
// writes it (the attribute set and order of dumpWindowRec/dumpNodeRec). It is
// written by hand from that source, not captured from a phone: no Android
// device was available. Display 0 is the phone's own screen with another app
// on it; display 12 is the one scrcpy made for WhatsApp, with the keyboard's
// window above the app's.
const sampleDump = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<displays>
<display id="0">
 <window index="0" title="Launcher" bounds="[0,0][1080,2400]" active="true" focused="true" accessibility-focused="false" id="3" layer="1" type="TYPE_APPLICATION">
  <hierarchy rotation="0">
   <node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="com.google.android.apps.nexuslauncher" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,0][1080,2400]" drawing-order="0" hint="">
    <node index="0" text="Chrome" resource-id="" class="android.widget.TextView" package="com.google.android.apps.nexuslauncher" content-desc="Chrome" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="true" password="false" selected="false" bounds="[100,2000][300,2200]" drawing-order="1" hint="" />
   </node>
  </hierarchy>
 </window>
</display>
<display id="12">
 <window index="0" title="WhatsApp" bounds="[0,0][720,1520]" active="true" focused="true" accessibility-focused="false" id="41" layer="2" type="TYPE_APPLICATION">
  <hierarchy rotation="0">
   <node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,0][720,1520]" drawing-order="0" hint="">
    <node index="0" text="" resource-id="com.whatsapp:id/conversation_contact" class="android.widget.LinearLayout" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[80,40][600,140]" drawing-order="1" hint="">
     <node index="0" text="Mum" resource-id="com.whatsapp:id/conversation_contact_name" class="android.widget.TextView" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[90,50][300,90]" drawing-order="1" hint="" />
     <node index="1" text="online" resource-id="com.whatsapp:id/conversation_contact_status" class="android.widget.TextView" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[90,95][300,130]" drawing-order="2" hint="" />
    </node>
    <node index="1" text="See you at 6 &#128512;" resource-id="com.whatsapp:id/message_text" class="android.widget.TextView" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[40,600][500,660]" drawing-order="2" hint="" />
    <node index="2" text="" resource-id="com.whatsapp:id/menuitem_overflow" class="android.widget.ImageView" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[640,40][700,140]" drawing-order="3" hint="" />
    <node index="3" text="Hello" resource-id="com.whatsapp:id/entry" class="android.widget.EditText" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="true" scrollable="false" long-clickable="true" password="false" selected="false" bounds="[20,1400][600,1480]" drawing-order="4" hint="Message" />
    <node index="4" text="" resource-id="com.whatsapp:id/send" class="android.widget.ImageButton" package="com.whatsapp" content-desc="Send" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[620,1400][700,1480]" drawing-order="5" hint="" />
    <node index="5" text="" resource-id="" class="android.widget.Switch" package="com.whatsapp" content-desc="Read receipts" checkable="true" checked="true" clickable="true" enabled="false" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[600,300][700,360]" drawing-order="6" hint="" />
    <node index="6" text="" resource-id="" class="android.widget.EditText" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="true" password="true" selected="false" bounds="[20,400][600,460]" drawing-order="7" hint="PIN" />
    <node index="7" text="Gone" resource-id="" class="android.widget.Button" package="com.whatsapp" content-desc="" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,0][0,0]" drawing-order="8" hint="" />
   </node>
  </hierarchy>
 </window>
 <window index="1" title="Gboard" bounds="[0,1000][720,1520]" active="false" focused="false" accessibility-focused="false" id="42" layer="9" type="TYPE_INPUT_METHOD">
  <hierarchy rotation="0">
   <node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="com.google.android.inputmethod.latin" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,1000][720,1520]" drawing-order="0" hint="">
    <node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="com.google.android.inputmethod.latin" content-desc="Enter" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[620,1420][700,1500]" drawing-order="1" hint="" />
   </node>
  </hierarchy>
 </window>
</display>
</displays>`

func byName(nodes []phonehost.Node, name string) (phonehost.Node, int) {
	for i, n := range nodes {
		if n.Name == name {
			return n, i
		}
	}
	return phonehost.Node{}, -1
}

func TestADumpBecomesTheAppsControls(t *testing.T) {
	d, err := parseDump([]byte(sampleDump))
	if err != nil {
		t.Fatal(err)
	}

	tree := treeFor(d, 12, "com.whatsapp", 720, 1520)
	if tree == nil {
		t.Fatal("display 12 was not found")
	}
	nodes := tree.Nodes

	// The phone's own screen is another display, and none of it is here.
	if _, i := byName(nodes, "Chrome"); i >= 0 {
		t.Errorf("a control from display 0 leaked in: %+v", nodes)
	}

	// An icon button is named by its description: what the approval gate reads.
	if send, i := byName(nodes, "Send"); i < 0 || send.Role != "button" || send.Box != [4]int{620, 1400, 80, 80} {
		t.Errorf("Send = %+v (at %d)", send, i)
	}

	// A clickable row is one button named by the text inside it, not three things.
	if row, i := byName(nodes, "Mum online"); i < 0 || row.Role != "button" {
		t.Errorf("the contact row = %+v; nodes %+v", row, nodes)
	}
	if _, i := byName(nodes, "Mum"); i >= 0 {
		t.Error("text inside a button was also listed on its own")
	}

	// Plain text on the screen is readable, emoji and all.
	if msg, i := byName(nodes, "See you at 6 😀"); i < 0 || msg.Role != "text" {
		t.Errorf("message text = %+v", msg)
	}

	// A field: named by its hint, holding what is typed.
	if field, i := byName(nodes, "Message"); i < 0 || field.Role != "textbox" || field.Value != "Hello" || !field.Focused {
		t.Errorf("the message field = %+v", field)
	}
	if pin, i := byName(nodes, "PIN"); i < 0 || !pin.Password {
		t.Errorf("the PIN field = %+v", pin)
	}

	// No label at all: its developer's id is better than nothing.
	if _, i := byName(nodes, "menuitem overflow"); i < 0 {
		t.Errorf("an unlabelled button got no name from its id: %+v", nodes)
	}

	if sw, i := byName(nodes, "Read receipts"); i < 0 || sw.Role != "switch" || sw.Checked == nil || !*sw.Checked || !sw.Disabled {
		t.Errorf("the switch = %+v", sw)
	}

	if _, i := byName(nodes, "Gone"); i >= 0 {
		t.Error("a control with no area was listed")
	}

	// The keyboard is above the app: its keys come later, so they are drawn on top.
	_, enter := byName(nodes, "Enter")
	_, send := byName(nodes, "Send")
	if enter < 0 || enter < send {
		t.Errorf("the keyboard's Enter (%d) should follow the app's Send (%d)", enter, send)
	}

	// What goes to the viewer is plain JSON.
	if raw, err := json.Marshal(tree); err != nil || !strings.Contains(string(raw), `"role":"button"`) {
		t.Errorf("%s %v", raw, err)
	}
}

func TestTheAppsDisplayIsFoundWithoutItsID(t *testing.T) {
	d, err := parseDump([]byte(sampleDump))
	if err != nil {
		t.Fatal(err)
	}

	if tree := treeFor(d, -1, "com.whatsapp", 720, 1520); tree == nil || len(tree.Nodes) == 0 {
		t.Fatal("the app's display was not found by its package")
	}

	// The phone's own screen is never taken for the app's display.
	if tree := treeFor(d, -1, "com.google.android.apps.nexuslauncher", 720, 1520); tree != nil {
		t.Errorf("display 0 was used: %+v", tree)
	}

	if tree := treeFor(d, 99, "com.whatsapp", 720, 1520); tree != nil {
		t.Error("a display that is not in the dump gave a tree")
	}
}

func TestAnEmptyDumpIsAnError(t *testing.T) {
	if _, err := parseDump([]byte("  \n")); err == nil {
		t.Error("no error for an empty dump")
	}
	if _, err := parseDump([]byte("ERROR: could not get idle state.")); err == nil {
		t.Error("no error for uiautomator's own error text")
	}
}

func TestTheServersNewDisplayLineIsRead(t *testing.T) {
	w, h, id, ok := parseNewDisplay("[server] INFO: New display: 720x1520/280 (id=12)")
	if !ok || w != 720 || h != 1520 || id != 12 {
		t.Errorf("got %d %d %d %v", w, h, id, ok)
	}
	if _, _, _, ok := parseNewDisplay("[server] INFO: Device: [Google] google Pixel 8 (Android 15)"); ok {
		t.Error("another line was read as the display")
	}
}

func TestIdsBecomeWords(t *testing.T) {
	for id, want := range map[string]string{
		"com.whatsapp:id/send":         "send",
		"com.bank:id/payNowButton":     "pay now button",
		"com.x:id/btn_confirm_payment": "btn confirm payment",
		"":                             "",
		"android:id/button1":           "button1",
	} {
		if got := idName(id); got != want {
			t.Errorf("idName(%q) = %q, want %q", id, got, want)
		}
	}
}
