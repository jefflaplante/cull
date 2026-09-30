# Correctness Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close section 2 of the 2026-09-30 review. Specifically:
- stored state survives later commands, folder renames and crashes;
- calibration numbers agree with each other;
- the subscription backend stops at quota;
- Capture One edits are not overwritten;
- the focus-landed noise floor holds on clipped frames.

**Architecture:** Each fix is local to the package that owns the state. Where two commands must agree, the shared behaviour goes in one place:
- `report` (`Rebase`, `Sequences`, schema guard);
- `xmp` (`Write`, `Ours`);
- `pipeline` (`DecideCopy`, used by calibrate).

The CLI resolves stored settings the same way it already resolves `Policy`.

**Tech Stack:** Go 1.22+, stdlib + cobra. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-30-repo-review-backlog.md`, section 2 (items 2.1–2.14; 2.10 was already fixed in section 1, `08911a4`).

## Global Constraints

- Never modify or delete DNGs. Never overwrite a sidecar cull didn't write unless `--overwrite-xmp`.
- Keep/review/cull is decided in Go (`eval.Policy`), not by the model.
- Dependencies: stdlib, cobra, pigo `core` only.
- Tests use synthetic fixtures only. Loopback tests (`llm`, `cli`, `review`) need an unsandboxed run.
- Build with `GOCACHE=$TMPDIR/gocache` inside the sandbox.
- One commit per task, on branch `fix/correctness`.

## Review Focus

- **A report from before this change** (no stored sequences, no `xmp_develop`) must still load and decide exactly as before. *Task 5: `TestDecideWithoutStoredSequencesUsesFlags`.*
- **A folder renamed with its report inside**, then `decide --write-xmp`: sidecars land beside the DNGs in the new folder, and nothing is re-judged. *Task 6: `TestResumeAfterFolderRenameRejudgesNothing`, `TestDecideAfterFolderRenameWritesInNewFolder`.*
- **A sidecar Capture One rewrote** (no longer carrying cull's toolkit marker) is foreign, not ours: never overwritten. *Task 3: `TestWriteSidecarLeavesForeignSidecar`.*
- **A torn final line followed by a new label:** both the earlier labels and the new one read back. *Task 2: `TestAppendAfterTornLineKeepsLog`.*
- **A claude-code call rejected for quota:** stops the run with the resume hint, instead of failing every remaining frame. *Task 8: `TestClaudeCodeQuotaOnErrorResultStops`.*

---

### Task 1: Refuse reports from a newer schema (2.8)

**Files:**
- Modify: `internal/report/report.go` (`Load`)
- Test: `internal/report/report_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestLoadRefusesNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(p, []byte(fmt.Sprintf(`{"schema_version":%d,"results":[]}`, SchemaVersion+1)), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("got %v", err)
	}
}
```

- [ ] **Step 2: Run it and check it fails.** `go test ./internal/report -run NewerSchema`. Expected: FAIL (nil error).
- [ ] **Step 3: Implement.** In `Load`, after unmarshalling:

```go
	if r.SchemaVersion > SchemaVersion {
		// Saving it back would silently drop whatever the newer version added.
		return nil, fmt.Errorf("%s is schema v%d, newer than this cull (v%d): upgrade cull", path, r.SchemaVersion, SchemaVersion)
	}
