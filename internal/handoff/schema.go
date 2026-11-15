// schema.go is the contract a delegated result may be held to (after hermes-agent's delegate output_schema, in our own
// terms): a small subset of JSON Schema - an object, the keys it must have, and each key's type - that the receiving
// worker's report ends with as one JSON object, checked in code when the result comes back.

package handoff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Limits on a contract.
const (
	// MaxSchemaBytes bounds a contract as sent.
	MaxSchemaBytes = 4000
	// MaxSchemaKeys bounds the keys a contract names.
	MaxSchemaKeys = 40
	// MaxDataBytes bounds the result object read back from a report.
	MaxDataBytes = 32 << 10
)

// ResultMarker is the line the receiving worker puts before its result object.
const ResultMarker = "RESULT JSON:"

// Schema is a delegated result's contract: an object with Required keys and each key's Type.
type Schema struct {
	Type       string              `json:"type"`
	Required   []string            `json:"required,omitempty"`
	Properties map[string]Property `json:"properties,omitempty"`
}

// Property is one key's type, and for an array the type of its items.
type Property struct {
	Type  string    `json:"type"`
	Items *Property `json:"items,omitempty"`
}

// schemaTypes are the JSON types a property may be.
var schemaTypes = map[string]bool{"string": true, "number": true, "integer": true, "boolean": true, "array": true,
	"object": true, "null": true}

// ParseSchema reads a contract, refusing (InputError) anything outside the subset: it must be an object type, name at
// most MaxSchemaKeys keys, each with a known type, and require only keys it describes. Empty raw is no contract.
func ParseSchema(raw json.RawMessage) (*Schema, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if len(trimmed) > MaxSchemaBytes {
		return nil, refuse("output_schema is longer than %d bytes", MaxSchemaBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var s Schema
	if err := dec.Decode(&s); err != nil {
		return nil, refuse("output_schema is not a contract this control plane reads: an object type, required keys and each key's type")
	}
	if s.Type != "object" {
		return nil, refuse("output_schema must describe an object")
	}
	if len(s.Properties) > MaxSchemaKeys {
		return nil, refuse("output_schema names more than %d keys", MaxSchemaKeys)
	}
	for k, p := range s.Properties {
		if strings.TrimSpace(k) == "" || len(k) > MaxInputKey {
			return nil, refuse("output_schema has an empty or too long key")
		}
		if !schemaTypes[p.Type] || (p.Items != nil && (p.Type != "array" || !schemaTypes[p.Items.Type] || p.Items.Items != nil)) {
			return nil, refuse("output_schema key %q has a type this control plane does not check", k)
		}
	}
	for _, k := range s.Required {
		if _, ok := s.Properties[k]; !ok {
			return nil, refuse("output_schema requires %q but does not describe it", k)
		}
	}
	return &s, nil
}

// typeOf is the JSON type of a decoded value (json.Number for numbers).
func typeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if !strings.ContainsAny(x.String(), ".eE") {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// fits reports whether a value of type got satisfies want (an integer is a number).
func fits(got, want string) bool {
	return got == want || (want == "number" && got == "integer")
}

// Validate checks data against the contract and lists every mismatch ([] when it holds). An empty object, or one
// that is not an object at all, never holds: an empty result is a failure.
func (s *Schema) Validate(data json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return []string{"the result is not JSON"}
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return []string{"the result is not a JSON object"}
	}
	if len(obj) == 0 {
		return []string{"the result object is empty"}
	}
	var errs []string
	for _, k := range s.Required {
		if _, ok := obj[k]; !ok {
			errs = append(errs, fmt.Sprintf("required key %q is missing", k))
		}
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		val, ok := obj[k]
		if !ok {
			continue
		}
		p := s.Properties[k]
		got := typeOf(val)
		if !fits(got, p.Type) {
			errs = append(errs, fmt.Sprintf("key %q is %s, the contract says %s", k, got, p.Type))
			continue
		}
		if items, isList := val.([]any); isList && p.Items != nil {
			for i, item := range items {
				if g := typeOf(item); !fits(g, p.Items.Type) {
					errs = append(errs, fmt.Sprintf("item %d of key %q is %s, the contract says %s", i, k, g, p.Items.Type))
					break
				}
			}
		}
	}
	return errs
}

// ExtractResult reads the result object a report ends with: the JSON value after the last ResultMarker line (a
// ```json fence around it is allowed). false when there is none, or it is larger than MaxDataBytes.
func ExtractResult(report string) (json.RawMessage, bool) {
	at := strings.LastIndex(report, ResultMarker)
	if at < 0 {
		return nil, false
	}
	rest := strings.TrimSpace(report[at+len(ResultMarker):])
	if strings.HasPrefix(rest, "```") {
		rest = strings.TrimPrefix(strings.TrimPrefix(rest, "```"), "json")
	}
	dec := json.NewDecoder(strings.NewReader(rest))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil || len(raw) > MaxDataBytes {
		return nil, false
	}
	return raw, true
}

// contractText is the contract as the receiving worker is told it.
func contractText(schema json.RawMessage) string {
	return "End your report with the line " + ResultMarker + " and then one JSON object that matches this contract " +
		"(a JSON Schema: the keys under \"required\" must be there, each key of the type given):\n" + string(schema) +
		"\nPut in it only values you saw while doing the work. An empty object is not a result."
}
