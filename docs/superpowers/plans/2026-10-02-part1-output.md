# Part 1: Output (log levels and progress display) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every command honours a verbosity level (`-q`, default, `-v`, `--debug`).
Long-running commands (`scan`, `judge`, `rank`; later `offload`) show a live Bubble Tea
view on an interactive terminal, and plain lines everywhere else.

**Architecture:**
- **A new `internal/ui` package** holds:
  - a small event model: notes at a level, stage start/advance/done, frame results;
  - a `Sink` interface;
  - two sinks: plain (level-filtered lines to an `io.Writer`) and Bubble Tea (a live view
    on stderr);
  - `LineWriter`, which adapts an `io.Writer` call site so it emits notes.
- **The pipeline** keeps `Config.Log io.Writer`, now backed by a `LineWriter`, so the ~20
  existing call sites keep working. It gains `Config.UI ui.Sink` for structured events:
  stages, frames, verbose and debug notes.
- **The CLI** owns level and sink selection.
- **Backends** get an optional `Debug io.Writer` for request and backend-event detail.

**Tech Stack:** Go 1.22+, cobra, plus `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`
and `charm.land/lipgloss/v2` (current majors, checked 2026-10-02: 2.0.10 / 2.2.1 / 2.0.6).

**Spec:** `docs/superpowers/specs/2026-10-02-preingest-workflow-design.md`, Part 1.

## Global Constraints

- **Plain-output contract:** non-terminal output (pipes, scripts, `--plain`, `--quiet`)
  stays plain lines.
  - At the default level, today's lines are unchanged in content, so existing CLI tests
    keep passing.
  - The final summary always prints.
- **Ctrl-C keeps its meaning:** finish in-flight work, save, exit. The Bubble Tea program
  runs **without** taking input or signals: `WithInput(nil)`, and no signal handler of its
  own. `signal.NotifyContext` in `main` keeps owning SIGINT.
- **Prompts run before any live view starts:** `confirmSpend`, Task 5 of the
  workflow-UX plan.
- **Never print credentials:** debug output prints request *sizes* and headers *names*,
  never `x-api-key` or token values.
- **Dependencies:** only the three charm modules and what they pull in. `go mod tidy`
  must leave nothing unexpected. The justification goes in CLAUDE.md.
- **Running Go:**
  - adding dependencies needs an unsandboxed `go get` (the sandbox can't write
    `~/go/pkg/mod`);
  - tests run with `GOCACHE=$TMPDIR/gocache`;
  - the loopback packages run unsandboxed.
- One commit per task, on branch `feat/output`.

## Review Focus

- **`cull judge … 2>log.txt`** (stderr not a terminal) produces plain lines, not escape
  codes. *Task 5: `TestNoLiveViewWithoutTerminal`.*
- **`cull judge -q`** still prints errors, the cost line and the final summary.
  *Task 2: `TestQuietKeepsSummary`.*
- **Ctrl-C during a live view** leaves the terminal usable: the program is released, with
  no raw mode stuck on, and the report is saved. *Task 5:* the program never takes input;
  a test checks the sink's `Close` is idempotent, and is called on every exit path.
- **`--debug` with the anthropic backend** never prints the API key. *Task 4:
  `TestDebugNeverPrintsKey`.*
- **`-q -v` together** is refused. *Task 2.*

---

### Task 1: `internal/ui`: levels, events, plain sink, line adapter

**Files:** create `internal/ui/ui.go`, `internal/ui/plain.go`, `internal/ui/ui_test.go`.

**Interfaces (Produces):**

