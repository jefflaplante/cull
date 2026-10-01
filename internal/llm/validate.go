package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// Validate checks raw JSON against the subset of JSON Schema the prompts use:
// type, required, properties, additionalProperties:false, enum, items. Not a
// general validator: backends that can't enforce schemas (local servers) and
// models that drift still get caught before the policy sees the result.
func Validate(schema map[string]any, raw []byte) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return check(schema, doc, "")
}

func check(s map[string]any, v any, path string) error {
	at := path
	if at == "" {
		at = "(root)"
	}
	if typ, _ := s["type"].(string); typ != "" {
		if !typeMatches(typ, v) {
			return fmt.Errorf("%s: want %s, got %s", at, typ, describe(v))
		}
	}
	if enum := strings2(s["enum"]); enum != nil {
		str, ok := v.(string)
		if !ok || !contains(enum, str) {
			return fmt.Errorf("%s: %v not one of %v", at, v, enum)
		}
	}
	switch val := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for _, req := range strings2(s["required"]) {
			if _, ok := val[req]; !ok {
				return fmt.Errorf("%s: missing required %q", at, join(path, req))
			}
		}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic error messages
		for _, k := range keys {
			sub, ok := props[k].(map[string]any)
			if !ok {
				if ap, set := s["additionalProperties"].(bool); set && !ap {
					return fmt.Errorf("%s: unexpected property %q", at, k)
				}
				continue
			}
			if err := check(sub, val[k], join(path, k)); err != nil {
				return err
			}
		}
	case []any:
		if items, ok := s["items"].(map[string]any); ok {
			for i, it := range val {
				if err := check(items, it, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func typeMatches(typ string, v any) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "null":
		return v == nil
	}
	return true // unknown type keyword: don't reject
}

func describe(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// strings2 accepts []string (Go-literal schemas) or []any (decoded schemas).
func strings2(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// unportable are schema keywords that structured-output implementations reject
// (Anthropic) or ignore; ranges are enforced in Go (eval.Policy.Sanitize).
var unportable = map[string]bool{"minimum": true, "maximum": true, "minLength": true, "maxLength": true}

// Portable returns a deep copy of schema without unportable keywords, as a Schema
// that marshals its properties in their required order.
func Portable(schema map[string]any) Schema {
	return Schema(strip(schema).(map[string]any))
}

// Schema is a JSON Schema that marshals each object's properties in the order of
// its "required" list (the rest after them, sorted). Structured output is written
// in schema order, so this is the order the model answers in: evidence fields
// listed first are written before the verdict they support. A Go map would
// otherwise marshal its keys alphabetically.
type Schema map[string]any

func (s Schema) MarshalJSON() ([]byte, error) { return marshalOrdered(map[string]any(s)) }

func marshalOrdered(v any) ([]byte, error) {
	switch x := v.(type) {
	case Schema:
		return marshalOrdered(map[string]any(x))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if props, ok := x["properties"].(map[string]any); ok {
			return writeObject(keys, func(k string) ([]byte, error) {
				if k == "properties" {
					return writeObject(propertyOrder(props, requiredOf(x)), func(p string) ([]byte, error) { return marshalOrdered(props[p]) })
				}
				return marshalOrdered(x[k])
			})
		}
		return writeObject(keys, func(k string) ([]byte, error) { return marshalOrdered(x[k]) })
	case []any:
		var b bytes.Buffer
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			eb, err := marshalOrdered(e)
			if err != nil {
				return nil, err
			}
			b.Write(eb)
		}
		b.WriteByte(']')
		return b.Bytes(), nil
	}
	return json.Marshal(v)
}

// writeObject writes a JSON object with keys in the given order.
func writeObject(keys []string, value func(string) ([]byte, error)) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := value(k)
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// requiredOf reads an object schema's "required" names.
func requiredOf(x map[string]any) []string {
	switch r := x["required"].(type) {
	case []string:
		return r
	case []any:
		var out []string
		for _, e := range r {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// propertyOrder is required's order for the properties it names, then the rest sorted.
func propertyOrder(props map[string]any, required []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range required {
		if _, ok := props[k]; ok && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range props {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func strip(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if !unportable[k] {
				out[k] = strip(e)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = strip(e)
		}
		return out
	}
	return v
}
