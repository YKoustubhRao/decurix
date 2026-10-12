"""
A person at a desktop's screen, with no browser: a minimal VNC (RFB 3.8) client
that only sends input. What it sends reaches the X server as the VNC server's
own pointer and keyboard - the devices a person using the viewer arrives on -
so the demonstration recorder takes it for a person's (it drops what xdotool
injects). Standard library only, so it runs inside a desktop:

    docker exec -i -u headless desktop_<id> python3 - < scenario.py

where scenario.py starts with this file's text, or imports it from a copy.

    me = Person()                      # localhost:5901, password from VNC_PW
    me.click(640, 360)
    me.type("Acme Traders")
    me.key("Return"); me.key("ctrl+s")

It never reads the screen: pair it with `gui observe --coords` to know where
things are (looking is not an action, and is not recorded).
"""

import os
import socket
import struct
import time

# --- DES, for the VNC password challenge (the key's bits are mirrored, VNC's own quirk) ---------------------------

_PC1 = [57, 49, 41, 33, 25, 17, 9, 1, 58, 50, 42, 34, 26, 18, 10, 2, 59, 51, 43, 35, 27, 19, 11, 3, 60, 52, 44, 36,
        63, 55, 47, 39, 31, 23, 15, 7, 62, 54, 46, 38, 30, 22, 14, 6, 61, 53, 45, 37, 29, 21, 13, 5, 28, 20, 12, 4]
_PC2 = [14, 17, 11, 24, 1, 5, 3, 28, 15, 6, 21, 10, 23, 19, 12, 4, 26, 8, 16, 7, 27, 20, 13, 2,
        41, 52, 31, 37, 47, 55, 30, 40, 51, 45, 33, 48, 44, 49, 39, 56, 34, 53, 46, 42, 50, 36, 29, 32]
_SHIFTS = [1, 1, 2, 2, 2, 2, 2, 2, 1, 2, 2, 2, 2, 2, 2, 1]
_IP = [58, 50, 42, 34, 26, 18, 10, 2, 60, 52, 44, 36, 28, 20, 12, 4, 62, 54, 46, 38, 30, 22, 14, 6, 64, 56, 48, 40, 32, 24, 16, 8,
       57, 49, 41, 33, 25, 17, 9, 1, 59, 51, 43, 35, 27, 19, 11, 3, 61, 53, 45, 37, 29, 21, 13, 5, 63, 55, 47, 39, 31, 23, 15, 7]
_FP = [40, 8, 48, 16, 56, 24, 64, 32, 39, 7, 47, 15, 55, 23, 63, 31, 38, 6, 46, 14, 54, 22, 62, 30, 37, 5, 45, 13, 53, 21, 61, 29,
       36, 4, 44, 12, 52, 20, 60, 28, 35, 3, 43, 11, 51, 19, 59, 27, 34, 2, 42, 10, 50, 18, 58, 26, 33, 1, 41, 9, 49, 17, 57, 25]
_E = [32, 1, 2, 3, 4, 5, 4, 5, 6, 7, 8, 9, 8, 9, 10, 11, 12, 13, 12, 13, 14, 15, 16, 17,
      16, 17, 18, 19, 20, 21, 20, 21, 22, 23, 24, 25, 24, 25, 26, 27, 28, 29, 28, 29, 30, 31, 32, 1]
