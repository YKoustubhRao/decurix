package desktop

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// A Windows shortcut (.lnk) names a program and the arguments it is started
// with. A worker's shortcut is often one - the profile shortcut Chrome puts
// on a person's desktop, "Koustubh - Chrome.lnk" - and what it is a shortcut
// to decides whether it is the worker's browser. The helper resolves one with
// Windows' own shell when it launches it; this reads the same two facts out
// of the file, so asking needs no helper and no Windows.
//
// The layout is Microsoft's [MS-SHLLINK]: a 76-byte header with flags, then,
// each only when its flag is set, the target as shell item IDs (skipped), a
// LinkInfo block holding the target's path, and counted strings - name,
// relative path, working folder, arguments, icon.

const (
	lnkHeaderSize = 0x4C

	lnkHasIDList       = 1 << 0
	lnkHasLinkInfo     = 1 << 1
	lnkHasName         = 1 << 2
	lnkHasRelativePath = 1 << 3
	lnkHasWorkingDir   = 1 << 4
	lnkHasArguments    = 1 << 5
	lnkIsUnicode       = 1 << 7

	// A shortcut is a few kilobytes; anything far past that is not one.
	lnkMaxBytes = 1 << 20
)

// readShortcut is the program a .lnk file starts and the arguments it gives
// it. ok is false for a file that cannot be read, is not a shortcut, or names
// no program on a disk (a Store app, a folder of the shell).
func readShortcut(path string) (target, args string, ok bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > lnkMaxBytes {
		return "", "", false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}

	target, relative, args, ok := parseShortcut(data)
	if !ok {
		return "", "", false
	}

	// A shortcut written without a LinkInfo block says where its target is
	// only from where it is itself.
	if target == "" && relative != "" {
		target = filepath.Join(filepath.Dir(path), filepath.FromSlash(strings.ReplaceAll(relative, `\`, "/")))
	}

	return target, args, target != ""
}

// parseShortcut reads a shortcut's bytes: the target's full path when the
// file holds one, its path relative to the shortcut, and its arguments. Every
// offset is checked against the data: a shortcut is a file from anywhere.
func parseShortcut(data []byte) (target, relative, args string, ok bool) {
	if len(data) < lnkHeaderSize || binary.LittleEndian.Uint32(data) != lnkHeaderSize {
		return "", "", "", false
	}

	flags := binary.LittleEndian.Uint32(data[20:])
	rest := data[lnkHeaderSize:]

	if flags&lnkHasIDList != 0 {
		if len(rest) < 2 {
			return "", "", "", false
		}
		size := 2 + int(binary.LittleEndian.Uint16(rest))
		if size > len(rest) {
			return "", "", "", false
		}
		rest = rest[size:]
	}

	if flags&lnkHasLinkInfo != 0 {
		if len(rest) < 4 {
			return "", "", "", false
		}
		size := int(binary.LittleEndian.Uint32(rest))
		if size < 4 || size > len(rest) {
			return "", "", "", false
		}
		target = linkInfoPath(rest[:size])
		rest = rest[size:]
	}

	// The counted strings, in the order the format gives them.
	for _, s := range []struct {
		flag uint32
		into *string
	}{
		{lnkHasName, nil},
		{lnkHasRelativePath, &relative},
		{lnkHasWorkingDir, nil},
		{lnkHasArguments, &args},
	} {
		if flags&s.flag == 0 {
			continue
		}
		if len(rest) < 2 {
			return "", "", "", false
		}

		count := int(binary.LittleEndian.Uint16(rest))
		size := count
		if flags&lnkIsUnicode != 0 {
			size = count * 2
		}
		if 2+size > len(rest) {
			return "", "", "", false
		}

		if s.into != nil {
			if flags&lnkIsUnicode != 0 {
				*s.into = utf16String(rest[2 : 2+size])
			} else {
				*s.into = string(rest[2 : 2+size])
			}
		}
		rest = rest[2+size:]
	}

	return target, relative, args, true
}

// linkInfoPath is the target's path on a local disk out of a LinkInfo block:
// its base path and what follows it, in Unicode when the block has that.
// "" for a target on a network share or a block that does not hold together.
func linkInfoPath(info []byte) string {
	if len(info) < 28 {
		return ""
	}

	header := int(binary.LittleEndian.Uint32(info[4:]))
	if binary.LittleEndian.Uint32(info[8:])&1 == 0 { // VolumeIDAndLocalBasePath
		return ""
	}

	if header >= 0x24 && len(info) >= 0x24 {
		base := int(binary.LittleEndian.Uint32(info[28:]))
		suffix := int(binary.LittleEndian.Uint32(info[32:]))
		if base > 0 && base < len(info) {
			path := utf16Z(info[base:])
			if suffix > 0 && suffix < len(info) {
				path += utf16Z(info[suffix:])
			}
			if path != "" {
				return path
			}
		}
	}

	base := int(binary.LittleEndian.Uint32(info[16:]))
	suffix := int(binary.LittleEndian.Uint32(info[24:]))
	if base <= 0 || base >= len(info) {
		return ""
	}

	path := ansiZ(info[base:])
	if suffix > 0 && suffix < len(info) {
		path += ansiZ(info[suffix:])
	}

	return path
}

// ansiZ is the string that ends at the first zero byte. The bytes are in the
// computer's own code page; a program's path is nearly always plain ASCII,
// and one that is not is carried in the Unicode fields beside it.
func ansiZ(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}

	return string(b)
}

// utf16Z is the UTF-16 string that ends at the first zero character.
func utf16Z(b []byte) string {
	for i := 0; i+1 < len(b); i += 2 {
		if b[i] == 0 && b[i+1] == 0 {
			return utf16String(b[:i])
		}
	}

	return utf16String(b[:len(b)&^1])
}

func utf16String(b []byte) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[2*i:])
	}

	return string(utf16.Decode(units))
}
