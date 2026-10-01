package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var testSchema = map[string]any{
	"type":                 "object",
	"required":             []string{"sharpness", "tags", "count"},
	"additionalProperties": false,
	"properties": map[string]any{
		"sharpness": map[string]any{
			"type":                 "object",
			"required":             []string{"score", "status"},
			"additionalProperties": false,
			"properties": map[string]any{
				"score":  map[string]any{"type": "number", "minimum": 0, "maximum": 10},
				"status": map[string]any{"type": "string", "enum": []string{"sharp", "soft"}},
			},
		},
		"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string", "maxLength": 20}},
		"count": map[string]any{"type": "integer"},
	},
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name, doc, wantErr string
	}{
		{"valid", `{"sharpness":{"score":8.5,"status":"sharp"},"tags":["a"],"count":2}`, ""},
		{"missing required", `{"sharpness":{"score":8.5,"status":"sharp"},"tags":[]}`, "count"},
		{"wrong type", `{"sharpness":{"score":"8","status":"sharp"},"tags":[],"count":1}`, "sharpness.score"},
		{"enum", `{"sharpness":{"score":8,"status":"blurry"},"tags":[],"count":1}`, "sharpness.status"},
		{"extra key", `{"sharpness":{"score":8,"status":"sharp"},"tags":[],"count":1,"x":1}`, "x"},
		{"array items", `{"sharpness":{"score":8,"status":"sharp"},"tags":[1],"count":1}`, "tags[0]"},
		{"integer rejects fraction", `{"sharpness":{"score":8,"status":"sharp"},"tags":[],"count":1.5}`, "count"},
		{"not json", `{"sharpness":`, "json"},
		{"top level not object", `[]`, "object"},
	}
	for _, c := range cases {
		err := Validate(testSchema, []byte(c.doc))
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.wantErr != "" && err == nil:
			t.Errorf("%s: expected error mentioning %q", c.name, c.wantErr)
		case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.wantErr)
		}
	}
}

func TestPortableStripsUnsupportedKeywordsWithoutMutating(t *testing.T) {
	p := Portable(testSchema)
	b, _ := json.Marshal(p)
	for _, k := range []string{"minimum", "maximum", "maxLength"} {
		if strings.Contains(string(b), k) {
			t.Errorf("portable schema still has %s: %s", k, b)
		}
	}
	orig, _ := json.Marshal(testSchema)
	if !strings.Contains(string(orig), "minimum") || !strings.Contains(string(orig), "maxLength") {
		t.Fatal("Portable mutated its input")
	}
	if !strings.Contains(string(b), `"additionalProperties":false`) || !strings.Contains(string(b), `"enum"`) {
		t.Fatalf("portable schema lost supported keywords: %s", b)
	}
}

func TestValidatedRetriesOnceAndSumsUsage(t *testing.T) {
	good := `{"sharpness":{"score":8,"status":"sharp"},"tags":[],"count":1}`
	replies := []string{`{"sharpness":{}}`, good}
	calls := 0
	resp, err := validated(context.Background(), testSchema, func(context.Context) (*Response, error) {
		r := &Response{JSON: json.RawMessage(replies[calls]), Usage: Usage{InputTokens: 10, OutputTokens: 1}}
		calls++
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || string(resp.JSON) != good || resp.Usage.InputTokens != 20 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("calls=%d json=%s usage=%+v", calls, resp.JSON, resp.Usage)
	}

	calls = 0
	_, err = validated(context.Background(), testSchema, func(context.Context) (*Response, error) {
		calls++
		return &Response{JSON: json.RawMessage(`{}`)}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "schema") || calls != 2 {
		t.Fatalf("want schema error after 2 calls, got calls=%d err=%v", calls, err)
	}
}

func TestValidatedPassesThroughQuotaStopWithResponse(t *testing.T) {
	good := `{"sharpness":{"score":8,"status":"sharp"},"tags":[],"count":1}`
	resp, err := validated(context.Background(), testSchema, func(context.Context) (*Response, error) {
		return &Response{JSON: json.RawMessage(good)}, ErrQuotaStop
	})
	if !errors.Is(err, ErrQuotaStop) || resp == nil || string(resp.JSON) != good {
		t.Fatalf("want response plus ErrQuotaStop, got resp=%v err=%v", resp, err)
	}
}

func TestValidatedDoesNotRetryCallErrors(t *testing.T) {
	calls := 0
	_, err := validated(context.Background(), testSchema, func(context.Context) (*Response, error) {
		calls++
		return nil, errors.New("boom")
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestValidatedKeepsQuotaStopWhenOutputIsInvalid(t *testing.T) {
	_, err := validated(context.Background(), testSchema, func(context.Context) (*Response, error) {
		return &Response{JSON: json.RawMessage(`{}`)}, ErrQuotaStop
	})
	if !errors.Is(err, ErrQuotaStop) || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("want quota stop plus the schema problem, got %v", err)
	}
}

func TestValidatedKeepsUsageWhenBothAttemptsFail(t *testing.T) {
	resp, err := validated(context.Background(), testSchema, func(context.Context) (*Response, error) {
		return &Response{JSON: json.RawMessage(`{"wrong":1}`), Usage: Usage{InputTokens: 10, OutputTokens: 2}}, nil
	})
	if err == nil || resp == nil || resp.Usage.InputTokens != 20 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestPortableKeepsRequiredOrder(t *testing.T) {
	s := map[string]any{"type": "object", "required": []string{"zeta", "alpha", "mid"},
		"properties": map[string]any{"alpha": map[string]any{"type": "string"}, "mid": map[string]any{"type": "string"}, "zeta": map[string]any{"type": "string"}}}
	b, err := json.Marshal(Portable(s))
	if err != nil {
		t.Fatal(err)
	}
	z, a, m := bytes.Index(b, []byte(`"zeta":`)), bytes.Index(b, []byte(`"alpha":`)), bytes.Index(b, []byte(`"mid":`))
	if !(z < a && a < m) {
		t.Fatalf("properties not in required order: %s", b)
	}
	// Nested objects inside a wrapper map keep their order too (the request body).
	body, _ := json.Marshal(map[string]any{"schema": Portable(s)})
	if z, a := bytes.Index(body, []byte(`"zeta":`)), bytes.Index(body, []byte(`"alpha":`)); z > a {
		t.Fatalf("order lost when nested: %s", body)
	}
}