```

- [ ] **Step 4: Run it and check it passes.** Then run `go test ./internal/...` (sandbox; loopback packages excepted).
- [ ] **Step 5: Commit.** `Refuse reports from a newer schema instead of downgrading them`

---

### Task 2: A torn line in the labels log heals on the next append (2.4)

**Files:**
- Modify: `internal/labels/labels.go` (`Append`)
- Test: `internal/labels/labels_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestAppendAfterTornLineKeepsLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	if err := Append(p, Entry{File: "A.DNG", Label: "keep"}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"file":"B.DNG","lab`) // a crash mid-append
	f.Close()
	if err := Append(p, Entry{File: "C.DNG", Label: "cull"}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatalf("log unreadable after a torn line: %v", err)
	}
	if got["A.DNG"].Label != "keep" || got["C.DNG"].Label != "cull" {
		t.Fatalf("got %+v", got)
	}
}
```

- [ ] **Step 2: Run it and check it fails.** Expected: `Read` errors on the glued line.
- [ ] **Step 3: Implement.** Heal in `Append`, not `Read`: before writing, drop any bytes after the file's last newline. Those bytes are an interrupted append, which `Read` already treats as never written, so dropping them loses nothing that counted.

```go
// healTail drops an interrupted append (bytes after the last newline) so the next
// entry doesn't glue onto it: Read already treats such a tail as never written.
func healTail(f *os.File) error {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	buf := make([]byte, min(st.Size(), 64*1024))
	off := st.Size() - int64(len(buf))
	if _, err := f.ReadAt(buf, off); err != nil {
		return err
	}
	if buf[len(buf)-1] == '\n' {
		return nil
	}
	i := bytes.LastIndexByte(buf, '\n')
	return f.Truncate(off + int64(i+1)) // i = -1 (no newline in the window, so a single torn line): truncate to off
}
```

Open with `os.O_RDWR|os.O_APPEND|os.O_CREATE` and call `healTail(f)` before the write. With `O_APPEND`, the write goes to the new end after the truncate.

- [ ] **Step 4: Run it and check it passes,** plus `go test ./internal/labels`.
- [ ] **Step 5: Commit.** `Labels log: an append drops a torn tail instead of gluing onto it`

---

### Task 3: Sidecar writes are atomic and recognise their own files (2.13, 2.3)

**Files:**
- Modify: `internal/xmp/xmp.go` (`Write`, new `Ours`)
- Modify: `internal/labels/sidecar.go` (`WriteSidecar`)
- Test: `internal/xmp/xmp_test.go`, `internal/labels/sidecar_test.go` (or the existing test file for `WriteSidecar`)

**Interfaces:**
- Produces: `func Ours(path string) bool`, true when the sidecar at `path` is one cull wrote (its toolkit marker `x:xmptk="cull"`). Used by `WriteSidecar`.

- [ ] **Step 1: Write the failing tests**

```go
// xmp_test.go
func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "L1.xmp")
	if err := Write(p, Sidecar{Label: "Green"}, false); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, Sidecar{Label: "Red"}, false); !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	os.Mkdir(filepath.Join(dir, "L2.xmp"), 0o755) // a destination that can't be replaced
	if err := Write(filepath.Join(dir, "L2.xmp"), Sidecar{}, true); err == nil {
		t.Fatal("want an error writing over a directory")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "Green") {
		t.Fatal("existing sidecar replaced")
	}
}

func TestOursRecognisesOnlyCullSidecars(t *testing.T) {
	dir := t.TempDir()
	mine, theirs := filepath.Join(dir, "a.xmp"), filepath.Join(dir, "b.xmp")
	Write(mine, Sidecar{Label: "Green"}, false)
	os.WriteFile(theirs, []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="Capture One"/>`), 0o644)
	if !Ours(mine) || Ours(theirs) || Ours(filepath.Join(dir, "none.xmp")) {
		t.Fatalf("mine=%v theirs=%v", Ours(mine), Ours(theirs))
	}
}
```

```go
// labels: a crash between the sidecar write and the report's checkpoint leaves
// r.XMP empty; the sidecar is still ours and must be rewritable.
func TestWriteSidecarRewritesOwnUnrecordedSidecar(t *testing.T) {
	dir := t.TempDir()
	r := report.Result{File: filepath.Join(dir, "L1.DNG"), Decision: eval.Keep, Evaluation: &eval.Evaluation{}}
	if err := WriteSidecar(&r, Entry{}, false, false); err != nil {
		t.Fatal(err)
	}
	r.XMP, r.Decision = "", eval.Cull
	if err := WriteSidecar(&r, Entry{}, false, false); err != nil {
		t.Fatalf("own sidecar treated as foreign: %v", err)
	}
	if b, _ := os.ReadFile(r.XMP); !strings.Contains(string(b), "cull:cull") {
		t.Fatal("not rewritten")
	}
}

func TestWriteSidecarLeavesForeignSidecar(t *testing.T) {
	dir := t.TempDir()
	r := report.Result{File: filepath.Join(dir, "L1.DNG"), Decision: eval.Keep, Evaluation: &eval.Evaluation{}}
	os.WriteFile(filepath.Join(dir, "L1.xmp"), []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="Capture One"/>`), 0o644)
	if err := WriteSidecar(&r, Entry{}, false, false); !errors.Is(err, xmp.ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
}
```

- [ ] **Step 2: Run them and check they fail.** Expected: `Ours` is undefined; once stubbed, the rewrite test fails with ErrExists.
- [ ] **Step 3: Implement.**

```go
// marker is the toolkit attribute Render writes: a sidecar still carrying it was
// written by cull and not rewritten since (Capture One or Adobe would replace it).
const marker = `x:xmptk="cull"`

// Ours reports whether the sidecar at path was written by cull.
func Ours(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 4096) // the marker is in the packet's first lines
	n, _ := io.ReadFull(f, b)
	return bytes.Contains(b[:n], []byte(marker))
}

