"""A tiny xclip for the e2e test: `get` prints the CLIPBOARD, `set TEXT` owns it until someone else takes it."""
import sys
import time

from Xlib import X, Xatom, display

d = display.Display()
w = d.screen().root.create_window(0, 0, 1, 1, 0, X.CopyFromParent)
CLIP, UTF8, TARGETS, PROP = (d.intern_atom(n) for n in ("CLIPBOARD", "UTF8_STRING", "TARGETS", "XCLIP_OUT"))

if sys.argv[1] == "get":
    w.convert_selection(CLIP, UTF8, PROP, X.CurrentTime)
    d.flush()
    until = time.time() + 3
    while time.time() < until:
        if d.pending_events():
            e = d.next_event()
            if e.type == X.SelectionNotify:
                if e.property == X.NONE:
                    break
                prop = w.get_full_property(PROP, X.AnyPropertyType)
                sys.stdout.write(prop.value.decode("utf-8") if isinstance(prop.value, bytes) else str(prop.value))
                break
        else:
            time.sleep(0.02)
    sys.exit(0)

text = sys.argv[2].encode("utf-8")
w.set_selection_owner(CLIP, X.CurrentTime)
d.flush()
until = time.time() + 20
while time.time() < until:
    e = d.next_event()
    if e.type == X.SelectionClear:
        break
    if e.type == X.SelectionRequest:
        if e.target == TARGETS:
            e.requestor.change_property(e.property, Xatom.ATOM, 32, [TARGETS, UTF8, Xatom.STRING])
        else:
            e.requestor.change_property(e.property, e.target, 8, text)
        from Xlib.protocol import event as xevent
        e.requestor.send_event(xevent.SelectionNotify(time=e.time, requestor=e.requestor, selection=e.selection,
                                                      target=e.target, property=e.property))
        d.flush()