```go
type Level int
const (Quiet Level = iota; Normal; Verbose; Debug)
func ParseLevel(s string) (Level, error) // "quiet|normal|verbose|debug"

type Severity int
const (Info Severity = iota; Warn; Error) // Warn and Error show at every level

// Event is one thing a command reports. Exactly one of the pointer fields is set.
type Event struct {
	Note  *Note  // a line of text at a level (or a warning/error)
	Stage *Stage // a stage starting, advancing or finishing
	Frame *Frame // one frame's result (a per-frame line plus tallies)
}
type Note struct { Level Level; Sev Severity; Text string }
type Stage struct {
	Name  string // "judge", "scan", "rank", "offload"
	Unit  string // "frames", "sets", "bytes"
	Total int64  // 0 = unknown
	Add   int64  // progress since the last event; 0 with Total set = start
	Done  bool
}
type Frame struct {
	Line     string  // the one-line summary (pipeline.summarize)
	Decision string  // keep|review|cull|"" (measured)|"error"
	CostUSD  float64
}

type Sink interface {
	Emit(Event)
	Close() error // idempotent; flushes and releases the terminal
}

func NewPlain(w io.Writer, l Level) Sink
func LineWriter(s Sink, l Level, sev Severity) io.Writer // each written line becomes a Note
```

- [ ] **Step 1: Write the failing tests.**
  - `TestPlainLevels`: emit a Normal note, a Verbose note, a Debug note, a Warn note and a
    Frame. Golden output per level:
    - Quiet shows the Warn only;
    - Normal shows the Normal note, the Warn, and the Frame line;
    - Verbose adds the Verbose note;
    - Debug adds the Debug note.
  - `TestPlainStageLines`: at Verbose, a stage start prints `judge: 17 frames`, and done
    prints `judge: done (17)`. At Normal, stages print nothing (today's output has no
    stage lines).
  - `TestLineWriterSplitsLines`: writing `"a\nb"`, then `"c\n"`, yields notes "a" and
    "bc". `Close` flushes a partial line.
  - `TestParseLevel` accepts the four names and rejects others.
