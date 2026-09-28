package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCode runs `claude -p` (Claude Code headless) once per call so model calls
// use the user's Claude subscription. The flag set was verified to give a clean
// ~525-token context: no tools, no MCP, no settings (so no hooks or plugins), no
// session files. See CLAUDE.md, "Model access via a Claude subscription".
type ClaudeCode struct {
	Bin       string
	Model     string
	QuotaStop float64 // stop the run once 5-hour utilization reaches this
	Timeout   time.Duration
}

func NewClaudeCode(bin, model string, quotaStop float64) *ClaudeCode {
	return &ClaudeCode{Bin: bin, Model: model, QuotaStop: quotaStop, Timeout: 5 * time.Minute}
}

func (c *ClaudeCode) Name() string { return "claude-code" }

// scrubbedEnv are variables that would switch claude to API billing, route it to
// a cloud provider account (Bedrock/Vertex/Foundry), or make it think it is
// nested inside another Claude Code session.
var scrubbedEnv = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT",
}

func (c *ClaudeCode) Call(ctx context.Context, req Request) (*Response, error) {
	schema, err := json.Marshal(Portable(req.Schema))
	if err != nil {
		return nil, err
	}
	stdin, err := streamJSONMessage(req.Parts)
	if err != nil {
		return nil, err
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", c.Model, "--tools", "", "--no-session-persistence", "--strict-mcp-config",
		"--setting-sources", "", "--system-prompt", req.System, "--json-schema", string(schema)}
	return validated(ctx, req.Schema, func(ctx context.Context) (*Response, error) { return c.run(ctx, args, stdin) })
}

func streamJSONMessage(parts []Part) ([]byte, error) {
	content := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.JPEG != nil {
			content = append(content, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/jpeg", "data": base64.StdEncoding.EncodeToString(p.JPEG),
			}})
		} else {
			content = append(content, map[string]any{"type": "text", "text": p.Text})
		}
	}
	b, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	return append(b, '\n'), err
}

func (c *ClaudeCode) run(ctx context.Context, args []string, stdin []byte) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "gophotocull-claude-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = dir // empty: no CLAUDE.md or project settings to discover
	cmd.Env = childEnv()
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Bin, err)
	}

	resp, perr := c.parse(out)
	if perr != nil {
		cancel() // e.g. wrong auth: kill before it spends anything
	}
	io.Copy(io.Discard, out) // a blocked writer would never exit, and Wait would hang
	werr := cmd.Wait()
	switch {
	case perr != nil:
		return resp, perr
	case resp == nil && werr != nil:
		return nil, fmt.Errorf("claude exited: %v: %s", werr, truncate(stderr.Bytes(), 300))
	case resp == nil:
		return nil, fmt.Errorf("claude produced no result event: %s", truncate(stderr.Bytes(), 300))
	}
	return resp, c.quotaErr(resp.Quota)
}

func childEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		keep := true
		for _, k := range scrubbedEnv {
			if strings.HasPrefix(kv, k+"=") {
				keep = false
				break
			}
		}
		if keep {
			env = append(env, kv)
		}
	}
	return env
}

func (c *ClaudeCode) quotaErr(q *Quota) error {
	if q == nil {
		return nil
	}
	// Any "allowed…" status (e.g. allowed_warning near the limit) still allows the
	// call; --quota-stop is what decides when to stop.
	if (q.Status != "" && !strings.HasPrefix(q.Status, "allowed")) || (c.QuotaStop > 0 && q.FiveHour >= c.QuotaStop) {
		return fmt.Errorf("%w: status %s, 5-hour window %.0f%% used (stop at %.0f%%)", ErrQuotaStop, q.Status, 100*q.FiveHour, 100*c.QuotaStop)
	}
	return nil
}

// parse reads stream-json events until the result. It returns an error wrapping
// ErrAbortRun as soon as init shows claude is not on subscription auth.
func (c *ClaudeCode) parse(r io.Reader) (*Response, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 256*1024), 16*1024*1024)
	var quota *Quota
	sawInit := false
	for sc.Scan() {
		var ev struct {
			Type          string `json:"type"`
			Subtype       string `json:"subtype"`
			APIKeySource  string `json:"apiKeySource"`
			RateLimitInfo *struct {
				Status         string `json:"status"`
				UnifiedWindows map[string]struct {
					Utilization float64 `json:"utilization"`
				} `json:"unifiedWindows"`
			} `json:"rate_limit_info"`
			IsError          bool            `json:"is_error"`
			Result           string          `json:"result"`
			StructuredOutput json.RawMessage `json:"structured_output"`
			Usage            struct {
				InputTokens              int `json:"input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				OutputTokens             int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype != "init" {
				continue
			}
			if ev.APIKeySource != "none" {
				return nil, fmt.Errorf("%w: claude reported apiKeySource=%q, not the subscription; refusing to bill an API account",
					ErrAbortRun, ev.APIKeySource)
			}
			sawInit = true
		case "rate_limit_event":
			if ev.RateLimitInfo != nil {
				quota = &Quota{
					Status:   ev.RateLimitInfo.Status,
					FiveHour: ev.RateLimitInfo.UnifiedWindows["five_hour"].Utilization,
					SevenDay: ev.RateLimitInfo.UnifiedWindows["seven_day"].Utilization,
				}
			}
		case "result":
			if !sawInit { // fail closed: the billing guard depends on the init event
				return nil, fmt.Errorf("%w: claude emitted no init event, so subscription auth can't be confirmed", ErrAbortRun)
			}
			u := Usage{
				InputTokens:  ev.Usage.InputTokens + ev.Usage.CacheCreationInputTokens + ev.Usage.CacheReadInputTokens,
				OutputTokens: ev.Usage.OutputTokens,
			}
			if ev.IsError {
				return &Response{Usage: u, Quota: quota}, fmt.Errorf("claude: %s", ev.Result)
			}
			if len(ev.StructuredOutput) == 0 || string(ev.StructuredOutput) == "null" {
				return &Response{Usage: u, Quota: quota}, fmt.Errorf("claude result has no structured_output")
			}
			return &Response{JSON: ev.StructuredOutput, Usage: u, Quota: quota}, nil
		}
	}
	return nil, sc.Err()
}
