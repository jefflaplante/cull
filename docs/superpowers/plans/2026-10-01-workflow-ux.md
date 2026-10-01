# Workflow and UX Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Section 5 of the review. Make labelling a sample fast, make the CLI say what it will do before it spends, and remove flags that are accepted but ignored.

**Architecture:**
- **CLI changes** stay in `internal/cli`. The new commands are `status` and `import-labels`; flag registration is scoped per command.
- **Review page** changes stay in `internal/review`:
  - `page.html` gets the loupe, set compare, navigation keys and save revert;
  - `serve.go` renders the loupe's native image on first request and caches it in the sheet's `assets/`.
- **Nothing here changes a verdict.**

**Tech Stack:** Go 1.22+, stdlib + cobra; vanilla JS in the embedded page (no build step, no libraries).

**Spec:** `docs/superpowers/specs/2026-09-30-repo-review-backlog.md`, section 5.

## Global Constraints

- The review server stays on 127.0.0.1 with its Host, Origin and token checks. A new image endpoint serves only names it generated, from the sheet's own assets folder.
- **Never modify DNGs.** The loupe reads the embedded preview only.
- **Static mode (`--static`) keeps working offline.** Server-only features (the loupe) are hidden there.
- **Asking before spending:** only on a terminal and only above $1. Non-interactive runs (scripts, Claude) behave as today, and `--yes` skips the question.
- **Tests:** synthetic fixtures. Loopback packages (`cli`, `review`, `llm`) run unsandboxed. The page JS has no unit-test harness, so Go tests check the served page for the new handlers, and the Finish step checks the UI in a real browser.
- One commit per task, on branch `feat/workflow-ux`.

## Review Focus

- **A user running `cull review --max-edge 1024 <dir>` after this change** gets "unknown flag", not a silently ignored setting. The help for each command lists only flags it uses. *Task 2: `TestFlagsOnlyWhereUsed`.*
- **`cull calibrate ~/Pictures/shoot`** (a folder) finds `cull-report.json` and the labels log inside it. *Task 3.*
- **`judge` on a priced backend from a script (stdin not a terminal)** doesn't hang waiting for an answer. *Task 5: `TestNoPromptWithoutTerminal`.*
- **A loupe request for a name the sheet didn't generate** (`../`, another frame's DNG, a `.DNG`) gets 404 and nothing is decoded. *Task 8: `TestLoupeServesOnlyKnownFrames`.*
- **A label the server rejects** doesn't stay on screen as if saved. *Task 7: the page reverts it; checked in the browser.*

---

### Task 1: Small flag and doc fixes

**Files:** `internal/cli/decide.go`, `cull.go`, `review.go`, `applyc1.go`; `README.md`, `WORKFLOW.md`; `internal/cli/cli_test.go`.

- [ ] **Step 1: Tests.**
  - Flag-validation table: `{"decide", "--overwrite-xmp", dir}` errors.
  - `TestLabelsFlagOnJudgeAndReview`: both commands define `--labels`.
  - `TestApplyC1ProbeNeedsNoDir`: `apply-c1 --probe` with no dir arg succeeds and prints the probe script.
  - `TestApplyC1NeedsDirWithoutProbe`: `apply-c1` with no args errors.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - `decide`: `--overwrite-xmp requires --write-xmp`.
  - `judge`: `--labels`, passed to `userLabels`.
  - `review`: `--labels` sets `ServeOptions.LabelsPath` (default `labels.DefaultPath`).
  - `apply-c1`: `Args: cobra.RangeArgs(0, 1)`; without `--probe`, require exactly one.
  - Review help: "sidecars not written by cull write are never touched" becomes "sidecars cull didn't write are never touched".
  - README and WORKFLOW: your label is the outlined badge top right, and the model's verdict is in the card's bottom row. K/R/C label and advance in the detail view; in the grid they label only.
- [ ] **Step 4: Run** `go test ./internal/cli` (unsandboxed).
- [ ] **Step 5: Commit.** `Flag fixes: decide --overwrite-xmp needs --write-xmp; --labels on judge and review; apply-c1 --probe without a dir; doc badge positions`

### Task 2: Flags only where they do something

**Files:** `internal/cli/root.go` (persistent: `-o`, `-r` only; `registerPrep`, `registerSeq` helpers), every command constructor, `internal/cli/calibrate.go` (its own seq flags for `flagSeq`), and the tests that look up flags on the root's persistent set.

**Interfaces:** `func (so *sharedOpts) registerPrep(f *pflag.FlagSet)` registers `--max-edge`, `--tiles`, `--min-preview-edge`, `--face-min-q`, `--save-inputs` and `--landed-with-subject`. `func (so *sharedOpts) registerSeq(f *pflag.FlagSet)` registers `--seq-gap` and `--seq-look`.

