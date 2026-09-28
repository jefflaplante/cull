# Feature Batch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Calibration tooling (review sheet, decide, calibrate), scale (lean decode, cost
guard, batches, tiers), photographic signals (eyes, EXIF, bursts, raw clipping),
Capture One script generation, and the deferred review minors.

**Architecture:** Extends existing packages; new `internal/review` (HTML sheet),
`internal/group` (dHash + bursts), `internal/rawclip` (lossless JPEG), `internal/c1`
(script generation), `internal/calib` (label agreement). Policy stays in `eval.Policy`.

**Tech Stack:** Go 1.22, stdlib, cobra, pigo core. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-27-feature-batch-design.md`

**Execution note:** executed natively by the author in the same session (user: "build
it all now"); steps list the tests (written first, watched fail) and the implementation
outline. No commits until the user picks an integration option. Branch `features`.

## Global Constraints

- No new dependencies. Never modify or delete DNGs; moves only via `--move-culled` / `restore` / `decide --move-culled`.
- Never print/log API keys. Decisions only in `eval.Policy`; new signals default to `review`, never `cull`.
- Tests synthetic and offline; never commit real images.
- Report `SchemaVersion` 3.
- No paid API calls and no `osascript` against the user's catalog without explicit user approval.

## Review Focus

1. `decide` must never overwrite a sidecar the report doesn't record as ours, and `--move-culled` sync must never overwrite a file.
2. Batch mode must never resubmit a batch already in flight after a restart (double spend).
3. Lean decoding must keep crops/luma/downscale correct for orientations 1/3/6/8.
4. Frames missing EXIF (or with partial EXIF) must still group, evaluate and render.
5. The review sheet must work offline from `file://` with 1000 frames, and labels must survive a reload.

---

### Task 1: Lean decoding
Files: `internal/imageprep/prep.go` (+tests), `internal/focus/*.go` (uint8 luma), `internal/pipeline/pipeline.go` call sites.
- [ ] Tests: orientation 1/3/6/8 marker test via `Crop` and `Downscaled` (not RGBA field); `Luma` is `[]uint8` rotated for display; `Measure` unchanged values on gradient; grayscale JPEG decodes; focus tests on `[]uint8`.
- [ ] Implement YCbCr-backed `Frame` with stored→display rect mapping; custom YCbCr box downscale; per-crop rotation.
- [ ] Measure peak RSS on real frames before/after (unsandboxed `/usr/bin/time -l`).

### Task 2: EXIF
Files: `internal/dng/exif.go` (+test), `internal/report/report.go`, `internal/pipeline/pipeline.go` (stats text).
- [ ] Verify tag presence on a real M11-P DNG with a throwaway dump.
- [ ] Tests: synthetic TIFF with IFD0 Make/Model + Exif IFD (RATIONAL ExposureTime/FNumber/FocalLength, SHORT ISO, ASCII DateTimeOriginal/SubSec/LensModel) → parsed; missing Exif IFD → zero value, no error; `CaptureTime()` combines date + subsec; stats text formats `1/125 s, f/1.4, ISO 400, 50 mm`.
- [ ] Implement `dng.ReadExif(path) (Exif, error)`; result `exif`.

### Task 3: Eyes/expression + policy config + schema v3
Files: `internal/eval/{types,prompt}.go`, tests; `internal/report` (SchemaVersion 3); CLI flags.
- [ ] Tests: schema requires `people`; Policy table: eyes closed → review (default) / cull / ignore; partial → reason only; `ReviewBelowSharpness`; parse `Action` flag values; CLI rejects bad action.
- [ ] Implement.

### Task 4: Cost accounting and guard
Files: `internal/llm/pricing.go` (+test), pipeline budget stop, CLI `--estimate`, `--max-cost`, summary.
- [ ] Tests: price table math (in/out, batch 50%); unknown model → 0, known=false; `Result.CostUSD` set per frame; budget stop (`ErrBudget`) keeps results and stops dispatch; `cull --estimate` prints and exits without a key.
- [ ] Implement.