// Write renders and writes the sidecar atomically: a unique temp file beside it,
// synced, then linked into place (link(2) fails if the name exists, so an existing
// sidecar is never replaced unless overwrite, even by a concurrent writer) or
// renamed over it when overwriting. Filesystems without hard links fall back to
// check-then-rename. The temp file never outlives the call.
func Write(path string, s Sidecar, overwrite bool) error {
	if !overwrite {
		if _, err := os.Lstat(path); err == nil {
			return ErrExists
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after a link or rename this removes nothing that matters
	if _, err := f.Write(Render(s)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil { // CreateTemp makes 0600
		return err
	}
	if overwrite {
		return os.Rename(tmp, path)
	}
	switch err := os.Link(tmp, path); {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return ErrExists
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrExists
	}
	return os.Rename(tmp, path)
}
```

In `WriteSidecar`, the overwrite argument becomes `r.XMP == p || overwrite || xmp.Ours(p)`. Update its doc comment: a sidecar the report records as ours, *or that still carries cull's marker*, is rewritten.

- [ ] **Step 4: Run** `go test ./internal/xmp ./internal/labels ./internal/pipeline`. Expected: PASS.
- [ ] **Step 5: Commit.** `Sidecars: atomic link-based no-clobber write with a unique temp file; recognise our own by content`

---

### Task 4: Review keeps a sidecar's develop settings (2.12)

**Files:**
- Modify: `internal/report/report.go` (`Result.XMPDevelop`)
- Modify: `internal/labels/sidecar.go` (`WriteSidecar` records it)
- Modify: `internal/review/serve.go:173` (reuse it)
- Test: `internal/labels` (unit) and `internal/review/serve_test.go` (follow its existing label-POST test)

- [ ] **Step 1: Write the failing test** (labels package, no server needed):

```go
func TestWriteSidecarRecordsDevelop(t *testing.T) {
	dir := t.TempDir()
	ev := &eval.Evaluation{Exposure: eval.Exposure{Status: "fixable", EVAdjust: 0.5}}
	r := report.Result{File: filepath.Join(dir, "L1.DNG"), Decision: eval.Keep, Evaluation: ev}
	if err := WriteSidecar(&r, Entry{}, true, false); err != nil || !r.XMPDevelop {
		t.Fatalf("err=%v develop=%v", err, r.XMPDevelop)
	}
}
```

In `serve_test.go`, after the existing setup that writes a label through the server, write the frame's sidecar first with develop on and record it in the report (`r.XMPDevelop = true`, `r.XMP = path`). Then POST a label and assert the sidecar still contains `crs:Exposure2012`.

- [ ] **Step 2: Run them and check they fail** (the field is undefined, then the serve test loses `crs:`).
- [ ] **Step 3: Implement.** `Result` gets `XMPDevelop bool \`json:"xmp_develop,omitempty"\`` (the sidecar at XMP carries develop settings). `WriteSidecar` sets `r.XMPDevelop = develop` on success. `serve.go` passes `r.XMPDevelop` instead of `false`, and saves the report when `r.XMP` *or* `r.XMPDevelop` changed.
- [ ] **Step 4: Run** `go test ./internal/labels ./internal/review` (unsandboxed for review).
- [ ] **Step 5: Commit.** `Review keeps the develop settings of a sidecar written with them`

---

### Task 5: Sequence settings are stored and resolved like the policy (2.1)

**Files:**
- Modify: `internal/report/report.go` (`Sequences` type, `Report.Seq`)
- Modify: `internal/pipeline/groups.go` (`decideAll` records `rep.Seq`)
- Modify: `internal/cli/policy.go` (new `resolveSeq`)
- Modify: `internal/cli/decide.go`, `internal/cli/rank.go`, `internal/cli/cull.go` (resume branch), `internal/cli/calibrate.go` (Task 7 uses it)
- Test: `internal/pipeline/decide_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Produces: `type Sequences struct { GapSeconds float64 \`json:"gap_seconds"\`; Look float64 \`json:"look"\` }`; `Report.Seq *Sequences \`json:"seq,omitempty"\``; `func (s Sequences) Options() group.Options`; `func SequencesOf(o group.Options) *Sequences`.
- Produces: `func resolveSeq(fs *pflag.FlagSet, cur group.Options, saved *report.Sequences) (group.Options, []string)`.

`report` importing `group` is fine: `group` imports only stdlib. Check with `go list -deps ./internal/group`.

- [ ] **Step 1: Write the failing tests**

```go
// pipeline: decideAll records what it grouped with.
func TestDecideRecordsSequenceSettings(t *testing.T) {
	c, rep := seqShoot(t, 3)
	decideAll(rep, c.Policy, group.Options{Gap: 90 * time.Second, MaxLook: 0.12})
	if rep.Seq == nil || rep.Seq.GapSeconds != 90 || rep.Seq.Look != 0.12 {
		t.Fatalf("seq %+v", rep.Seq)
	}
}
```

```go
// cli: a stored --seq-look is reused by decide unless typed again.
func TestDecideReusesStoredSequenceSettings(t *testing.T) {
	fs := NewRootCmd().PersistentFlags()
	fs.Parse(nil)
	got, notes := resolveSeq(fs, group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}, &report.Sequences{GapSeconds: 60, Look: 0.12})
	if got.MaxLook != 0.12 || len(notes) != 1 || notes[0] != "--seq-look 0.12" {
		t.Fatalf("got %+v notes %v", got, notes)
	}
	fs.Parse([]string{"--seq-look", "0.05"})
	if got, _ := resolveSeq(fs, group.Options{Gap: time.Minute, MaxLook: 0.05}, &report.Sequences{GapSeconds: 60, Look: 0.12}); got.MaxLook != 0.05 {
		t.Fatalf("typed flag didn't win: %+v", got)
	}
}

func TestDecideWithoutStoredSequencesUsesFlags(t *testing.T) {
	fs := NewRootCmd().PersistentFlags()
	fs.Parse(nil)
	cur := group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}
	if got, notes := resolveSeq(fs, cur, nil); got != cur || len(notes) != 0 {
		t.Fatalf("got %+v %v", got, notes)
	}
}
```

- [ ] **Step 2: Run them and check they fail** (undefined names).
- [ ] **Step 3: Implement.**

```go
// report.go
// Sequences are the grouping settings the sets came from (--seq-gap, --seq-look).
type Sequences struct {
	GapSeconds float64 `json:"gap_seconds"`
	Look       float64 `json:"look"`
}

func (s Sequences) Options() group.Options {
	return group.Options{Gap: time.Duration(s.GapSeconds * float64(time.Second)), MaxLook: s.Look}
}

func SequencesOf(o group.Options) *Sequences {
	return &Sequences{GapSeconds: o.Gap.Seconds(), Look: o.MaxLook}
}
```

In `Report`, after `Policy`, add: `Seq *Sequences \`json:"seq,omitempty"\`` (the grouping the sets came from; later commands regroup with it unless --seq-gap/--seq-look are typed).

In `decideAll`, beside `rep.Policy = &p`: `rep.Seq = report.SequencesOf(seq)`.

```go
// policy.go
// resolveSeq is resolve for the grouping flags: the report's stored settings, with
// a typed --seq-gap or --seq-look overriding its own. A regroup at other settings
// would move frames between sets, change which are best, and drop paid rankings.
func resolveSeq(fs *pflag.FlagSet, cur group.Options, saved *report.Sequences) (group.Options, []string) {
	if saved == nil {
		return cur, nil
	}
	var notes []string
	s := saved.Options()
	if fl := fs.Lookup("seq-gap"); fl != nil && !fl.Changed {
		cur.Gap = s.Gap
		if v := s.Gap.String(); v != fl.DefValue {
			notes = append(notes, "--seq-gap "+v)
		}
	}
	if fl := fs.Lookup("seq-look"); fl != nil && !fl.Changed {
		cur.MaxLook = s.MaxLook
		if v := fmt.Sprint(s.MaxLook); v != fl.DefValue {
			notes = append(notes, "--seq-look "+v)
		}
	}
	return cur, notes
}
```

Call sites (each already has the loaded report and prints `noteStoredPolicy`):
- `decide`: keep the loaded report (not just `saved`). Set `cfg.Seq, seqNotes = resolveSeq(cmd.Flags(), cfg.Seq, rep.Seq)`, then `noteStoredPolicy(w, append(notes, seqNotes...))`.
- `rank`: the same after `o.policy.resolve`.
- `judge --resume`: the same inside the `if o.resume` block.

Validate the result like `base` does: gap ≥ 0 and look in [0, 1]. A hand-edited report can hold anything.

- [ ] **Step 4: Run** `go test ./internal/report ./internal/pipeline` (sandbox) and `./internal/cli` (unsandboxed).
- [ ] **Step 5: Docs.** In README "Reuse" and WORKFLOW step 5: the report also stores `--seq-gap`/`--seq-look`, and later commands reuse them unless typed.
- [ ] **Step 6: Commit.** `Store the sequence settings in the report; decide, rank and resume reuse them unless typed`

---

### Task 6: A renamed shoot folder keeps its report; a changed file replaces its old result (2.2)

**Files:**
- Modify: `internal/report/report.go` (`Rebase`)
- Modify: `internal/pipeline/pipeline.go` (`startRun`, `guardOverwrite`), `decide.go` (`DecideOptions.Dir`), `move.go` (`Restore(reportPath, dir, log)`), `rank.go` (`rank`)
- Modify: `internal/cli/decide.go`, `restore.go`, `review.go` (rebase, and save when rebased, since the server reloads), `applyc1.go` (rebase in memory)
- Test: `internal/report/report_test.go`, `internal/pipeline/pipeline_test.go`, `internal/pipeline/decide_test.go`

**Interfaces:**
- Produces: `func (r *Report) Rebase(dir string) bool`; `DecideOptions.Dir string`; `Restore(reportPath, dir string, log io.Writer)`.

- [ ] **Step 1: Write the failing tests**

```go
// report_test.go
func TestRebaseMovesEveryPathUnderTheOldDir(t *testing.T) {
	r := &Report{Dir: "/old", Results: []Result{{File: "/old/a/L1.DNG", MovedTo: "/old/a/culled/L1.DNG", XMP: "/old/a/culled/L1.xmp"}, {File: "/elsewhere/L2.DNG"}},
		Sets: []Set{{Members: []string{"/old/a/L1.DNG"}, Order: []string{"/old/a/L1.DNG"}, Notes: []RankNote{{File: "/old/a/L1.DNG"}}}}}
	if !r.Rebase("/new") {
		t.Fatal("no change reported")
	}
	x := r.Results[0]
	if r.Dir != "/new" || x.File != "/new/a/L1.DNG" || x.MovedTo != "/new/a/culled/L1.DNG" || x.XMP != "/new/a/culled/L1.xmp" ||
		r.Results[1].File != "/elsewhere/L2.DNG" || r.Sets[0].Members[0] != "/new/a/L1.DNG" || r.Sets[0].Order[0] != "/new/a/L1.DNG" || r.Sets[0].Notes[0].File != "/new/a/L1.DNG" {
		t.Fatalf("%+v %+v", r.Results, r.Sets)
	}
	if r.Rebase("/new") {
		t.Fatal("rebasing to the same dir changed something")
	}
}
```

```go
// pipeline_test.go
func TestResumeAfterFolderRenameRejudgesNothing(t *testing.T) {
	parent := t.TempDir()
	old := filepath.Join(parent, "shoot")
	os.Mkdir(old, 0o755)
	minimalDNG(t, filepath.Join(old, "L1000001.DNG"))
	if _, _, err := Run(context.Background(), cfg(old), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(parent, "2026-10-04 shoot")
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}
	c := cfg(renamed)
	c.Resume = true
	rep, usage, err := Run(context.Background(), c, &fakeBackend{status: "missed_focus"})
	if err != nil || usage.InputTokens != 0 || len(rep.Results) != 1 || !strings.HasPrefix(rep.Results[0].File, renamed) {
		t.Fatalf("err=%v usage=%+v results=%+v", err, usage, rep.Results)
	}
}

// A file changed since it was judged is judged again, and replaces its old result.
func TestResumeReplacesResultOfChangedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "L1000001.DNG")
	minimalDNG(t, p)
	if _, _, err := Run(context.Background(), cfg(dir), &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, time.Now(), time.Now().Add(time.Hour))
	c := cfg(dir)
	c.Resume = true
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil || len(rep.Results) != 1 {
		t.Fatalf("err=%v results=%d", err, len(rep.Results))
	}
}
```

```go
// decide_test.go
func TestDecideAfterFolderRenameWritesInNewFolder(t *testing.T) {
	parent := t.TempDir()
	old := filepath.Join(parent, "shoot")
	os.Mkdir(old, 0o755)
	minimalDNG(t, filepath.Join(old, "L1000001.DNG"))
	c := cfg(old)
	c.WriteXMP = false
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(parent, "renamed")
	os.Rename(old, renamed)
	if _, err := Decide(context.Background(), filepath.Join(renamed, "r.json"), DecideOptions{Dir: renamed, WriteXMP: true, Policy: eval.Policy{MinCropArea: 0.6}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(renamed, "L1000001.xmp")); err != nil {
		t.Fatalf("sidecar not in the renamed folder: %v", err)
	}
}
```

- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**

```go
// Rebase moves every path under the report's Dir to dir: the shoot folder was
// renamed or moved together with its report. Paths outside Dir are left alone.
// Returns whether anything changed; the caller saves.
func (r *Report) Rebase(dir string) bool {
	if dir == "" || r.Dir == "" || r.Dir == dir {
		return false
	}
	old := r.Dir + string(filepath.Separator)
	re := func(p string) string {
		if strings.HasPrefix(p, old) {
			return filepath.Join(dir, p[len(old):])
		}
		return p
	}
	for i := range r.Results {
		x := &r.Results[i]
		x.File, x.MovedTo, x.XMP = re(x.File), re(x.MovedTo), re(x.XMP)
	}
	for i := range r.Sets {
		s := &r.Sets[i]
		for j := range s.Members {
			s.Members[j] = re(s.Members[j])
		}
		for j := range s.Order {
			s.Order[j] = re(s.Order[j])
		}
		for j := range s.Notes {
			s.Notes[j].File = re(s.Notes[j].File)
		}
	}
	r.Dir = dir
	return true
}
```

Call `Rebase(dir)` right after each `report.Load` whose caller knows the shoot folder:
- `startRun` (resume branch: log `report was written for <old>; its paths now point under <new>` when it returns true);
- `guardOverwrite`;
- `Decide` (`o.Dir`, when set);
- `Restore` (its new `dir`);
- `rank` (`cfg.Dir`);
- cli `review` (save the report when it returns true, before serving);
- cli `apply-c1`;
- cli `decide`, `rank` and `judge --resume`, which load early to resolve the policy (those copies are read-only, so no save is needed).

CLI callers pass `cfg.Dir`. `rank`'s `DecideOptions` gets `Dir: cfg.Dir`.

In `startRun`, after `todo` is built:

```go
	// A file changed since it was judged (new size or mtime) is in todo again: its
	// old result goes, or the report would hold the frame twice.
	again := make(map[string]bool, len(todo))
	for _, f := range todo {
		again[f] = true
	}
	kept := rep.Results[:0]
	for _, r := range rep.Results {
		if again[r.File] {
			rep.DiscardedCostUSD += r.CostUSD
			continue
		}
		kept = append(kept, r)
	}
	rep.Results = kept
```

Update the `Restore` callers: `cli/restore.go` passes `cfg.Dir`, and tests pass `""` or the shoot dir (`sed` the test files).

- [ ] **Step 4: Run** `go test ./internal/report ./internal/pipeline` and `./internal/cli ./internal/review` unsandboxed.
- [ ] **Step 5: Commit.** `Rebase a report onto a renamed shoot folder; a changed file replaces its old result`

---

### Task 7: Calibrate's numbers agree with each other (2.5, 2.6)

**Files:**
- Modify: `internal/pipeline/rankcalls.go` (export `DecideCopy`)
- Modify: `internal/calib/calib.go` (`Sweep` takes a decide function)
- Modify: `internal/cli/calibrate.go`
- Test: `internal/calib/calib_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Produces: `func DecideCopy(rep *report.Report, p eval.Policy, seq group.Options) *report.Report` (a decided copy; `rep` untouched).
- Changes: `calib.Sweep(rep, labels, base, thresholds, decide func(eval.Policy) *report.Report)`.

- [ ] **Step 1: Write the failing tests**

```go
// calib_test.go
// With a decide function that demotes, the sweep must report the demoted decisions.
func TestSweepUsesTheDecideFunction(t *testing.T) {
	rep := testReport() // existing fixture
	demoteAll := func(p eval.Policy) *report.Report {
		cp := *rep
		cp.Results = append([]report.Result(nil), rep.Results...)
		for i := range cp.Results {
			cp.Results[i].Decision = eval.Review
		}
		return &cp
	}
	rows := Sweep(rep, map[string]string{filepath.Base(rep.Results[0].File): "keep"}, eval.Policy{MinCropArea: 0.6}, []float64{0}, demoteAll)
	if rows[0].Matrix.Counts["keep"]["review"] != 1 {
		t.Fatalf("%+v", rows[0].Matrix.Counts)
	}
}
```

```go
// cli_test.go
func TestCalibrateRefusesDuplicateNamesWithLabels(t *testing.T) {
	dir := t.TempDir()
	rep := &report.Report{SchemaVersion: report.SchemaVersion, Backend: "anthropic", Model: "m", Results: []report.Result{
		{File: filepath.Join(dir, "a", "L1.DNG"), Evaluation: &eval.Evaluation{}, Decision: eval.Keep},
		{File: filepath.Join(dir, "b", "L1.DNG"), Evaluation: &eval.Evaluation{}, Decision: eval.Cull}}}
	rp := filepath.Join(dir, "cull-report.json")
	rep.Save(rp)
	labels.Append(filepath.Join(dir, labels.FileName), labels.Entry{File: "L1.DNG", Label: "keep"})
	if _, err := run(t, "calibrate", rp); err == nil || !strings.Contains(err.Error(), "share a file name") {
		t.Fatalf("got %v", err)
	}
}
```

- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**

```go
// DecideCopy returns rep decided with p and seq, sets and all, leaving rep
// untouched: calibrate's sweep compares against exactly what decide would do.
func DecideCopy(rep *report.Report, p eval.Policy, seq group.Options) *report.Report {
	cp := cloneForRankCalls(rep)
	decideAll(cp, p, seq)
	return cp
}
```

`Sweep` calls `decide(p)` per threshold and compares its `Decision`s through `compare` with `func(r report.Result) string { return string(r.Decision) }`. Look results up by file in the decided copy; the result order is the same, so iterate over it directly.

In `cli/calibrate.go`, per report:
- refuse with the decide-style error when `labels.Duplicates(rep.Results)` is non-empty and there are verdicts;
- resolve `seq` with `resolveSeq(cmd.Flags(), defaults, rep.Seq)` (defaults from the persistent flags: `cmd.Flag("seq-gap")`, `cmd.Flag("seq-look")`);
- pass `func(p eval.Policy) *report.Report { return pipeline.DecideCopy(rep, p, seq) }` to `Sweep`;
- use `p.KeepBest` (not `rep.KeepBest`) for `calib.Sets`.

- [ ] **Step 4: Run** `go test ./internal/calib ./internal/pipeline` and `./internal/cli` unsandboxed.
- [ ] **Step 5: Commit.** `calibrate: sweep through decide (set demotion included), honour --keep-best, refuse duplicate names`

---

### Task 8: The subscription backend stops at quota, on errors and on the 7-day window (2.7)

**Files:**
- Modify: `internal/llm/claudecode.go` (`run`, `quotaErr`)
- Modify: `internal/cli/backend.go` (`--quota-stop` help)
- Test: `internal/llm/claudecode_test.go` (follow its fake-`claude` script pattern)

- [ ] **Step 1: Write the failing tests.** In the file's existing style (a fake `claude` that prints stream-json events):
  - `TestClaudeCodeQuotaOnErrorResultStops`: the fake emits init (`apiKeySource: none`), then a `rate_limit_event` with `status: "rejected"`, then a `result` with `is_error: true`. Expect `errors.Is(err, ErrQuotaStop)`.
  - `TestClaudeCodeSevenDayWindowStops`: the fake emits `seven_day` utilization 0.95 and `five_hour` 0.1, then a successful result, with `QuotaStop` 0.9. Expect `ErrQuotaStop`.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.** In `run`:

```go
	case perr != nil:
		if resp != nil {
			if qerr := c.quotaErr(resp.Quota); qerr != nil { // a call rejected for quota stops the run, not just this frame
				return resp, fmt.Errorf("%w (%v)", qerr, perr)
			}
		}
		return resp, perr
```

In `quotaErr`, the stop condition also includes `c.QuotaStop > 0 && q.SevenDay >= c.QuotaStop`, and the message names both windows: `5-hour window %.0f%%, 7-day %.0f%% used (stop at %.0f%%)`. The `--quota-stop` help becomes "stop when this fraction of the 5-hour or 7-day subscription window is used".

- [ ] **Step 4: Run** `go test ./internal/llm` unsandboxed.
- [ ] **Step 5: Commit.** `claude-code: stop at quota on rejected calls and on the 7-day window`

---

### Task 9: A crash between the batch report save and state removal can't duplicate results (2.9)

**Files:**
- Modify: `internal/pipeline/batch.go` (finish loop)
- Test: `internal/pipeline/batch_test.go`

- [ ] **Step 1: Write the failing test**

```go
// The report was saved, but the judge state wasn't removed (a crash between the
// two): the resume must not add the frames a second time.
func TestBatchResumeAfterFinishCrashAddsNothingTwice(t *testing.T) {
	_, c := batchShoot(t)
	fb := &fakeBatch{locate: map[string]string{locatePrompt: `{"confident":true,"kind":"eye","subject":"eye","box":{"left":0.4,"top":0.3,"right":0.45,"bottom":0.35}}`}}
	c.Rank = false
	rep, _, err := RunBatch(context.Background(), c, fb)
	if err != nil {
		t.Fatal(err)
	}
	st := &batchState{Version: batchStateVersion, Backend: c.Backend, Model: c.Model, Frames: map[string]*batchFrame{}}
	for _, r := range rep.Results { // what the state held when the crash hit
		st.Frames[frameID(r.File)] = &batchFrame{Result: r, Stage: "done"}
	}
	if err := saveState(batchStatePath(c), st); err != nil {
		t.Fatal(err)
	}
	c.Resume = true
	rep2, _, err := RunBatch(context.Background(), c, fb)
	if err != nil || len(rep2.Results) != 3 {
		t.Fatalf("err=%v results=%d", err, len(rep2.Results))
	}
}
```

- [ ] **Step 2: Run it and check it fails** (6 results).
- [ ] **Step 3: Implement.** Before the finish loop, collect the keys of `rep.Results` (what `startRun` resumed). In the loop, skip a frame whose `f.Result.Key()` is already there: no cost, no usage, no append. After `rep.Save`, report a failed `os.Remove(statePath)` to `cfg.Log` instead of ignoring it.
- [ ] **Step 4: Run** `go test ./internal/pipeline`.
- [ ] **Step 5: Commit.** `Batch: a resume after a crash between save and state removal adds nothing twice`

---

### Task 10: apply-c1 never overwrites exposure or crop set in Capture One (2.11)

**Files:**
- Modify: `internal/c1/c1.go` (`Script`)
- Test: `internal/c1/c1_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestScriptAppliesEditsOnlyOverDefaults(t *testing.T) {
	s := Script(testReport(), Options{Exposure: true, Crop: true})
	b := block(s, "L1.DNG")
	if !strings.Contains(b, "if (exposure of adjustments of v) is 0 then set exposure of adjustments of v to 0.7") {
		t.Errorf("exposure not guarded:\n%s", b)
	}
	if !strings.Contains(b, "if my isFullFrame(crop of v, w, h) then set crop of v to") {
		t.Errorf("crop not guarded:\n%s", b)
	}
	if !strings.Contains(s, "on isFullFrame(") {
		t.Error("helper missing")
	}
}
```

- [ ] **Step 2: Run it and check it fails.**
- [ ] **Step 3: Implement.** The exposure line becomes `if (exposure of adjustments of v) is 0 then set exposure of adjustments of v to <ev>`. The crop line becomes `if my isFullFrame(crop of v, w, h) then set crop of v to {…}`. Add to `helpers`:

```applescript
-- An untouched crop covers the whole image (within a pixel). Any other crop was
-- set in Capture One and is kept. If the crop's orientation doesn't match
-- dimensions (unverified), this is false and the suggestion is skipped, which is the safe way to fail.
on isFullFrame(cr, w, h)
	return ((item 3 of cr) ≥ w - 1) and ((item 4 of cr) ≥ h - 1)
end isFullFrame
```

Update the package doc and README's apply-c1 note: suggested exposure and crop apply only where the image still has its default exposure or crop.

- [ ] **Step 4: Run** `go test ./internal/c1`, then compile a generated script with `osacompile` as in section 1.
- [ ] **Step 5: Commit.** `apply-c1: suggested exposure and crop apply only over Capture One's defaults`

---

### Task 11: The landed-tile noise floor ignores clipped and flat cells (2.14)

**Files:**
- Modify: `internal/focus/landed.go` (`fineCoarse` returns the mean; `Landed` picks noise from eligible cells)
- Test: `internal/focus/focus_test.go`

- [ ] **Step 1: Write the failing test**

```go
// Half the frame blown to white (flat, clipped): the noise estimate must come
// from the half that has noise, not collapse to 0.
func TestLandedNoiseIgnoresClippedCells(t *testing.T) {
	w, h := CellSize*8, CellSize*4
	luma := make([]uint8, w*h)
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				luma[y*w+x] = 255
			} else {
				luma[y*w+x] = uint8(128 + rng.Intn(9) - 4)
			}
		}
	}
	_, noise := Landed(luma, w, h, image.Rectangle{}, 0)
	all := make([]uint8, w*h) // the same noise over the whole frame
	rng = rand.New(rand.NewSource(1))
	for i := range all {
		all[i] = uint8(128 + rng.Intn(9) - 4)
	}
	_, want := Landed(all, w, h, image.Rectangle{}, 0)
	if noise < want/2 {
		t.Fatalf("noise %v on a half-clipped frame, %v without clipping", noise, want)
	}
}
```

- [ ] **Step 2: Run it and check it fails** (noise = 0).
- [ ] **Step 3: Implement.** `fineCoarse` also returns the cell's mean luma (the downsample loop already sums every pixel: `mean = total / (cw*ch*16)`). `Ratio` ignores the mean. In `Landed`, noise is the 10th-percentile fine variance of the cells whose mean is in [16, 239] and whose fine variance is > 0. With no such cell, fall back to every cell. Update the comment on why: clipped highlights, crushed shadows and JPEG-flattened sky have no noise left to measure.
- [ ] **Step 4: Run** `go test ./internal/focus ./internal/pipeline`. Then compare on the real sample, which is free (no model):
  - run `./bin/cull scan -o $TMPDIR/before.json photos` on `main`, and `-o $TMPDIR/after.json` on this branch;
  - diff `focus_target.landed_sharpness` per frame;
  - record in the commit message how many frames changed.
- [ ] **Step 5: Commit.** `Landed noise floor: estimate from cells with measurable noise, not clipped or flat ones`

---

### Finish

- [ ] `make vet`, and `go test -count=1 ./...` unsandboxed.
- [ ] Mark 2.1–2.14 **done** in the backlog (2.10 with `08911a4`), with commit hashes.
- [ ] Whole-branch review, then merge per the user's earlier choice (merge locally).
