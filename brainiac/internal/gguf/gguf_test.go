package gguf

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// builder writes a GGUF header by hand, so the tests own every byte.
type builder struct {
	b    bytes.Buffer
	keys uint64
}

func (w *builder) str(s string) {
	binary.Write(&w.b, binary.LittleEndian, uint64(len(s)))
	w.b.WriteString(s)
}

func (w *builder) kv(key string, kind uint32, value func()) {
	w.keys++
	w.str(key)
	binary.Write(&w.b, binary.LittleEndian, kind)
	value()
}

func (w *builder) file(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("GGUF")
	binary.Write(&out, binary.LittleEndian, uint32(3))
	binary.Write(&out, binary.LittleEndian, uint64(0))
	binary.Write(&out, binary.LittleEndian, w.keys)
	out.Write(w.b.Bytes())
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(p, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// model is a header with the kinds of metadata real files have around the
// template, so reading it has to skip each of them.
func model(template string) *builder {
	w := &builder{}
	w.kv("general.architecture", typeString, func() { w.str("qwen35") })
	w.kv("general.alignment", typeUint32, func() { binary.Write(&w.b, binary.LittleEndian, uint32(32)) })
	w.kv("general.flag", typeBool, func() { w.b.WriteByte(1) })
	w.kv("tokenizer.ggml.scores", typeArray, func() {
		binary.Write(&w.b, binary.LittleEndian, uint32(typeFloat32))
		binary.Write(&w.b, binary.LittleEndian, uint64(3))
		w.b.Write(make([]byte, 12))
	})
	w.kv("tokenizer.ggml.tokens", typeArray, func() {
		binary.Write(&w.b, binary.LittleEndian, uint32(typeString))
		binary.Write(&w.b, binary.LittleEndian, uint64(2))
		w.str("hello")
		w.str("world")
	})
	if template != "" {
		w.kv(templateKey, typeString, func() { w.str(template) })
	}
	return w
}

func TestReadsTheTemplatePastOtherMetadata(t *testing.T) {
	got, err := Templates(model("{{ messages }}").file(t))
	if err != nil {
		t.Fatal(err)
	}
	if got[templateKey] != "{{ messages }}" || len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestReadsNamedTemplatesAndHashesThemAll(t *testing.T) {
	w := model("{{ a }}")
	w.kv(templateKey+".tool_use", typeString, func() { w.str("{{ b }}") })
	withTools, err := FileSHA(w.file(t))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := FileSHA(model("{{ a }}").file(t))
	if err != nil {
		t.Fatal(err)
	}
	if withTools == "" || withTools == plain {
		t.Fatalf("a named template did not change the hash: %q %q", withTools, plain)
	}
}

func TestSHAIsEmptyWithoutATemplateAndFollowsItsText(t *testing.T) {
	if got, err := FileSHA(model("").file(t)); err != nil || got != "" {
		t.Fatalf("%q %v", got, err)
	}
	a, _ := FileSHA(model("{{ a }}").file(t))
	b, _ := FileSHA(model("{{ a }}{{ 'send it elsewhere' }}").file(t))
	if a == "" || a == b {
		t.Fatalf("%q %q", a, b)
	}
}

func TestSHADoesNotConfuseKeysAndValues(t *testing.T) {
	if SHA(map[string]string{"ab": "c"}) == SHA(map[string]string{"a": "bc"}) {
		t.Fatal("key and value boundaries are not part of the hash")
	}
}

// A hostile header must fail cleanly, not allocate what it claims or read on.
func TestRejectsMalformedHeaders(t *testing.T) {
	huge := &builder{}
	huge.keys = 1
	binary.Write(&huge.b, binary.LittleEndian, uint64(1<<62))

	hugeValue := &builder{}
	hugeValue.kv(templateKey, typeString, func() { binary.Write(&hugeValue.b, binary.LittleEndian, uint64(1<<62)) })

	hugeArray := &builder{}
	hugeArray.kv("a", typeArray, func() {
		binary.Write(&hugeArray.b, binary.LittleEndian, uint32(typeUint8))
		binary.Write(&hugeArray.b, binary.LittleEndian, uint64(1<<62))
	})

	nested := &builder{}
	nested.kv("a", typeArray, func() {
		binary.Write(&nested.b, binary.LittleEndian, uint32(typeArray))
		binary.Write(&nested.b, binary.LittleEndian, uint64(1))
	})

	unknown := &builder{}
	unknown.kv("a", 99, func() {})

	truncated := &builder{}
	truncated.keys = 5

	for name, w := range map[string]*builder{"huge key": huge, "huge value": hugeValue, "huge array": hugeArray,
		"nested array": nested, "unknown type": unknown, "truncated": truncated} {
		if _, err := Templates(w.file(t)); err == nil {
			t.Errorf("%s: read without an error", name)
		}
	}
	p := filepath.Join(t.TempDir(), "x.gguf")
	for name, data := range map[string]string{"empty": "", "wrong magic": strings.Repeat("x", 40),
		"old version": "GGUF\x01\x00\x00\x00" + strings.Repeat("\x00", 16)} {
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Templates(p); err == nil {
			t.Errorf("%s: read without an error", name)
		}
	}
}

// A real model's file, or just its first megabytes: the header is all this
// reads. BRAINIAC_LIVE_GGUF is the path.
func TestLiveRealFile(t *testing.T) {
	path := os.Getenv("BRAINIAC_LIVE_GGUF")
	if path == "" {
		t.Skip("set BRAINIAC_LIVE_GGUF to read a real file")
	}
	got, err := Templates(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no chat template in a chat model's file")
	}
	for k, v := range got {
		t.Logf("%s: %d bytes, starts %.60q", k, len(v), v)
	}
	t.Logf("sha %s", SHA(got))
}
