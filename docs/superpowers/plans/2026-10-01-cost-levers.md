# Cost Levers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Section 4 of the review. Make the cost levers available and safe to measure, and take the free savings now.

**Architecture:** The levers split by whether they can change verdicts.
- **On by default, because they don't change verdicts:**
  - prompt caching on the Anthropic API, with cache tokens counted and priced;
  - removing the never-used `straighten_degrees` output;
  - stopping `claude-code` model aliases from drifting within a report.
- **Flags with today's defaults, because they could change verdicts:**
  - `--effort` for evaluate and rank;
  - `--locate-effort` for locate;
  - the existing `--max-edge`.

  The report records the effort used, and resume refuses a mismatch, so an A/B pair compares like with like through `calibrate --compare`.

**Tech Stack:** Go 1.22+, stdlib + cobra. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-30-repo-review-backlog.md`, section 4.

## Global Constraints

- **No default change that could change verdicts:**
  - effort stays the model's default unless the flag is typed;
  - `--max-edge` stays 1568.
- **Prices from the claude-api skill (cached 2026-09-25):**
  - cache writes are 1.25× input (5-minute TTL);
  - cache reads are 0.1× input, except Opus 5.5 at $0.20 and Fable 5.1 at $0.25;
  - `input_tokens` excludes cached tokens.
- Never run `judge` without the user's go-ahead. Tests use fakes and `httptest` (loopback: unsandboxed).
- One commit per task, on branch `feat/cost-levers`.

## Review Focus

- **A report from before this plan** has no effort recorded. Resuming it with no `--effort` must work; with `--effort` it is refused. *Task 3: `TestResumeRefusesDifferentEffort`.*
- **Cost of a call that read from the cache:** priced at the read rate, never at 0, and never at the full input rate. *Task 2: `TestCostPricesCacheTokens`.*
- **An old report holding `straighten_degrees`** still loads and decides. *Task 1: `TestOldEvaluationWithStraightenLoads`.*
- **A `claude-code` run whose alias resolves to two different models mid-run** stops instead of mixing models. *Task 4: `TestClaudeCodeStopsWhenAliasResolvesDifferently`.*
- **`--effort` on the openai backend** is refused, since a local server has no such knob. *Task 3: CLI validation case.*

---

### Task 1: Drop `straighten_degrees` (never applied)

**Files:** `internal/eval/prompt.go` (schema and prompt text), `internal/eval/types.go` (`Composition.StraightenDegrees`), `internal/eval/eval_test.go`, plus any fixture JSON in tests that includes it.

- [ ] **Step 1: Write the failing tests.** `TestSchemaHasNoStraighten`: the marshalled `Portable(EvaluationSchema())` doesn't contain `straighten_degrees`, and `SystemPrompt` doesn't contain "straighten". `TestOldEvaluationWithStraightenLoads`: `DecodeEvaluation` of a JSON object that includes `"straighten_degrees": 3` succeeds (unknown fields are ignored).
- [ ] **Step 2: Run them and check they fail** (the schema still has the field).
- [ ] **Step 3: Implement.**
  - Remove the property and its `required` entry from the composition schema.
  - Remove the field from `Composition`.
  - In the prompt, replace "Report tilt as straighten_degrees (positive = rotate clockwise)." with "A tilted horizon or verticals is an issue to list."
  - Fix test fixtures that send the field: `fakeBackend` JSON in `pipeline_test.go`, `validate_test.go`, and others found by grep. The JSON validator rejects additional properties, so fixtures must drop it.
- [ ] **Step 4: Run** `go test ./...` (sandbox, plus loopback packages unsandboxed).
- [ ] **Step 5: Commit.** `Drop straighten_degrees: requested from the model, never used`

### Task 2: Prompt caching on the Anthropic API, priced

**Files:** `internal/llm/llm.go` (`Usage` cache fields; `Add`), `internal/llm/anthropic.go` (system as a cached block; parse), `internal/llm/pricing.go` (`Price.Read`; `Cost`), `internal/llm/anthropic_test.go`, `internal/llm/pricing_test.go`, `internal/llm/claudecode.go` (keep folding cache tokens into input; unchanged, comment only).

**Interfaces:** `Usage{InputTokens, OutputTokens, CacheWriteTokens, CacheReadTokens}` with JSON tags `cache_creation_input_tokens` and `cache_read_input_tokens`. `Price{In, Out, Read}`. Cost is `(in·In + write·In·1.25 + read·Read + out·Out)/1e6`, halved for batch. `Read` 0 means 0.1·In.

- [ ] **Step 1: Write the failing tests.**
  - `TestAnthropicCachesTheSystemPrompt`: `params(req)["system"]` is a one-element block list with `cache_control: {type: ephemeral}` and the prompt text.
  - `TestParseAnthropicCacheUsage`: a response body with `cache_creation_input_tokens: 900, cache_read_input_tokens: 0, input_tokens: 5000` parses into the matching fields.
  - `TestCostPricesCacheTokens`: Sonnet 5.5, 1M of each kind costs `2 + 2.5 + 0.2 + 10`.
  - `TestUsageAddSumsCache`.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - `system` becomes `[]any{map[string]any{"type": "text", "text": req.System, "cache_control": map[string]any{"type": "ephemeral"}}}`. The prefix (system plus format) is identical for every frame of a run, so frames after the first read it. Below the model's minimum (512 or 1024 tokens) it silently doesn't cache, which costs nothing.
  - Read prices: sonnet-5-5 0.2, sonnet-5 0.2, opus-5-5 0.2, opus-5 0.5, fable-5-1 0.25, fable-5 1.0, opus-4-x 0.5, sonnet-4-6 0.3, haiku 0.1.
  - Everywhere tokens are reported (`printSummary`'s "tokens this run", the report's `Usage`), the total input is `InputTokens + CacheWriteTokens + CacheReadTokens`. Add `func (u Usage) TotalIn() int` and use it in the summary line.
  - Batch requests use the same `params`, so caching applies there too.
- [ ] **Step 4: Run** `go test ./internal/llm ./internal/pipeline ./internal/cli` (unsandboxed).
- [ ] **Step 5: Docs.** README "Cost control": the system prompt is cached on the API, and the report's cost includes cache writes and reads.
- [ ] **Step 6: Commit.** `Anthropic: cache the system prompt; count and price cache writes and reads`

### Task 3: Effort as an explicit, recorded setting

**Files:** `internal/llm/llm.go` (`Request.Effort`), `internal/llm/anthropic.go` (`output_config.effort`), `internal/llm/claudecode.go` (`--effort`), `internal/eval/evaluate.go`, `locate.go` and `rank.go` (an effort parameter on the request builders, or set by the caller), `internal/pipeline/pipeline.go` (`Config.Effort`, `Config.LocateEffort`; set them on requests; resume guard), `internal/report/report.go` (`Report.Effort`, `Report.LocateEffort`), `internal/cli/backend.go` (`--effort`, `--locate-effort` on judge and rank; validation), and the tests.

**Interfaces:** `Request.Effort string` ("" = the model's default). `Config.Effort` and `Config.LocateEffort`; `Report.Effort` and `Report.LocateEffort` (`omitempty`).

- [ ] **Step 1: Write the failing tests.**
  - anthropic: with `Effort: "low"`, `output_config.effort == "low"`; without it, there's no `effort` key.
  - claude-code: the fake's `args.txt` contains `--effort low` only when it is set.
  - pipeline: with `cfg.Effort = "medium"` and `cfg.LocateEffort = "low"`, the routed fake sees `Effort` "low" on `focus_target` requests and "medium" on `evaluation` requests. `rep.Effort == "medium"`.
  - `TestResumeRefusesDifferentEffort`: a report judged with no effort, resumed with `Effort: "low"`, refuses with a message naming `--effort`. Resumed without it, it works.
  - CLI: `--effort turbo` errors; `--effort low --backend openai --model m` errors ("openai has no effort setting").
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - Requests: `eval.EvalRequest`, `LocateRequest` and `RankRequest` leave `Effort` empty. `processOne` and the batch builders set `req.Effort` from `cfg` before calling. Simplest: `Input` gets `Effort` and `EvalRequest` copies it; `Locate(..., effort)` and `RankRequest(..., effort)` gain a parameter. Rank uses `cfg.Effort`.
  - Backends: anthropic sets `output_config["effort"]` when non-empty. claude-code adds `"--effort", req.Effort` before `--system-prompt`. openai ignores it, and the CLI refuses it there.
  - Report and resume: the report records `Effort` and `LocateEffort`. The resume guard in `startRun` compares them like the model: "this report was judged with --effort X; rerun with --effort X to continue it, or -o for a separate report".
  - Flags in `backendFlags` (shared by judge and rank): `--effort` "model effort for evaluations and rankings: low, medium, high, xhigh, max (default: the model's own)"; `--locate-effort` "effort for the locate call (default: the model's own)". Validate both against that set.
- [ ] **Step 4: Run** `go test ./...` unsandboxed.
- [ ] **Step 5: Docs.** README gets a short "Cost experiments" section. Measure before adopting:
  - `judge -o base.json`;
  - `judge --effort low -o low.json`;
  - `judge --max-edge 1024 -o small.json`;
  - `calibrate --compare base.json low.json`.

  Adopt a lever only if crossings stay 0 and labels agree. On `--backend claude-code` the experiments cost quota, not money.
- [ ] **Step 6: Commit.** `--effort / --locate-effort: explicit, recorded in the report, guarded on resume`

### Task 4: `claude-code`: the alias can't silently change model

**Files:** `internal/llm/claudecode.go` (scrub the model env vars; record the init event's `model`; pin), `internal/llm/llm.go` (optional interface `ModelPinner`), `internal/pipeline/pipeline.go` and `rank.go` (pin from the report; record), `internal/report/report.go` (`Report.ResolvedModel`), and the tests.

**Interfaces:**

```go
// ModelPinner is a backend whose model name is an alias resolved per call: Resolved
// reports what it resolved to, and Pin makes a different resolution an ErrAbortRun.
type ModelPinner interface {
	Resolved() string
	Pin(model string)
}
```

`Report.ResolvedModel string` (`omitempty`).

- [ ] **Step 1: Write the failing tests** (claude-code fake-script style).
  - `TestClaudeCodeScrubsModelEnv`: `ANTHROPIC_MODEL` and `ANTHROPIC_DEFAULT_SONNET_MODEL` set in the parent are absent from the fake's `env.txt`.
  - `TestClaudeCodeRecordsResolvedModel`: after a call whose init says `"model":"claude-sonnet-5"`, `Resolved() == "claude-sonnet-5"`.
  - `TestClaudeCodeStopsWhenAliasResolvesDifferently`: `Pin("claude-sonnet-5")`, and the init says `claude-sonnet-5-5`, gives `errors.Is(err, ErrAbortRun)` with both names in the message.
  - Pipeline: a fake implementing `ModelPinner` records into `rep.ResolvedModel`. On resume, `Pin` is called with the stored value.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - **Environment:** `scrubbedEnv` adds `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL` and `ANTHROPIC_SMALL_FAST_MODEL`, so `--model` is the only thing that picks the model.
  - **Parsing:** `parse` reads `model` from the init event.
  - **Pinning:**
    - `ClaudeCode` holds `mu sync.Mutex`, `resolved` and `pinned`.
    - On init: if `pinned` is set and differs, return `ErrAbortRun` ("claude resolved %q to %s, but this report was judged with %s").
    - Otherwise record `resolved`, and the first resolution pins it for the rest of the run.
  - **Pipeline:**
    - `Run` (after `startRun`): if the backend is a `ModelPinner` and `rep.ResolvedModel != ""`, call `Pin`.
    - `finishRun`: store `Resolved()` when non-empty.
    - `Rank` (sync): the same, for the report it loads.
- [ ] **Step 4: Run** `go test ./internal/llm ./internal/pipeline ./internal/report`.
- [ ] **Step 5: Commit.** `claude-code: scrub model env vars; record and pin the model the alias resolves to`

### Finish

- [ ] `make vet`, and `go test -count=1 ./...` unsandboxed.
- [ ] Mark section 4 in the backlog.
- [ ] Final whole-branch review, then the finishing menu.
- [ ] Offer the user free A/B runs on `claude-code`: `--effort low` vs the default, and `--max-edge 1024`, each through `calibrate --compare` against the stability pair already measured.