### Task 5: `decide`
Files: `internal/pipeline/decide.go` (+test), `internal/cli/decide.go`.
- [ ] Tests: re-decide changes decisions per new policy and reports counts; sidecars: rewrites ours, creates missing, never touches foreign ones; `--move-culled` sync moves new culls and restores un-culled; no backend needed.
- [ ] Implement.

### Task 6: Burst grouping
Files: `internal/group/group.go` (+test), pipeline/decide integration, policy duplicates.
- [ ] Tests: dHash stable & Hamming; grouping by time gap + hash; missing EXIF falls back to file order; best-of-group ordering; policy `Duplicates` action; scan records groups.
- [ ] Implement.

### Task 7: Tiered escalation
Files: `internal/pipeline/pipeline.go`, CLI flags, report `first_pass`, resume guard.
- [ ] Tests: escalates only matching statuses; final evaluation from second backend; first_pass recorded; usage/cost summed; guard refuses a different escalation.
- [ ] Implement.

### Task 8: Review sheet
Files: `internal/review/{review.go,page.html}` (+test), `internal/cli/review.go`, `dng` smallest-preview-above helper.
- [ ] Tests: writes index.html + thumbs for a synthetic report; page embeds data JSON; HTML contains controls and no external URLs; frames without focus box get no subject image; scan report (no decisions) renders.
- [ ] Implement; render a real sheet and check it in a browser screenshot if possible.

### Task 9: `calibrate`
Files: `internal/calib/calib.go` (+test), `internal/cli/calibrate.go`.
- [ ] Tests: CSV parsing (header optional, base-name matching, unknown labels rejected); confusion matrix and rates; sweep changes with threshold; multiple reports side by side.
- [ ] Implement.

### Task 10: Raw clipping
Files: `internal/rawclip/{ljpeg.go,raw.go}` (+tests with a test-only lossless-JPEG encoder), pipeline, policy.
- [ ] Tests: encoder→decoder round trip for predictors 1–7, 1 and 2 components; clip % against white level; DNG with raw IFD tags (WhiteLevel/BlackLevel) parsed; policy: preview-clipped + raw-unclipped → no review; raw-clipped → action.
- [ ] Verify on a real frame: decoded raw thumbnail matches the scene; time per frame.
- [ ] Implement; default on/off per measured cost.

### Task 11: Batches API
Files: `internal/llm/batch.go` (+test), `internal/pipeline/batch.go` (+test), CLI `--batch`.
- [ ] Read the claude-api skill's batch docs for exact shapes before writing.
- [ ] Tests (httptest fake batch server): create → poll → results mapping by custom_id in any order; chunking by size; state file written before submit and re-attached on resume (no second create); errored/expired items → frame errors; locate phase then eval phase.
- [ ] Implement.

### Task 12: `apply-c1`
Files: `internal/c1/{c1.go,script}` (+golden test), `internal/cli/applyc1.go`.
- [ ] Read the real dictionary with `sdef`; record the verified property/command names in CLAUDE.md.
- [ ] Tests: golden script for a synthetic report (rating/color/keyword; exposure only for fixable with `--exposure`; crop only for croppable with `--crop`); escaping of file names; dry-run default prints, `--run` shells to osascript (fake binary in test).
- [ ] Implement; compile-check with `osacompile` if the sandbox allows, else hand the command to the user.

### Task 13: Housekeeping minors
- [ ] One RED→GREEN test each for spec §12 items 1, 3, 4, 5; comments for 2 and 6.

### Task 14: Verification, docs, review
- [ ] Real frames (free): `scan` (lean decode timing/RSS, EXIF, groups, raw clip), `review` sheet, `decide`, local `cull` with eyes; subscription run of a few frames if useful.
- [ ] Paid (ask first): one tiny `--batch` run.
- [ ] README/CLAUDE.md; final fresh review; fix Critical/Important.
