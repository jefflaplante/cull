package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeClaude = `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_DIR/args.txt"
env > "$FAKE_DIR/env.txt"
pwd > "$FAKE_DIR/pwd.txt"
cat > "$FAKE_DIR/stdin.json"
cat "$FAKE_OUT"
`

func initEvent(src string) string {
	return `{"type":"system","subtype":"init","apiKeySource":"` + src + `","model":"claude-sonnet-5"}`
}

func rateEvent(status string, five float64) string {
	b, _ := json.Marshal(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
		"status": status, "unifiedWindows": map[string]any{
			"five_hour": map[string]any{"utilization": five}, "seven_day": map[string]any{"utilization": 0.5}}}})
	return string(b)
}

const okResult = `{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"score":7},` +
	`"usage":{"input_tokens":2,"cache_creation_input_tokens":5000,"cache_read_input_tokens":400,"output_tokens":90}}`

// setupFake installs the fake claude and returns its dir; out is the canned stdout.
func setupFake(t *testing.T, out ...string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(dir, "out.jsonl")
	os.WriteFile(outFile, []byte(strings.Join(out, "\n")+"\n"), 0o644)
	t.Setenv("FAKE_DIR", dir)
	t.Setenv("FAKE_OUT", outFile)
	return bin, dir
}

func TestClaudeCodeSuccessQuotaUsageAndIsolation(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "must-not-leak")
	t.Setenv("CLAUDECODE", "1")
	for _, k := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Setenv(k, "1") // would route claude to a cloud account and bill it
	}
	bin, dir := setupFake(t, initEvent("none"), rateEvent("allowed", 0.2), okResult)

	resp, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), tinyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.JSON) != `{"score":7}` || resp.Usage.InputTokens != 5402 || resp.Usage.OutputTokens != 90 {
		t.Fatalf("json=%s usage=%+v", resp.JSON, resp.Usage)
	}
	if resp.Quota == nil || resp.Quota.FiveHour != 0.2 || resp.Quota.SevenDay != 0.5 || resp.Quota.Status != "allowed" {
		t.Fatalf("quota=%+v", resp.Quota)
	}

	args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
	schema := got[len(got)-1]
	want := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", "sonnet", "--tools", "", "--no-session-persistence", "--strict-mcp-config",
		"--setting-sources", "", "--system-prompt", "sys", "--json-schema"}
	if strings.Join(got[:len(got)-1], "|") != strings.Join(want, "|") {
		t.Fatalf("argv\n got %q\nwant %q", got[:len(got)-1], want)
	}
	if strings.Contains(schema, "minimum") || !strings.Contains(schema, `"score"`) {
		t.Fatalf("json-schema arg %s", schema)
	}

	env, _ := os.ReadFile(filepath.Join(dir, "env.txt"))
	for _, k := range []string{"ANTHROPIC_API_KEY=", "CLAUDECODE=", "CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_USE_VERTEX=", "CLAUDE_CODE_USE_FOUNDRY="} {
		if strings.Contains("\n"+string(env), "\n"+k) {
			t.Errorf("child env contains %s", k)
		}
	}

	pwd, _ := os.ReadFile(filepath.Join(dir, "pwd.txt"))
	cwd, _ := os.Getwd()
	child := strings.TrimSpace(string(pwd))
	if child == cwd || child == "" {
		t.Errorf("child ran in the caller's cwd %q", child)
	}
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Errorf("child temp dir %s not removed", child)
	}

	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin.json"))
	lines := strings.Split(strings.TrimSpace(string(stdin)), "\n")
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &msg) != nil || msg.Type != "user" || msg.Message.Role != "user" {
		t.Fatalf("stdin not one user message: %s", stdin)
	}
	img := msg.Message.Content[1]
	if img["type"] != "image" || img["source"].(map[string]any)["type"] != "base64" {
		t.Fatalf("image block %v", img)
	}
}