_P = [16, 7, 20, 21, 29, 12, 28, 17, 1, 15, 23, 26, 5, 18, 31, 10, 2, 8, 24, 14, 32, 27, 3, 9, 19, 13, 30, 6, 22, 11, 4, 25]
_S = [
    [14, 4, 13, 1, 2, 15, 11, 8, 3, 10, 6, 12, 5, 9, 0, 7, 0, 15, 7, 4, 14, 2, 13, 1, 10, 6, 12, 11, 9, 5, 3, 8,
     4, 1, 14, 8, 13, 6, 2, 11, 15, 12, 9, 7, 3, 10, 5, 0, 15, 12, 8, 2, 4, 9, 1, 7, 5, 11, 3, 14, 10, 0, 6, 13],
    [15, 1, 8, 14, 6, 11, 3, 4, 9, 7, 2, 13, 12, 0, 5, 10, 3, 13, 4, 7, 15, 2, 8, 14, 12, 0, 1, 10, 6, 9, 11, 5,
     0, 14, 7, 11, 10, 4, 13, 1, 5, 8, 12, 6, 9, 3, 2, 15, 13, 8, 10, 1, 3, 15, 4, 2, 11, 6, 7, 12, 0, 5, 14, 9],
    [10, 0, 9, 14, 6, 3, 15, 5, 1, 13, 12, 7, 11, 4, 2, 8, 13, 7, 0, 9, 3, 4, 6, 10, 2, 8, 5, 14, 12, 11, 15, 1,
     13, 6, 4, 9, 8, 15, 3, 0, 11, 1, 2, 12, 5, 10, 14, 7, 1, 10, 13, 0, 6, 9, 8, 7, 4, 15, 14, 3, 11, 5, 2, 12],
    [7, 13, 14, 3, 0, 6, 9, 10, 1, 2, 8, 5, 11, 12, 4, 15, 13, 8, 11, 5, 6, 15, 0, 3, 4, 7, 2, 12, 1, 10, 14, 9,
     10, 6, 9, 0, 12, 11, 7, 13, 15, 1, 3, 14, 5, 2, 8, 4, 3, 15, 0, 6, 10, 1, 13, 8, 9, 4, 5, 11, 12, 7, 2, 14],
    [2, 12, 4, 1, 7, 10, 11, 6, 8, 5, 3, 15, 13, 0, 14, 9, 14, 11, 2, 12, 4, 7, 13, 1, 5, 0, 15, 10, 3, 9, 8, 6,
     4, 2, 1, 11, 10, 13, 7, 8, 15, 9, 12, 5, 6, 3, 0, 14, 11, 8, 12, 7, 1, 14, 2, 13, 6, 15, 0, 9, 10, 4, 5, 3],
    [12, 1, 10, 15, 9, 2, 6, 8, 0, 13, 3, 4, 14, 7, 5, 11, 10, 15, 4, 2, 7, 12, 9, 5, 6, 1, 13, 14, 0, 11, 3, 8,
     9, 14, 15, 5, 2, 8, 12, 3, 7, 0, 4, 10, 1, 13, 11, 6, 4, 3, 2, 12, 9, 5, 15, 10, 11, 14, 1, 7, 6, 0, 8, 13],
    [4, 11, 2, 14, 15, 0, 8, 13, 3, 12, 9, 7, 5, 10, 6, 1, 13, 0, 11, 7, 4, 9, 1, 10, 14, 3, 5, 12, 2, 15, 8, 6,
     1, 4, 11, 13, 12, 3, 7, 14, 10, 15, 6, 8, 0, 5, 9, 2, 6, 11, 13, 8, 1, 4, 10, 7, 9, 5, 0, 15, 14, 2, 3, 12],
    [13, 2, 8, 4, 6, 15, 11, 1, 10, 9, 3, 14, 5, 0, 12, 7, 1, 15, 13, 8, 10, 3, 7, 4, 12, 5, 6, 11, 0, 14, 9, 2,
     7, 11, 4, 1, 9, 12, 14, 2, 0, 6, 10, 13, 15, 3, 5, 8, 2, 1, 14, 7, 4, 10, 8, 13, 15, 12, 9, 0, 3, 5, 6, 11],
]


def _bits(data):
    return [(byte >> (7 - i)) & 1 for byte in data for i in range(8)]


def _pick(bits, table):
    return [bits[i - 1] for i in table]


def _des_block(key, block):
    """One 8-byte block encrypted with an 8-byte key (single DES, ECB)."""

    k = _pick(_bits(key), _PC1)
    c, d = k[:28], k[28:]
    rounds = []
    for shift in _SHIFTS:
        c, d = c[shift:] + c[:shift], d[shift:] + d[:shift]
        rounds.append(_pick(c + d, _PC2))

    m = _pick(_bits(block), _IP)
    left, right = m[:32], m[32:]
    for sub in rounds:
        mixed = [a ^ b for a, b in zip(_pick(right, _E), sub)]
        out = []
        for box in range(8):
            six = mixed[box * 6:box * 6 + 6]
            row, col = six[0] * 2 + six[5], six[1] * 8 + six[2] * 4 + six[3] * 2 + six[4]
            value = _S[box][row * 16 + col]
            out += [(value >> 3) & 1, (value >> 2) & 1, (value >> 1) & 1, value & 1]
        left, right = right, [a ^ b for a, b in zip(left, _pick(out, _P))]
    final = _pick(right + left, _FP)
    return bytes(int("".join(map(str, final[i:i + 8])), 2) for i in range(0, 64, 8))


