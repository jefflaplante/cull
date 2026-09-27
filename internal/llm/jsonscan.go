package llm

// FirstJSONObject returns the first complete top-level {...} in s, skipping any
// prefix (prose, code fences) and ignoring whatever follows. Braces inside JSON
// strings don't count. ok is false until the object closes, which is what lets
// the streaming OpenAI backend hang up as soon as the answer is complete.
func FirstJSONObject(s string) (obj string, ok bool) {
	start := -1
	depth := 0
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if start < 0 {
			if c == '{' {
				start, depth = i, 1
			}
			continue
		}
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}
