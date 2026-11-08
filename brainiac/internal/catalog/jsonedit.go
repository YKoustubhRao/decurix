package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// jnode is a JSON value that keeps its keys in the order they were written,
// so a catalog file can be changed in place: the pins added, nothing else
// moved. A signed catalog is reviewed as a diff, and a diff of a file whose
// keys were reordered says nothing.
type jnode struct {
	kind byte // '{' object, '[' array, 's' a scalar kept as its JSON text
	keys []string
	vals []*jnode
	raw  []byte
}

// parseOrdered reads one JSON value.
func parseOrdered(data []byte) (*jnode, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := readNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("catalog: more than one JSON value in the file")
	}
	return n, nil
}

func readNode(dec *json.Decoder) (*jnode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &jnode{kind: '{'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("catalog: object key %v", kt)
				}
				v, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.keys, n.vals = append(n.keys, k), append(n.vals, v)
			}
			_, err := dec.Token() // }
			return n, err
		case '[':
			n := &jnode{kind: '['}
			for dec.More() {
				v, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.vals = append(n.vals, v)
			}
			_, err := dec.Token() // ]
			return n, err
		}
		return nil, fmt.Errorf("catalog: unexpected %v", t)
	default:
		raw, err := scalarJSON(t)
		return &jnode{kind: 's', raw: raw}, err
	}
}

// scalarJSON writes a scalar the way the catalog's files hold it: strings
// without HTML escaping, numbers as written.
func scalarJSON(v any) ([]byte, error) {
	if n, ok := v.(json.Number); ok {
		return []byte(n.String()), nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// encode writes the value with two-space indentation, as the catalog's
// files are written.
func (n *jnode) encode(b *bytes.Buffer, indent string) {
	switch n.kind {
	case 's':
		b.Write(n.raw)
	case '{':
		if len(n.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range n.keys {
			kj, _ := scalarJSON(k)
			b.WriteString(indent + "  ")
			b.Write(kj)
			b.WriteString(": ")
			n.vals[i].encode(b, indent+"  ")
			if i < len(n.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case '[':
		if len(n.vals) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, v := range n.vals {
			b.WriteString(indent + "  ")
			v.encode(b, indent+"  ")
			if i < len(n.vals)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	}
}

func (n *jnode) bytes() []byte {
	var b bytes.Buffer
	n.encode(&b, "")
	b.WriteByte('\n')
	return b.Bytes()
}

// get is the value under key, or nil.
func (n *jnode) get(key string) *jnode {
	if n == nil || n.kind != '{' {
		return nil
	}
	for i, k := range n.keys {
		if k == key {
			return n.vals[i]
		}
	}
	return nil
}

// set puts key's value: in place when the key is there, else right after
// the key named after (or at the end when that is not there either).
func (n *jnode) set(key string, v *jnode, after string) {
	for i, k := range n.keys {
		if k == key {
			n.vals[i] = v
			return
		}
	}
	at := len(n.keys)
	for i, k := range n.keys {
		if k == after {
			at = i + 1
		}
	}
	n.keys = append(n.keys[:at], append([]string{key}, n.keys[at:]...)...)
	n.vals = append(n.vals[:at], append([]*jnode{v}, n.vals[at:]...)...)
}

func str(s string) *jnode {
	raw, _ := scalarJSON(s)
	return &jnode{kind: 's', raw: raw}
}

func (n *jnode) text() string {
	var s string
	if n != nil && n.kind == 's' {
		_ = json.Unmarshal(n.raw, &s)
	}
	return s
}

// WritePins copies the pins of models and runtime (rt may be nil) into a
// catalog or overlay file's JSON, changing nothing else: a source's
// "revision" goes after its "repo", a GitHub source's "sha256" after its
// "asset", a file's "sha256" after its "role", and the runtime's
// "llamacpp_sha256" after its "llamacpp_tag". Models are matched by id and
// sources by position. The file must already be laid out the way this
// writes JSON (two-space indentation, as the shipped catalog is), or the
// change could not be read as a diff, and it is refused.
func WritePins(data []byte, models []Model, rt *Runtime) ([]byte, error) {
	root, err := parseOrdered(data)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(root.bytes(), normaliseEOL(data)) {
		return nil, errors.New("catalog: the file is not laid out as two-space JSON, so its pins cannot be added as a plain diff; format it first")
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if ms := root.get("models"); ms != nil {
		for _, mn := range ms.vals {
			m, ok := byID[mn.get("id").text()]
			if !ok {
				continue
			}
			srcs := mn.get("sources")
			if srcs == nil || len(srcs.vals) != len(m.Sources) {
				return nil, fmt.Errorf("catalog: model %s: the file's sources do not match", m.ID)
			}
			for j, sn := range srcs.vals {
				pinSource(sn, m.Sources[j])
			}
		}
	}
	if rt != nil && len(rt.LlamaCppSHA256) > 0 {
		if rn := root.get("runtime"); rn != nil && rn.get("llamacpp_tag").text() == rt.LlamaCppTag {
			names := make([]string, 0, len(rt.LlamaCppSHA256))
			for k := range rt.LlamaCppSHA256 {
				names = append(names, k)
			}
			sort.Strings(names)
			sums := &jnode{kind: '{'}
			for _, k := range names {
				sums.keys, sums.vals = append(sums.keys, k), append(sums.vals, str(rt.LlamaCppSHA256[k]))
			}
			rn.set("llamacpp_sha256", sums, "llamacpp_tag")
		}
	}
	return root.bytes(), nil
}

func pinSource(sn *jnode, s Source) {
	if s.Revision != "" {
		sn.set("revision", str(s.Revision), "repo")
	}
	if s.SHA256 != "" {
		sn.set("sha256", str(s.SHA256), "asset")
	}
	files := sn.get("files")
	if files == nil {
		return
	}
	for _, fn := range files.vals {
		for _, f := range s.Files {
			if f.SHA256 != "" && f.Path == fn.get("path").text() {
				fn.set("sha256", str(f.SHA256), "role")
			}
		}
	}
}

func normaliseEOL(data []byte) []byte {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	return []byte(strings.TrimRight(s, "\n") + "\n")
}
