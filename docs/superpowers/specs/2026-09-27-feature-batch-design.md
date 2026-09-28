# Feature batch: calibration tooling, scale, photographic features — design

Date: 2026-09-27 · Status: user asked to "build it all now" after the suggestion list;
decisions below are the executor's, with conservative defaults, reported to the user.

## Scope (all from the 2026-09-27 suggestion list)

1. Review sheet (`gophotocull review`) — HTML contact sheet + labeling, exports labels CSV.
2. `decide` — re-apply policy to stored evaluations, no model calls.
3. Eyes / expression assessment + policy gate.
4. EXIF context (shutter, aperture, ISO, focal length, lens, capture time).
5. Cost guard: per-result cost, pre-run estimate, `--max-cost`, `cull --estimate`.
6. Message Batches API mode (`cull --batch`, anthropic).
7. Tiered models (`--escalate-*`).
8. Lean decoding (~1 GB → a few hundred MB per in-flight frame).
9. Burst / near-duplicate grouping.
10. `apply-c1` (generate Capture One script; dry-run default).
11. Raw-level clipping (pure-Go lossless-JPEG decode of the DNG raw; no LibRaw).
12. Housekeeping: the six deferred review minors.
Plus `calibrate` (roadmap #2), which consumes the review sheet's labels.

Non-goals: running `apply-c1` against the user's catalog without explicit approval;
paid live tests without approval; deleting anything.

## Cross-cutting

- Report `SchemaVersion` 3 (evaluation gains `people`; results gain `exif`, `dhash`,
  `group`, `raw_clip`, `cost_usd`, `first_pass`). Resume guard unchanged in spirit
  (schema/backend/model/escalation must match).
- `eval.Policy` stays the single place decisions are made. New fields, all with
  conservative defaults (never cull on a new signal unless asked):
  - `ReviewBelowSharpness float64` (0 = off): keep → review below this score.
  - `EyesClosed`, `Duplicates`, `RawClipped` — each an `Action`: `ignore | review | cull`
    (defaults: `review`, `review`, `review`).
  - `RawClipThreshold float64` (percent of raw samples at white level; default 0.5).
  Flags on `cull` and `decide`: `--review-below-sharpness`, `--eyes-closed`,
  `--duplicates`, `--raw-clipped`, `--raw-clip-threshold`.
- Per-call cost: a list-price table for Anthropic models (from the claude-api skill,
  cached 2026-06-24; batch = 50%). `Result.CostUSD` sums the frame's calls; unknown
  models (openai/local) cost 0 and are labelled as such.

## 1. Review sheet

`gophotocull review <dir> [--out DIR] [-j N]` reads the report and writes
`DIR/index.html` (default `<dir>/gophotocull-review/`) plus per-frame JPEGs:
`<base>.thumb.jpg` (from the smallest embedded preview with long edge ≥ 1500,
downscaled to 1200) and `<base>.subject.jpg` (native-resolution crop of the recorded
focus-target box, from the full preview, capped at 1024 px). Self-contained page
(inline CSS/JS, no network): cards with image, subject crop, decision, reasons, the
model's focus/eyes notes, EXIF line, group. Keyboard: ←/→ move, `k` keep, `c` cull,
`r` review, `u` clear; filters: all / keep / review / cull / unlabeled / disagreements
(label ≠ decision). Labels persist in `localStorage` keyed by report path; "Export
labels.csv" downloads `file,label`; "Import" reads one back. Works on `scan` reports
(no decisions) for pure labeling. The user opens it in a browser.

## 2. `decide`

`gophotocull decide <dir>` loads the report, re-runs grouping (9) and `Policy.Decide`
on every stored evaluation, prints a change summary (`keep→review: 8 …`), saves.
`--write-xmp` rewrites sidecars the report says we wrote (`xmp` field set) and writes
new ones for frames without; never touches other sidecars unless `--overwrite-xmp`.
`--move-culled` syncs: newly culled frames move; moved frames no longer culled are
restored. No model calls, no API key needed.

## 3. Eyes / expression

Evaluation schema gains required
`people: {present: bool, eyes: open|closed|partial|not_visible, expression: good|neutral|awkward|not_applicable}`.
Prompt: judge from the subject crop; `partial` = mid-blink. Policy: `eyes == closed`
→ `EyesClosed` action; `partial` → reason only; expression → reason only (subjective).

## 4. EXIF

`internal/dng` parses IFD0 `Make/Model` and the Exif IFD (0x8769): ExposureTime,
FNumber, ISO (0x8827), FocalLength, LensModel (0xA434), DateTimeOriginal (0x9003),
SubSecTimeOriginal (0x9291); RATIONAL/ASCII/SHORT readers. Missing tags are simply
absent (uncoded Leica lenses report no focal length). Stored as `result.exif`; the
stats text sent to the model gains e.g. `1/125 s, f/1.4, ISO 400, 50 mm, <lens>`.
Verify tag presence on the real frames.

## 5. Cost guard

- `cull` with `--backend anthropic` prints `estimate: N frames ≈ $X list price
  (≈ in/out tokens per frame from measured runs)` before starting.
- `cull --estimate` prints that and exits (no API calls, no key needed for the estimate).
- `--max-cost USD`: after each result, if the run's summed `CostUSD` ≥ max → stop
  dispatch like the quota stop (`llm.ErrBudget`), in-flight frames finish, resume later.
- Summary prints the run's cost.

## 6. Batches API (`cull --batch`, anthropic only)

Two-phase, disk-backed so nothing large lives in memory and a restart re-attaches:
1. Prepare every frame (decode, detect). Frames with no face and `--locate model` get a
   locate request → submitted as batch(es) → poll → boxes.
2. Build every evaluation request → batch(es) → poll → results → policy → report.
Requests are written to JSONL chunks capped at 200 MB (API limit 256 MB) and 10k
requests. State (`<report>.batch.json`: phase, batch IDs, custom_id→file) lets
`cull --batch --resume` re-attach instead of resubmitting (no double spend). Poll
interval 30 s with progress. Failed/expired/invalid items become frame errors
(resume without `--batch` to redo). Cost at 50%. `--max-cost` is checked against the
estimate before submitting (batches can't be stopped midway cheaply).

## 7. Tiered models

`--escalate-backend B --escalate-model M [--escalate-on soft,missed_focus,motion_blur,eyes_closed]`:
first pass with the primary backend; frames matching re-evaluated on the escalation
backend with the same inputs (no second locate). Final evaluation is the escalated
one; `result.first_pass = {backend, model, evaluation}`. Report records
`escalation` (`B/M`), part of the resume guard. Cost summed per call.

## 8. Lean decoding

Keep the decoded `*image.YCbCr` in stored orientation; `Frame.Luma` becomes the Y plane
rotated for display (`[]uint8`, 60 MB) instead of RGBA + float32 (~480 MB). Crops and the
downscaled frame are cut/scaled from the YCbCr in stored coordinates and only the small
result is converted to RGB and rotated. `Measure` converts per pixel on the fly.
`focus` works on `[]uint8`. Non-YCbCr JPEGs (grayscale) handled. Target: measured peak
RSS per frame at least halved; `scan` faster. Orientation tests (1/3/6/8) must hold.

## 9. Burst / near-duplicate grouping

Per frame: 64-bit dHash of the downscaled frame (`result.dhash`). After all frames (and
in `decide`): sort by capture time (EXIF, file name when missing); consecutive frames
within `--burst-gap` (default 2 s) whose dHash Hamming distance ≤ `--burst-hash`
(default 12) form a group. Best frame = highest sharpness score, then eyes open, then
composition, exposure. Others get `group {id, size, best}` and the `Duplicates` action
with reason `duplicate of X (burst of N)`. `scan` records groups (no decisions).

## 10. `apply-c1`

Read the real dictionary (`sdef "/Applications/Capture One.app"`) before writing it.
`gophotocull apply-c1 <dir>` generates an AppleScript for the open Capture One
document: per variant matched by file name, set rating/color tag/keywords, and (flags)
exposure for `fixable`, crop for `croppable`. Default prints the script (dry run);
`--run` pipes it to `osascript` (the user runs it; sandbox blocks Apple Events).

## 11. Raw clipping

Pure-Go lossless-JPEG (ITU T.81 process 14, predictors 1–7, any component count)
decoder for the DNG's raw strip/tiles; read WhiteLevel (0xC61D), BlackLevel (0xC61A),
CFAPattern where present. `result.raw_clip = {highlight_pct, channel_pct[]}`. Policy:
exposure `clipped` with raw clip < threshold → no review (preview overstated it);
raw clip ≥ threshold → `RawClipped` action. `--raw-clip` (default on for `cull`, off
for `scan`) once timing is measured. Verified by decoding a real frame and rendering a
grayscale thumbnail that must match the preview.

## 12. Housekeeping (deferred minors)

1. `validated`: keep `ErrQuotaStop` when the same call's JSON is invalid.
2. Quota stop during locate: comment the "one call past threshold" behaviour.
3. Claude Code `allowed_warning`: treat any `allowed*` status as allowed; threshold decides.
4. OpenAI: a 200 with a non-JSON body is not retried.
5. Resume refusal after a `scan` report says so plainly.
6. Landed noise estimate: comment the small-grid behaviour.

## `calibrate`

`gophotocull calibrate --labels labels.csv REPORT...`: labels matched by base name;
per report: confusion matrix (label keep/review/cull × decision), false-cull rate
(label keep → cull), missed-cull rate (label cull → keep), review rate; plus a sweep of
`--review-below-sharpness` 0–10 re-deciding with the stored evaluations.
