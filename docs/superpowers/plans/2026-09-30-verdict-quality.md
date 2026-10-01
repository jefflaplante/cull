# Verdict Quality Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make verdicts measurable and harder to get wrong. Specifically:
- measure run-to-run flips;
- cull only when the evidence agrees;
- ask for evidence before scores;
- let doubtful frames and rankings get a second look (opt-in);
- aim the local focus measures at the right face and its eyes.

**Architecture:** The model still only assesses; every new rule lives in `eval.Policy` or `decideAll`, so `decide` re-applies it for free and `calibrate` can tune it.
- New **assessments** (a second opinion, a reversed-order ranking) are stored beside the first in the report.
- New **measurements** (eye detail) are advisory fields that no rule reads yet.
- **Cost:** two features add model calls; both are off by default.

**Tech Stack:** Go 1.22+, stdlib + cobra + pigo `core`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-30-repo-review-backlog.md`, section 3 (items 1–7).

## Global Constraints

- Keep/review/cull is decided in Go (`eval.Policy` / `decideAll`), never by the model.
- Never run `judge` on real photos without asking: these changes are verified with synthetic fixtures and fake backends only.
- **A report from before this plan must decide exactly as before:**
  - new policy fields default to "off" when absent from a stored policy;
  - new report fields are `omitempty`.
- **Default costs don't rise:** `--second-opinion` and `--rank-twice` are opt-in.
- Tests use synthetic fixtures. Loopback packages (`llm`, `cli`, `review`) run unsandboxed. Build with `GOCACHE=$TMPDIR/gocache`.
- One commit per task, on branch `feat/verdict-quality`.

## Review Focus

- **An old report** (stored policy without `cull_max_sharpness`; no second opinions; sets without a reversed order) decides as before. *Task 3: `TestOldStoredPolicyCullsOnStatusAlone`. Task 6: `TestSetWithoutReversedOrderDecidesAsBefore`.*
- **An escalation where both models agree** the frame is missed focus is still culled; only disagreement goes to review. *Task 4: `TestAgreeingAssessmentsStillCull`.*
- **`--second-opinion` with `--batch`** is refused, not silently ignored. *Task 5: CLI flag validation case.*
- **A disputed rank where the frame is already review or cull:** the dispute never raises a frame above where the outranked rule would. *Task 6: `TestDisputedRankOnlyDemotesKeeps`.*
- **A face with one pupil found** still aims at that eye. *Task 7: `TestOnePupilStillAims`, a pure function test on `eyePoint`.*

---

### Task 1: `calibrate --compare`: how often verdicts flip between runs (item 1)

**Files:**
- Create: `internal/calib/compare.go`, `internal/calib/compare_test.go`
- Modify: `internal/cli/calibrate.go` (`--compare` mode: exactly two reports, no labels needed)

**Interfaces:**
- Produces: `type RunDiff struct { N int; Same int; Flips map[string]int; StatusFlips int; MeanAbsSharpDelta float64; Missing int }`; `func CompareRuns(a, b *report.Report) RunDiff`; `func FormatRunDiff(w io.Writer, an, bn string, d RunDiff)`.

- [ ] **Step 1: Write the failing test**

```go
func TestCompareRunsCountsFlips(t *testing.T) {
	mk := func(d eval.Decision, status string, score float64) report.Result {
		return report.Result{Decision: d, Evaluation: &eval.Evaluation{Sharpness: eval.Sharpness{Status: status, Score: score}}}
	}
	a := &report.Report{Results: []report.Result{mk(eval.Keep, "sharp", 8), mk(eval.Cull, "missed_focus", 2), mk(eval.Review, "soft", 5), mk(eval.Keep, "sharp", 9)}}
	b := &report.Report{Results: []report.Result{mk(eval.Keep, "acceptable", 7), mk(eval.Review, "soft", 4), mk(eval.Review, "soft", 5)}}
	names := []string{"A.DNG", "B.DNG", "C.DNG", "D.DNG"}
	for i := range a.Results {
		a.Results[i].File = "/x/" + names[i]
	}
	for i := range b.Results {
		b.Results[i].File = "/y/" + names[i] // runs are matched by base name
	}
	d := CompareRuns(a, b)
	if d.N != 3 || d.Same != 2 || d.Flips["cull→review"] != 1 || d.StatusFlips != 2 || d.Missing != 1 {
		t.Fatalf("%+v", d)
	}
	if math.Abs(d.MeanAbsSharpDelta-1) > 1e-9 { // |8-7|, |2-4|, |5-5|: (1+2+0)/3
		t.Fatalf("mean delta %v", d.MeanAbsSharpDelta)
	}
}
```

- [ ] **Step 2: Run it and check it fails** (undefined).
- [ ] **Step 3: Implement.**
  - `CompareRuns` matches frames by base name.
  - It counts only frames that have an evaluation and a decision in both reports. `Missing` is the frames evaluated in only one of them.
  - `Flips` is keyed `"<a>→<b>"`, and only for differing decisions.
  - `StatusFlips` counts differing sharpness statuses.
  - `FormatRunDiff` prints:
    - the number of shared frames and the agreement rate;
    - every flip kind, sorted, with `keep→cull` and `cull→keep` called out as "crossings";
    - sharpness status flips;
    - the mean |Δ sharpness score|.

  In `cli/calibrate.go`, add a `--compare` bool. With it:
  - require exactly 2 report args;
  - skip labels entirely;
  - print `FormatRunDiff`.

  Help: "`--compare A.json B.json`: how often two runs over the same frames disagree (run-to-run stability, or two backends)".
- [ ] **Step 4: Run** `go test ./internal/calib` and, unsandboxed, `./internal/cli`. Add a CLI test: two saved reports, `calibrate --compare a b`, and the output contains `agree` and `cull→review`.
- [ ] **Step 5: Docs.** README and WORKFLOW calibrate sections: one bullet on `--compare`, measured by running `judge -o run2.json` twice.
- [ ] **Step 6: Commit.** `calibrate --compare: run-to-run verdict flips between two reports`

---

### Task 2: Evidence before score: ordered schemas and status score bands (item 3)

**Files:**
- Modify: `internal/llm/validate.go` (`Portable` returns an order-preserving schema)
- Modify: `internal/eval/prompt.go` (evaluation schema `required` order; score bands in the prompt), `internal/eval/rank.go` (summary before ranking; prompt says so)
- Test: `internal/llm/validate_test.go`, `internal/eval/eval_test.go`

**Interfaces:**
- Produces: `type Schema map[string]any` with `MarshalJSON` that writes each object's `properties` in the order of its sibling `required` array (any others after them, sorted), and every other map's keys sorted. `Portable` returns `Schema`.

- [ ] **Step 1: Write the failing tests**

```go
// validate_test.go
func TestPortableKeepsRequiredOrder(t *testing.T) {
	s := map[string]any{"type": "object", "required": []string{"zeta", "alpha", "mid"},
		"properties": map[string]any{"alpha": map[string]any{"type": "string"}, "mid": map[string]any{"type": "string"}, "zeta": map[string]any{"type": "string"}}}
	b, err := json.Marshal(Portable(s))
	if err != nil {
		t.Fatal(err)
	}
	z, a, m := bytes.Index(b, []byte(`"zeta"`)), bytes.Index(b, []byte(`"alpha":`)), bytes.Index(b, []byte(`"mid":`))
	if !(z < a && a < m) {
		t.Fatalf("properties not in required order: %s", b)
	}
}
```

```go
// eval_test.go
func TestEvaluationAsksForEvidenceFirst(t *testing.T) {
	b, _ := json.Marshal(llm.Portable(EvaluationSchema()))
	s := string(b)
	if !(strings.Index(s, `"sharpness"`) < strings.Index(s, `"exposure"`) &&
		strings.Index(s, `"focus_target"`) < strings.Index(s, `"status"`) &&
		strings.Index(s, `"status"`) < strings.Index(s, `"score"`)) {
		t.Fatalf("sharpness evidence must come before its status and score: %s", s)
	}
	p := SystemPrompt(0.6, Camera{})
	for _, band := range []string{"sharp 8-10", "acceptable 6-7.9", "soft 3-5.9", "missed_focus or motion_blur 0-2.9"} {
		if !strings.Contains(p, band) {
			t.Errorf("prompt lacks band %q", band)
		}
	}
}

func TestRankAsksForComparisonBeforeOrder(t *testing.T) {
	b, _ := json.Marshal(llm.Portable(RankSchema()))
	if s := string(b); strings.Index(s, `"summary"`) > strings.Index(s, `"ranking"`) {
		t.Fatalf("summary must precede ranking: %s", s)
	}
}
```

- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**

```go
// Schema is a JSON Schema that marshals each object's properties in the order of
// its "required" list. Structured output is generated in schema order, so this
// order is the order the model writes in: evidence fields listed first are written
// before the verdict they support. Go maps would otherwise marshal alphabetically.
type Schema map[string]any

func (s Schema) MarshalJSON() ([]byte, error) { return marshalOrdered(map[string]any(s)) }

func marshalOrdered(v any) ([]byte, error) {
	switch x := v.(type) {
	case Schema:
		return marshalOrdered(map[string]any(x))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b bytes.Buffer
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			var vb []byte
			var err error
			if props, ok := x[k].(map[string]any); ok && k == "properties" {
				vb, err = marshalProps(props, requiredOf(x))
			} else {
				vb, err = marshalOrdered(x[k])
			}
			if err != nil {
				return nil, err
			}
			b.Write(vb)
		}
		b.WriteByte('}')
		return b.Bytes(), nil
	case []any:
		var b bytes.Buffer
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			eb, err := marshalOrdered(e)
			if err != nil {
				return nil, err
			}
			b.Write(eb)
		}
		b.WriteByte(']')
		return b.Bytes(), nil
	}
	return json.Marshal(v)
}
```

`requiredOf` reads `x["required"]` as `[]string` or `[]any` of strings. `marshalProps` writes the required names in order, then any remaining keys sorted, each value via `marshalOrdered`. `Portable` returns `Schema(strip(schema).(map[string]any))`. Callers that take `map[string]any` keep working, since `Schema`'s underlying type is that map. Update their types only where the compiler asks.

Reorder `required` in the evaluation schema:
- sharpness: `focus_target, status, score`;
- exposure: `reason, clipping, status, ev_adjust, score`;
- composition: `issues, status, crop, straighten_degrees, score`.

The rank schema becomes `summary, ranking`. Update the rank prompt's last line: "First write the summary: how the frames differ, and why the best one wins. Then return every frame exactly once, best first."

Replace the evaluation prompt's score line with:

```
Write each section's evidence first (sharpness: focus_target; exposure: reason and clipping; composition: issues), then its status, then a score that fits the status. Sharpness bands: sharp 8-10, acceptable 6-7.9, soft 3-5.9, missed_focus or motion_blur 0-2.9. Other scores are 0-10: 5 = usable, 7 = good, 9+ = exceptional. Do not inflate. Keep text fields terse.
```

- [ ] **Step 4: Run** `go test ./internal/eval ./internal/llm` (unsandboxed for llm).
- [ ] **Step 5: CLAUDE.md "Unverified assumptions":** add "Structured output is generated in schema property order (the evidence-first ordering relies on it); not checked on a live call."
- [ ] **Step 6: Commit.** `Evidence before score: schemas keep their required order; sharpness score bands per status`

---

### Task 3: Cull only when status and score agree (item 2, policy half)

**Files:**
- Modify: `internal/eval/types.go` (`Policy.CullMaxSharpness`, `DecideFacts`)
- Modify: `internal/cli/policy.go` (`--cull-max-sharpness`, default 3; resolve; validate [0, 10])
- Test: `internal/eval/eval_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Produces: `Policy.CullMaxSharpness float64 \`json:"cull_max_sharpness"\``. A `missed_focus` or `motion_blur` frame culls only when its sharpness score is ≤ this; above it, it goes to review. 0 = off (the status alone culls, as in reports from before this field).

- [ ] **Step 1: Write the failing tests**

```go
func TestCullNeedsAgreeingScore(t *testing.T) {
	p := Policy{MinCropArea: 0.6, CullMaxSharpness: 3}
	e := &Evaluation{Sharpness: Sharpness{Status: "missed_focus", Score: 5}}
	if d, reasons := p.Decide(e); d != Review || !strings.Contains(strings.Join(reasons, ";"), "scored 5.0") {
		t.Fatalf("contradicting score: %s %v", d, reasons)
	}
	e.Sharpness.Score = 2
	if d, _ := p.Decide(e); d != Cull {
		t.Fatalf("agreeing score: %s", d)
	}
}

func TestOldStoredPolicyCullsOnStatusAlone(t *testing.T) {
	p := Policy{MinCropArea: 0.6} // CullMaxSharpness absent: 0
	if d, _ := p.Decide(&Evaluation{Sharpness: Sharpness{Status: "motion_blur", Score: 6}}); d != Cull {
		t.Fatalf("got %s", d)
	}
}
```

In the CLI, extend the existing policy-flag validation test with `--cull-max-sharpness 11` (an error). Add a check that the flag's default is 3.

- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.** In `DecideFacts`:

```go
	case "missed_focus", "motion_blur":
		if p.CullMaxSharpness > 0 && e.Sharpness.Score > p.CullMaxSharpness {
			// The status and the score disagree: the model's own score says it isn't
			// that bad. A cull needs both, so this one is only doubtful.
			raise(Review, fmt.Sprintf("sharpness: %s but scored %.1f (cull needs %.1f or less)", e.Sharpness.Status, e.Sharpness.Score, p.CullMaxSharpness))
		} else {
			raise(Cull, "sharpness: "+e.Sharpness.Status)
		}
```

Flag: `f.Float64Var(&pf.cullMaxSharpness, "cull-max-sharpness", 3, "cull missed_focus/motion_blur only when the sharpness score is at most this; above it, review (0 = the status alone culls)")`. Add it to `policy()`, `validPolicy` ([0, 10]) and `resolve`'s field list.
- [ ] **Step 4: Run** `go test ./internal/eval ./internal/pipeline` and, unsandboxed, `./internal/cli`.
- [ ] **Step 5: Docs.** README "How decisions are made" step 4 and the flag reference: `--cull-max-sharpness` (3).
- [ ] **Step 6: Commit.** `Policy: a sharpness cull needs the score to agree with the status (--cull-max-sharpness)`

---

### Task 4: Assessments that disagree go to review (item 2, escalation half)

**Files:**
- Modify: `internal/eval/types.go` (`Facts.Others`, a rule in `DecideFacts`)
- Modify: `internal/report/report.go` (`Result.Facts()` fills `Others` from `FirstPass` and, after Task 5, `Second`)
- Test: `internal/eval/eval_test.go`, `internal/pipeline/escalate_test.go`

**Interfaces:**
- Produces: `Facts.Others []string`: the sharpness statuses of the frame's other assessments (escalation's first pass, a second opinion).

- [ ] **Step 1: Write the failing tests**

```go
func TestDisagreeingAssessmentsGoToReview(t *testing.T) {
	p := Policy{MinCropArea: 0.6}
	keep := &Evaluation{Sharpness: Sharpness{Status: "sharp", Score: 8}}
	if d, r := p.DecideFacts(keep, Facts{Others: []string{"missed_focus"}}); d != Review || !strings.Contains(strings.Join(r, ";"), "disagree") {
		t.Fatalf("sharp vs missed_focus: %s %v", d, r)
	}
	cull := &Evaluation{Sharpness: Sharpness{Status: "missed_focus", Score: 1}}
	if d, _ := p.DecideFacts(cull, Facts{Others: []string{"acceptable"}}); d != Review {
		t.Fatalf("missed_focus vs acceptable: %s", d)
	}
}

func TestAgreeingAssessmentsStillCull(t *testing.T) {
	p := Policy{MinCropArea: 0.6}
	cull := &Evaluation{Sharpness: Sharpness{Status: "missed_focus", Score: 1}}
	if d, _ := p.DecideFacts(cull, Facts{Others: []string{"motion_blur"}}); d != Cull {
		t.Fatalf("got %s", d)
	}
}
```

In `escalate_test.go`, the existing test that asserts first-pass `missed_focus` → escalated `sharp` → keep changes to expect **review** with a "disagree" reason. Say so in the commit message: this is the intended behaviour change.

- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.** Classify statuses: `cullish` = missed_focus/motion_blur, `keepish` = sharp/acceptable (soft is neither). After the sharpness switch:

```go
	for _, o := range f.Others {
		if (cullish(e.Sharpness.Status) && !cullish(o)) || (keepish(e.Sharpness.Status) && cullish(o)) {
			// One assessment says the frame failed and another says it didn't: the
			// model is unsure, which is what review is for. A cull needs agreement.
			if d == Cull {
				d = Review
			}
			raise(Review, fmt.Sprintf("assessments disagree on sharpness: %s vs %s", e.Sharpness.Status, o))
			break
		}
	}
```

Lowering an existing Cull is deliberate and the only place a rule lowers a decision. Comment why: the cull came from the very status now contradicted. Place the loop *before* the raw-clip and eyes rules, so a later rule set to cull can still cull.

`report.Result.Facts()` sets `Others` from `r.FirstPass.Evaluation.Sharpness.Status` when present. It must still return the `RawKnown`/`RawClipPct` facts as today when `RawClip` is nil; restructure it so `Others` is filled in either case.
- [ ] **Step 4: Run** `go test ./internal/eval ./internal/pipeline ./internal/report ./internal/calib`.
- [ ] **Step 5: Commit.** `Policy: when assessments disagree on sharpness (escalation), review instead of trusting the last`

---

### Task 5: Opt-in second opinion on doubtful frames (item 4)

**Files:**
- Modify: `internal/report/report.go` (`Result.Second *FirstPass`; `Facts()` includes it)
- Modify: `internal/pipeline/pipeline.go` (`Config.SecondOpinion`; `processOne`)
- Modify: `internal/cli/cull.go` (`--second-opinion`; refused with `--batch`; estimate note)
- Test: `internal/pipeline/pipeline_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: Task 4's `Facts.Others`.
- Produces: `Config.SecondOpinion bool`; `report.Result.Second *report.FirstPass \`json:"second,omitempty"\`` (same backend and model, asked again).

- [ ] **Step 1: Write the failing tests**

```go
// A doubtful frame (soft or worse) is asked again; when the second answer says
// sharp, the frame goes to review rather than trusting either.
func TestSecondOpinionOnDoubtfulFrames(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	minimalDNG(t, filepath.Join(dir, "L1000002.DNG"))
	c := cfg(dir)
	c.WriteXMP, c.SecondOpinion = false, true
	b := &alternatingBackend{first: map[string]string{"L1000001": "missed_focus", "L1000002": "sharp"}, then: "sharp"}
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	r1, r2 := result(t, rep, "L1000001.DNG"), result(t, rep, "L1000002.DNG")
	if r1.Second == nil || r1.Decision != eval.Review || r2.Second != nil {
		t.Fatalf("r1 second=%v decision=%s; r2 second=%v", r1.Second, r1.Decision, r2.Second)
	}
	if r1.Usage.InputTokens != 2*r2.Usage.InputTokens {
		t.Fatalf("second opinion's usage not counted: %d vs %d", r1.Usage.InputTokens, r2.Usage.InputTokens)
	}
}

// alternatingBackend answers a file's first evaluation from first, later ones with then.
type alternatingBackend struct {
	mu    sync.Mutex
	first map[string]string
	then  string
	seen  map[string]bool
}

func (a *alternatingBackend) Name() string { return "fake" }
func (a *alternatingBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	a.mu.Lock()
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	status := a.then
	for name, s := range a.first {
		if strings.Contains(requestText(&req), name) && !a.seen[name] {
			status, a.seen[name] = s, true
		}
	}
	a.mu.Unlock()
	return (&fakeBackend{status: status}).Call(ctx, req)
}
```

This relies on the evaluate request text containing the file name. Check how `perFileBackend` finds it (`req.Parts[0].Text`) and match that.

CLI: add `"second-opinion with batch": {"judge", "--second-opinion", "--batch", dir}` to the flag-validation table.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.** In `processOne`, after escalation and before `finish`:

```go
	if cfg.SecondOpinion && doubtful(e) && stopErr == nil {
		// Same model, same inputs, asked again: its verdicts on borderline frames
		// vary between runs, and a cull should survive a second look.
		e2, u2, err := eval.Evaluate(ctx, b, in)
		res.Usage.Add(u2)
		switch {
		case e2 != nil && (err == nil || errors.Is(err, llm.ErrQuotaStop)):
			res.Second = &report.FirstPass{Backend: b.Name(), Model: cfg.Model, Evaluation: e2}
			if err != nil {
				stopErr = err
			}
		case errors.Is(err, llm.ErrAbortRun), errors.Is(err, llm.ErrQuotaStop):
			res.Fixups = append(res.Fixups, "second opinion failed: "+err.Error())
			stopErr = err
		case err != nil:
			res.Fixups = append(res.Fixups, "second opinion failed: "+err.Error())
		}
	}
```

`doubtful(e)` is sharpness status soft, missed_focus or motion_blur. The second evaluation is not sanitized: only its status is used. `Result.Facts()` appends `Second`'s status to `Others`.

In `cli/cull.go`:
- `--second-opinion`: "ask the model again about soft or worse frames (about 10–15% more calls); disagreement goes to review";
- `PreRunE` refuses it with `--batch` ("needs a synchronous run");
- when it's on, `printEstimate` adds a line: "second opinions: up to N more calls if every frame were doubtful; usually 10–15%".

Write the line without inventing a number: say "one more evaluation per soft-or-worse frame".
- [ ] **Step 4: Run** `go test ./internal/pipeline ./internal/report` and, unsandboxed, `./internal/cli`.
- [ ] **Step 5: Docs.** README judge flags and "How decisions are made": second opinions are opt-in.
- [ ] **Step 6: Commit.** `judge --second-opinion: ask again about soft-or-worse frames; disagreement goes to review`

---

### Task 6: Opt-in reversed-order ranking; trust only agreed places (item 5)

**Files:**
- Modify: `internal/report/report.go` (`Set.Reversed []string \`json:"reversed,omitempty"\``)
- Modify: `internal/pipeline/rank.go` (`Config.RankTwice`; round 1 adds `S<id>-R` for single-chunk sets; record `Reversed`)
- Modify: `internal/pipeline/groups.go` (`decideAll`: carry `Reversed` like `Order`; disputed places)
- Modify: `internal/pipeline/rankcalls.go` (`callsFor` counts the extra calls)
- Modify: `internal/eval/types.go` (`Policy.ApplyDisputed`)
- Modify: `internal/cli/cull.go`, `internal/cli/rank.go` (`--rank-twice`)
- Test: `internal/pipeline/rank_test.go`, `internal/pipeline/decide_test.go`

**Interfaces:**
- Produces: `Config.RankTwice bool`; `Set.Reversed`, the model's order when shown the same frames in reverse capture order, best first; `func (p Policy) ApplyDisputed(d Decision, reasons []string, a, b, of, set int) (Decision, []string)`; `callsFor(rep, todo, twice bool)`.

- [ ] **Step 1: Write the failing tests**

```go
// rank_test.go: with RankTwice a single-chunk set gets a second call with its
// frames reversed, and both orders are stored.
func TestRankTwiceSendsReversedCall(t *testing.T) {
	c, rep := seqShoot(t, 3)
	c.RankTwice = true
	b := &rankBackend{} // the file's existing fake that answers rank calls; reuse it
	if err := RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 1}, false); err != nil {
		t.Fatal(err)
	}
	s := rep.Sets[0]
	if len(s.Order) != 3 || len(s.Reversed) != 3 || b.calls != 2 {
		t.Fatalf("order %v reversed %v calls %d", s.Order, s.Reversed, b.calls)
	}
}
```

Use whatever fake rank backend `rank_test.go` already defines; read its top first. If it counts calls, assert `2`; otherwise add a counter.

```go
// decide_test.go
func TestDisputedRankOnlyDemotesKeeps(t *testing.T) {
	// A 3-frame set, KeepBest 1: frame A wins one order and B the reversed one.
	// Neither is "best"; both keeps go to review as disputed; C (last in both)
	// gets the outranked action.
}

func TestSetWithoutReversedOrderDecidesAsBefore(t *testing.T) {
	// The same set with Order only: A best and keep; B and C outranked → review.
}
```

Build both on `seqShoot(t, 3)`: decide once, write `Order`/`Reversed` and `By: "model"` into `rep.Sets[0]`, decide again, then assert `Group.Best` and decisions.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - **`rankWave`, round 1:** when `cfg.RankTwice && len(j.parts) == 1`, also send `j.call(fmt.Sprintf("S%d-R", id), reversed(j.parts[0]))`.
  - **Taking the answers:** take the reversed answer *first*, so the forward call's notes and summary win. Store its order in `j.reversed`.
  - **Failure:** if the reversed call fails, the set still ranks from the forward order, with no `Reversed`, and the log line notes it.
  - **Recording:** fill `s.Reversed` from `j.reversed` the same way `s.Order` is filled. Reset `s.Reversed = nil` when the set is ranked without it.
  - **`decideAll`:** carry `old.Reversed`, filtered to members, beside `Order`. Compute `rank2Of` when `s.By == "model"` and the reversed order covers every rankable member. Then per frame, with `k := p.KeepBest`:

```go
			agreeTop := rank2 == 0 || (g.Rank <= best) == (rank2 <= best)
			g.Best = g.Rank >= 1 && g.Rank <= best && (rank2 == 0 || rank2 <= best)
			switch {
			case p.KeepBest == 0:
			case !agreeTop && decisions[i] == eval.Keep:
				decisions[i], reasons[i] = p.ApplyDisputed(decisions[i], reasons[i], g.Rank, rank2, g.Of, g.ID)
			case g.Rank > p.KeepBest && (rank2 == 0 || rank2 > p.KeepBest) && decisions[i] == eval.Keep:
				decisions[i], reasons[i] = p.ApplyOutranked(decisions[i], reasons[i], g.Rank, g.Of, g.ID, s.By == "scores")
			}
```

  - **`ApplyDisputed`** raises to at most review, and only when `Outranked` acts. With `Outranked` ignore, a disputed frame is left alone. Reason: `rank %d of %d in set %d, %d with the frames reversed: disputed`.
  - **`callsFor`** adds one call per single-chunk set when `twice`. `RankCalls` passes `cfg.RankTwice`. The budget pre-check in `rankSets` uses it too.
  - **CLI:** `--rank-twice` on `judge` and `rank`: "rank each set of up to 8 a second time with its frames reversed; keep only frames both orders put in the best --keep-best (doubles ranking calls)". It sets `cfg.RankTwice`.
- [ ] **Step 4: Run** `go test ./internal/pipeline ./internal/eval` and, unsandboxed, `./internal/cli`.
- [ ] **Step 5: Docs.** README "Sequences and best of set": a `--rank-twice` paragraph. CLAUDE.md "Unverified": whether reversed-order agreement tracks ranking quality.
- [ ] **Step 6: Commit.** `--rank-twice: rank small sets again in reverse; only places both orders agree on count as best`

---

### Task 7: The right face, its angle, and one-pupil faces (items 6–7, detection half)

**Files:**
- Modify: `internal/focus/detect.go` (per-angle clustering keeps the angle; puploc at that angle; `Face.Pupils`; `eyePoint`)
- Modify: `internal/focus/focus.go` or wherever `Target` is defined (`Target.Pupils`), and `FaceTarget`
- Modify: `internal/pipeline/stages.go` (`faceTarget` picks the largest confident face)
- Test: `internal/focus/detect_test.go`, `internal/pipeline/pipeline_test.go`

**Interfaces:**
- Produces: `Face.Angle float64`, `Face.Pupils []image.Point` (native pixels, 0–2 points), and `Face.Eyes` (now the pupil itself when only one was found); `func eyePoint(pupils []image.Point) *image.Point`; `Target.Pupils []image.Point`.

- [ ] **Step 1: Write the failing tests**

```go
func TestOnePupilStillAims(t *testing.T) {
	if p := eyePoint([]image.Point{{100, 50}}); p == nil || *p != (image.Point{100, 50}) {
		t.Fatalf("got %v", p)
	}
	if p := eyePoint([]image.Point{{100, 50}, {140, 54}}); *p != (image.Point{120, 52}) {
		t.Fatalf("got %v", p)
	}
	if eyePoint(nil) != nil {
		t.Fatal("no pupils: no eye point")
	}
}
```

```go
// pipeline_test.go: of two confident faces, the larger is the subject: Q measures
// how frontal a face is, not how important.
func TestSubjectIsTheLargestConfidentFace(t *testing.T) {
	small := focus.Face{Rect: image.Rect(10, 10, 60, 60), Q: 200}
	large := focus.Face{Rect: image.Rect(300, 200, 600, 500), Q: 90}
	c := cfg(t.TempDir())
	c.FaceMinQ = 80
	c.detect = func(*imageprep.Frame) []focus.Face { return []focus.Face{small, large} }
	ft := &report.FocusTarget{}
	frame := &imageprep.Frame{W: 1600, H: 1067}
	if _, need := faceTarget(c, frame, ft); need || ft.Box == nil || ft.Box.Left < 0.18 || ft.FaceQ != 200 {
		t.Fatalf("box %+v faceQ %v", ft.Box, ft.FaceQ)
	}
}
```

The existing pigo sample test (`TestDetectFindsFaceInPigoSample`) must still pass. Extend it to assert `len(f.Pupils) >= 1` for its face if the sample has visible eyes. Check what it does first, and only add the assertion if pupils are found today (log them before deciding).
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - **Detection angles:** in `Detect`, for each angle, `ClusterDetections(RunCascade(cp, a), 0.2)` and tag the results with `a`. Merge across angles in Q-descending order, keeping a detection unless it overlaps an already kept one with IoU > 0.2. Compute the IoU on the square boxes from Row/Col/Scale.
  - **Pupils:** `eyes(det, img, scale, angle)` passes `angle` to `RunDetector` and returns the pupils found (0–2) in native pixels. `Face.Pupils` and `Face.Eyes = eyePoint(pupils)` are set from them, and `FaceTarget` copies `Pupils` into `Target.Pupils`.
  - **Subject face:** in `faceTarget`, choose the confident face with the largest `Rect` area, with ties going to higher Q. `ft.FaceQ` stays the best Q overall (for calibrating `--face-min-q`).
- [ ] **Step 4: Run** `go test ./internal/focus ./internal/pipeline`. Then, for free on the real sample:
  - run `cull scan -o $S/after.json photos` against main's `before.json`;
  - count frames whose `focus_target.box` moved;
  - check those frames' `--save-inputs` subject crops by eye, and record what changed in the commit message.
- [ ] **Step 5: Commit.** `Faces: subject is the largest confident face; pupils found at the face's angle; one pupil is enough to aim`

---

### Task 8: Advisory eye-detail measure (item 6, measure half)

**Files:**
- Modify: `internal/report/report.go` (`FocusTarget.EyeSharpness`, `FocusTarget.FaceSharpness`)
- Modify: `internal/focus/focus.go` (`EyeWindows(t Target) []image.Rectangle`)
- Modify: `internal/pipeline/stages.go` (`buildInput` fills them)
- Modify: `internal/cli/summary.go` (scan summary: how many faces had eyes measured)
- Test: `internal/focus/focus_test.go`, `internal/pipeline/pipeline_test.go`

**Interfaces:**
- Consumes: Task 7's `Target.Pupils`.
- Produces: `FocusTarget.EyeSharpness float64 \`json:"eye_sharpness,omitempty"\`` (the best Ratio over windows around each pupil) and `FaceSharpness \`json:"face_sharpness,omitempty"\`` (Ratio over the face box). These are advisory: no policy reads them until calibration shows they help (CLAUDE.md "Unverified").

- [ ] **Step 1: Write the failing tests**

```go
func TestEyeWindowsAroundPupils(t *testing.T) {
	tg := Target{Center: image.Pt(500, 400), Size: 200, Pupils: []image.Point{{460, 400}, {540, 400}}}
	ws := EyeWindows(tg, 1600, 1067)
	if len(ws) != 2 || ws[0].Dx() != 60 || !image.Pt(460, 400).In(ws[0]) {
		t.Fatalf("%v", ws)
	}
}
```

Pipeline: with a fake face that has pupils on `texturedDNG`, `FocusTarget.EyeSharpness > 0` and `FaceSharpness > 0`. Without pupils, both are 0 and omitted.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.** `EyeWindows` returns, per pupil, a square of side `0.3 × t.Size` (at least 32 px), clamped inside the frame. In `buildInput`, when `target != nil && len(target.Pupils) > 0`:
  - `ft.EyeSharpness` = the max of `focus.Ratio(..., w, noise)` over those windows, rounded to 3 places;
  - `ft.FaceSharpness` = `focus.Ratio` over the face box.

  Rebuild the face box from the target: centre ± Size/2, clamped. Scan summary: "eyes measured: N/M faces".
- [ ] **Step 4: Run** `go test ./internal/focus ./internal/pipeline` and `./internal/cli` unsandboxed. Then `scan` the real sample and record eye and face sharpness for the face frames in the commit message. That's data for later calibration, not a rule.
- [ ] **Step 5: Docs.** CLAUDE.md "Unverified": "eye_sharpness / face_sharpness (advisory) — whether a low eye-to-face ratio flags front/back focus; compare with labels before any rule uses it."
- [ ] **Step 6: Commit.** `Advisory eye-detail measure: fine detail at the pupils vs the whole face`

---

### Finish

- [ ] `make vet`, then `go test -count=1 ./...` unsandboxed.
- [ ] Mark section 3 items done in the backlog, with hashes.
- [ ] Whole-branch review, then the finishing menu.
