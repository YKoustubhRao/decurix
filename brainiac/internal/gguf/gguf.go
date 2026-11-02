// Package gguf reads the chat templates stored in a GGUF file's header. A
// template is code the model server runs on every request, so what a file
// carries must be visible before the file is trusted. The file is not a
// trusted input: every length is bounded, and a malformed header is an error.
//
// Depends on nothing internal.
package gguf

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const (
	templateKey = "tokenizer.chat_template"
	maxKeys     = 1 << 20
	maxKeyLen   = 1 << 16
	maxString   = 64 << 20
	maxArray    = 1 << 40
)

// The value types of the GGUF format.
const (
	typeUint8 = iota
	typeInt8
	typeUint16
	typeInt16
	typeUint32
	typeInt32
	typeFloat32
	typeBool
	typeString
	typeArray
	typeUint64
	typeInt64
	typeFloat64
)

// Templates returns every chat template in a GGUF file, by metadata key: the
// default one and any named variants (tokenizer.chat_template.<name>).
func Templates(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out, err := read(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// SHA is the sha256 of a file's templates, "" when it has none. It changes
// when any template changes, and on nothing else in the file.
func SHA(templates map[string]string) string {
	if len(templates) == 0 {
		return ""
	}
	keys := make([]string, 0, len(templates))
	for k := range templates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%d:%s%d:%s", len(k), k, len(templates[k]), templates[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FileSHA is SHA of the templates in the file at path.
func FileSHA(path string) (string, error) {
	t, err := Templates(path)
	if err != nil {
		return "", err
	}
	return SHA(t), nil
}

func read(r *bufio.Reader) (map[string]string, error) {
	var head [24]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("not a GGUF file: %w", err)
	}
	if string(head[:4]) != "GGUF" {
		return nil, errors.New("not a GGUF file")
	}
	if v := binary.LittleEndian.Uint32(head[4:8]); v < 2 || v > 3 {
		return nil, fmt.Errorf("GGUF version %d is not read here", v)
	}
	count := binary.LittleEndian.Uint64(head[16:24])
	if count > maxKeys {
		return nil, fmt.Errorf("%d metadata keys is too many", count)
	}
	out := map[string]string{}
	for i := uint64(0); i < count; i++ {
		key, err := readString(r, maxKeyLen)
		if err != nil {
			return nil, fmt.Errorf("metadata key %d: %w", i, err)
		}
		var kind uint32
		if err := binary.Read(r, binary.LittleEndian, &kind); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if kind == typeString && (key == templateKey || strings.HasPrefix(key, templateKey+".")) {
			if out[key], err = readString(r, maxString); err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			continue
		}
		if err := skip(r, kind); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	return out, nil
}

func readString(r *bufio.Reader, limit uint64) (string, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > limit {
		return "", fmt.Errorf("a string of %d bytes is too long", n)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return string(b), err
}

func skipString(r *bufio.Reader) error {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return err
	}
	if n > maxString {
		return fmt.Errorf("a string of %d bytes is too long", n)
	}
	_, err := io.CopyN(io.Discard, r, int64(n))
	return err
}

func width(kind uint32) (int64, bool) {
	switch kind {
	case typeUint8, typeInt8, typeBool:
		return 1, true
	case typeUint16, typeInt16:
		return 2, true
	case typeUint32, typeInt32, typeFloat32:
		return 4, true
	case typeUint64, typeInt64, typeFloat64:
		return 8, true
	}
	return 0, false
}

func skip(r *bufio.Reader, kind uint32) error {
	if w, ok := width(kind); ok {
		_, err := io.CopyN(io.Discard, r, w)
		return err
	}
	switch kind {
	case typeString:
		return skipString(r)
	case typeArray:
		var elem uint32
		var n uint64
		if err := binary.Read(r, binary.LittleEndian, &elem); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			return err
		}
		if n > maxArray {
			return fmt.Errorf("an array of %d items is too long", n)
		}
		if w, ok := width(elem); ok {
			_, err := io.CopyN(io.Discard, r, w*int64(n))
			return err
		}
		if elem != typeString {
			return fmt.Errorf("an array of type %d is not read here", elem)
		}
		for i := uint64(0); i < n; i++ {
			if err := skipString(r); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown value type %d", kind)
}
