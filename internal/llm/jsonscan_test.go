package llm

import "testing"

func TestFirstJSONObject(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{`{"a":1}`, `{"a":1}`, true},
		{"```json\n{\"a\":{\"b\":\"}\"}}\n```", `{"a":{"b":"}"}}`, true},
		{`prefix {"s":"\"{"} tail}`, `{"s":"\"{"}`, true},
		{`{"a":1}   ` + "\n\n", `{"a":1}`, true},
		{`{"a":`, "", false},
		{`[]`, "", false},
		{``, "", false},
		{`{"s":"a\\"} still inside? no: backslash escaped"}`, `{"s":"a\\"}`, true},
	}
	for _, c := range cases {
		got, ok := FirstJSONObject(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("FirstJSONObject(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}