Which command takes which flags:

| command | prep | seq |
|---|---|---|
| scan | yes | yes |
| judge | yes | yes |
| rank | no | yes |
| decide | no | yes |
| calibrate | no | yes (own vars) |
| review, apply-c1, restore, status, import-labels | no | no |

`so.base` still reads `so.*`: unregistered flags keep their zero values there, and `base` only validates seq when `seq-gap` is registered (`cmd.Flags().Lookup`). Simplest is for `base` to take the defaults when a field is zero, since `registerSeq` sets defaults. Do it by initialising `so` with defaults in `NewRootCmd` before registering.

- [ ] **Step 1: Test.**

```go
func TestFlagsOnlyWhereUsed(t *testing.T) {
	cmds := map[string]*cobra.Command{}
	for _, c := range NewRootCmd().Commands() {
		cmds[c.Name()] = c
	}
	has := func(cmd, flag string) bool { return cmds[cmd].Flags().Lookup(flag) != nil || cmds[cmd].InheritedFlags().Lookup(flag) != nil }
	for _, cmd := range []string{"review", "restore", "apply-c1"} {
		for _, f := range []string{"max-edge", "tiles", "seq-gap", "save-inputs"} {
			if has(cmd, f) {
				t.Errorf("%s accepts --%s, which it ignores", cmd, f)
			}
		}
	}
	for _, cf := range [][2]string{{"judge", "max-edge"}, {"scan", "tiles"}, {"decide", "seq-look"}, {"rank", "seq-gap"}, {"calibrate", "seq-look"}, {"review", "report"}} {
		if !has(cf[0], cf[1]) {
			t.Errorf("%s lacks --%s", cf[0], cf[1])
		}
	}
}
```

  Update `TestFaceMinQDefault`, `TestMaxEdgeDefault` and the two `resolveSeq` tests to look the flags up on `judge`'s or `decide`'s own flag set.
- [ ] **Step 2: Run it and check it fails.**
- [ ] **Step 3: Implement** the table above.
- [ ] **Step 4: Run** `go test ./internal/cli` (unsandboxed). Check `cull review --help` lists no prep or seq flags.
- [ ] **Step 5: Commit.** `Flags only where they apply: prep flags on scan/judge, grouping flags on the commands that group`

### Task 3: `calibrate <dir>`

**Files:** `internal/cli/calibrate.go`, `cli_test.go`.

- [ ] **Step 1: Test.** `TestCalibrateTakesAFolder`: a folder holding `cull-report.json` and a labels log. `calibrate <dir>` prints the confusion matrix. `calibrate --compare <dirA> <dirB>` works too.
- [ ] **Step 2: Run it and check it fails** (today it says "no labels in <parent>/cull-labels.jsonl").
- [ ] **Step 3: Implement** `reportArg(p string) string`: if `p` is a directory, return `<p>/cull-report.json`. Map `args` through it before anything else, so the default labels path (`labels.DefaultPath(args[0])`) is then beside the report. Usage becomes `calibrate [--labels …] <dir|REPORT.json>...`.
- [ ] **Step 4: Run** `go test ./internal/cli`.
- [ ] **Step 5: Commit.** `calibrate takes a shoot folder as well as a report`

### Task 4: `cull status <dir>`

**Files:** create `internal/cli/status.go`, `internal/cli/status_test.go`; register it in `root.go`.

**Output** (one block, plain text):

```
<dir>: 17 DNGs; report cull-report.json (judged: claude-code/sonnet → claude-sonnet-5-5, effort default)
  assessed 17 · errors 0 · not yet judged 0
  model: keep 14 · review 2 · cull 1 · moved to culled/ 0
  you: labelled 0/17 · rated 0 · disagree 0
  sets: 2 (ranked 2, by scores 0)
  spent: $0.00 (subscription)
  pending: none            (or: judge batch state …, rank batch state …)
next: label a sample in `cull review <dir>`, then `cull calibrate <dir>`
```

**The next-step rule**, first match wins:
1. No report: `cull scan <dir>` or `cull judge --estimate <dir>`.
2. A batch state file: re-attach (`judge --batch --resume` or `rank --batch`).
3. DNGs not in the report: `cull judge --resume <dir>`.
4. A scan-only report: `cull judge --estimate <dir>`.
5. Sets by scores: `cull rank --estimate <dir>`.
6. Fewer than 30 labelled frames: `cull review <dir>`.
7. Otherwise: `cull calibrate <dir>`, then `cull decide --write-xmp --move-culled <dir>`.

