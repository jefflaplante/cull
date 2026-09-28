# Review Server, Stars and JSONL Labels Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every keypress in the review sheet is saved to disk (keep/review/cull labels
plus 1–5 stars) through `gophotocull review --serve`. The user's judgement flows into
sidecars, `--move-culled` and Capture One, and CSV is removed everywhere.

**Architecture:**
- New `internal/labels`: the append-only JSONL log, plus the one sidecar mapping
  shared by `cull`, `decide`, the server and `apply-c1`.
- `internal/review` gains a localhost HTTP server (`serve.go`) behind Host, Origin and
  token checks; the existing page learns stars, combined filters and saving.
- `decide`, `apply-c1` and `calibrate` read the log. `cull --csv` goes.

**Tech Stack:** Go 1.22 (`net/http` method patterns, `PathValue`), stdlib, cobra, pigo
core. No new dependencies. Page JavaScript is plain ES2020, embedded as today.

**Spec:** `docs/superpowers/specs/2026-09-27-review-serve-design.md`

**Execution note:**
- Branch `review-serve`.
- Per the user's standing preference, **no commits until the user picks an
  integration option**. The "Commit" steps below are therefore ledger checkpoints: run
  the listed test command and record the result. The single commit happens at the end.
- The sandbox blocks loopback binding, so `go test` for `internal/cli` and
  `internal/llm` runs unsandboxed (`dangerouslyDisableSandbox`). Build with
  `GOCACHE=<scratchpad>/gocache`.

## Global Constraints

- **Dependencies:** stdlib plus cobra and pigo core only (CLAUDE.md "Dependencies").
- **DNGs:** never modified or deleted. Only `--move-culled`, `restore` and
  `decide --move-culled` move them.
- **Sidecars:** never overwrite one gophotocull didn't write (the report's `xmp` field
  names it) unless `--overwrite-xmp`.
- **The report stays the model's record:** a label never replaces `Result.Decision`.
- **Labels log:** file name `gophotocull-labels.jsonl` in the report's directory.
  Entry `{"file","label","stars","at"}`, label ∈ {keep, review, cull, ""}, stars 0–5,
  last line per file wins.
- **Server:**
  - binds 127.0.0.1 only;
  - Host must be `127.0.0.1:<port>` or `localhost:<port>`, and a present Origin
    must be `http://` plus one of those;
  - `/api/*` needs the header `X-Gophotocull-Token`, whose value is carried in the
    URL fragment `#token=` (128-bit hex).
- **Sidecar mapping:**
  - `xmp:Rating` = the user's stars, omitted when 0;
  - `xmp:Label` = Green (keep), Yellow (review), Red (cull), from the effective
    verdict;
  - keywords `gophotocull:<verdict>`, plus `gophotocull:labeled` when the verdict is
    the user's.
- **Capture One colour tags:** keep 4, review 3, cull 1. A rating is set only where
  the user gave stars.
- **Tests:** synthetic fixtures only, no network beyond loopback, and no Node in
  `make test`.

## Review Focus

1. **Server path escape:** a request naming `../gophotocull-report.json` (encoded or
   not), in the URL or in a POST body, must never read or write outside the sheet's
   images, the labels log and listed frames' sidecars.
   - Tests: Task 7, `TestServerAccessControl` (escape case) and
     `TestServerRejectsBadInput`.
2. **Foreign sidecars:** a sidecar gophotocull didn't write, including one created
   after the server started, must survive any number of POSTs without
   `--overwrite-xmp`, and the page must say so.
   - Test: Task 7, `TestServerSkipsForeignSidecar`.
3. **Concurrent saves:** two tabs, or fast key repeat, sending POSTs concurrently
   must leave a log of whole, valid lines, one per accepted request.
   - Tests: Task 1, `TestConcurrentAppendsStayWhole`, and Task 7,
     `TestServerConcurrentPosts`.
4. **`decide --labels --move-culled`:**
   - it must restore a frame the model culled but the user labelled keep;
   - it must move a model keep the user labelled cull;
   - it must never change the report's decisions.
   - Test: Task 3, `TestDecideLabelsDriveSidecarsAndMoves`.
5. **Server dies mid-session:**
   - changes made while it is unreachable must queue and replay in order;
   - after replay, the folded log must equal what the page shows.
   - Check: Task 9, jsdom check 5.

---

### Task 1: Labels log (`internal/labels`)

**Files:**
- Create: `internal/labels/labels.go`
- Test: `internal/labels/labels_test.go`

**Interfaces:**
- Produces:
  - `labels.FileName` = `"gophotocull-labels.jsonl"`;
  - `type Entry struct{ File, Label string; Stars int; At time.Time }` (JSON
    `file,label,stars,at`);
  - `(Entry) Validate() error` and `(Entry) Empty() bool`;
  - `DefaultPath(reportPath string) string`;
  - `Append(path string, e Entry) error`;
  - `Read(path string) (map[string]Entry, error)`;
  - `Verdicts(map[string]Entry) map[string]string`.

- [ ] **Step 1: Write the failing tests**

```go
package labels

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendReadFoldsLastWins(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	for _, e := range []Entry{
		{File: "A.DNG", Label: "keep"},
		{File: "B.DNG", Label: "cull", Stars: 2},
		{File: "A.DNG", Label: "review", Stars: 4},
		{File: "B.DNG"}, // cleared
	} {
		e.At = time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
		if err := Append(p, e); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m["A.DNG"].Label != "review" || m["A.DNG"].Stars != 4 {
		t.Fatalf("fold: %+v", m)
	}
	b, _ := os.ReadFile(p)
	if n := strings.Count(string(b), "\n"); n != 4 {
		t.Fatalf("log must keep history: %d lines\n%s", n, b)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	m, err := Read(filepath.Join(t.TempDir(), FileName))
	if err != nil || len(m) != 0 {
		t.Fatalf("%v %v", m, err)
	}
}

func TestReadSkipsTornFinalLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(p, []byte(`{"file":"A.DNG","label":"keep","stars":0,"at":"2026-09-27T20:00:00Z"}`+"\n"+`{"file":"B.DNG","la`), 0o644)
	m, err := Read(p)
	if err != nil || len(m) != 1 || m["A.DNG"].Label != "keep" {
		t.Fatalf("%v %v", m, err)
	}
}

func TestReadRejectsBadLinesNamingThem(t *testing.T) {
	ok := `{"file":"A.DNG","label":"keep","stars":0,"at":"2026-09-27T20:00:00Z"}`
	for name, body := range map[string]string{
		"not json":         ok + "\nnot json\n" + ok + "\n",
		"bad label":        ok + "\n" + `{"file":"B.DNG","label":"maybe","stars":0}` + "\n",
		"bad stars":        ok + "\n" + `{"file":"B.DNG","label":"","stars":6}` + "\n",
		"bad final w/ \\n": ok + "\n" + `{"file":` + "\n",
	} {
		p := filepath.Join(t.TempDir(), FileName)
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := Read(p); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAppendValidates(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	for _, e := range []Entry{
		{File: "A.DNG", Label: "maybe"}, {File: "A.DNG", Stars: 6}, {File: "A.DNG", Stars: -1},
		{File: "sub/A.DNG"}, {File: ".."}, {File: ""},
	} {
		if err := Append(p, e); err == nil {
			t.Errorf("accepted %+v", e)
		}
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("rejected entries created the log")
	}
}

func TestConcurrentAppendsStayWhole(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Append(p, Entry{File: fmt.Sprintf("L%d.DNG", i%7), Label: "keep", Stars: i % 6, At: time.Now()}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("%d lines", len(lines))
	}
	for i, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("line %d torn: %q", i+1, l)
		}
	}
}