- [ ] **Step 2: Run them and check they fail** (the package doesn't exist).
- [ ] **Step 3: Implement.**
  - The plain sink writes `Note.Text`, then a newline, when `Sev >= Warn` or
    `Level <= cfg level`. Warnings are prefixed `warning: ` unless the text already starts
    with it; errors with `error: `.
  - A Frame writes `Line` at Normal.
  - Stages print only at Verbose and above.
  - A mutex guards writes, since sinks are called from worker goroutines.
- [ ] **Step 4: Run** `go test ./internal/ui`.
- [ ] **Step 5: Commit.** `ui: levels, events, plain sink, line adapter`

### Task 2: CLI flags and sink selection

**Files:** `internal/cli/root.go` (persistent flags, `so.ui()`), `internal/cli/output.go`
(new: choose the level and sink), `internal/cli/cli_test.go`.

**Interfaces:**
- **Consumes:** Task 1.
- **Produces:**
  - `func (so *sharedOpts) level() ui.Level`;
  - `func (so *sharedOpts) sink(cmd *cobra.Command, live bool) ui.Sink`. `live` asks for
    the Bubble Tea view when allowed; plain until Task 5.
  - `pipeline.Config` gets `UI ui.Sink`, and `base()` sets `cfg.UI` and
    `cfg.Log = ui.LineWriter(cfg.UI, ui.Normal, ui.Info)`.

**Flags:** persistent, on every command:
- `-q, --quiet`;
- `-v, --verbose`;
- `--debug`;
- `--log-level` (overrides the shorthands; refused together with them);
- `--plain` (never the live view).

- [ ] **Step 1: Write the failing tests.**
  - The flag table: `{"scan", "-q", "-v", dir}` errors ("pick one of").
  - `TestQuietKeepsSummary`: `scan -q` on one `tinyDNG` prints no `[1/1]` line, but does
    print `results:`. The summary is printed with `ui.Warn` severity, or directly to
    stderr, bypassing the level.
  - `TestVerboseShowsStages`: `scan -v` prints `scan: 1 frames`.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - `printSummary`, `ScanSummary` and the cost line write straight to
    `cmd.ErrOrStderr()`. They're the end-of-run record, so they print at every level.
  - Everything that goes through `cfg.Log` obeys the level.
  - Each command's `RunE` defers `cfg.UI.Close()`.
- [ ] **Step 4: Run** `go test ./internal/cli ./internal/pipeline` (unsandboxed). Existing
  tests that expect today's lines pass unchanged at the default level.
- [ ] **Step 5: Docs.** README "Flag reference → Global": the verbosity flags.
- [ ] **Step 6: Commit.** `CLI: -q/-v/--debug/--log-level/--plain on every command`

### Task 3: Pipeline events: stages, frames, verbose detail

**Files:** `internal/pipeline/pipeline.go` (`Run`: stage start/advance; Frame events
instead of the `[i/n]` line; verbose notes in `processOne`), `batch.go` (stage per round;
batch status as verbose notes), `rank.go` (stage "rank" in sets; ranked-set lines stay
Normal), `internal/pipeline/pipeline_test.go`.

**Interfaces:**
- **Consumes:** `Config.UI` (nil-safe: a nil sink means plain lines through `cfg.Log`, as
  today).
- **Produces:**
  - `func (cfg Config) note(l ui.Level, format string, args ...any)`;
  - `func (cfg Config) emit(e ui.Event)`. When `UI` is nil, a Frame falls back to
    `fmt.Fprintf(cfg.Log, "[%d/%d] %s\n", …)`, keeping today's line exactly.

- [ ] **Step 1: Write the failing test.**

```go
// recordSink keeps every event.
type recordSink struct{ mu sync.Mutex; ev []ui.Event }
func (r *recordSink) Emit(e ui.Event) { r.mu.Lock(); r.ev = append(r.ev, e); r.mu.Unlock() }
func (r *recordSink) Close() error   { return nil }

func TestRunEmitsStagesAndFrames(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	rs := &recordSink{}
	c.UI = rs
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	var start, adds, frames int
	for _, e := range rs.ev {
		switch {
		case e.Stage != nil && e.Stage.Total == 4 && e.Stage.Add == 0 && !e.Stage.Done:
			start++
		case e.Stage != nil && e.Stage.Add > 0:
			adds++
		case e.Frame != nil:
			frames++
		}
	}
	if start != 1 || adds != 4 || frames != 4 {
		t.Fatalf("start %d adds %d frames %d", start, adds, frames)
	}
}
```

  Also `TestVerboseNotesNameTheFocusTarget`: a verbose note per frame names the focus
  source (`face q=…` or `model: …`) and the call's tokens.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - `Run` emits `Stage{Name: "judge"|"scan", Unit: "frames", Total: len(todo)}` before
    dispatch, and `Stage{Add: 1}` plus a `Frame` per result.
  - `processOne` adds verbose notes: the focus target chosen, the locate answer, and the
    tokens and cost per call.
  - `RunBatch` emits a stage per round (`judge batch round 1`). Its "submitted batch" and
    status lines become verbose notes.
  - `rankSets` emits `Stage{Name: "rank", Unit: "sets"}`.
  - The `Fprintf(cfg.Log, …)` warnings (the duplicate-names warning, checkpoint failure,
    state-file removal) become `Warn` notes.
- [ ] **Step 4: Run** `go test ./internal/pipeline`, and `./internal/cli` unsandboxed.
- [ ] **Step 5: Commit.** `Pipeline: stage and frame events; verbose per-frame detail; warnings as warnings`

### Task 4: `--debug`: backend detail, never secrets

**Files:** `internal/llm/anthropic.go`, `claudecode.go`, `openai.go` (a `Debug io.Writer`
field each); `internal/cli/backend.go` (set it when the level is Debug); tests in
`internal/llm`.

**What debug shows:**
- **anthropic:** the request body size, model, effort, and the image count and bytes;
  each HTTP status, with retries and their wait; the usage; the response JSON, truncated
  to 4 KB.
- **claude-code:** the argv, with the system prompt and schema replaced by their lengths;
  the init event's `apiKeySource` and resolved model; each `rate_limit_event`'s windows;
  the result's usage; the structured output, truncated.
- **openai:** the endpoint, request size, stream on or off, and the final JSON, truncated.

- [ ] **Step 1: Write the failing tests.**
  - `TestDebugNeverPrintsKey`: with `Debug` set to a buffer, an httptest Anthropic call
    with key `sk-test-SECRET` prints `POST`, `bytes` and the model, and does not contain
    `SECRET`.
  - `TestClaudeCodeDebugShowsInit`: the fake script's init yields a debug line with
    `apiKeySource=none model=claude-sonnet-5`.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement** as listed, with each write guarded by `if a.Debug != nil`.
  `buildBackend` sets `Debug` to `ui.LineWriter(sink, ui.Debug, ui.Info)` when the level
  is Debug.
- [ ] **Step 4: Run** `go test ./internal/llm` (unsandboxed).
- [ ] **Step 5: Commit.** `--debug: backend request/event detail (sizes, statuses, init, rate limits), never credentials`

### Task 5: The live view (Bubble Tea)

**Files:** `go.mod`/`go.sum` (the three charm modules); create `internal/ui/live.go` and
`internal/ui/live_test.go`; `internal/cli/output.go` (choose live when stderr is a
terminal, the command asked for it, the level isn't Quiet, and `--plain` is off);
CLAUDE.md (dependency justification).

**Interfaces:**
- **Produces:** `func NewLive(w io.Writer, l Level) (Sink, error)`.
  - It starts a `tea.Program` with output `w`, no input, and no signal handler.
  - `Emit` → `program.Send`.
  - `Close` → `program.Quit()`, wait, then print the last frame lines and notes once as
    plain text, so the scrollback keeps a record.
- **The model:**
  - stages, as a `bubbles` progress bar each, with an ETA from the rate since start;
  - tallies (keep / review / cull / junk / errors);
  - spend (summed `CostUSD`; the CLI adds "subscription" when the backend isn't priced);
  - the last 6 frame lines;
  - warnings, sticky at the top;
  - the Verbose and Debug notes go to the lines area, by level.

- [ ] **Step 0: Add the dependencies,** unsandboxed:
  `go get charm.land/bubbletea/v2@latest charm.land/bubbles/v2@latest charm.land/lipgloss/v2@latest`,
  then `go mod tidy`.
  - If `charm.land` doesn't resolve, use the `github.com/charmbracelet/*/v2` paths, and
    check each module's declared path matches.
  - Read the v2 API from the downloaded sources (`go doc charm.land/bubbletea/v2`) before
    writing code. v2 changed the shape of `View` and of the program options against v1;
    write code against what the docs show, not from memory.
- [ ] **Step 1: Write the failing tests** (model-level; no terminal needed).
  - `TestLiveModelShowsStageAndTallies`: feed `Update` a stage start (judge, 10 frames),
    3 advances, and Frames keep, keep and cull. `View()` contains `judge`, `3/10`,
    `keep 2` and `cull 1`, plus the last frame line.
  - `TestLiveModelKeepsWarningsVisible`: a Warn note, then 20 frames: the warning is still
    in `View()`.
  - `TestNoLiveViewWithoutTerminal`: in the CLI, with `isTerminal` returning false for
    stderr, `sink(cmd, true)` is the plain sink. With `--plain` it's plain even on a
    terminal.
  - `TestLiveCloseIsIdempotent`: `Close` twice returns no error the second time.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement** as above. `scan`, `judge` and `rank` ask for the live view;
  every other command passes `live=false`.
- [ ] **Step 4: Run** `go test ./internal/ui ./internal/cli` (unsandboxed). Then **real
  terminal checks:**
  - run `cull scan photos -o $S/s.json` in the user's terminal, using the `!` prefix,
    since the live view needs a TTY;
  - check the bars, tallies and recent lines, that it exits cleanly, and that Ctrl-C mid-run
    leaves the terminal sane and the report saved;
  - repeat with `2>file` and check the file has no escape codes.
- [ ] **Step 5: Docs.** README "Flag reference": the live view and `--plain`. CLAUDE.md:
  the dependency justification under Invariants ("Dependencies: … charm v2 for the live
  progress view (2026-10-02)").
- [ ] **Step 6: Commit.** `Live progress view on terminals (Bubble Tea v2); plain lines elsewhere`

### Finish

- [ ] `make vet`, and `go test -count=1 ./...` unsandboxed.
- [ ] Final whole-branch review, then the finishing menu.