- [ ] **Step 1: Tests.**
  - `TestStatusWithoutReport` says to scan.
  - `TestStatusCountsAndNextStep`: a fixture report with 3 frames (keep, review, cull), one labelled, and one DNG not in the report. Its output has `assessed 3`, `not yet judged 1`, `labelled 1/3`, and next `judge --resume`.
  - `TestStatusSeesPendingBatch`: a `<report>.batch.json` exists, so the next step mentions `--batch --resume`.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.** Use `pipeline.Discover`, `report.Load` (rebased with `Relocate`), and `labels.Read` (default path, `--labels`). Check the batch states with `os.Stat` (`<report>.batch.json`, `<report>.rank-batch.json`). Spend is `rep.Cost()`, shown as "(subscription)" when the backend isn't priced.
- [ ] **Step 4: Run** `go test ./internal/cli`.
- [ ] **Step 5: Docs.** README Commands table, and WORKFLOW step 0 ("lost? `cull status <dir>`").
- [ ] **Step 6: Commit.** `cull status: where a shoot stands and what to run next`

### Task 5: Ask before spending

**Files:** `internal/cli/cull.go`, `rank.go`, a new `internal/cli/confirm.go`, `cli_test.go`.

**Interfaces:**
- `var isTerminal = func(f *os.File) bool { st, err := f.Stat(); return err == nil && st.Mode()&os.ModeCharDevice != 0 }`
- `func confirmSpend(cmd *cobra.Command, usd float64, yes bool) error`

The CLI reads the answer from `cmd.InOrStdin()`. It asks only when `!yes && usd > 1 && isTerminal(os.Stdin)` (a test hook overrides `isTerminal`). The question is "Spend about $X.XX? [y/N] "; anything but y or yes returns "not confirmed: nothing was spent".

- [ ] **Step 1: Tests.**
  - `TestJudgeAsksBeforeSpending`: `isTerminal` returns true, and the dir holds 200 empty `.DNG` files (the estimate is about $4.40 at Sonnet 5.5). With stdin "n\n" it errors with "not confirmed" and no key error. With stdin "y\n" it gets past the prompt (the error is then the missing API key).
  - `TestNoPromptWithoutTerminal`: `isTerminal` returns false, so there's no prompt (the error is the missing key, straight away).
  - `TestYesSkipsPrompt`: `--yes` with `isTerminal` true and empty stdin gets past it.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.** In `judge`, call it after `printEstimate` with the estimate's USD (frames plus ranking; return the total from `printEstimate`). In `rank`, call it after `printRankEstimate`. Add `--yes` to both commands.
- [ ] **Step 4: Run** `go test ./internal/cli` (unsandboxed).
- [ ] **Step 5: Docs.** README "Cost control": over $1 on a terminal, `judge` and `rank` ask first; `--yes` skips that.
- [ ] **Step 6: Commit.** `judge and rank ask before spending over $1 on a terminal; --yes skips`

### Task 6: `cull import-labels`

**Files:** create `internal/cli/importlabels.go`, tests in `cli_test.go`; register it in `root.go`.

