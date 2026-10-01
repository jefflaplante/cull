package cli

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jefflaplante/cull/internal/config"
	"github.com/jefflaplante/cull/internal/llm"
)

// backendFlags select and construct a model backend: shared by judge and rank so
// both take the same --backend, --model, and per-backend credential flags.
type backendFlags struct {
	backend       string
	model         string
	apiKeyFile    string
	baseURL       string
	openaiKeyFile string
	openaiStream  bool
	claudeBin     string
	quotaStop     float64
	effort        string
	locateEffort  string
}

func (o *backendFlags) register(f *pflag.FlagSet) {
	f.StringVar(&o.backend, "backend", "anthropic", "model backend: anthropic, claude-code, or openai")
	f.StringVarP(&o.model, "model", "m", "", "model (default claude-sonnet-5-5 for anthropic, sonnet for claude-code; required for openai)")
	f.StringVar(&o.apiKeyFile, "api-key-file", "", "file containing the Anthropic API key")
	f.StringVar(&o.baseURL, "base-url", "http://127.0.0.1:8000/v1", "OpenAI-compatible endpoint (openai backend)")
	f.StringVar(&o.openaiKeyFile, "openai-key-file", "", "file containing a key for the openai backend (optional)")
	f.BoolVar(&o.openaiStream, "openai-stream", true, "stream and hang up once the JSON closes (openai backend)")
	f.StringVar(&o.claudeBin, "claude-bin", "claude", "Claude Code executable (claude-code backend)")
	f.StringVar(&o.effort, "effort", "", "model effort for evaluations and rankings: low, medium, high, xhigh or max (default: the model's own); recorded in the report")
	f.StringVar(&o.locateEffort, "locate-effort", "", "model effort for the locate call (default: the model's own); recorded in the report")
	f.Float64Var(&o.quotaStop, "quota-stop", 0.9, "stop when this fraction of the 5-hour or 7-day subscription window is used (claude-code backend)")
}

// validate checks the flags common to every backend. Callers with extra
// backend-dependent flags (e.g. judge's --batch) check those separately.
func (o *backendFlags) validate() error {
	if _, ok := backendDefaults[o.backend]; !ok {
		return fmt.Errorf("unknown --backend %q (want anthropic, claude-code, or openai)", o.backend)
	}
	if o.backend == "openai" && o.model == "" {
		return fmt.Errorf("--backend openai requires --model (see GET <base-url>/models)")
	}
	if o.quotaStop <= 0 || o.quotaStop > 1 {
		return fmt.Errorf("--quota-stop must be in (0, 1]")
	}
	for _, e := range []struct{ flag, v string }{{"--effort", o.effort}, {"--locate-effort", o.locateEffort}} {
		switch e.v {
		case "", "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Errorf("%s %q: want low, medium, high, xhigh or max", e.flag, e.v)
		}
		if e.v != "" && o.backend == "openai" {
			return fmt.Errorf("%s: the openai backend has no effort setting", e.flag)
		}
	}
	return nil
}

// newBackend builds the selected backend and describes its credential source
// without ever printing a key.
func (o *backendFlags) newBackend(cmd *cobra.Command) (llm.Backend, string, error) {
	if o.model == "" {
		o.model = backendDefaults[o.backend].model
	}
	return o.buildBackend(cmd, o.backend, o.model)
}

// buildBackend constructs a backend and describes its credential source without
// ever printing a key.
func (o *backendFlags) buildBackend(cmd *cobra.Command, name, model string) (llm.Backend, string, error) {
	switch name {
	case "claude-code":
		path, err := exec.LookPath(o.claudeBin)
		if err != nil {
			return nil, "", fmt.Errorf("claude binary %q not found: %w", o.claudeBin, err)
		}
		return llm.NewClaudeCode(path, model, o.quotaStop), "auth: Claude subscription via " + path, nil
	case "openai":
		key, source, warnings, err := config.LoadOpenAIKey(o.openaiKeyFile)
		if err != nil {
			return nil, "", err
		}
		warn(cmd, warnings)
		b := llm.NewOpenAI(o.baseURL, key, model)
		b.Stream = o.openaiStream
		return b, "endpoint: " + b.BaseURL + ", key: " + source, nil
	default:
		key, source, warnings, err := config.LoadAPIKey(o.apiKeyFile)
		if err != nil {
			return nil, "", err
		}
		warn(cmd, warnings)
		return llm.NewAnthropic(key, model), "api key: " + source, nil
	}
}

// concurrencyOrDefault returns explicit unless it's 0 (unset), in which case it
// returns this backend's default concurrency (claude-code 2, otherwise 4):
// shared by judge and rank so a plain 'cull rank --backend claude-code' doesn't
// run twice as many 'claude -p' processes as judge would.
func (o *backendFlags) concurrencyOrDefault(explicit int) int {
	if explicit != 0 {
		return explicit
	}
	return backendDefaults[o.backend].concurrency
}

type backendDefault struct {
	model       string
	concurrency int
	basis       string // how the token counts relate to money
}

var backendDefaults = map[string]backendDefault{
	"anthropic":   {"claude-sonnet-5-5", 4, "API-billed"},
	"claude-code": {"sonnet", 2, "subscription, not billed per token"},
	"openai":      {"", 4, "OpenAI-compatible server"},
}

// checkPriced makes --max-cost fail closed: on the anthropic backend a model
// missing from the price table would be counted at $0, so the limit would never
// trip. Without --max-cost it only warns that costs won't be recorded.
func checkPriced(cmd *cobra.Command, backend, model string, maxCost float64) error {
	if backend != "anthropic" {
		return nil
	}
	if _, ok := llm.PriceFor(backend, model); ok {
		return nil
	}
	if maxCost > 0 {
		return fmt.Errorf("model %q has no price in cull's table, so --max-cost can't be enforced: drop --max-cost, or use a priced model", model)
	}
	warn(cmd, []string{fmt.Sprintf("model %q has no price in cull's table: its cost is recorded as $0", model)})
	return nil
}

func warn(cmd *cobra.Command, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
	}
}