func TestClaudeCodeAbortsWhenNotOnSubscription(t *testing.T) {
	bin, _ := setupFake(t, initEvent("ANTHROPIC_API_KEY"), okResult)
	_, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), tinyRequest())
	if !errors.Is(err, ErrAbortRun) || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("want ErrAbortRun naming the source, got %v", err)
	}
}

func TestClaudeCodeQuotaStop(t *testing.T) {
	for _, c := range []struct {
		status string
		five   float64
	}{{"allowed", 0.95}, {"rejected", 0.1}} {
		bin, _ := setupFake(t, initEvent("none"), rateEvent(c.status, c.five), okResult)
		resp, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), tinyRequest())
		if !errors.Is(err, ErrQuotaStop) || resp == nil || string(resp.JSON) != `{"score":7}` {
			t.Errorf("%s/%.2f: want result plus ErrQuotaStop, got resp=%v err=%v", c.status, c.five, resp, err)
		}
	}
}

func TestClaudeCodeReportsErrors(t *testing.T) {
	bin, _ := setupFake(t, initEvent("none"),
		`{"type":"result","subtype":"success","is_error":true,"result":"API Error: boom","usage":{}}`)
	if _, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), tinyRequest()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("is_error: got %v", err)
	}

	bin, _ = setupFake(t, initEvent("none")) // exits without a result event
	if _, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), tinyRequest()); err == nil || !strings.Contains(err.Error(), "no result") {
		t.Fatalf("missing result: got %v", err)
	}
}

func TestClaudeCodeResultWithoutInitAborts(t *testing.T) {
	bin, _ := setupFake(t, okResult) // no init event: subscription auth unconfirmed
	_, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), tinyRequest())
	if !errors.Is(err, ErrAbortRun) {
		t.Fatalf("want ErrAbortRun when auth can't be confirmed, got %v", err)
	}
}

func TestClaudeCodeAllowedWarningIsNotAStop(t *testing.T) {
	bin, _ := setupFake(t, initEvent("none"), rateEvent("allowed_warning", 0.5), okResult)
	if _, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), tinyRequest()); err != nil {
		t.Fatalf("allowed_warning below the threshold must not stop the run: %v", err)
	}
}

// A call claude rejects for quota is an error result: it must still stop the run
// (with the resume hint), not fail every remaining frame one by one.
func TestClaudeCodeQuotaOnErrorResultStops(t *testing.T) {
	errResult := `{"type":"result","subtype":"error","is_error":true,"result":"usage limit reached","usage":{"input_tokens":0,"output_tokens":0}}`
	bin, _ := setupFake(t, initEvent("none"), rateEvent("rejected", 1), errResult)
	_, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), tinyRequest())
	if !errors.Is(err, ErrQuotaStop) {
		t.Fatalf("want ErrQuotaStop, got %v", err)
	}
}

func TestClaudeCodeSevenDayWindowStops(t *testing.T) {
	week, _ := json.Marshal(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
		"status": "allowed", "unifiedWindows": map[string]any{
			"five_hour": map[string]any{"utilization": 0.1}, "seven_day": map[string]any{"utilization": 0.95}}}})
	bin, _ := setupFake(t, initEvent("none"), string(week), okResult)
	resp, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), tinyRequest())
	if !errors.Is(err, ErrQuotaStop) || resp == nil || !strings.Contains(err.Error(), "7-day") {
		t.Fatalf("want ErrQuotaStop naming the 7-day window with the answer kept, got resp=%v err=%v", resp, err)
	}
}

func TestClaudeCodePassesEffort(t *testing.T) {
	bin, dir := setupFake(t, initEvent("none"), okResult)
	req := tinyRequest()
	req.Effort = "low"
	if _, err := NewClaudeCode(bin, "sonnet", 0.9).Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	if !strings.Contains(string(args), "--effort\nlow\n") {
		t.Fatalf("args:\n%s", args)
	}
}