Usage: `cull import-labels <export.jsonl> <dir>`. It reads the export with `labels.Read` (the same format as the log; the last line per file wins, and an export can't express clears, which is fine). Every entry is appended to the shoot's labels log (`--labels` overrides the path), with its original `At` kept. It prints how many it imported. It refuses entries for files not in the report, listing them: a sheet from another shoot.

- [ ] **Step 1: Tests.**
  - An export with 2 labels for frames in the report: both are in the log after the import.
  - An export naming a file not in the report errors and imports nothing.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement** as above.
- [ ] **Step 4: Run** `go test ./internal/cli`.
- [ ] **Step 5: Docs.** README "Reviewing and labelling" and `review --static`'s help: labels from the static page come in with `cull import-labels`.
- [ ] **Step 6: Commit.** `cull import-labels: bring labels exported from review --static into the log`

### Task 7: Review page: a rejected save reverts; jump keys

**Files:** `internal/review/page.html`, `internal/review/serve_test.go` or `review_test.go` (page content).

- [ ] **Step 1: Tests (page content).** The served page contains the handlers `"n"` (next unlabelled), `"]"` and `"["` (next and previous set), and a revert on a 400 (`prev`). The help line lists `N next unlabelled · [ ] sets`.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - **Queue entries:** each queued change carries the label it replaced (`prev: {label, stars}`). The body sent is `{file, label, stars}` only.
  - **On a 400:** unless a later queued change is for the same file, restore `labels[file] = prev` (or delete it when `prev` was empty), store, and show "rejected: … (reverted)".
  - **`n`:** from the current position in `visible()`, go to the next frame without a label, wrapping once. If there is none, the header note says "every shown frame is labelled".
  - **`]` and `[`:** go to the first visible frame of the next or previous set (`group.id` different from the current one's; frames without a set are skipped).
  - **Help line:** the header lists the new keys.
- [ ] **Step 4: Run** `go test ./internal/review` (unsandboxed).
- [ ] **Step 5: Commit.** `Review: a rejected save reverts its badge; N next unlabelled, [ ] previous/next set`

### Task 8: Review page: native-resolution loupe (Z)

**Files:** `internal/review/review.go` (`card.Native`), `internal/review/serve.go` (`/assets/{base}.native.jpg` rendered on first request), `page.html`, `serve_test.go`.

**Server:**
- `NewServer` builds `natives map[string]string`, from `<base>.native.jpg` to the frame's current path (`MovedTo` or `File`) for every frame with a preview.
- In `image()`, a name ending `.native.jpg` that isn't on disk is generated:
  - only if it is in `natives` (anything else is a 404);
  - under a mutex, one at a time (a 60MP decode is about 0.5 GB);
  - by `dng.Extract`, then `imageprep.Decode`, then `f.Crop(full, 90)`, written to `assets/` atomically (temp file, then rename), then served.
- Static sheets never reference it.

**Page:**
- **Opening:** `Z` (and a "100% (Z)" button in the detail view, serve mode only) opens a full-window overlay showing `assets/<native>`.
- **Scale:** `img.style.width = naturalWidth / devicePixelRatio` px, so 1 image pixel is 1 screen pixel.
- **Position:** it scrolls to the subject box's centre (`c.focus.box`, normalised, display orientation), or else the frame's centre.
- **Controls:**
  - drag to pan, and the arrow keys pan by 25% of the view;
  - `Z` or `Esc` closes it;
  - K/R/C/U and 0–5 still label the frame shown.
- **Loading:** a caption says "loading native preview… (first time takes a few seconds)" until it has loaded.

- [ ] **Step 1: Tests.**
  - `TestLoupeServesNativePreview`: `GET /assets/L1.native.jpg` returns a JPEG whose decoded size equals the fixture's preview size, in display orientation. A second GET is served from disk (mtime unchanged).
  - `TestLoupeServesOnlyKnownFrames`: `GET /assets/..%2Fx.native.jpg`, `/assets/nope.native.jpg` and `/assets/L1.DNG` all return 404.
  - Page content: the handler for `"z"` and `natives` in the card data.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement** as above.
- [ ] **Step 4: Run** `go test ./internal/review` (unsandboxed).
- [ ] **Step 5: Commit.** `Review: Z opens a native-resolution loupe on the subject (rendered on first use, cached)`

### Task 9: Review page: set compare (S)

**Files:** `page.html`, page-content test.

**Opening and layout:**
- `S`, or a "Compare set (S)" button in the detail view, opens an overlay with every member of the current frame's set, numbered 1…n in capture order.
- Each member shows its subject crop, or its thumbnail without one.

**Equal scale:**
- once every image has loaded, compute `k = min(1, columnWidth / max(naturalWidth))`;
- size each image `naturalWidth × k`;
- subject crops are native pixels, so equal `k` means equal scale.

**Each tile shows:** the file name, the model's badge, the rank (`#r/of`, and "best"), and your label.

**Keys:**
- `1`–`9` labels member N `keep`, closes the overlay, and selects that frame;
- `Esc` or `S` closes it;
- `Enter` on a focused tile opens that frame.

**Outside the overlay:** K/R/C/U and the star keys act on the current frame as before.

- [ ] **Step 1: Test (page content):** the handler for `"s"` and the compare overlay's id.
- [ ] **Step 2: Run it and check it fails.**
- [ ] **Step 3: Implement** as above.
- [ ] **Step 4: Run** `go test ./internal/review` (unsandboxed).
- [ ] **Step 5: Docs.** README/WORKFLOW key table: Z, S, N, [ ].
- [ ] **Step 6: Commit.** `Review: S compares a set's subject crops side by side at equal scale; 1-9 keeps one`

### Finish

- [ ] `make vet`, and `go test -count=1 ./...` unsandboxed.
- [ ] **Browser check**, if the Chrome extension is connected:
  - run `cull review --no-open --no-xmp -o <scratch>/r.json photos` against a report copied to the scratchpad, so labels go to the scratchpad and nothing touches `photos/`;
  - open the printed URL;
  - exercise Z (loupe loads, scrolls to the face, pans), S (compare shows 2 frames at equal scale), N, ] and [, and a label save;
  - fix what's broken.

  Otherwise, give the user the command to try it.
- [ ] Mark section 5 done in the backlog.
- [ ] Final whole-branch review, then the finishing menu.