func TestDefaultPathAndVerdicts(t *testing.T) {
	if got := DefaultPath("/shoot/gophotocull-report.json"); got != "/shoot/"+FileName {
		t.Fatal(got)
	}
	v := Verdicts(map[string]Entry{"A.DNG": {Label: "cull"}, "B.DNG": {Stars: 3}})
	if len(v) != 1 || v["A.DNG"] != "cull" {
		t.Fatalf("stars-only entries are not verdicts: %v", v)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/labels`
Expected: FAIL, build failed (`undefined: FileName`, `Append`, …).

- [ ] **Step 3: Implement `internal/labels/labels.go`**

```go
// Package labels is the user's judgement of frames: an append-only JSONL log of
// keep/review/cull labels and star ratings, kept apart from the model's report so
// calibration can compare the two.
package labels

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// FileName is the log's name, in the report's directory.
const FileName = "gophotocull-labels.jsonl"

// Entry is one frame's full state at one time. The last entry per file wins.
type Entry struct {
	File  string    `json:"file"`  // base name
	Label string    `json:"label"` // keep | review | cull | "" (unlabelled)
	Stars int       `json:"stars"` // 0-5; 0 = unrated
	At    time.Time `json:"at"`
}

// Validate checks an entry's fields.
func (e Entry) Validate() error {
	if e.File == "" || e.File == "." || e.File == ".." || e.File != filepath.Base(e.File) {
		return fmt.Errorf("file %q: want a file name without a directory", e.File)
	}
	switch e.Label {
	case "", "keep", "review", "cull":
	default:
		return fmt.Errorf("label %q: want keep, review, cull or empty", e.Label)
	}
	if e.Stars < 0 || e.Stars > 5 {
		return fmt.Errorf("stars %d: want 0-5", e.Stars)
	}
	return nil
}

// Empty reports whether the entry clears the frame.
func (e Entry) Empty() bool { return e.Label == "" && e.Stars == 0 }

// DefaultPath is the log beside a report: one log per shoot directory, shared by
// every report there, since labels describe photos, not a model run.
func DefaultPath(reportPath string) string {
	return filepath.Join(filepath.Dir(reportPath), FileName)
}

// Append adds one entry with a single write, so the log is never rewritten and a
// crash can lose at most the line being written.
func Append(path string, e Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Read folds the log: the last entry per file wins, and cleared frames are absent.
// A final line without its newline is an interrupted append and is skipped; any
// other bad line is an error, because silently dropping labels would skew
// calibration. A missing log is empty.
func Read(path string) (map[string]Entry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Entry{}
	lines := bytes.Split(b, []byte("\n"))
	for i, line := range lines[:len(lines)-1] { // the last piece is "" or a torn append
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		if e.Empty() {
			delete(out, e.File)
		} else {
			out[e.File] = e
		}
	}
	return out, nil
}

// Verdicts returns file → label for the entries that have a label.
func Verdicts(m map[string]Entry) map[string]string {
	v := map[string]string{}
	for f, e := range m {
		if e.Label != "" {
			v[f] = e.Label
		}
	}
	return v
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test -race ./internal/labels`
Expected: PASS (7 tests).

- [ ] **Step 5: Checkpoint.** Run `go vet ./internal/labels` and record it in the ledger.

---

### Task 2: One sidecar mapping (`labels.Sidecar`), optional rating

**Files:**
- Create: `internal/labels/sidecar.go`
- Test: `internal/labels/sidecar_test.go`
- Modify:
  - `internal/xmp/xmp.go` (omit a 0 rating), with `internal/xmp/xmp_test.go`;
  - `internal/pipeline/stages.go` `finish`;
  - `internal/pipeline/decide.go` `writeDecidedSidecar`;
  - `internal/pipeline/pipeline.go` (delete `buildSidecar`);
  - `internal/pipeline/decide_test.go` (expectations for the new mapping).

**Interfaces:**
- Consumes: `labels.Entry` (Task 1).
- Produces:
  - `labels.Effective(r report.Result, l Entry) (eval.Decision, bool)`;
  - `labels.Sidecar(r report.Result, l Entry, orientation int, develop bool) xmp.Sidecar`;
  - `labels.WriteSidecar(r *report.Result, l Entry, develop, overwrite bool) error`,
    which returns `xmp.ErrExists` when skipped and sets `r.XMP` on success.

- [ ] **Step 1: Write the failing tests**

`internal/labels/sidecar_test.go`:

```go
package labels

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/xmp"
)

func TestSidecarMapping(t *testing.T) {
	for _, c := range []struct {
		name  string
		model eval.Decision
		l     Entry
		want  xmp.Sidecar
	}{
		{"model keep", eval.Keep, Entry{}, xmp.Sidecar{Label: "Green", Keywords: []string{"gophotocull:keep"}}},
		{"model review", eval.Review, Entry{}, xmp.Sidecar{Label: "Yellow", Keywords: []string{"gophotocull:review"}}},
		{"your keep over model cull", eval.Cull, Entry{Label: "keep", Stars: 4},
			xmp.Sidecar{Rating: 4, Label: "Green", Keywords: []string{"gophotocull:keep", "gophotocull:labeled"}}},
		{"stars only", eval.Review, Entry{Stars: 2}, xmp.Sidecar{Rating: 2, Label: "Yellow", Keywords: []string{"gophotocull:review"}}},
		{"scan frame you culled", "", Entry{Label: "cull"}, xmp.Sidecar{Label: "Red", Keywords: []string{"gophotocull:cull", "gophotocull:labeled"}}},
		{"nothing", "", Entry{}, xmp.Sidecar{}},
	} {
		got := Sidecar(report.Result{Decision: c.model}, c.l, 1, false)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestSidecarDevelopNeverForCulls(t *testing.T) {
	e := &eval.Evaluation{Exposure: eval.Exposure{Status: "fixable", EVAdjust: 0.5},
		Composition: eval.Composition{Crop: eval.Crop{Apply: true, Left: 0.1, Top: 0.1, Right: 0.9, Bottom: 0.9}}}
	r := report.Result{Decision: eval.Keep, Evaluation: e}
	if sc := Sidecar(r, Entry{}, 1, true); sc.ExposureEV == nil || sc.Crop == nil {
		t.Fatalf("keep: develop missing: %+v", sc)
	}
	if sc := Sidecar(r, Entry{Label: "cull"}, 1, true); sc.ExposureEV != nil || sc.Crop != nil {
		t.Fatalf("your cull still got develop settings: %+v", sc)
	}
}

func TestWriteSidecarOwnership(t *testing.T) {
	dir := t.TempDir()
	r := report.Result{File: filepath.Join(dir, "L1.DNG"), Decision: eval.Keep}
	p := filepath.Join(dir, "L1.xmp")
	if err := WriteSidecar(&r, Entry{Stars: 3}, false, false); err != nil || r.XMP != p {
		t.Fatalf("create: %v %q", err, r.XMP)
	}
	if err := WriteSidecar(&r, Entry{Stars: 5}, false, false); err != nil {
		t.Fatalf("ours must be rewritten: %v", err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), `xmp:Rating="5"`) {
		t.Fatalf("not rewritten:\n%s", b)
	}
	foreign := report.Result{File: filepath.Join(dir, "L2.DNG"), Decision: eval.Keep}
	os.WriteFile(filepath.Join(dir, "L2.xmp"), []byte("foreign"), 0o644)
	if err := WriteSidecar(&foreign, Entry{}, false, false); !errors.Is(err, xmp.ErrExists) || foreign.XMP != "" {
		t.Fatalf("foreign: %v %q", err, foreign.XMP)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "L2.xmp")); string(b) != "foreign" {
		t.Fatal("foreign sidecar overwritten")
	}
	moved := report.Result{File: filepath.Join(dir, "L3.DNG"), MovedTo: filepath.Join(dir, "culled", "L3.DNG"), Decision: eval.Cull}
	os.MkdirAll(filepath.Join(dir, "culled"), 0o755)
	if err := WriteSidecar(&moved, Entry{}, false, false); err != nil || moved.XMP != filepath.Join(dir, "culled", "L3.xmp") {
		t.Fatalf("moved: %v %q", err, moved.XMP)
	}
}
```

`internal/xmp/xmp_test.go`, new test:

```go
func TestRenderOmitsUnsetRating(t *testing.T) {
	if s := string(Render(Sidecar{Label: "Green"})); strings.Contains(s, "xmp:Rating") || !strings.Contains(s, `xmp:Label="Green"`) {
		t.Fatalf("unrated sidecar:\n%s", s)
	}
}
```

`internal/pipeline/decide_test.go`, `TestDecideRewritesOnlyOurSidecars`: replace the
two rating assertions:

```go
	if b, _ := os.ReadFile(l2); !strings.Contains(string(b), `xmp:Label="Green"`) || strings.Contains(string(b), "xmp:Rating") {
		t.Fatalf("keep sidecar: green, and no stars from the model:\n%s", b)
	}
```

```go
	if b, _ := os.ReadFile(l2); !strings.Contains(string(b), `xmp:Label="Yellow"`) {
		t.Fatalf("our sidecar not rewritten for the new decision:\n%s", b)
	}
```

At line 139 (`TestBurstDuplicatesGoToReviewAndDecideCanCullThem`), replace
`` `xmp:Rating="2"` `` with `` `xmp:Label="Yellow"` ``.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/labels ./internal/xmp ./internal/pipeline`
Expected:
- labels: build failed (`undefined: Sidecar`);
- xmp: FAIL in `TestRenderOmitsUnsetRating` (`xmp:Rating="0"` present);
- pipeline: FAIL in the updated decide tests (`xmp:Label="Green"` missing, since keep
  had no label before).

- [ ] **Step 3: Implement**

`internal/xmp/xmp.go`:
- the `Rating` field comment becomes `// 1-5; 0 omits xmp:Rating`;
- in `Render`, replace the unconditional rating line with:

```go
	if s.Rating > 0 {
		attrs = append(attrs, fmt.Sprintf(`xmp:Rating="%d"`, s.Rating))
	}
```

`internal/labels/sidecar.go`:

```go
package labels

import (
	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/xmp"
)

// Effective is the verdict that drives outputs: the user's label if set, else the
// model's decision. yours reports which.
func Effective(r report.Result, l Entry) (d eval.Decision, yours bool) {
	if l.Label != "" {
		return eval.Decision(l.Label), true
	}
	return r.Decision, false
}

// colors are the xmp:Label values Capture One shows as colour tags.
var colors = map[eval.Decision]string{eval.Keep: "Green", eval.Review: "Yellow", eval.Cull: "Red"}

// Sidecar maps a frame and the user's entry for it (zero if none) to sidecar
// metadata: the user's stars as the rating (omitted when unrated; the model never
// sets stars), the effective verdict as colour and keyword, plus
// gophotocull:labeled when the verdict is the user's. Develop settings only when
// asked, and never for a cull.
func Sidecar(r report.Result, l Entry, orientation int, develop bool) xmp.Sidecar {
	d, yours := Effective(r, l)
	sc := xmp.Sidecar{Rating: l.Stars, Label: colors[d]}
	if d != "" {
		sc.Keywords = append(sc.Keywords, "gophotocull:"+string(d))
	}
	if yours {
		sc.Keywords = append(sc.Keywords, "gophotocull:labeled")
	}
	if !develop || r.Evaluation == nil || d == eval.Cull {
		return sc
	}
	e := r.Evaluation
	if e.Exposure.Status == "fixable" {
		v := e.Exposure.EVAdjust
		sc.ExposureEV = &v
	}
	if c := e.Composition.Crop; c.Apply {
		b := xmp.FromDisplay(c.Left, c.Top, c.Right, c.Bottom, orientation)
		sc.Crop = &b
	}
	return sc
}

// WriteSidecar writes the frame's sidecar where the frame now lives (its culled/
// path if moved). A sidecar the report records as ours is rewritten; any other
// existing one is left alone (xmp.ErrExists) unless overwrite. On success the path
// is recorded in r.XMP.
func WriteSidecar(r *report.Result, l Entry, develop, overwrite bool) error {
	at := r.File
	if r.MovedTo != "" {
		at = r.MovedTo
	}
	p := xmp.Path(at)
	orientation := 1
	if r.Preview != nil {
		orientation = r.Preview.Orientation
	}
	if err := xmp.Write(p, Sidecar(*r, l, orientation, develop), r.XMP == p || overwrite); err != nil {
		return err
	}
	r.XMP = p
	return nil
}
```

`internal/pipeline/stages.go`, in `finish`: replace
`buildSidecar(*res, orientation, cfg.XMPDevelop)` with
`labels.Sidecar(*res, labels.Entry{}, orientation, cfg.XMPDevelop)`, and import
`internal/labels`.

`internal/pipeline/decide.go`: replace `writeDecidedSidecar`:

```go
// writeDecidedSidecar writes the frame's sidecar where the frame currently lives,
// from the effective verdict and the user's stars (o.Labels; nil = the model's
// verdict alone). A sidecar the report records as ours is rewritten; any other
// existing one is left alone unless OverwriteXMP.
func writeDecidedSidecar(r *report.Result, o DecideOptions) {
	switch err := labels.WriteSidecar(r, o.Labels[filepath.Base(r.File)], o.XMPDevelop, o.OverwriteXMP); {
	case err == nil:
	case errors.Is(err, xmp.ErrExists):
		addFixup(r, "xmp: sidecar exists and is not ours; not overwritten")
	default:
		addFixup(r, "xmp: "+err.Error())
	}
}
```

Add `Labels map[string]labels.Entry` to `DecideOptions` with the comment
`// the user's labels by base name; nil = the model's verdicts alone`. It is used in
Task 3; adding it here keeps `writeDecidedSidecar` compiling.

`internal/pipeline/pipeline.go`: delete `buildSidecar` and its comment.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test -race ./internal/labels ./internal/xmp ./internal/pipeline`
Expected: PASS.

- [ ] **Step 5: Checkpoint.** Run `go vet ./...` and record it in the ledger.

---

### Task 3: `decide --labels`

**Files:**
- Modify:
  - `internal/pipeline/decide.go` (the restore loop, and the `moveCulled` call);
  - `internal/pipeline/move.go` (`moveCulled` takes the labels);
  - `internal/pipeline/pipeline.go` (the `moveCulled(rep, nil, cfg.Log)` call site);
  - `internal/cli/decide.go` (the `--labels` flag).
- Test: `internal/pipeline/decide_test.go` and `internal/cli/cli_test.go`.

**Interfaces:**
- Consumes: `labels.Read`, `labels.Effective`, and `DecideOptions.Labels` (Task 2).
- Produces:
  - `moveCulled(rep *report.Report, lab map[string]labels.Entry, log io.Writer) int`;
  - the CLI flag `decide --labels <log>`.

- [ ] **Step 1: Write the failing tests**

`internal/pipeline/decide_test.go`:

```go
func TestDecideLabelsDriveSidecarsAndMoves(t *testing.T) {
	dir, c := culledShoot(t, nil) // model: L1 cull, L2 keep, L3 review
	pol := eval.Policy{MinCropArea: 0.6}
	if _, err := Decide(c.ReportPath, DecideOptions{Policy: pol, MoveCulled: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, CulledDir, "L1000001.DNG")); err != nil {
		t.Fatal("the model's cull was not moved")
	}
	lab := map[string]labels.Entry{
		"L1000001.DNG": {File: "L1000001.DNG", Label: "keep"}, // you overrule the model's cull
		"L1000002.DNG": {File: "L1000002.DNG", Label: "cull"}, // and cull a model keep
		"L1000003.DNG": {File: "L1000003.DNG", Stars: 5},      // stars only: the model's review stands
	}
	if _, err := Decide(c.ReportPath, DecideOptions{Policy: pol, WriteXMP: true, MoveCulled: true, Labels: lab}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "L1000001.DNG")); err != nil {
		t.Error("frame you labeled keep was not restored")
	}
	if _, err := os.Stat(filepath.Join(dir, CulledDir, "L1000002.DNG")); err != nil {
		t.Error("frame you labeled cull was not moved")
	}
	rep, _ := report.Load(c.ReportPath)
	if r := result(t, rep, "L1000001.DNG"); r.Decision != eval.Cull {
		t.Errorf("report decision replaced by your label: %s", r.Decision)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if s := read(filepath.Join(dir, "L1000001.xmp")); !strings.Contains(s, `xmp:Label="Green"`) || !strings.Contains(s, "gophotocull:labeled") {
		t.Errorf("L1 sidecar:\n%s", s)
	}
	if s := read(filepath.Join(dir, CulledDir, "L1000002.xmp")); !strings.Contains(s, `xmp:Label="Red"`) {
		t.Errorf("L2 sidecar did not follow the frame into culled/:\n%s", s)
	}
	if s := read(filepath.Join(dir, "L1000003.xmp")); !strings.Contains(s, `xmp:Rating="5"`) || !strings.Contains(s, `xmp:Label="Yellow"`) || strings.Contains(s, "labeled") {
		t.Errorf("L3 sidecar:\n%s", s)
	}
}
```

Add the `internal/labels` import. In `internal/cli/cli_test.go`:

```go
func TestDecideLabelsMustExist(t *testing.T) {
	_, err := run(t, "decide", "--labels", filepath.Join(t.TempDir(), "nope.jsonl"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "nope.jsonl") {
		t.Fatalf("missing labels log: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/pipeline -run TestDecideLabels` (sandboxed is fine)
Expected: FAIL. `frame you labeled keep was not restored` and
`frame you labeled cull was not moved`, because moves still follow the model's
decision.

Run the CLI test unsandboxed: `go test ./internal/cli -run TestDecideLabelsMustExist`
Expected: FAIL with `unknown flag: --labels`.

- [ ] **Step 3: Implement**

`internal/pipeline/move.go`:

```go
// moveCulled moves frames whose effective verdict is cull (the user's label when
// lab has one, else the model's decision) into culled/ beside them.
func moveCulled(rep *report.Report, lab map[string]labels.Entry, log io.Writer) int {
	n := 0
	for i := range rep.Results {
		r := &rep.Results[i]
		if d, _ := labels.Effective(*r, lab[filepath.Base(r.File)]); d != eval.Cull || r.Error != "" || r.MovedTo != "" {
			continue
		}
		// ... unchanged body ...
```

`internal/pipeline/decide.go`, in the restore loop:

```go
			if d, _ := labels.Effective(*r, o.Labels[filepath.Base(r.File)]); r.MovedTo == "" || d == eval.Cull {
				continue
			}
```

and `sum.Moved = moveCulled(rep, o.Labels, log)`.

`internal/pipeline/pipeline.go` (`finishRun`): `moveCulled(rep, nil, cfg.Log)`.

`internal/cli/decide.go`: add `labelsPath string`, the flag
`f.StringVar(&labelsPath, "labels", "", "your labels log (gophotocull-labels.jsonl): your verdicts and stars drive sidecars and --move-culled; the report keeps the model's")`,
and in `RunE`, before `pipeline.Decide`:

```go
			var lab map[string]labels.Entry
			if labelsPath != "" {
				if _, err := os.Stat(labelsPath); err != nil {
					return err
				}
				if lab, err = labels.Read(labelsPath); err != nil {
					return err
				}
			}
```

Pass `Labels: lab` in `DecideOptions`. Append to `Long`: `--labels uses your verdicts from the review sheet where you gave one.`

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test -race ./internal/pipeline`, and unsandboxed `go test ./internal/cli -run 'Decide'`
Expected: PASS.

- [ ] **Step 5: Checkpoint.** Record in the ledger.

---

### Task 4: `apply-c1`: new mapping and `--labels`

**Files:**
- Modify: `internal/c1/c1.go` and `internal/cli/applyc1.go`
- Test: `internal/c1/c1_test.go` and `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `labels.Effective`, `labels.Read`, `labels.Entry`.
- Produces: `c1.Options.Labels map[string]labels.Entry` and the flag `apply-c1 --labels <log>`.

- [ ] **Step 1: Write the failing tests**

In `internal/c1/c1_test.go`, replace `TestScriptSetsRatingLabelKeyword` and add a
labels test:

```go
// block returns one frame's part of the script.
func block(s, name string) string {
	i := strings.Index(s, "\n\t-- "+name+":")
	if i < 0 {
		return ""
	}
	rest := s[i+1:]
	if j := strings.Index(rest[1:], "\n\t-- "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

func TestScriptColorsAndKeywordsWithoutLabels(t *testing.T) {
	s := Script(testReport(), Options{Rating: true, Label: true, Keyword: true})
	for name, tag := range map[string]string{"L1.DNG": "4", "L2.DNG": "3", `L"3\.DNG`: "1"} {
		if b := block(s, name); !strings.Contains(b, "set color tag of v to "+tag) {
			t.Errorf("%s: want color tag %s:\n%s", name, tag, b)
		}
	}
	for _, want := range []string{`matchImages(doc, "L\"3\\.DNG", "L\"3\\")`, `"gophotocull:cull"`, `"gophotocull:keep"`} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %s", want)
		}
	}
	for _, bad := range []string{"set rating", "gophotocull:labeled", "L4.DNG"} {
		if strings.Contains(s, bad) {
			t.Errorf("script contains %s (ratings come only from your stars)", bad)
		}
	}
}

func TestScriptUsesYourLabelsAndStars(t *testing.T) {
	lab := map[string]labels.Entry{
		"L1.DNG": {File: "L1.DNG", Label: "cull"}, // model keep, you cull
		"L2.DNG": {File: "L2.DNG", Stars: 4},      // model review, your stars
	}
	s := Script(testReport(), Options{Rating: true, Label: true, Keyword: true, Labels: lab})
	b1, b2, b3 := block(s, "L1.DNG"), block(s, "L2.DNG"), block(s, `L"3\.DNG`)
	if !strings.Contains(b1, "set color tag of v to 1") || !strings.Contains(b1, `"gophotocull:labeled"`) || strings.Contains(b1, "set rating") {
		t.Errorf("L1:\n%s", b1)
	}
	if !strings.Contains(b2, "set rating of v to 4") || !strings.Contains(b2, "set color tag of v to 3") || strings.Contains(b2, "labeled") {
		t.Errorf("L2:\n%s", b2)
	}
	if !strings.Contains(b3, "set color tag of v to 1") || strings.Contains(b3, "set rating") {
		t.Errorf("L3:\n%s", b3)
	}
}
```

In `TestScriptDevelopOnlyWhenAskedAndApplicable`, the final `set rating` check stays
as it is. Add the `internal/labels` import.

In `internal/cli/cli_test.go`:

```go
func TestApplyC1LabelsSetYourStars(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	if out, err := run(t, "cull", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("cull: %v\n%s", err, out)
	}
	log := filepath.Join(dir, "gophotocull-labels.jsonl")
	os.WriteFile(log, []byte(`{"file":"L1000001.DNG","label":"","stars":5,"at":"2026-09-27T20:00:00Z"}`+"\n"), 0o644)
	out, err := run(t, "apply-c1", "--labels", log, dir)
	if err != nil || !strings.Contains(out, "set rating of v to 5") {
		t.Fatalf("with labels: %v\n%s", err, out)
	}
	if out, _ := run(t, "apply-c1", dir); strings.Contains(out, "set rating") {
		t.Fatalf("rating without your stars:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/c1`
Expected: build failed (`unknown field Labels in struct literal`). After adding only
the field, `TestScriptColorsAndKeywordsWithoutLabels` fails on `color tag 4` for L1
and on `set rating` being present.

- [ ] **Step 3: Implement**

`internal/c1/c1.go`:
- delete the `rating` map;
- make `colorTag` `map[eval.Decision]int{eval.Keep: 4, eval.Review: 3, eval.Cull: 1}`,
  with the comment `// 4 green: numbering not in the dictionary; confirm with Probe.`;
- add to `Options`:

```go
	Labels map[string]labels.Entry // your verdicts and stars by base name; nil = the model's verdicts, no ratings
```

The per-frame part of `Script` becomes:

```go
	for _, r := range rep.Results {
		l := o.Labels[filepath.Base(r.File)]
		d, yours := labels.Effective(r, l)
		if r.Error != "" || (d == "" && l.Stars == 0) {
			continue
		}
		name := filepath.Base(r.File)
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		who := "model"
		if yours {
			who = "yours"
		}
		fmt.Fprintf(&b, "\n\t-- %s: %s (%s)\n", name, d, who)
		fmt.Fprintf(&b, "\tset imgs to my matchImages(doc, %s, %s)\n", quote(name), quote(stem))
		fmt.Fprintf(&b, "\tif (count of imgs) is 0 then set end of notFound to %s\n", quote(name))
		b.WriteString("\trepeat with img in imgs\n")
		e := r.Evaluation
		crop := o.Crop && e != nil && e.Composition.Status == "croppable" && e.Composition.Crop.Apply
		if crop {
			b.WriteString("\t\tset d to dimensions of img\n\t\tset w to item 1 of d\n\t\tset h to item 2 of d\n")
		}
		b.WriteString("\t\trepeat with v in (variants of img)\n")
		if o.Rating && l.Stars > 0 { // only your stars: never reset ratings made in Capture One
			fmt.Fprintf(&b, "\t\t\tset rating of v to %d\n", l.Stars)
		}
		if tag, ok := colorTag[d]; ok && o.Label {
			fmt.Fprintf(&b, "\t\t\tset color tag of v to %d\n", tag)
		}
		if o.Keyword && d != "" {
			kws := []string{"gophotocull:" + string(d)}
			if yours {
				kws = append(kws, "gophotocull:labeled")
			}
			for _, kw := range kws {
				fmt.Fprintf(&b, "\t\t\tset k to my ensureKeyword(doc, %s)\n", quote(kw))
				b.WriteString("\t\t\tif k is not missing value then apply keyword k to {v}\n")
			}
		}
		if o.Exposure && e != nil && e.Exposure.Status == "fixable" {
			fmt.Fprintf(&b, "\t\t\tset exposure of adjustments of v to %s\n", num(e.Exposure.EVAdjust))
		}
		if crop {
			c := e.Composition.Crop
			fmt.Fprintf(&b, "\t\t\tset crop of v to {%s * w, %s * h, %s * w, %s * h}\n",
				num((c.Left+c.Right)/2), num((c.Top+c.Bottom)/2), num(c.Right-c.Left), num(c.Bottom-c.Top))
		}
		b.WriteString("\t\tend repeat\n\tend repeat\n")
	}
```

`block()` in the test searches `-- name:`, and the comment is now `-- name: d (who)`,
so it still matches. Update the package doc: `rating from your stars (--labels),
color tag and keyword per verdict`.

`internal/cli/applyc1.go`: add `labelsPath`, the flag
`f.StringVar(&labelsPath, "labels", "", "your labels log: your verdicts override the model's, and your stars set ratings")`,
and load it as in Task 3. `os.Stat` means a named log must exist. Put it into
`o.Labels` before `c1.Script`. Flag help texts:
- `--rating`: `"set star ratings from --labels (only frames you rated)"`;
- `--label`: `"set color tags (keep green, review yellow, cull red)"`;
- `--keyword`: `"apply gophotocull:<verdict> keywords (+ gophotocull:labeled for your verdicts)"`.

Update `Long` to match.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test -race ./internal/c1`, and unsandboxed `go test ./internal/cli -run ApplyC1`
Expected: PASS. Then check that the generated script still compiles:
`./bin/gophotocull apply-c1 --labels <log> <dir> > $SCRATCH/a.applescript && osacompile -o $SCRATCH/a.scpt $SCRATCH/a.applescript`.
Expected: exit 0, run unsandboxed after `make build`.

- [ ] **Step 5: Checkpoint.** Record in the ledger.

---

### Task 5: `calibrate` reads the JSONL log

**Files:**
- Modify:
  - `internal/calib/calib.go` (delete `ReadLabels`);
  - `internal/calib/calib_test.go` (delete `TestReadLabels`);
  - `internal/cli/calibrate.go`;
  - `internal/cli/cli_test.go` (`TestCalibrateCommand`).

**Interfaces:**
- Consumes: `labels.DefaultPath`, `labels.Read`, `labels.Verdicts`.
- Produces: `calibrate [--labels <log>] REPORT.json...`, which defaults to the log
  beside the first report.

- [ ] **Step 1: Write the failing test.** Replace the labels part of `TestCalibrateCommand` (after the `cull` run):

```go
	rp := filepath.Join(dir, "gophotocull-report.json")
	log := filepath.Join(dir, "gophotocull-labels.jsonl")
	os.WriteFile(log, []byte(`{"file":"L1000001.DNG","label":"keep","stars":0,"at":"2026-09-27T20:00:00Z"}`+"\n"+
		`{"file":"X.DNG","label":"","stars":3,"at":"2026-09-27T20:00:01Z"}`+"\n"), 0o644)
	out, err := run(t, "calibrate", rp) // the log beside the report, by default
	if err != nil || !strings.Contains(out, "false-cull rate (keep → cull):   1/1") || !strings.Contains(out, "sweep") {
		t.Fatalf("calibrate: %v\n%s", err, out)
	}
	if strings.Contains(out, "not in the report") {
		t.Fatalf("a stars-only entry counted as a label:\n%s", out)
	}
	moved := filepath.Join(t.TempDir(), "mine.jsonl")
	os.Rename(log, moved)
	if out, err := run(t, "calibrate", "--labels", moved, rp); err != nil || !strings.Contains(out, "1/1") {
		t.Fatalf("explicit --labels: %v\n%s", err, out)
	}
	if _, err := run(t, "calibrate", rp); err == nil || !strings.Contains(err.Error(), "gophotocull-labels.jsonl") {
		t.Fatalf("no log: %v", err)
	}
```

- [ ] **Step 2: Run it to verify it fails**

Run (unsandboxed): `go test ./internal/cli -run TestCalibrateCommand`
Expected: FAIL with `--labels is required`.

- [ ] **Step 3: Implement.** `internal/cli/calibrate.go`:

```go
		Use:   "calibrate [--labels gophotocull-labels.jsonl] REPORT.json...",
		Long: `calibrate compares each report's decisions with your labels from the review
sheet (gophotocull-labels.jsonl beside the first report, unless --labels): a
confusion matrix, the false-cull rate (you said keep, it culled), the missed-cull
rate, and the review rate. Star ratings without a label don't count. It also
re-decides the stored assessments across --review-below-sharpness values, so
thresholds can be tuned without new model calls; apply the chosen ones with
'gophotocull decide'.`,
		Example: "  gophotocull calibrate sonnet.json local.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := pol.policy()
			if err != nil {
				return err
			}
			if labelsPath == "" {
				labelsPath = labels.DefaultPath(args[0])
			}
			all, err := labels.Read(labelsPath)
			if err != nil {
				return err
			}
			verdicts := labels.Verdicts(all)
			if len(verdicts) == 0 {
				return fmt.Errorf("no labels in %s: label frames with 'gophotocull review --serve' first, or pass --labels", labelsPath)
			}
			// ... loop over reports with calib.Compare(rep, verdicts) / calib.Sweep(rep, verdicts, ...) as today ...
```

Flag: `cmd.Flags().StringVar(&labelsPath, "labels", "", "labels log (default: gophotocull-labels.jsonl beside the first report)")`.
Drop the `os` and `calib.ReadLabels` uses. In `calib.go`, delete `ReadLabels` and the
imports it alone used (`encoding/csv`, `errors`, `strings` if unused; the compiler
will say). In `calib_test.go`, delete `TestReadLabels`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/calib`, and unsandboxed `go test ./internal/cli -run Calibrate`
Expected: PASS.

- [ ] **Step 5: Checkpoint.** Record in the ledger.

---

### Task 6: Remove `cull --csv`

**Files:**
- Modify: `internal/report/report.go` (delete `WriteCSV`, `f1`, and imports that become unused), `internal/cli/cull.go`, `.gitignore`
- Delete: `internal/report/report_test.go` (its only test is `TestCSVIncludesFocusTargetColumns`)
- Test: `internal/cli/cli_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestCullHasNoCSVFlag(t *testing.T) {
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "cull" && c.Flags().Lookup("csv") != nil {
			t.Fatal("cull --csv should be gone: the JSON report is the only run output")
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run (unsandboxed): `go test ./internal/cli -run TestCullHasNoCSVFlag`
Expected: FAIL with `cull --csv should be gone`.

- [ ] **Step 3: Implement**

`internal/cli/cull.go`:
- delete the `csv` option field, the `--csv` flag, `MarkFlagFilename("csv", …)`, and
  the `rep.WriteCSV` block;
- in `Example`, replace `gophotocull cull --csv cull.csv ~/Pictures/2026-09-26` with
  `gophotocull cull ~/Pictures/2026-09-26`.

`internal/report/report.go`: delete `WriteCSV` and `f1`, and drop `encoding/csv` and
any imports left unused.

`.gitignore`: replace the `gophotocull-report.csv` line with two lines,
`gophotocull-labels.jsonl` and `gophotocull-review/`.

Delete `internal/report/report_test.go`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go vet ./... && grep -rn -i csv --include='*.go' internal cmd`
Expected: vet clean, and grep prints nothing. Unsandboxed:
`go test ./internal/cli ./internal/report` passes.

- [ ] **Step 5: Checkpoint.** Record in the ledger.

---

### Task 7: Review sheet object and the server (`internal/review/serve.go`)

**Files:**
- Modify: `internal/review/review.go` (`Build` returns `*Sheet`; `pageData.Serve`), and `internal/cli/review.go` (takes `sheet.Index`)
- Create: `internal/review/serve.go`
- Test: `internal/review/review_test.go` and `internal/review/serve_test.go`

**Interfaces:**
- Consumes:
  - `labels.Append`, `labels.Read`, `labels.Entry`, `labels.WriteSidecar`;
  - `report.Load` and `report.Report.Save`;
  - `xmp.ErrExists`.
- Produces:
  - `type Sheet struct{ Index, Dir string; data pageData }` and
    `(*Sheet) Page(serve bool) ([]byte, error)`;
  - `Build(...) (*Sheet, error)`;
  - `type ServeOptions struct{ ReportPath, LabelsPath string; WriteXMP, OverwriteXMP bool; Token string }`;
  - `NewServer(s *Sheet, rep *report.Report, o ServeOptions) (*Server, error)`;
  - `(*Server) Handler(addr string) http.Handler`.

- [ ] **Step 1: Write the failing tests**

`internal/review/review_test.go`: in `TestBuildWritesAnOfflinePageWithImages`, change
the start to `sheet, err := Build(...)` and `index := sheet.Index`. Add
`"gophotocull-labels.jsonl"` to the `want` list and `"labels.csv"` to the `bad` list.
New test:

```go
func TestPageMarksServeMode(t *testing.T) {
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	rep := &report.Report{Dir: dir, Results: []report.Result{{File: filepath.Join(dir, "L1.DNG")}}}
	sheet, err := Build(rep, filepath.Join(dir, "r.json"), Options{Out: t.TempDir(), Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	static, _ := os.ReadFile(sheet.Index)
	served, err := sheet.Page(true)
	if err != nil || !strings.Contains(string(served), `"serve":true`) || strings.Contains(string(static), `"serve"`) {
		t.Fatalf("serve flag: %v", err)
	}
}
```

`internal/review/serve_test.go`:

```go
package review

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/labels"
	"github.com/jefflaplante/gophotocull/internal/report"
)

const testAddr = "127.0.0.1:4567"

// serveFixture: L1 (model cull) and L2 (model keep), a saved report, a built sheet.
func serveFixture(t *testing.T, writeXMP bool) (dir string, h http.Handler, token string) {
	t.Helper()
	dir = t.TempDir()
	f1, f2 := filepath.Join(dir, "L1.DNG"), filepath.Join(dir, "L2.DNG")
	tinyDNG(t, f1)
	tinyDNG(t, f2)
	pv := &report.PreviewInfo{Width: 1600, Height: 1067, Orientation: 1, Source: "tiff-ifd"}
	ev := &eval.Evaluation{Sharpness: eval.Sharpness{Score: 2, Status: "missed_focus"}}
	rep := &report.Report{Dir: dir, Results: []report.Result{
		{File: f1, Preview: pv, Evaluation: ev, Decision: eval.Cull},
		{File: f2, Preview: pv, Evaluation: ev, Decision: eval.Keep},
	}}
	rp := filepath.Join(dir, "gophotocull-report.json")
	if err := rep.Save(rp); err != nil {
		t.Fatal(err)
	}
	sheet, err := Build(rep, rp, Options{Out: filepath.Join(dir, "gophotocull-review"), Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	token = strings.Repeat("ab", 16)
	s, err := NewServer(sheet, rep, ServeOptions{ReportPath: rp, LabelsPath: labels.DefaultPath(rp), WriteXMP: writeXMP, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return dir, s.Handler(testAddr), token
}

func call(h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = testAddr
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func api(token string) map[string]string {
	return map[string]string{"X-Gophotocull-Token": token, "Content-Type": "application/json"}
}

func dngHashes(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	m := map[string][32]byte{}
	files, _ := filepath.Glob(filepath.Join(dir, "*.DNG"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		m[f] = sha256.Sum256(b)
	}
	return m
}

func TestServerAccessControl(t *testing.T) {
	_, h, tok := serveFixture(t, false)
	for _, c := range []struct {
		name, method, target string
		hdr                  map[string]string
		want                 int // 0: anything but 200
	}{
		{"page", "GET", "/", nil, 200},
		{"foreign host", "GET", "/", map[string]string{"Host": "evil.example:4567"}, 403},
		{"rebinding host", "GET", "/api/labels", map[string]string{"Host": "attacker.test:4567", "X-Gophotocull-Token": tok}, 403},
		{"no token", "GET", "/api/labels", nil, 403},
		{"wrong token", "GET", "/api/labels", map[string]string{"X-Gophotocull-Token": strings.Repeat("0", 32)}, 403},
		{"foreign origin", "POST", "/api/labels", map[string]string{"X-Gophotocull-Token": tok, "Origin": "http://evil.example"}, 403},
		{"localhost", "GET", "/api/labels", map[string]string{"Host": "localhost:4567", "Origin": "http://localhost:4567", "X-Gophotocull-Token": tok}, 200},
		{"image", "GET", "/L1.thumb.jpg", nil, 200},
		{"escape", "GET", "/..%2Fgophotocull-report.json", nil, 0},
		{"not an image", "GET", "/index.json", nil, 404},
	} {
		rec := call(h, c.method, c.target, "{}", c.hdr)
		if (c.want == 0 && rec.Code == 200) || (c.want != 0 && rec.Code != c.want) {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
	}
	if rec := call(h, "GET", "/", "", nil); !strings.Contains(rec.Body.String(), `"serve":true`) {
		t.Error("served page not in serve mode")
	}
}

func TestServerSavesLabelsAndSidecars(t *testing.T) {
	dir, h, tok := serveFixture(t, true)
	before := dngHashes(t, dir)
	rec := call(h, "POST", "/api/labels", `{"file":"L1.DNG","label":"keep","stars":4}`, api(tok))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sidecar":"written"`) {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	sc, _ := os.ReadFile(filepath.Join(dir, "L1.xmp"))
	for _, want := range []string{`xmp:Rating="4"`, `xmp:Label="Green"`, "gophotocull:keep", "gophotocull:labeled"} {
		if !strings.Contains(string(sc), want) {
			t.Errorf("sidecar lacks %s:\n%s", want, sc)
		}
	}
	rep, _ := report.Load(filepath.Join(dir, "gophotocull-report.json"))
	if r := rep.Results[0]; r.XMP != filepath.Join(dir, "L1.xmp") || r.Decision != eval.Cull {
		t.Fatalf("report: xmp=%q decision=%s (must record ownership, keep the model's decision)", r.XMP, r.Decision)
	}
	if rec := call(h, "GET", "/api/labels", "", api(tok)); !strings.Contains(rec.Body.String(), `"L1.DNG":{"label":"keep","stars":4}`) {
		t.Fatalf("get: %s", rec.Body)
	}
	call(h, "POST", "/api/labels", `{"file":"L1.DNG","label":"","stars":0}`, api(tok)) // clear
	sc, _ = os.ReadFile(filepath.Join(dir, "L1.xmp"))
	if s := string(sc); strings.Contains(s, "xmp:Rating") || !strings.Contains(s, `xmp:Label="Red"`) || strings.Contains(s, "labeled") {
		t.Fatalf("cleared frame's sidecar should be the model's cull again:\n%s", s)
	}
	if rec := call(h, "GET", "/api/labels", "", api(tok)); strings.Contains(rec.Body.String(), "L1.DNG") {
		t.Fatalf("cleared frame still labeled: %s", rec.Body)
	}
	b, _ := os.ReadFile(filepath.Join(dir, labels.FileName))
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Fatalf("log lines %d", n)
	}
	if !reflect.DeepEqual(before, dngHashes(t, dir)) {
		t.Fatal("a DNG changed")
	}
}

func TestServerSkipsForeignSidecar(t *testing.T) {
	dir, h, tok := serveFixture(t, true)
	p := filepath.Join(dir, "L2.xmp")
	os.WriteFile(p, []byte("foreign"), 0o644) // appears after the server started
	for i := 0; i < 2; i++ {
		rec := call(h, "POST", "/api/labels", `{"file":"L2.DNG","label":"cull","stars":0}`, api(tok))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "skipped: sidecar exists and isn't gophotocull's") {
			t.Fatalf("post %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if b, _ := os.ReadFile(p); string(b) != "foreign" {
		t.Fatal("foreign sidecar overwritten")
	}
	if m, _ := labels.Read(filepath.Join(dir, labels.FileName)); m["L2.DNG"].Label != "cull" {
		t.Fatalf("label not saved: %+v", m)
	}
}

func TestServerRejectsBadInput(t *testing.T) {
	dir, h, tok := serveFixture(t, true)
	for _, body := range []string{
		`{"file":"../gophotocull-report.json","label":"keep","stars":0}`,
		`{"file":"X.DNG","label":"keep","stars":0}`,
		`{"file":"L1.DNG","label":"maybe","stars":0}`,
		`{"file":"L1.DNG","label":"","stars":6}`,
		`not json`,
	} {
		if rec := call(h, "POST", "/api/labels", body, api(tok)); rec.Code != 400 {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, labels.FileName)); err == nil {
		t.Fatal("a rejected request wrote the log")
	}
}

func TestServerWithoutWriteXMP(t *testing.T) {
	dir, h, tok := serveFixture(t, false)
	if rec := call(h, "POST", "/api/labels", `{"file":"L1.DNG","label":"keep","stars":1}`, api(tok)); !strings.Contains(rec.Body.String(), `"sidecar":"off"`) {
		t.Fatalf("%s", rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "L1.xmp")); err == nil {
		t.Fatal("sidecar written without --write-xmp")
	}
}

func TestServerConcurrentPosts(t *testing.T) {
	dir, h, tok := serveFixture(t, true)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			call(h, "POST", "/api/labels", fmt.Sprintf(`{"file":"L%d.DNG","label":"keep","stars":%d}`, 1+i%2, i%6), api(tok))
		}(i)
	}
	wg.Wait()
	b, _ := os.ReadFile(filepath.Join(dir, labels.FileName))
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 20 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("torn line %q", l)
		}
	}
}

func TestNewServerRefusesDuplicateNames(t *testing.T) {
	rep := &report.Report{Results: []report.Result{{File: "/a/L1.DNG"}, {File: "/b/L1.DNG"}}}
	_, err := NewServer(&Sheet{}, rep, ServeOptions{Token: strings.Repeat("ab", 16)})
	if err == nil || !strings.Contains(err.Error(), "/a/L1.DNG") || !strings.Contains(err.Error(), "/b/L1.DNG") {
		t.Fatalf("%v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/review`
Expected: build failed (`undefined: NewServer`, `Build` returns 2 values, …).
`httptest.NewRecorder` needs no listener, so this runs sandboxed.

- [ ] **Step 3: Implement**

`internal/review/review.go`:
- `pageData` gains `Serve bool `json:"serve,omitempty"``.
- `Build` ends with:

```go
	s := &Sheet{Dir: o.Out, Index: filepath.Join(o.Out, "index.html"), data: pageData{
		Title: filepath.Base(rep.Dir), Report: reportPath, Backend: rep.Backend, Model: rep.Model,
		Escalation: rep.Escalation, Cards: cards,
	}}
	page, err := s.Page(false)
	if err != nil {
		return nil, err
	}
	return s, os.WriteFile(s.Index, page, 0o644)
}

// Sheet is a built review sheet: its images in Dir, the static page at Index.
type Sheet struct {
	Index, Dir string
	data       pageData
}

// Page renders the sheet. serve marks it as saving through a review server.
func (s *Sheet) Page(serve bool) ([]byte, error) {
	d := s.data
	d.Serve = serve
	b, err := json.Marshal(d) // Marshal escapes <, >, & so the data can't close its script
	if err != nil {
		return nil, err
	}
	return []byte(strings.Replace(pageTemplate, placeholder, string(b), 1)), nil
}
```

The signature becomes `func Build(...) (*Sheet, error)`, and every `return "", err`
becomes `return nil, err`.

`internal/cli/review.go`: `sheet, err := review.Build(...)`, then print `sheet.Index`.
Task 8 adds serving.

`internal/review/serve.go`:

```go
package review

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jefflaplante/gophotocull/internal/labels"
	"github.com/jefflaplante/gophotocull/internal/report"
	"github.com/jefflaplante/gophotocull/internal/xmp"
)

// ServeOptions configure a review server.
type ServeOptions struct {
	ReportPath   string
	LabelsPath   string
	WriteXMP     bool // rewrite each changed frame's sidecar
	OverwriteXMP bool // also sidecars gophotocull did not write
	Token        string
}

// Server saves the user's labels (and optionally sidecars) for one sheet.
type Server struct {
	sheet *Sheet
	o     ServeOptions
	files map[string]string // base name → the frame's path in the report
	mu    sync.Mutex        // one change at a time: log, sidecar, report
}

// NewServer refuses a report where two frames share a base name: a label couldn't
// say which one (or which sidecar) it means.
func NewServer(s *Sheet, rep *report.Report, o ServeOptions) (*Server, error) {
	if len(o.Token) < 32 {
		return nil, errors.New("review server: token too short")
	}
	files := map[string]string{}
	var dups []string
	for _, r := range rep.Results {
		b := filepath.Base(r.File)
		if prev, ok := files[b]; ok {
			dups = append(dups, prev+" and "+r.File)
			continue
		}
		files[b] = r.File
	}
	if len(dups) > 0 {
		return nil, fmt.Errorf("frames share a file name, so labels can't tell them apart: %s", strings.Join(dups, "; "))
	}
	return &Server{sheet: s, o: o, files: files}, nil
}

// Handler serves the sheet for a listener at addr (host:port): the page, its
// images, and the labels API. Every request must name this listener as its Host
// (defeats DNS rebinding); an Origin, when sent, must be this listener; /api
// needs the session token.
func (s *Server) Handler(addr string) http.Handler {
	_, port, _ := net.SplitHostPort(addr)
	hosts := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /index.html", s.page)
	mux.HandleFunc("GET /api/labels", s.getLabels)
	mux.HandleFunc("POST /api/labels", s.postLabel)
	mux.HandleFunc("GET /{name}", s.image)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !(strings.HasPrefix(o, "http://") && hosts[strings.TrimPrefix(o, "http://")]) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gophotocull-Token")), []byte(s.o.Token)) != 1 {
			http.Error(w, "bad or missing token: open the URL the server printed", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) page(w http.ResponseWriter, _ *http.Request) {
	b, err := s.sheet.Page(true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// image serves the sheet's own JPEGs and nothing else.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !strings.HasSuffix(name, ".jpg") || name != filepath.Base(name) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.sheet.Dir, name))
}

type state struct {
	Label string `json:"label"`
	Stars int    `json:"stars"`
}

func (s *Server) getLabels(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	m, err := labels.Read(s.o.LabelsPath)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]state{}
	for f, e := range m {
		out[f] = state{e.Label, e.Stars}
	}
	writeJSON(w, map[string]any{"labels": out})
}

func (s *Server) postLabel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		File  string `json:"file"`
		Label string `json:"label"`
		Stars int    `json:"stars"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	path, ok := s.files[in.File]
	if !ok {
		http.Error(w, "not in the report: "+in.File, http.StatusBadRequest)
		return
	}
	e := labels.Entry{File: in.File, Label: in.Label, Stars: in.Stars, At: time.Now()}
	if err := e.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := labels.Append(s.o.LabelsPath, e); err != nil {
		http.Error(w, "not saved: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sidecar := "off"
	if s.o.WriteXMP {
		sidecar = s.writeSidecar(path, e)
	}
	writeJSON(w, map[string]any{"entry": e, "sidecar": sidecar})
}

// writeSidecar rewrites one frame's sidecar from its effective verdict and stars.
// A newly created sidecar is recorded in the report, re-read just before so that a
// concurrent writer loses at most this ownership record (which fails safe: the
// sidecar is then treated as foreign and never overwritten).
func (s *Server) writeSidecar(file string, e labels.Entry) string {
	rep, err := report.Load(s.o.ReportPath)
	if err != nil {
		return "skipped: " + err.Error()
	}
	for i := range rep.Results {
		r := &rep.Results[i]
		if r.File != file {
			continue
		}
		before := r.XMP
		switch err := labels.WriteSidecar(r, e, false, s.o.OverwriteXMP); {
		case errors.Is(err, xmp.ErrExists):
			return "skipped: sidecar exists and isn't gophotocull's"
		case err != nil:
			return "skipped: " + err.Error()
		}
		if r.XMP != before {
			if err := rep.Save(s.o.ReportPath); err != nil {
				return "written; report not updated: " + err.Error()
			}
		}
		return "written"
	}
	return "skipped: frame no longer in the report"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test -race ./internal/review`
Expected: PASS (9 tests). If "escape" returns 301 (the mux cleaning the path), that
is fine: the test asserts only "not 200".

- [ ] **Step 5: Checkpoint.** Run `go vet ./...` and record it in the ledger.

---

### Task 8: `gophotocull review --serve`

**Files:**
- Modify: `internal/cli/review.go`
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `review.Build`, `review.NewServer`, `(*Server).Handler`, `labels.DefaultPath`.
- Produces:
  - flags `--serve`, `--port` (default 0), `--open`, `--write-xmp`, `--overwrite-xmp`;
  - a stderr line `review server: http://127.0.0.1:<port>/#token=<hex>`.

- [ ] **Step 1: Write the failing tests**

```go
func TestReviewServeFlagValidation(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"review", "--write-xmp", dir},
		{"review", "--port", "8080", dir},
		{"review", "--open", dir},
		{"review", "--serve", "--overwrite-xmp", dir},
	} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "requires") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestReviewServeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1.DNG"))
	if out, err := run(t, "scan", dir); err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	pr, pw := io.Pipe()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(pw)
	cmd.SetArgs([]string{"review", "--serve", dir})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx); pw.Close() }()
	var url string
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		if u, ok := strings.CutPrefix(sc.Text(), "review server: "); ok {
			url = u
			break
		}
	}
	go io.Copy(io.Discard, pr)
	base, tok, ok := strings.Cut(url, "/#token=")
	if !ok || !strings.HasPrefix(base, "http://127.0.0.1:") || len(tok) != 32 {
		t.Fatalf("url %q", url)
	}
	req, _ := http.NewRequest("POST", base+"/api/labels", strings.NewReader(`{"file":"L1.DNG","label":"keep","stars":3}`))
	req.Header.Set("X-Gophotocull-Token", tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("post: %v %v", err, res)
	}
	res.Body.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve exit: %v", err)
	}
	if m, err := labels.Read(filepath.Join(dir, labels.FileName)); err != nil || m["L1.DNG"].Stars != 3 {
		t.Fatalf("log: %v %v", m, err)
	}
}
```

Imports to add: `bufio`, `io`, `net/http`, `internal/labels`.

- [ ] **Step 2: Run the tests to verify they fail**

Run (unsandboxed, since it binds loopback): `go test ./internal/cli -run ReviewServe`
Expected: FAIL with `unknown flag: --write-xmp` / `--serve`.

- [ ] **Step 3: Implement.** `internal/cli/review.go`: add the flags and this `RunE`:

```go
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			for _, name := range []string{"port", "open", "write-xmp", "overwrite-xmp"} {
				if fl.Changed(name) && !serve {
					return fmt.Errorf("--%s requires --serve", name)
				}
			}
			if overwrite && !writeXMP {
				return fmt.Errorf("--overwrite-xmp requires --write-xmp")
			}
			cfg, err := so.base(args[0])
			// ... load report, default out, Build as today ...
			if !serve {
				fmt.Fprintf(cmd.ErrOrStderr(), "review sheet: %s\nopen it with: open %q\n", sheet.Index, sheet.Index)
				return nil
			}
			return serveSheet(cmd, sheet, rep, review.ServeOptions{
				ReportPath: cfg.ReportPath, LabelsPath: labels.DefaultPath(cfg.ReportPath),
				WriteXMP: writeXMP, OverwriteXMP: overwrite,
			}, port, openIt)
		},
```

```go
// serveSheet runs the review server on 127.0.0.1 until the command's context ends
// (Ctrl-C).
func serveSheet(cmd *cobra.Command, sheet *review.Sheet, rep *report.Report, o review.ServeOptions, port int, openIt bool) error {
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	o.Token = hex.EncodeToString(tok)
	srv, err := review.NewServer(sheet, rep, o)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	url := "http://" + addr + "/#token=" + o.Token
	w := cmd.ErrOrStderr()
	fmt.Fprintf(w, "review server: %s\nlabels: %s\n", url, o.LabelsPath)
	if o.WriteXMP {
		fmt.Fprintln(w, "sidecars: written on every change (never over ones gophotocull didn't write)")
	}
	fmt.Fprintln(w, "Ctrl-C to stop. Don't run cull on this report while reviewing.")
	if openIt {
		if err := exec.Command("open", url).Start(); err != nil {
			fmt.Fprintf(w, "could not open a browser (%v): open the URL above\n", err)
		}
	}
	hs := &http.Server{Handler: srv.Handler(addr), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-cmd.Context().Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
```

Imports: `context`, `crypto/rand`, `encoding/hex`, `errors`, `net`, `net/http`,
`os/exec`, `time`, `internal/labels`.

Flags:

```go
	f := cmd.Flags()
	f.BoolVar(&serve, "serve", false, "serve the sheet on 127.0.0.1 and save every label to gophotocull-labels.jsonl beside the report")
	f.IntVar(&port, "port", 0, "port for --serve (0 = any free port)")
	f.BoolVar(&openIt, "open", false, "open the served sheet in the default browser")
	f.BoolVar(&writeXMP, "write-xmp", false, "with --serve: rewrite each changed frame's sidecar (your stars, verdict colour and keyword)")
	f.BoolVar(&overwrite, "overwrite-xmp", false, "with --write-xmp: also overwrite sidecars gophotocull did not write")
```

Update `Long`: labels are saved in the browser, or with `--serve` to
gophotocull-labels.jsonl beside the report on every keypress; `calibrate`,
`decide --labels` and `apply-c1 --labels` read that log.

Update `Example`:
`gophotocull review --serve --open ~/Pictures/2026-09-26`.

- [ ] **Step 4: Run the tests to verify they pass**

Run (unsandboxed): `go test -race ./internal/cli -run 'Review'`
Expected: PASS.

- [ ] **Step 5: Checkpoint.** Record in the ledger.

---

### Task 9: The page (`internal/review/page.html`)

**Files:**
- Modify: `internal/review/page.html`
- Verification harness (outside the repo): `$SCRATCH/jsdom-review/check.mjs`

**Interfaces:**
- Consumes:
  - the page data field `serve` (Task 7);
  - `GET /api/labels` → `{"labels": {file: {label, stars}}}`;
  - `POST /api/labels` with body `{file, label, stars}` → `{"entry", "sidecar"}`, 400
    on bad input, and the header `X-Gophotocull-Token` taken from `location.hash`
    (`#token=`).

- [ ] **Step 1: Write the harness (fails against the current page)**

Set up once:

```sh
mkdir -p $SCRATCH/jsdom-review && cd $SCRATCH/jsdom-review
npm init -y >/dev/null && npm_config_cache=$SCRATCH/npm-cache npm install jsdom@24 >/dev/null
```

The sheet under test is built from the 17-frame report:
`./bin/gophotocull review -o photos/gophotocull-review/report.json --out $SCRATCH/sheet-test photos`.

`check.mjs`:

```js
import { JSDOM } from "jsdom";
import { readFileSync } from "node:fs";

const html = readFileSync(process.argv[2], "utf8");
const results = [];
const ok = (name, cond, detail = "") => results.push([cond ? "PASS" : "FAIL", name, detail]);
const tick = () => new Promise(r => setTimeout(r, 0));

// page(serve, opts): a fresh page. In serve mode, fetch is a stub recording calls;
// opts.fail makes POSTs reject (server down); opts.disk is GET's label map.
async function page(serve, opts = {}) {
  const src = serve ? html.replace('{"title"', '{"serve":true,"title"') : html;
  const posts = [];
  const dom = new JSDOM(src, {
    runScripts: "dangerously", url: "http://127.0.0.1:4567/#token=" + "ab".repeat(16), pretendToBeVisual: true,
    beforeParse(w) {
      w.HTMLElement.prototype.scrollIntoView = () => {}; // jsdom has no layout
      if (opts.storage) for (const [k, v] of Object.entries(opts.storage)) w.localStorage.setItem(k, v);
      w.fetch = async (url, init = {}) => {
        if (init.method === "POST") {
          if (opts.fail && opts.fail()) throw new TypeError("network down");
          posts.push({ body: JSON.parse(init.body), token: init.headers["X-Gophotocull-Token"] });
          return { ok: true, status: 200, json: async () => ({ sidecar: "off" }), text: async () => "" };
        }
        return { ok: true, status: 200, json: async () => ({ labels: opts.disk || {} }), text: async () => "" };
      };
    },
  });
  await tick(); await tick();
  const w = dom.window, d = w.document;
  const key = k => { d.dispatchEvent(new w.KeyboardEvent("keydown", { key: k })); };
  const click = text => [...d.querySelectorAll("button")].find(b => b.textContent === text).click();
  const report = JSON.parse(d.getElementById("data").textContent).report;
  const stored = () => JSON.parse(w.localStorage.getItem("gophotocull-labels:" + report) || "{}");
  const pending = () => JSON.parse(w.localStorage.getItem("gophotocull-pending:" + report) || "[]");
  const cap = () => d.querySelector(".cap")?.textContent || "";
  return { w, d, key, click, stored, pending, posts, cap, report };
}

// 1. Static: label and stars are stored as objects; "4" doesn't advance outside Unrated.
{
  const p = await page(false);
  p.key("Enter"); const first = p.cap(); p.key("4");
  ok("stars stay on the frame outside Unrated", p.cap() === first, p.cap());
  p.key("k");
  const f = first.split(" · ")[1];
  ok("label+stars stored", JSON.stringify(p.stored()[f]) === JSON.stringify({ label: "keep", stars: 4 }), JSON.stringify(p.stored()));
  ok("K advances", p.cap() !== first);
}
// 2. Keep + Unrated: a star advances, and the rated frame leaves the filter after navigation.
{
  const p = await page(false);
  p.click("Keep"); p.click("Unrated"); p.key("Enter");
  const n0 = Number(p.cap().split(" / ")[1].split(" ")[0]);
  p.key("3");
  const n1 = Number(p.cap().split(" / ")[1].split(" ")[0]);
  ok("star advances under Unrated, and the rated frame drops out", n1 === n0 - 1, `${n0} → ${n1}`);
}
// 3. Stay on screen: in the grid, relabel a Keep frame as cull; it stays until navigation.
{
  const p = await page(false);
  p.click("Keep");
  const before = p.d.querySelectorAll(".card").length;
  p.key("c");
  ok("changed frame stays visible", p.d.querySelectorAll(".card").length === before);
  p.key("ArrowRight");
  ok("filter re-applies on navigation", p.d.querySelectorAll(".card").length === before - 1);
}
// 4. Serve: a keypress POSTs {file,label,stars} with the token; header says saved.
{
  const p = await page(true);
  p.key("Enter"); p.key("k"); await tick(); await tick();
  ok("POST sent with token", p.posts.length === 1 && p.posts[0].token === "ab".repeat(16) && p.posts[0].body.label === "keep", JSON.stringify(p.posts));
  ok("saved note", p.d.getElementById("save").textContent === "all changes saved", p.d.getElementById("save").textContent);
  ok("export hidden in serve mode", p.d.getElementById("tools").hidden === true);
}
// 5. Server down: changes queue in order and replay when it's back.
{
  let down = true;
  const p = await page(true, { fail: () => down });
  p.key("Enter"); p.key("k"); await tick(); p.key("r"); await tick(); await tick();
  ok("unsaved counted", /2 unsaved/.test(p.d.getElementById("save").textContent), p.d.getElementById("save").textContent);
  ok("queue persisted", p.pending().length === 2);
  down = false;
  p.key("ArrowLeft"); p.key("4"); await tick(); await tick(); await tick();
  ok("replayed in order", p.posts.map(x => x.body.label).join(",") === "keep,review,review" && p.pending().length === 0, JSON.stringify(p.posts.map(x => x.body)));
}
// 6. Load: disk wins except frames with queued changes.
{
  const probe = await page(false);
  const files = [...probe.d.querySelectorAll(".card .name")].map(n => n.textContent);
  const disk = { [files[0]]: { label: "cull", stars: 1 }, [files[1]]: { label: "keep", stars: 2 } };
  const storage = {
    ["gophotocull-labels:" + probe.report]: JSON.stringify({ [files[1]]: { label: "review", stars: 5 } }),
    ["gophotocull-pending:" + probe.report]: JSON.stringify([{ file: files[1], label: "review", stars: 5 }]),
  };
  const p = await page(true, { disk, storage });
  await tick(); await tick();
  ok("disk label loaded", JSON.stringify(p.stored()[files[0]]) === JSON.stringify({ label: "cull", stars: 1 }), JSON.stringify(p.stored()));
  ok("queued change kept over disk", JSON.stringify(p.stored()[files[1]]) === JSON.stringify({ label: "review", stars: 5 }));
}
// 7. Import: folds last-wins; a bad middle line imports nothing; a torn last line is ignored.
{
  const p = await page(false);
  const f = p.d.querySelector(".card .name").textContent;
  const imp = async text => {
    const input = p.d.getElementById("import");
    Object.defineProperty(input, "files", { configurable: true, value: [{ text: async () => text }] });
    input.dispatchEvent(new p.w.Event("change")); await tick(); await tick();
  };
  await imp(`{"file":"${f}","label":"keep","stars":1}\n{"file":"${f}","label":"cull","stars":3}\n{"file":"x","la`);
  ok("import folds, ignores torn last line", JSON.stringify(p.stored()[f]) === JSON.stringify({ label: "cull", stars: 3 }), JSON.stringify(p.stored()[f]));
  await imp(`{"file":"${f}","label":"keep","stars":0}\nnope\n`);
  ok("bad line imports nothing", p.stored()[f].label === "cull" && /line 2/.test(p.d.getElementById("save").textContent));
}
// 8. Older browser copies (bare label strings) are read.
{
  const probe = await page(false);
  const f = probe.d.querySelector(".card .name").textContent;
  const p = await page(false, { storage: { ["gophotocull-labels:" + probe.report]: JSON.stringify({ [f]: "keep" }) } });
  ok("string labels normalized", p.d.querySelector(".card .label")?.textContent === "keep");
}

for (const r of results) console.log(r.join("  "));
process.exit(results.some(r => r[0] === "FAIL") ? 1 : 0);
```

- [ ] **Step 2: Run the harness against the current page to verify it fails**

Run: `node $SCRATCH/jsdom-review/check.mjs $SCRATCH/sheet-test/index.html`
Expected: FAIL. Check 1 fails (stars unsupported), check 2 fails on the
`button "Unrated"` lookup (a TypeError, which counts as a failure), and so on.

- [ ] **Step 3: Implement the page**

Header markup (replaces the `filters`/`tools` divs and the help line):

```html
  <div class="filters" id="verdicts" role="group" aria-label="Verdict"></div>
  <div class="filters" id="progress" role="group" aria-label="Progress"></div>
  <div class="tools" id="tools">
    <button id="export">Export labels</button>
    <label class="file">Import labels<input type="file" id="import" accept=".jsonl,application/jsonl,application/x-ndjson,text/plain"></label>
  </div>
  <span class="save" id="save"></span>
  <span class="help">← → move · K keep · R review · C cull · U clear · 1–5 stars · 0 no stars · Enter open · Esc grid</span>
```

CSS additions:

```css
.stars { color: var(--review); letter-spacing: 1px; white-space: nowrap; }
.save { font-size: 12px; color: var(--muted); } .save.warn, .status.warn { color: var(--cull); }
.status { font-size: 12px; color: var(--muted); margin: 6px 0 0; }
.group-name { color: var(--muted); font-size: 12px; align-self: center; }
```

Script (replaces the whole IIFE body):

```js
(() => {
  const data = JSON.parse(document.getElementById("data").textContent);
  const cards = data.cards;
  const serve = !!data.serve;
  const token = new URLSearchParams(location.hash.slice(1)).get("token") || "";
  const key = "gophotocull-labels:" + data.report;   // this browser's copy
  const qkey = "gophotocull-pending:" + data.report; // serve mode: changes not yet on disk
  const load = (k, def) => { try { return JSON.parse(localStorage.getItem(k) || "null") || def; } catch (e) { return def; } };
  const store = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); } catch (e) {} };

  // labels: file → {label, stars}. Older sheets stored the bare label string.
  let labels = {};
  for (const [f, v] of Object.entries(load(key, {}))) labels[f] = typeof v === "string" ? { label: v, stars: 0 } : v;
  let pending = serve ? load(qkey, []) : [];
  const status = {};  // file → note shown on that frame
  let note = "";      // header note (server state, import errors)

  const get = f => labels[f] || { label: "", stars: 0 };
  const effective = c => get(c.file).label || c.decision || "";

  let verdict = "all", progress = "all", sel = 0, detailOpen = false, pinned = -1;
  const verdicts = [["all", "All"], ["keep", "Keep"], ["review", "Review"], ["cull", "Cull"]];
  const progresses = [["all", "All"], ["unlabeled", "Unlabeled"], ["unrated", "Unrated"], ["disagree", "Disagreements"]];
  const matches = i => {
    const c = cards[i], l = get(c.file);
    if (verdict !== "all" && effective(c) !== verdict) return false;
    switch (progress) {
      case "unlabeled": return !l.label && !c.error;
      case "unrated": return !l.stars && !c.error;
      case "disagree": return !!l.label && !!c.decision && l.label !== c.decision;
      default: return true;
    }
  };
  // A frame changed while shown stays visible until the next navigation.
  const visible = () => cards.map((c, i) => i).filter(i => i === pinned || matches(i));

  const el = (tag, attrs = {}, ...kids) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "text") n.textContent = v; else if (k === "cls") n.className = v; else n.setAttribute(k, v);
    }
    kids.flat().forEach(k => k && n.appendChild(k));
    return n;
  };
  const img = (name, alt) => name ? el("img", { src: encodeURIComponent(name), alt, loading: "lazy" }) : null;
  const badge = (d, extra = "") => el("span", { cls: "badge " + (d || "none") + extra, text: d || "unscored" });
  const starText = n => n ? el("span", { cls: "stars", text: "★".repeat(n), title: n + " star" + (n > 1 ? "s" : "") }) : null;

  document.getElementById("title").textContent = data.title + " · review";
  document.title = data.title + " · gophotocull review";
  if (serve) document.getElementById("tools").hidden = true;

  function group(id, name, items, current, set) {
    document.getElementById(id).replaceChildren(el("span", { cls: "group-name", text: name }), ...items.map(([v, label]) => {
      const b = el("button", { text: label, "aria-pressed": String(current === v) });
      b.onclick = () => { set(v); pinned = -1; sel = 0; render(); };
      return b;
    }));
  }

  function renderHeader() {
    const n = { keep: 0, review: 0, cull: 0 };
    cards.forEach(c => { if (n[c.decision] !== undefined) n[c.decision]++; });
    const labeled = cards.filter(c => get(c.file).label).length, rated = cards.filter(c => get(c.file).stars).length;
    const model = [data.backend, data.model].filter(Boolean).join(" ") + (data.escalation ? " → " + data.escalation : "");
    document.getElementById("meta").textContent =
      `${cards.length} frames · model keep ${n.keep} · review ${n.review} · cull ${n.cull} · labeled ${labeled} · rated ${rated}` + (model ? ` · ${model}` : "");
    group("verdicts", "verdict", verdicts, verdict, v => { verdict = v; });
    group("progress", "show", progresses, progress, v => { progress = v; });
    const s = document.getElementById("save");
    let text = note;
    if (!text && serve) text = pending.length ? `${pending.length} unsaved change${pending.length > 1 ? "s" : ""}` : "all changes saved";
    s.textContent = text;
    s.className = "save" + (note || pending.length ? " warn" : "");
  }

  function renderGrid() {
    const grid = document.getElementById("grid");
    const vis = visible();
    if (!vis.length) { grid.replaceChildren(el("p", { cls: "empty", text: "No frames match this filter." })); return; }
    grid.replaceChildren(...vis.map((i, k) => {
      const c = cards[i], l = get(c.file);
      const card = el("div", { cls: "card" + (k === sel ? " sel" : ""), tabindex: "0" },
        img(c.thumb, c.file) || el("div", { cls: "empty", text: c.error || "no preview" }),
        el("div", { cls: "row" }, el("span", { cls: "name", text: c.file, title: c.path }), starText(l.stars), badge(c.decision)));
      if (l.label) card.appendChild(badge(l.label, " label"));
      card.onclick = () => { sel = k; detailOpen = true; render(); };
      return card;
    }));
    const s = grid.querySelector(".sel");
    if (s) s.scrollIntoView({ block: "nearest" });
  }

  function row(dl, term, value) { /* unchanged */ }

  function renderDetail() {
    // ... unchanged down to `row(info, "file", c.path);`, then:
    const l = get(c.file);
    const actions = el("div", { cls: "actions" }, ...[["keep", "Keep (K)"], ["review", "Review (R)"], ["cull", "Cull (C)"], ["", "Clear (U)"]].map(([v, name]) => {
      const b = el("button", { text: name, "aria-pressed": String(l.label === v && !!v) });
      b.onclick = () => setLabel(v);
      return b;
    }));
    const starBtns = el("div", { cls: "actions" }, ...[1, 2, 3, 4, 5, 0].map(n => {
      const b = el("button", { text: n ? "★".repeat(n) + ` (${n})` : "No stars (0)", "aria-pressed": String(!!n && l.stars === n) });
      b.onclick = () => setStars(n);
      return b;
    }));
    const st = status[c.file];
    // nav, as today
    box.replaceChildren(
      el("div", { cls: "pane" }, img(c.thumb, c.file) || el("p", { cls: "empty", text: c.error || "no preview" }),
        el("p", { cls: "cap", text: `${sel + 1} / ${vis.length} · ${c.file}` }), nav),
      el("div", { cls: "pane subject" },
        c.subject ? el("div", {}, img(c.subject, "subject crop"), el("p", { cls: "cap", text: "Subject at native preview resolution (what the model judged)" })) : null,
        el("div", {}, badge(c.decision), l.label ? el("span", { text: "  your label: " }) : null, l.label ? badge(l.label) : null,
          l.stars ? el("span", { text: "  " }) : null, starText(l.stars)),
        st ? el("p", { cls: "status" + (/^(not saved|rejected)|sidecar not written/.test(st) ? " warn" : ""), text: st }) : null,
        actions, starBtns, info));
  }
```

The `cap` stays the first `.cap` element, the image pane caption `n / N · file`. The
harness reads it.

```js
  function update(change, advance) {
    const vis = visible();
    if (!vis.length) return;
    const i = vis[Math.min(sel, vis.length - 1)], c = cards[i];
    const next = Object.assign({}, get(c.file), change);
    if (!next.label && !next.stars) delete labels[c.file]; else labels[c.file] = next;
    store(key, labels);
    pinned = i;
    if (serve) { pending.push({ file: c.file, label: next.label, stars: next.stars }); store(qkey, pending); flush(); }
    if (detailOpen && advance) move(1); else render();
  }
  const setLabel = v => update({ label: v }, !!v);
  const setStars = n => update({ stars: n }, n > 0 && progress === "unrated");

  // move steps from the shown frame to its neighbour in the re-filtered list.
  function move(d) {
    const vis = visible();
    if (!vis.length) return;
    const cur = vis[Math.min(sel, vis.length - 1)];
    pinned = -1;
    const now = visible();
    let k = -1;
    if (d > 0) { k = now.findIndex(i => i > cur); if (k < 0) k = now.length - 1; }
    else { for (let j = now.length - 1; j >= 0; j--) if (now[j] < cur) { k = j; break; } if (k < 0) k = 0; }
    sel = Math.max(0, k);
    render();
  }
  function render() {
    renderHeader();
    document.getElementById("grid").style.display = detailOpen ? "none" : "";
    document.getElementById("detail").className = detailOpen ? "open" : "";
    if (detailOpen) renderDetail(); else renderGrid();
  }

  // flush sends queued changes in order; a refused change (400) is dropped and
  // shown, a failure leaves the queue for the next change or reload.
  let flushing = false;
  async function flush() {
    if (flushing) return;
    flushing = true;
    try {
      while (pending.length) {
        const p = pending[0];
        let res;
        try {
          res = await fetch("/api/labels", { method: "POST", headers: { "Content-Type": "application/json", "X-Gophotocull-Token": token }, body: JSON.stringify(p) });
        } catch (e) {
          pending.forEach(q => { status[q.file] = "not saved: server unreachable"; });
          break;
        }
        if (!res.ok) {
          const msg = (await res.text()).trim();
          if (res.status === 400) { status[p.file] = "rejected: " + msg; pending.shift(); store(qkey, pending); continue; }
          status[p.file] = "not saved: " + msg;
          break;
        }
        const out = await res.json();
        pending.shift(); store(qkey, pending);
        if (!pending.some(q => q.file === p.file))
          status[p.file] = out.sidecar && out.sidecar.startsWith("skipped: ") ? "saved; sidecar not written: " + out.sidecar.slice(9) : "saved";
      }
    } finally { flushing = false; render(); }
  }

  // loadServer: the log on disk wins, except frames with changes still queued here.
  async function loadServer() {
    try {
      const res = await fetch("/api/labels", { headers: { "X-Gophotocull-Token": token } });
      if (!res.ok) throw new Error((await res.text()).trim());
      const disk = (await res.json()).labels || {};
      const merged = Object.assign({}, disk);
      for (const f of new Set(pending.map(p => p.file))) { if (labels[f]) merged[f] = labels[f]; else delete merged[f]; }
      labels = merged; store(key, labels); note = "";
    } catch (e) { note = "server unreachable (" + e.message + "): showing this browser's copy"; }
    render();
    flush();
  }

  document.addEventListener("keydown", ev => {
    if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
    const k = ev.key.toLowerCase();
    if (k === "arrowright" || k === "arrowdown") { move(1); ev.preventDefault(); }
    else if (k === "arrowleft" || k === "arrowup") { move(-1); ev.preventDefault(); }
    else if (k === "enter") { detailOpen = true; render(); }
    else if (k === "escape") { detailOpen = false; pinned = -1; render(); }
    else if (k === "k") setLabel("keep");
    else if (k === "r") setLabel("review");
    else if (k === "c") setLabel("cull");
    else if (k === "u") setLabel("");
    else if (k.length === 1 && k >= "0" && k <= "5") setStars(Number(k));
  });

  document.getElementById("export").onclick = () => {
    const at = new Date().toISOString();
    const lines = cards.filter(c => labels[c.file]).map(c => JSON.stringify({ file: c.file, label: get(c.file).label, stars: get(c.file).stars, at }) + "\n");
    const a = el("a", { href: URL.createObjectURL(new Blob(lines, { type: "application/jsonl" })), download: "gophotocull-labels.jsonl" });
    document.body.appendChild(a); a.click(); a.remove();
  };
  document.getElementById("import").onchange = ev => {
    const file = ev.target.files[0];
    if (!file) return;
    file.text().then(text => {
      const lines = text.split("\n"), torn = !text.endsWith("\n");
      const next = Object.assign({}, labels);
      for (let n = 0; n < lines.length; n++) {
        if (!lines[n].trim()) continue;
        let e = null;
        try { e = JSON.parse(lines[n]); } catch (err) {}
        const label = e && (e.label || ""), stars = e && (e.stars || 0);
        const valid = e && typeof e.file === "string" && e.file && ["", "keep", "review", "cull"].includes(label) && Number.isInteger(stars) && stars >= 0 && stars <= 5;
        if (!valid) {
          if (torn && n === lines.length - 1) break; // an interrupted final append
          note = `import: line ${n + 1} is not a valid label; nothing imported`; render(); return;
        }
        if (!label && !stars) delete next[e.file]; else next[e.file] = { label, stars };
      }
      labels = next; store(key, labels); note = ""; render();
    });
    ev.target.value = "";
  };

  render();
  if (serve) loadServer();
})();
```

`row()` and the unchanged parts of `renderDetail` are kept exactly as they are now.

- [ ] **Step 4: Rebuild and run the harness to verify it passes**

Run:
```sh
GOCACHE=$SCRATCH/gocache make build && ./bin/gophotocull review --force -o photos/gophotocull-review/report.json --out $SCRATCH/sheet-test photos
node $SCRATCH/jsdom-review/check.mjs $SCRATCH/sheet-test/index.html
```
Expected: all lines PASS, exit 0. Then run `GOCACHE=$SCRATCH/gocache go test ./internal/review`;
the Task 7 page assertions must still pass.

- [ ] **Step 5: Checkpoint.** Record the harness output in the ledger. It is not in
`make test`: say so in the final message.

---

### Task 10: Docs and live check

**Files:**
- Modify: `README.md`, `CLAUDE.md`

- [ ] **Step 1: README.**
  - Replace the review and calibration section: `review --serve --open` and its flags;
    the labels log's format and location; K/R/C/U and 1–5/0; the filters.
  - Document `calibrate` defaulting to the log, `decide --labels`, and
    `apply-c1 --labels`.
  - Add the sidecar mapping table, and "don't run cull on a report while reviewing it".
  - Delete every `--csv` and `labels.csv` mention.
  - Recommended workflow: `review --serve --write-xmp`, then import into Capture One.
    This is verified: Capture One reads DNG sidecars on import.
- [ ] **Step 2: CLAUDE.md.**
  - Layout: add `internal/labels`; `internal/review` gains the server;
    `internal/report` becomes "JSON source of truth (schema v3)", dropping "+ CSV".
  - Commands: replace `cull <dir> --csv x` with `cull <dir>`, and add
    `review --serve --open <dir>`.
  - Unverified assumptions: add "Capture One colour tag 4 = green (probe)".
- [ ] **Step 3: Full verification.**

  Run (unsandboxed): `go test -race -count=1 ./... && go vet ./... && test -z "$(gofmt -l .)"`
  Expected: all packages pass, vet and gofmt clean.

  Then: `grep -rn -i 'csv' --exclude-dir=docs --exclude-dir=photos --exclude-dir=.git --exclude-dir=bin .`
  Expected: nothing.
- [ ] **Step 4: Live check with the user.** The user runs
  `./bin/gophotocull review --serve --open --out photos/gophotocull-review -o photos/gophotocull-review/report.json photos`
  (the sandbox blocks both `open` and binding), labels and stars a few frames, and
  presses Ctrl-C. Then check:
  - the lines in `photos/gophotocull-review/gophotocull-labels.jsonl`;
  - that `./bin/gophotocull calibrate photos/gophotocull-review/report.json` reads
    them;
  - optionally, a `--write-xmp` run on `photos/c1-sidecar-test`, with import into a
    throwaway Capture One session to see the stars and Green colour.
- [ ] **Step 5: Final review, then finish.**
  - Dispatch the fresh whole-branch reviewer (most capable model) with the Review
    Focus list above.
  - Fix Critical and Important findings, each RED to GREEN.
  - Then use superpowers:finishing-a-development-branch. The commit goes on
    `review-serve`.
