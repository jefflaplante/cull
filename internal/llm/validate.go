package llm

import (
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

// Portable returns a deep copy of schema without unportable keywords.
func Portable(schema map[string]any) map[string]any {
	return strip(schema).(map[string]any)
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