def vnc_response(password, challenge):
    """The 16-byte answer to a VNC authentication challenge."""

    key = (password.encode("latin-1") + b"\0" * 8)[:8]
    key = bytes(int(f"{byte:08b}"[::-1], 2) for byte in key)
    return _des_block(key, challenge[:8]) + _des_block(key, challenge[8:16])


# --- keys ----------------------------------------------------------------------------------------------------

KEYSYMS = {
    "return": 0xFF0D, "enter": 0xFF0D, "tab": 0xFF09, "escape": 0xFF1B, "esc": 0xFF1B, "backspace": 0xFF08,
    "delete": 0xFFFF, "home": 0xFF50, "end": 0xFF57, "left": 0xFF51, "up": 0xFF52, "right": 0xFF53, "down": 0xFF54,
    "pageup": 0xFF55, "pagedown": 0xFF56, "space": 0x20, "shift": 0xFFE1, "ctrl": 0xFFE3, "control": 0xFFE3,
    "alt": 0xFFE9, "f2": 0xFFBF, "f4": 0xFFC1, "f5": 0xFFC2,
}


def keysym(name):
    if len(name) == 1:
        code = ord(name)
        return code if code < 0x100 else 0x01000000 | code
    try:
        return KEYSYMS[name.lower()]
    except KeyError:
        raise ValueError(f"no key is called {name!r}")


class Person:
    """Input to one screen. Every action pauses a little afterwards, as a person's hands do."""

    def __init__(self, host="127.0.0.1", port=5901, password=None, pace=0.35):
        self.pace = pace
        self.sock = socket.create_connection((host, port), timeout=20)
        self._handshake(os.environ.get("VNC_PW", "") if password is None else password)

    def _read(self, n):
        data = b""
        while len(data) < n:
            chunk = self.sock.recv(n - len(data))
            if not chunk:
                raise ConnectionError("the VNC server closed the connection")
            data += chunk
        return data

    def _handshake(self, password):
        self._read(12)
        self.sock.sendall(b"RFB 003.008\n")
        kinds = self._read(self._read(1)[0])
        if 2 in kinds:
            self.sock.sendall(b"\x02")
            self.sock.sendall(vnc_response(password, self._read(16)))
        elif 1 in kinds:
            self.sock.sendall(b"\x01")
        else:
            raise ConnectionError(f"the VNC server offers no way in this client knows: {list(kinds)}")
        if struct.unpack(">I", self._read(4))[0] != 0:
            raise PermissionError("the VNC server refused the password")
        self.sock.sendall(b"\x01")  # shared: whoever is watching stays connected
        self.width, self.height = struct.unpack(">HH", self._read(4))
        self._read(16)
        self._read(struct.unpack(">I", self._read(4))[0])
        # No framebuffer is ever asked for, so the server sends nothing back.

    def _pointer(self, x, y, buttons=0):
        self.sock.sendall(struct.pack(">BBHH", 5, buttons, int(x), int(y)))

    def _key(self, sym, down):
        self.sock.sendall(struct.pack(">BBHI", 4, 1 if down else 0, 0, sym))

    def move(self, x, y):
        self._pointer(x, y)
        time.sleep(0.08)

    def click(self, x, y, button=1, count=1):
        """A click (count=2: a double click) at a screen position."""

        mask = 1 << (button - 1)
        self.move(x, y)
        for _ in range(count):
            self._pointer(x, y, mask)
            time.sleep(0.05)
            self._pointer(x, y, 0)
            time.sleep(0.09)
        time.sleep(self.pace)

    def type(self, text, each=0.04):
        """Text as key presses, one character at a time."""

        for char in text:
            sym = keysym("Return" if char == "\n" else "Tab" if char == "\t" else char)
            self._key(sym, True)
            self._key(sym, False)
            time.sleep(each)
        time.sleep(self.pace)

    def key(self, combo):
        """One key or a chord: "Return", "ctrl+s", "shift+Tab"."""

        syms = [keysym(part) for part in combo.split("+")]
        for sym in syms:
            self._key(sym, True)
            time.sleep(0.03)
        for sym in reversed(syms):
            self._key(sym, False)
            time.sleep(0.03)
        time.sleep(self.pace)

    def wait(self, seconds):
        time.sleep(seconds)

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass
