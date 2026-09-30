# Safety and Spend Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close every section-1 finding of the 2026-09-30 review. After this, `cull` can no longer:
- silently destroy a paid report;
- lose track of moved DNGs;
- touch other shoots in Capture One;
- run injected AppleScript;
- spend money it doesn't count or cap.

**Architecture:** These are small, independent fixes in existing packages; nothing new is introduced.
- **Report overwrite:** a guard in `pipeline.startRun`, with a `--fresh` opt-in on `scan` and `judge`.
- **Moves:** made crash-safe from both ends.
  - Save the report before moving.
  - `relocate` refuses to replace an existing file, using `link(2)`.
  - `reconcileMove` adopts moves (or restores) that happened without a saved record, since a move's destination is deterministic.
- **Capture One:** matches on the image path and falls back only to a unique name.
- **Cost:** priced models fail closed; usage survives failed attempts, resumes and batch retries; batch ranking is checked against `--max-cost` before submitting.

**Tech Stack:** Go 1.22+, stdlib + cobra. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-30-repo-review-backlog.md`, section 1 (items 1.1–1.9).

## Global Constraints

- Never modify or delete DNGs. Only `--move-culled` moves them, never overwriting; `restore` undoes it.
- Never overwrite an existing `.xmp` unless `--overwrite-xmp`.
- Dependencies: stdlib, cobra, and pigo `core` only. No `golang.org/x/sys`.
- Tests use synthetic fixtures only (`minimalDNG`, `t.TempDir()`); no network or API key. Tests that bind loopback need `sandbox.network.allowLocalBinding: true`.
- Match surrounding style: comments explain *why*; error messages name the file and the next command to run.
- Prices come from the claude-api skill (cached 2026-09-25): `claude-sonnet-5-5` is $2 in / $10 out per MTok, the same as `claude-sonnet-5`.
- One commit per task, on branch `fix/safety-and-spend`.

## Review Focus

- **A folder that was only scanned:** re-running `scan` or `judge` without `--resume` must still just work, since there's nothing paid to lose. *Task 1: `TestFreshRunOverwritesScanReport`.*
- **`--fresh` while frames sit in `culled/`:** must refuse and point at `cull restore`, because the moves would otherwise be forgotten. *Task 1: `TestFreshRefusedWhileFramesAreMoved`.*
- **A move interrupted before its report save:** a later `restore` must bring the frame back, and a later `decide --move-culled` must not log "source: not exist" forever. *Task 2: `TestRestoreAdoptsUnrecordedMove`, `TestMoveCulledAdoptsUnrecordedMove`.*
- **Two images in the Capture One document with the report's file name but different paths:** neither gets touched; both are listed as ambiguous. *Task 3: `TestScriptMatchesByPathAndListsAmbiguousNames`.*
- **An unknown Anthropic model with `--max-cost`:** refused before any call; without `--max-cost`, a warning only. *Task 4: `TestJudgeRefusesMaxCostForUnpricedModel`.*

---

### Task 1: Refuse to overwrite a report that holds paid work (1.1)

**Files:**
- Modify: `internal/pipeline/pipeline.go` (Config: add `Fresh`; `startRun`)
- Modify: `internal/report/report.go` (add `(*Report).PaidWork()`)
- Modify: `internal/cli/cull.go`, `internal/cli/scan.go` (`--fresh` flag)
- Test: `internal/pipeline/pipeline_test.go`

**Interfaces:**
- Produces: `pipeline.Config.Fresh bool`; `func (r *Report) PaidWork() (evaluated, moved int)`.

- [ ] **Step 1: Write the failing tests** (`pipeline_test.go`)

```go
func TestRerunWithoutResumeRefusesPaidReport(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	if _, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "r.json"))
	for _, dry := range []bool{false, true} { // judge, then scan
		c := cfg(dir)
		c.DryRun = dry
		_, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
		if err == nil || !strings.Contains(err.Error(), "--resume") || !strings.Contains(err.Error(), "--fresh") {
			t.Fatalf("dry=%v: want a refusal naming --resume and --fresh, got %v", dry, err)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "r.json")); !bytes.Equal(before, after) {
		t.Fatal("report changed on disk")
	}
	c := cfg(dir)
	c.Fresh = true
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatalf("--fresh: %v", err)
	}
}

func TestFreshRunOverwritesScanReport(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	c := cfg(dir)
	c.DryRun = true
	for i := 0; i < 2; i++ { // scan twice: nothing paid, no refusal
		if _, _, err := Run(context.Background(), c, nil); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	if _, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatalf("judge over a scan report: %v", err)
	}
}

func TestFreshRefusedWhileFramesAreMoved(t *testing.T) {
	dir, b := shoot(t)
	if _, _, err := Run(context.Background(), moveCfg(dir), b); err != nil {
		t.Fatal(err)
	}
	c := moveCfg(dir)
	c.Fresh = true
	_, _, err := Run(context.Background(), c, b)
	if err == nil || !strings.Contains(err.Error(), "cull restore") {
		t.Fatalf("want a refusal naming cull restore, got %v", err)
	}
}
```

- [ ] **Step 2: Run them and check they fail**

Run: `go test ./internal/pipeline -run 'TestRerunWithoutResume|TestFreshRun|TestFreshRefused' -v`
Expected: FAIL. `c.Fresh` is undefined; once it compiles, the first run is not refused.

- [ ] **Step 3: Implement**

In `report.go`:

```go
// PaidWork counts what a report holds that a fresh run would lose: frames with a
// model assessment (paid for) and frames moved into culled/ (only the report
// knows where they came from).
func (r *Report) PaidWork() (evaluated, moved int) {
	for _, x := range r.Results {
		if x.Evaluation != nil {
			evaluated++
		}
		if x.MovedTo != "" {
			moved++
		}
	}
	return evaluated, moved
}
```

In `pipeline.go`, add to `Config` after `Resume`:

```go
	Fresh          bool // replace a report holding assessments; refused while it records moved frames
```

In `startRun`, before building `rep`:

```go
	if !cfg.Resume {
		if err := guardOverwrite(cfg); err != nil {
			return nil, nil, err
		}
	}
```

And add:

```go
// guardOverwrite refuses a run that would replace a report holding paid
// assessments (unless Fresh) or recording moved frames (always: restore first,
// or those frames can't be found again). A scan-only report is free to replace.
func guardOverwrite(cfg *Config) error {
	prev, err := report.Load(cfg.ReportPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s exists but can't be read (%v): move it aside, or use -o for a separate report", cfg.ReportPath, err)
	}
	evaluated, moved := prev.PaidWork()
	if moved > 0 {
		return fmt.Errorf("%s records %d frame(s) moved into %s/: run `cull restore %s` first, or use --resume or -o", cfg.ReportPath, moved, CulledDir, cfg.Dir)
	}
	if evaluated > 0 && !cfg.Fresh {
		return fmt.Errorf("%s holds %d assessed frame(s) ($%.2f): use --resume to continue it, --fresh to replace it, or -o for a separate report", cfg.ReportPath, evaluated, prev.Cost())
	}
	return nil
}
```

In `cli/cull.go`: add `fresh bool` to `cullOpts` and
`f.BoolVar(&o.fresh, "fresh", false, "replace an existing report that holds assessments (default: refuse; see --resume)")`.
Set `cfg.Fresh = o.fresh`, and reject `--fresh` together with `--resume` in `PreRunE`:
`if o.fresh && o.resume { return fmt.Errorf("--fresh and --resume contradict each other") }`.
In `cli/scan.go`: the same flag, with `cfg.Fresh = fresh`.

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/pipeline ./internal/cli`
Expected: the new tests pass. Existing tests that re-run on the same folder without `Resume` now fail; set `c.Fresh = true` in those, and only those.

- [ ] **Step 5: Docs.** In README "Cost control" and WORKFLOW "Interrupted", add: a re-run refuses to replace a report holding assessments; use `--resume`, or `--fresh` to start over.

- [ ] **Step 6: Commit**

```bash
git add internal/pipeline internal/report internal/cli README.md WORKFLOW.md
git commit -m "Refuse to overwrite a report holding paid assessments or moves; add --fresh"
```

---

### Task 2: Moves that can't replace a file or be forgotten (1.2, 1.3, 2.10)

**Files:**
- Modify: `internal/pipeline/move.go` (`relocate`, `moveCulled`, `Restore`, new `reconcileMove`)
- Modify: `internal/pipeline/pipeline.go` (`finishRun`: save before moving)
- Modify: `internal/pipeline/decide.go` (`redecide`: reconcile before restore/move)
- Test: `internal/pipeline/move_test.go`

**Interfaces:**
- Produces: `func reconcileMove(r *report.Result) bool` (true when it changed `r`); `func renameNoReplace(src, dst string) error`.

- [ ] **Step 1: Write the failing tests** (`move_test.go`)

```go
// A move whose report save never happened: the frame is in culled/, the report
// says it isn't. restore must still bring it back.
func TestRestoreAdoptsUnrecordedMove(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled = false
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	culled := filepath.Join(dir, "culled")
	os.MkdirAll(culled, 0o755)
	for _, n := range []string{"L1000001.DNG", "L1000001.xmp"} { // the move, without the save
		if err := os.Rename(filepath.Join(dir, n), filepath.Join(culled, n)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Restore(filepath.Join(dir, "r.json"), io.Discard)
	if err != nil || n != 1 {
		t.Fatalf("restored %d, err %v", n, err)
	}
	if !exists(filepath.Join(dir, "L1000001.DNG")) || !exists(filepath.Join(dir, "L1000001.xmp")) {
		t.Fatal("frame or sidecar not restored")
	}
}

func TestMoveCulledAdoptsUnrecordedMove(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled = false
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	culled := filepath.Join(dir, "culled")
	os.MkdirAll(culled, 0o755)
	os.Rename(filepath.Join(dir, "L1000001.DNG"), filepath.Join(culled, "L1000001.DNG"))
	sum, err := Decide(context.Background(), filepath.Join(dir, "r.json"), DecideOptions{MoveCulled: true, Policy: eval.Policy{MinCropArea: 0.6}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := report.Load(filepath.Join(dir, "r.json"))
	r := result(t, rep, "L1000001.DNG")
	if r.MovedTo != filepath.Join(culled, "L1000001.DNG") || len(r.Fixups) != 0 || sum.Moved != 0 {
		t.Fatalf("moved_to=%q fixups=%v moved=%d", r.MovedTo, r.Fixups, sum.Moved)
	}
}

// A restore whose report save never happened: the frame is back, the report
// still says culled/. The next restore must not fail on it.
func TestRestoreForgetsAlreadyRestoredMove(t *testing.T) {
	dir, b := shoot(t)
	if _, _, err := Run(context.Background(), moveCfg(dir), b); err != nil {
		t.Fatal(err)
	}
	culled := filepath.Join(dir, "culled")
	os.Rename(filepath.Join(culled, "L1000001.DNG"), filepath.Join(dir, "L1000001.DNG"))
	os.Rename(filepath.Join(culled, "L1000001.xmp"), filepath.Join(dir, "L1000001.xmp"))
	var log bytes.Buffer
	if _, err := Restore(filepath.Join(dir, "r.json"), &log); err != nil {
		t.Fatal(err)
	}
	rep, _ := report.Load(filepath.Join(dir, "r.json"))
	if r := result(t, rep, "L1000001.DNG"); r.MovedTo != "" || r.XMP != filepath.Join(dir, "L1000001.xmp") || strings.Contains(log.String(), "not restored") {
		t.Fatalf("moved_to=%q xmp=%q log=%s", r.MovedTo, r.XMP, log.String())
	}
}

func TestRenameNoReplaceRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(src, []byte("src"), 0o644)
	os.WriteFile(dst, []byte("dst"), 0o644)
	if err := renameNoReplace(src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("want ErrExist, got %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "dst" || !exists(src) {
		t.Fatal("destination replaced or source lost")
	}
	os.Remove(dst)
	if err := renameNoReplace(src, dst); err != nil || exists(src) || !exists(dst) {
		t.Fatalf("plain move: err=%v", err)
	}
}

func TestMoveFixupRecordedOnce(t *testing.T) {
	dir, b := shoot(t)
	culled := filepath.Join(dir, "culled")
	os.MkdirAll(culled, 0o755)
	os.WriteFile(filepath.Join(culled, "L1000001.DNG"), []byte("already here"), 0o644)
	if _, _, err := Run(context.Background(), moveCfg(dir), b); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		Decide(context.Background(), filepath.Join(dir, "r.json"), DecideOptions{MoveCulled: true, Policy: eval.Policy{MinCropArea: 0.6}}, io.Discard)
	}
	rep, _ := report.Load(filepath.Join(dir, "r.json"))
	n := 0
	for _, f := range result(t, rep, "L1000001.DNG").Fixups {
		if strings.HasPrefix(f, "move:") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d move fixups, want 1", n)
	}
}
```

(Add `errors` and `io/fs` to the test imports.)

- [ ] **Step 2: Run them and check they fail**

Run: `go test ./internal/pipeline -run 'Adopts|Forgets|RenameNoReplace|FixupRecordedOnce' -v`
Expected: FAIL. `renameNoReplace` is undefined; once stubbed, restore returns 0 and the fixups pile up.

- [ ] **Step 3: Implement `renameNoReplace`, and use it in `relocate`**

```go
// renameNoReplace moves src to dst and fails with fs.ErrExist if dst exists,
// atomically where the filesystem has hard links: link(2) refuses an existing
// name, while rename(2) silently replaces one. Filesystems without hard links
// (exFAT cards) fall back to check-then-rename. Across disks both fail: this
// never copies.
func renameNoReplace(src, dst string) error {
	err := os.Link(src, dst)
	switch {
	case err == nil:
		return os.Remove(src)
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%s already exists, not moved: %w", dst, fs.ErrExist)
	}
	if _, serr := os.Lstat(dst); serr == nil {
		return fmt.Errorf("%s already exists, not moved: %w", dst, fs.ErrExist)
	}
	return os.Rename(src, dst)
}
```

In `relocate`, replace the two `os.Rename` calls that move *into* place (`src→dst`, `srcXMP→dstXMP`) with `renameNoReplace`. Keep the pre-checks, so the pair moves together or not at all. Keep the roll-back `os.Rename(dst, src)`: `src` was just vacated.

- [ ] **Step 4: Implement `reconcileMove`, and call it**

```go
// reconcileMove repairs a result whose files moved without the report being
// saved (a crash or a failed save between the renames and the save). A frame's
// place in culled/ is fixed (CulledDir beside it), so where exactly one of the
// two paths exists, that is where it is. Returns whether r changed.
func reconcileMove(r *report.Result) bool {
	if r.Error != "" {
		return false
	}
	culled := filepath.Join(filepath.Dir(r.File), CulledDir, filepath.Base(r.File))
	from, to := r.File, culled // unrecorded move
	if r.MovedTo != "" {
		from, to = r.MovedTo, r.File // unrecorded restore
	}
	if exists(from) || !exists(to) {
		return false
	}
	if r.XMP == xmp.Path(from) && !exists(xmp.Path(from)) && exists(xmp.Path(to)) {
		r.XMP = xmp.Path(to)
	}
	if r.MovedTo == "" {
		r.MovedTo = to
	} else {
		r.MovedTo = ""
	}
	return true
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }
```

(Delete the test file's own `exists` helper, which the package-level one replaces.)

Call it:
- In `Restore`: at the top of the loop, for every result, before the `MovedTo == ""` check. A result it clears (unrecorded restore) counts as restored, so do nothing more with it.
- In `moveCulled`: at the top of the loop. A result that becomes `MovedTo != ""` is then skipped by the existing check.
- In `redecide`: once for every result, right after `rep.SchemaVersion = …`, before the restore and move blocks.

Change `r.Fixups = append(r.Fixups, "move: "+err.Error())` in `moveCulled` to `addFixup(r, "move: "+err.Error())`.

- [ ] **Step 5: Save before moving, in `finishRun`**

Replace the move block and the save with:

```go
	rep.Generated = time.Now()
	if cfg.MoveCulled && !cfg.DryRun {
		// Save first: every result is on disk before any frame moves, so a crash
		// mid-move leaves nothing reconcileMove can't find again.
		if err := rep.Save(cfg.ReportPath); err != nil {
			return used, err
		}
		// After a quota stop or Ctrl-C too: those decisions are final.
		if n := moveCulled(rep, lab, cfg.Log); n > 0 {
			fmt.Fprintf(cfg.Log, "moved %d culled frame(s) into %s/ (undo: cull restore %s)\n", n, CulledDir, cfg.Dir)
		}
	}
	if err := rep.Save(cfg.ReportPath); err != nil {
```

(`Decide` needs no pre-save: its report on disk already holds every result, and `reconcileMove` covers the rest.)

- [ ] **Step 6: Run the package tests**

Run: `go test ./internal/pipeline`
Expected: PASS, including the existing `TestMoveCulledNeverOverwrites`, `TestMoveCulledLeavesFileWhenFolderCannotBeMade` and the restore tests.

- [ ] **Step 7: Commit**

```bash
git add internal/pipeline
git commit -m "Moves: never replace a file (link-based), save before moving, reconcile unrecorded moves"
```

---

### Task 3: apply-c1 matches by path; no injection through file names (1.4, 1.5)

**Files:**
- Modify: `internal/c1/c1.go` (`Script`, `helpers`, `quote`, new `comment`)
- Test: `internal/c1/c1_test.go`

**Interfaces:**
- Produces: `func comment(s string) string`. `matchImages(doc, posixPath, fileName, stem)` becomes a 4-argument AppleScript handler.

- [ ] **Step 1: Write the failing tests** (`c1_test.go`)

```go
func TestScriptNameCannotEscapeComment(t *testing.T) {
	rep := &report.Report{Results: []report.Result{{File: "/s/x\ndo shell script \"touch /tmp/pwned\"\n\r¬.DNG", Decision: eval.Keep}}}
	s := Script(rep, Options{Label: true})
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "do shell script") {
			t.Fatalf("file name escaped into code: %q", line)
		}
	}
	if strings.ContainsAny(s, "\r") {
		t.Fatal("raw CR in script")
	}
}

func TestScriptMatchesByPathAndListsAmbiguousNames(t *testing.T) {
	rep := &report.Report{Results: []report.Result{
		{File: "/s/L1.DNG", Decision: eval.Keep},
		{File: "/s/L2.DNG", Decision: eval.Cull, MovedTo: "/s/culled/L2.DNG"},
	}}
	s := Script(rep, Options{Label: true})
	if !strings.Contains(s, `matchImages(doc, "/s/L1.DNG", "L1.DNG", "L1")`) {
		t.Errorf("L1 not matched by its path:\n%s", block(s, "L1.DNG"))
	}
	if !strings.Contains(s, `matchImages(doc, "/s/culled/L2.DNG", "L2.DNG", "L2")`) {
		t.Errorf("moved L2 not matched where it lives now:\n%s", block(s, "L2.DNG"))
	}
	for _, want := range []string{"whose path is posixPath", "(count of found) is 1", "ambiguous"} {
		if !strings.Contains(s, want) {
			t.Errorf("helpers lack %q", want)
		}
	}
}

func TestQuoteEscapesControlCharacters(t *testing.T) {
	if got := quote("a\"b\\c\nd\re\tf"); got != `"a\"b\\c\nd\re\tf"` {
		t.Fatalf("got %s", got)
	}
}
```

Update the existing expectation in `TestScriptColorsAndKeywordsWithoutLabels` to the new 4-argument call:
`matchImages(doc, "/s/L\"3\\.DNG", "L\"3\\.DNG", "L\"3\\")`.

- [ ] **Step 2: Run them and check they fail**

Run: `go test ./internal/c1 -v`
Expected: FAIL on all three new tests.

- [ ] **Step 3: Implement**

```go
// comment makes s safe inside an AppleScript "--" comment: any line break would
// end the comment and run the rest as code, and ¬ would continue it onto the
// next line. File names can contain both.
func comment(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '¬' || r == ' ' || r == ' ' {
			return '?'
		}
		return r
	}, s)
}

// quote makes an AppleScript string literal.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}
```

In `Script`:

```go
		where := r.File
		if r.MovedTo != "" {
			where = r.MovedTo
		}
		fmt.Fprintf(&b, "\n\t-- %s: %s (%s)\n", comment(name), d, who)
		fmt.Fprintf(&b, "\tset imgs to my matchImages(doc, %s, %s, %s)\n", quote(where), quote(name), quote(stem))
		fmt.Fprintf(&b, "\tif (count of imgs) is 0 then set end of notFound to %s\n", quote(name))
```

Replace `matchImages` in `helpers`, and collect the ambiguous names:

```applescript
on matchImages(doc, posixPath, fileName, stem)
	tell application "Capture One"
		try
			set found to (every image of doc whose path is posixPath)
			if (count of found) > 0 then return found
		end try
		-- By name only when exactly one image has it: file numbers repeat across
		-- shoots and bodies, and a verdict must never land on another shoot's frame.
		set found to (every image of doc whose name is fileName)
		if (count of found) is 0 then set found to (every image of doc whose name is stem)
		if (count of found) is 1 then return found
		if (count of found) > 1 then set end of my ambiguous to fileName
		return {}
	end tell
end matchImages
```

Add `property ambiguous : {}` at the top of `helpers`, and a closing line before `return "done"`:

```go
	b.WriteString("\n\tif (count of my ambiguous) > 0 then return \"not applied: several images share these names and none is at the report's path (ambiguous): \" & my joinList(my ambiguous) & \"; not found: \" & my joinList(notFound)\n")
```

The `(count of found) is 1` string is in the helper; the test checks for it. Also update the package doc: images are matched by path, and by name only when the name is unique.

- [ ] **Step 4: Check that the script compiles, where possible**

Run: `go test ./internal/c1 && go run ./cmd/cull apply-c1 <a test dir with a report> > $TMPDIR/t.applescript && osacompile -o $TMPDIR/t.scpt $TMPDIR/t.applescript`
Expected: tests pass. `osacompile` may be blocked by the sandbox. If so, note it in the commit message and leave the compile check to the user; it needs no Capture One running.

- [ ] **Step 5: Commit**

```bash
git add internal/c1
git commit -m "apply-c1: match images by path, by name only when unique; keep file names out of code"
```

---

### Task 4: Current Sonnet priced and default; unpriced models fail closed (1.6)

**Files:**
- Modify: `internal/llm/pricing.go` (price table)
- Modify: `internal/cli/backend.go` (default model and help text; new `checkPriced`)
- Modify: `internal/cli/cull.go`, `internal/cli/rank.go` (call `checkPriced`)
- Modify: `internal/llm/anthropic.go:19`, `pricing.go:8` comments; `README.md` backends table
- Test: `internal/llm/pricing_test.go` (create if missing), `internal/cli/cli_test.go`

**Interfaces:**
- Produces: `func checkPriced(cmd *cobra.Command, backend, model string, maxCost float64) error`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/llm/pricing_test.go
func TestCurrentModelsArePriced(t *testing.T) {
	for _, m := range []string{"claude-sonnet-5-5", "claude-sonnet-5", "claude-opus-5-5", "claude-haiku-4-5", "claude-haiku-4-5-20251001", "claude-fable-5-1"} {
		if _, ok := PriceFor("anthropic", m); !ok {
			t.Errorf("%s unpriced", m)
		}
	}
	if p, _ := PriceFor("anthropic", "claude-sonnet-5-5"); p != (Price{2, 10}) {
		t.Errorf("sonnet 5.5 = %+v", p)
	}
}
```

In `cli_test.go`, following the file's existing pattern for running a command with arguments and capturing its error:

```go
func TestJudgeRefusesMaxCostForUnpricedModel(t *testing.T) {
	dir := t.TempDir()
	// no DNGs needed: the refusal comes before discovery or any key lookup
	err := runCLI(t, "judge", "--model", "claude-unknown-9", "--max-cost", "5", dir)
	if err == nil || !strings.Contains(err.Error(), "no price") {
		t.Fatalf("got %v", err)
	}
}
```

(Use whatever helper `cli_test.go` already uses to execute the root command. Read the top of that file first and match it.)

- [ ] **Step 2: Run them and check they fail**

Run: `go test ./internal/llm ./internal/cli -run 'Priced|Unpriced' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `pricing.go`, add `"claude-sonnet-5-5": {2, 10}` and `"claude-haiku-4-5-20251001": {1, 5}`, and update the cache-date comment to `(claude-api skill, cached 2026-09-25)`.

In `backend.go`, set the default to `"claude-sonnet-5-5"` in `backendDefaults` and in the `--model` help. Then add:

```go
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
	warn(cmd, []string{fmt.Sprintf("model %q has no price in cull's table: costs will be recorded as $0", model)})
	return nil
}
```

Call it in `judge`'s `RunE` right after the model default is filled (`o.model = backendDefaults…`), before `PriceFor`. Call it in `rank`'s `RunE` at the equivalent point, after its backend and model are resolved from the report or flags.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/llm ./internal/cli`
Expected: PASS. Existing tests that assert the `claude-sonnet-5` default (grep `claude-sonnet-5"`) are updated to `claude-sonnet-5-5`, except the ones that construct a report with an explicit model.

- [ ] **Step 5: Docs.** In the README backends table, set the default to `claude-sonnet-5-5`. In CLAUDE.md "Verified facts", add one line saying that live runs so far used `claude-sonnet-5` and the default is now 5.5, so re-calibrate. `--resume` on an older report needs `--model claude-sonnet-5`.

- [ ] **Step 6: Commit**

```bash
git add internal/llm internal/cli README.md CLAUDE.md
git commit -m "Price claude-sonnet-5-5 and make it the default; --max-cost fails closed on unpriced models"
```

---

### Task 5: Spend that isn't lost (1.7, 1.8)

**Files:**
- Modify: `internal/llm/llm.go` (`validated`)
- Modify: `internal/report/report.go` (`DiscardedCostUSD`, `Cost`)
- Modify: `internal/pipeline/pipeline.go` (`startRun` carries the discarded cost)
- Modify: `internal/pipeline/batch.go` (round 2 keeps the locate usage on prepare failure)
- Test: `internal/llm/validate_test.go`, `internal/pipeline/pipeline_test.go`, `internal/pipeline/batch_test.go`

**Interfaces:**
- Produces: `report.Report.DiscardedCostUSD float64` (`json:"discarded_cost_usd,omitempty"`), included in `Cost()`. `validated` returns a non-nil `*Response` whose `Usage` totals every attempt, on every error path.

- [ ] **Step 1: Write the failing tests**

```go
// validate_test.go
func TestValidatedKeepsUsageWhenBothAttemptsFail(t *testing.T) {
	resp, err := validated(context.Background(), testSchema, func(context.Context) (*Response, error) {
		return &Response{JSON: json.RawMessage(`{"wrong":1}`), Usage: Usage{InputTokens: 10, OutputTokens: 2}}, nil
	})
	if err == nil || resp == nil || resp.Usage.InputTokens != 20 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}
```

```go
// pipeline_test.go
func TestResumeKeepsCostOfDiscardedResults(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.Concurrency, c.WriteXMP = 1, false
	c.Price = &llm.Price{In: 10_000} // $1 per call of 100 input tokens
	// the second call fails after being billed: its usage rides on the error
	if _, _, err := Run(context.Background(), c, &billedFailBackend{failOn: 2}); err != nil {
		t.Fatal(err)
	}
	c.Resume = true
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Cost(); got != 5 { // 4 frames + 1 discarded failure
		t.Fatalf("cost %v, want 5", got)
	}
}

// billedFailBackend answers like fakeBackend but fails call failOn with usage attached.
type billedFailBackend struct{ n, failOn int32 }

func (b *billedFailBackend) Name() string { return "fake" }
func (b *billedFailBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if atomic.AddInt32(&b.n, 1) == b.failOn {
		return &llm.Response{Usage: llm.Usage{InputTokens: 100}}, errors.New("model output does not match schema")
	}
	return (&fakeBackend{status: "sharp"}).Call(ctx, req)
}
```

Check that `eval.Evaluate` passes `resp.Usage` through on error (it does: `evaluate.go:66`) and that `processOne` adds it to `res.Usage` before returning the error (`pipeline.go:255`). If `res.CostUSD` isn't set on the error path, set it there.

For the batch round 2, in `batch_test.go`, follow the existing round-2 test setup: a located frame whose DNG is deleted between rounds, so `prepareFrame` fails. Assert that its result's `Usage` still carries round 1's locate tokens.

- [ ] **Step 2: Run them and check they fail**

Run: `go test ./internal/llm ./internal/pipeline -run 'KeepsUsage|DiscardedResults|Round2' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `validated`, change the two usage-dropping returns:

```go
		if err != nil && !errors.Is(err, ErrQuotaStop) {
			if resp == nil {
				resp = &Response{}
			}
			resp.Usage = total
			return resp, err
		}
		if resp == nil {
			return &Response{Usage: total}, fmt.Errorf("backend returned no response")
		}
		if verr := Validate(schema, resp.JSON); verr != nil {
			if err != nil { // quota stop: don't spend another call, and keep the stop signal
				return &Response{Usage: total}, fmt.Errorf("%w; also, model output does not match schema: %v", err, verr)
			}
			lastErr = verr
			continue
		}
		resp.Usage = total
		return resp, err
	}
	return &Response{Usage: total}, fmt.Errorf("model output does not match schema: %w", lastErr)
```

Callers already use `resp.Usage` when `resp != nil` on error. Check `eval/rank.go` does too, and add the same `if resp != nil` usage pass-through there if it doesn't.

In `report.go`, add to `Report` after `RankCostUSD`:

```go
	// DiscardedCostUSD is what calls for results later discarded cost: an errored
	// frame is re-run on --resume and its old result dropped, but its calls were paid.
	DiscardedCostUSD float64 `json:"discarded_cost_usd,omitempty"`
```

And in `Cost()`: `return c + r.RankCostUSD + r.DiscardedCostUSD`.

In `startRun`'s resume branch, carry `rep.DiscardedCostUSD = prev.DiscardedCostUSD`, then for every `prev.Results` entry that is *not* kept, add `r.CostUSD` to it.

In `batch.go` round 2, after `reqs = prepareRound(...)`, for each `id, prev := range prevs`: if `f := st.Frames[id]; f != nil && f.Stage == "error" && f.Result.Usage == (llm.Usage{})`, set `f.Result.Usage = prev.Result.Usage`. Then check how an error-stage frame's `CostUSD` is priced when it is collected, and price it the same way as the other frames.

- [ ] **Step 4: Run all tests**

Run: `go test ./...` (loopback-binding failures in `cli`/`llm` are the sandbox; run those two packages unsandboxed to confirm)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/llm internal/report internal/pipeline
git commit -m "Count spend from failed attempts, discarded results and batch round-2 prepare failures"
```

---

### Task 6: Batch ranking respects --max-cost (1.9)

**Files:**
- Modify: `internal/pipeline/rank.go` (`rankSets`: pre-check before a batch wave)
- Modify: `internal/pipeline/rankcalls.go` (extract `callsFor(rep, todo) int`)
- Test: `internal/pipeline/rank_batch_test.go`

**Interfaces:**
- Consumes: `llm.EstimateRank(calls int, p Price, batch bool)`, `chunks(n int)`.
- Produces: `func callsFor(rep *report.Report, todo []int) int`, used by both `RankCalls` and `rankSets`.

- [ ] **Step 1: Write the failing test** (`rank_batch_test.go`, reusing that file's fake `BatchClient` and set fixtures)

```go
func TestRankBatchRefusesEstimateOverBudgetBeforeSubmitting(t *testing.T) {
	// A report with one unranked set of 3 (use the file's existing fixture builder),
	// Price $10/MTok in, so one estimated call (10k in) costs $0.05 at batch price.
	// MaxCost $0.01, no state file: must refuse with ErrBudget and submit nothing.
	// Then with a recorded state file (a re-attach), the same call must not refuse.
}
```

Fill the body from the fixtures `TestRankBatchBudget` already uses in that file; assert `errors.Is(err, llm.ErrBudget)` and zero `Create` calls on the fake client.

- [ ] **Step 2: Run it and check it fails**

Run: `go test ./internal/pipeline -run TestRankBatchRefusesEstimate -v`
Expected: FAIL (the batch is submitted).

- [ ] **Step 3: Implement**

In `rankcalls.go`, extract the counting loop:

```go
// callsFor is the number of rank calls ranking todo takes: its chunks, plus a
// final merging call for a set split into more than one chunk.
func callsFor(rep *report.Report, todo []int) int {
	calls := 0
	for _, i := range todo {
		parts := chunks(rep.Sets[i].Of)
		calls += len(parts)
		if len(parts) > 1 {
			calls++
		}
	}
	return calls
}
```

(`RankCalls` uses it.)

In `rankSets`, before the loop:

```go
	if ex.batch() && len(todo) > 0 && cfg.MaxCost > 0 && cfg.Price != nil && !fileExists(ex.statePath()) {
		// One batch holds every set and can't be stopped midway without losing it,
		// so check up front, as RunBatch does for judging. A re-attach is exempt:
		// what it collects is already paid for.
		usd, _, _ := llm.EstimateRank(callsFor(rep, todo), *cfg.Price, true)
		if over, spent := budget.add(0, 0); spent+usd > cfg.MaxCost || over {
			return total, run, fmt.Errorf("%w: ranking estimated at $%.2f with $%.2f already spent exceeds --max-cost $%.2f; %d set(s) left by scores",
				llm.ErrBudget, usd, spent, cfg.MaxCost, len(todo))
		}
	}
```

Add `statePath() string` to the `rankExec` interface: `syncExec` returns `""` and `batchExec` returns `e.statePath`. `fileExists("")` is false, and the `ex.batch()` guard already excludes `syncExec`.

- [ ] **Step 4: Run all pipeline tests**

Run: `go test ./internal/pipeline`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline
git commit -m "Batch ranking: refuse up front when the estimate exceeds --max-cost"
```

---

### Finish

- [ ] `make vet && make test` (loopback tests unsandboxed).
- [ ] Mark items 1.1–1.9 **done** in the backlog spec, with their commit hashes.
- [ ] Whole-branch review (superpowers:requesting-code-review), then superpowers:finishing-a-development-branch.
