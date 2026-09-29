# cull (gophotocull)

`cull` culls Leica M11-P DNGs using a vision model on the embedded JPEG preview.
Priority: **sharpness gates, exposure gets fixed, composition gets cropped.**
Go + [cobra](https://github.com/spf13/cobra).

The binary is `cull` (`make build` → `bin/cull`, `make install` → `$GOPATH/bin/cull`);
the repo and Go module stay `gophotocull`. Checked 2026-09-28: no `cull` on macOS or
in Homebrew, Debian, Ubuntu, Arch or Fedora, but npm, crates.io and PyPI each have a
`cull` package that installs its own `cull` command (npm's deletes files). If you
install one of those globally, make sure `which cull` is this one.

## Quick start

For a step-by-step walkthrough of a whole shoot, from folder to Capture One, see
[WORKFLOW.md](WORKFLOW.md).

```sh
make build
# 1. Verify previews first (no model calls): resolution, source, faces found.
#    --save-inputs shows exactly what the model would be sent.
./bin/cull scan --save-inputs /tmp/inputs /path/to/shoot
# 2. Evaluate (pick a backend; see below)
./bin/cull judge /path/to/shoot
# 3. Check, label and star frames in your browser; every keypress is saved to the
#    labels log and the frame's sidecar (Capture One reads it on import)
./bin/cull review /path/to/shoot
# Optional, before importing into Capture One: move culls aside, and undo it
./bin/cull judge --resume --move-culled /path/to/shoot
./bin/cull restore /path/to/shoot
```

| Command | Purpose |
|---|---|
| `scan <dir>` | Extract + measure previews, detect faces; writes report only, calls no model |
| `judge <dir>` | Evaluate with the model, apply policy, optional sidecars; ranks sequences at the end unless `--no-rank`; `--move-culled` moves culls (with their `.xmp`) into `culled/` beside them |
| `rank <dir>` | Rank the sets `judge` found (or skipped with `--no-rank`), without re-judging; see "Sequences and best of set" below |
| `decide <dir>` | Re-apply the policy to stored assessments (no model calls); regroups sequences and re-applies stored ranks for free; `--write-xmp` / `--move-culled` sync; `--labels` applies your verdicts and stars |
| `review <dir>` | Opens the contact sheet in your browser: subject crops, decisions, reasons; label keep/review/cull and 1–5 stars. Every change is saved to `cull-labels.jsonl` and the frame's sidecar (`--no-xmp`: log only; `--static`: offline page) |
| `calibrate REPORT...` | Agreement with your labels (the log beside the first report, or `--labels`): confusion matrix, false-cull / missed-cull / review rates, threshold sweep |
| `apply-c1 <dir>` | AppleScript for the open Capture One document (color tag, keyword; your stars with `--labels`; optional exposure/crop); dry run by default, `--probe` first |
| `restore <dir>` | Move frames that `--move-culled` moved back to where they were (never overwrites) |
| `version` | Build version (set via `make build`) |
| `completion <shell>` | Shell completion (cobra built-in) |

Global flags: `-o/--report`, `-r/--recursive`, `--max-edge`, `--tiles` ("where focus
landed" tiles, default 1, sent only when there is no subject crop unless
`--landed-with-subject`), `--face-min-q` (default 80), `--save-inputs <dir>`,
`--seq-gap` (60s; 0 disables sequence grouping) / `--seq-look` (0.08; see
"Sequences and best of set" below), `--min-preview-edge`.

Policy flags (`judge`, `rank`, `decide`, `calibrate`): `--review-below-sharpness`, `--eyes-closed`,
`--outranked`, `--raw-clipped` (each `ignore|review|cull`, default `review`),
`--raw-clip-threshold` (0.5 % of raw samples at white level), `--min-crop-area`,
`--keep-best` (default 3). The report stores the policy its decisions came from.
`decide`, `rank`, `calibrate` and `judge --resume` start from that stored policy and
say which non-default settings they're reusing; a flag you type overrides only its
own setting. So `decide --review-below-sharpness 7` sticks until you change it.

Cost and scale (`judge`): `--estimate` (print and exit), `--max-cost USD`, `--batch`
(Message Batches API: half price; Ctrl-C safe, `--resume` re-attaches), `--no-rank`
(skip end-of-run ranking; rank later with `cull rank`),
`--escalate-backend/--escalate-model/--escalate-on` (re-evaluate doubtful frames on a
stronger model), `--raw-clip` (on for judge, off for scan). Run `cull judge --help`.

Calibration loop: `judge` → `review` (label) → `calibrate` → tune with `decide`
(free). Culling pass: `review` (confirm or override the model, add stars) →
`decide --move-culled` → import into Capture One.

### Review sheet and labels

`cull review <dir>` builds the sheet, serves it on 127.0.0.1 (a port fixed
per report, `--port` to pick; a per-session token in the printed URL) and opens it
in your browser (`--no-open` to skip). The header shows the shoot folder and the
labels log. Keys: **K/R/C** keep/review/cull (advance), **U** clears, **1–5** stars
(advance only under the *Unrated* filter), **0** clears stars, **←/→** one frame,
**↑/↓** one grid row (one frame in the detail view), Enter/Esc. Each card's top-right
badge (thin outline) is your label; the bottom one is the model's verdict. Filters
combine a verdict (All/Keep/Review/Cull: your label, else the model's) with progress
(All/Unlabeled/Unrated/Disagreements), e.g. Keep + Unrated to star the keepers. A
frame you change stays on screen until you move on. If the server is unreachable,
changes queue in the browser and are sent when it's back: restart `review` and
open the new URL (same port, so the same browser storage).

Every change is appended to `cull-labels.jsonl` beside the report, one line
per change; the last line per file wins:

```jsonl
{"file":"L1000123.DNG","label":"keep","stars":4,"at":"2026-09-27T20:14:09-07:00"}
```

Your labels never replace the report's decisions (the report stays the model's
record). Everything that writes sidecars or moves files uses them by default when
the log exists beside the report — `decide`, `apply-c1`, and `judge --move-culled` /
`--write-xmp` — so your verdict wins where you gave one and your stars become the
rating; `--labels <path>` points elsewhere, `--no-labels` ignores them. `calibrate`
reads the same log. If frames in a `-r` run share a file name, labels can't say
which one they mean: `decide`/`apply-c1` refuse, `judge` falls back to the model.
With `--static` the page works offline from `index.html` and keeps labels in the
browser; export/import them as the same JSONL. Don't run `judge` on a report while
reviewing it: both write the report.

### Backends (`judge --backend`)

| Backend | Model default | Auth / cost | Notes |
|---|---|---|---|
| `anthropic` (default) | `claude-sonnet-5` | API key, billed per token | `--api-key-file` > `$ANTHROPIC_API_KEY` > `~/.anthropic/api_key`, `~/.config/anthropic/api_key`, `~/.anthropic_api_key`, `~/.anthropic` (warns if group/world readable) |
| `claude-code` | `sonnet` | your Claude subscription via `claude -p` | never uses an API key (aborts if claude reports one); stops cleanly at `--quota-stop` (default 0.9 of the 5-hour window) — rerun with `--resume` later |
| `openai` | required (`--model`) | optional key (`--openai-key-file`, `$OPENAI_API_KEY`) | any OpenAI-compatible server at `--base-url` (default `http://127.0.0.1:8000/v1`). Streams and hangs up once the JSON closes (`--openai-stream=false` to disable) |

`--resume` refuses a report written by a different backend or model: use `-o` to
keep one report per backend when comparing them.

`--move-culled` is meant for culling before import: moving files that Capture One
already references makes them show as missing. Moves are same-disk renames that
never overwrite; each is recorded as `moved_to` in the report, `culled/` folders are
skipped by later runs, and `cull restore` puts everything back. Until the
sharpness gate is calibrated, look through `culled/` before deleting anything.

## Sequences and best of set

The user shoots **sequences** far more than bursts: the same subject or scene over
tens of seconds to minutes, with small changes in pose, expression, framing or
distance. `judge` groups similar frames and asks the model to pick the strongest
few, instead of scoring every frame in isolation.

**Grouping.** Frames are ordered by capture time, then file name (frames without a
capture time sort by file name among their neighbours). A frame joins the previous
frame's set when both hold:

- the gap to the previous frame is ≤ `--seq-gap` (default **60s**; ignored when
  either frame has no capture time — rewritten, near-identical times leave the look
  to decide; `0` disables sequence grouping entirely);
- its look distance to the **previous** frame (not the first frame of the set) is ≤
  `--seq-look` (default **0.08**). The look is an 8×8 grid of mean RGB from the
  preview, levelled for exposure and compared over small shifts, so small reframing
  or zoom stays close while a different scene or subject is far apart. See the
  measurement table in `CLAUDE.md` for how the default was chosen.

A set is capped at 40 frames, so a slow pan can't chain a whole walk into one set.
Frames that failed the sharpness gate stay in their set (visible in `review`) but
aren't ranked.

**Ranking.** Once every frame in a set (of at least 2 rankable frames) is judged,
one model call compares them side by side — the model never scored them in
isolation, so it isn't anchored on those scores. Each frame sends a full-frame view
(768px) and, when a focus target was found (face detection or the model's locate
call), its crop at native resolution (capped at 512px) — both re-extracted from the
DNG at rank time. The rubric, in priority order:

1. subject sharpness where it matters (the eyes);
2. eyes and expression (open, engaged, natural; not mid-blink or mid-word);
3. gesture and moment;
4. composition and background (framing, horizon, edge distractions, cropped limbs);
5. exposure only if it can't be fixed — fixable exposure never counts against a frame.

A set larger than 8 frames is split into nearly equal chunks (≤ 8 frames per call),
each ranked, then the top finishers from each chunk go to one final call — a full
40-frame set with the default `--keep-best` (3) makes 5 chunk calls of 8 plus 1
final call of 5: 6 calls total. Go, not the model, then keeps the top N:
**`--keep-best`** (default **3**, range 0–5; `0` means
rank only, nothing is demoted) and **`--outranked`** (`ignore|review|cull`, default
**`review`**) decide what happens to the rest. Ranking only ever *demotes*: a kept
frame can become review or cull, but review and cull are never promoted back to
keep, and nothing is promoted past keep.

A set the model didn't rank (`--no-rank`, a failed call, or the cost budget) falls
back to ordering by the frames' own scores (sharpness, open eyes, composition,
exposure) — the same `--keep-best`/`--outranked` policy still applies, with the
reason noting it wasn't compared.

**Commands.** `judge --no-rank` skips ranking (judge every frame, rank later).
`cull rank <dir>` ranks the sets in an existing report without re-judging — useful
after `--no-rank`, after tuning `--keep-best`/`--outranked`, or to rank an older
schema-v3 report (it computes any missing look fingerprints from the DNGs first,
free, about 1s/frame). It takes `--force` (re-rank every set, even one that already
has a model order), `--estimate` (exact call count and cost, no model calls),
`--max-cost`, and `--batch` (anthropic only, half price through the Message Batches
API; `--batch-poll`, default 30s, sets how often it checks progress). With `--batch`
and no `--backend`, `rank` defaults to anthropic. Without `--backend`/`--model`,
`rank` defaults to whatever the report was judged with.

A `--batch` ranking run is re-attachable: Ctrl-C leaves `<report>.rank-batch.json`
beside the report, and rerunning `cull rank --batch` picks up where it left off
(a sync `judge` with ranking on, or a sync `cull rank`, refuses to run while that
file exists, since it would pay for the same sets again); delete the file to
abandon it instead.

**Reuse.** A set's stored order is reused — no new model call — while every
currently rankable member of the set appears in it. A member that drops out (say, a
frame newly culled by a policy change) is simply removed from the stored order; the
relative order of the rest stays valid. A new or newly rankable member makes the
set unranked again until the next `cull rank`. `cull decide` re-applies stored
orders (and regroups sequences) for free — it never calls a model — so retuning
`--keep-best` or `--outranked` after ranking doesn't cost anything. Regrouping
can lose paid orders:

- A regroup that **merges** two ranked sets (a changed `--seq-look` or `--seq-gap`)
  keeps only the first-matched set's order, summary and cost. The merged set falls
  back to "scores" and needs `cull rank` again.
- `decide --seq-gap 0` turns grouping off and saves the report with no sets, which
  drops **every** stored order. Going back to a normal gap regroups, but every set
  is unranked until `cull rank` runs again.
- A **split** keeps each piece's order; the set's cost is counted once, on the first
  piece.

What was paid always stays in the report's running cost total.

**Estimates.** `judge --estimate` adds an approximate ranking cost that assumes every
frame lands in a full 8-frame set (~10k in / ~1k out tokens per call). It is neither
a bound nor exact, because set sizes aren't known before judging: pairs and small
sets cost more per frame, and frames in no set cost nothing. `--max-cost` is the hard
cap. `cull rank --estimate`
is exact, because the sets are already known.

**Sidecars, Capture One and the review sheet.** A set's best frame(s) get the
keyword `cull:best`, in sidecars and in `apply-c1`. `calibrate` gains a sets
section over multi-frame sets containing labelled frames: how often a frame you
labelled keep got ranked out of the best cut, how often one you labelled cull or
review got ranked into it, and a `--keep-best` 1–5 sweep recomputed from the stored
ranks, without new model calls. The review sheet marks a set frame with a
"set N · #rank/of" badge and a distinct "best" marker, adds a **Sets: All / Best /
Outranked** filter, and the detail view shows the frame's rank with the model's
strength/weakness notes, the set's summary, and a filmstrip of the set's thumbnails
(click one to jump to it, if it's visible under the current filters).

## Pipeline

1. `internal/dng` — walks TIFF IFDs/SubIFDs for the largest reduced-resolution JPEG
   (reads IFDs + preview bytes only, never raw data). On the M11-P that is a
   full-resolution 9504×6320 preview. Falls back to `exiftool` below `--min-preview-edge`.
2. `internal/imageprep` — applies EXIF orientation; downsizes the full frame to
   `--max-edge`; crops at native resolution; measures luma percentiles and clipping.
3. `internal/focus` — what should be sharp, and where focus landed:
   - pigo face detection (embedded cascades); the most confident face with
     Q ≥ `--face-min-q` is the target, centred on the eyes when found;
   - otherwise `judge` asks the model to locate the intended focus target on a small
     frame (`--locate off` to skip);
   - the subject crop (768–1536 px, native) plus `--tiles` regions with the most fine
     detail relative to local contrast — raw Laplacian variance rewards contrast, not
     focus, and picks sunlit out-of-focus foreground.
4. `internal/llm` — one structured call (system prompt, text + JPEG parts, JSON
   Schema) implemented by the `anthropic`, `claude-code` and `openai` backends; every
   answer is schema-validated in Go, with one retry.
5. `eval.Policy` — **deterministic decision in Go**, not the model's call:
   - `missed_focus` / `motion_blur` → cull; `soft` → review; optional score floor
   - raw clipping (pure-Go decode of the DNG's lossless-JPEG raw) decides exposure:
     preview "clipped" with raw headroom → no review; raw ≥ threshold → `--raw-clipped`
   - closed eyes → `--eyes-closed`; frames ranked below `--keep-best` in their
     sequence → `--outranked`
   - composition never culls; invalid or < `-min-crop-area` crops are dropped
6. `internal/report` — JSON is the source of truth (schema v4; checkpointed every
   `--checkpoint` results; `--resume` keys on path+size+mtime). Filter it with `jq`,
   e.g. `jq '.results[] | select(.decision=="cull") | .file'`.

## Write-back

| Target | Rating / label / keyword | Exposure / crop |
|---|---|---|
| XMP sidecar → Capture One | yes, read on import (verified with C1 16.7.2); Image › Sync Metadata after import | **no** — C1 doesn't reliably translate `crs:` settings |
| XMP sidecar → Lightroom Classic | ignored for DNG (LR uses embedded XMP) | ignored |
| `apply-c1` (AppleScript) | yes | yes |

What gets written (`review` unless `--no-xmp`; `judge`/`decide` with `--write-xmp`; `apply-c1`):

| Field | Value |
|---|---|
| Rating | your stars only; left out when you haven't rated (the model never sets stars) |
| Colour | the verdict — yours if you labeled, else the model's: keep **Green**, review **Yellow**, cull **Red** (C1 colour tags 4/3/1 in `apply-c1`) |
| Keywords | `cull:<verdict>`, plus `cull:labeled` when the verdict is yours, plus `cull:best` for a set's best frame(s) |

Sidecars not written by `cull` are never overwritten without `--overwrite-xmp`.

`-xmp-develop` writes `crs:Exposure2012` and `crs:Crop*` for ACR/Bridge-style consumers.

## Known gaps / TODO

- [x] Verify M11-P preview dimensions: full resolution (9504×6320), no raw rendering needed.
- [ ] Calibrate the sharpness gate and `--face-min-q` against hand labels (tooling done:
      `review` → `calibrate` → `decide`).
- [x] Raw-level clipping check (pure Go, no LibRaw).
- [x] `apply-c1`: names verified against C1 16.7.2's dictionary (compiles with osacompile);
      run `--probe` to confirm color-tag numbering and image naming before writing.
- [ ] `crs:Crop*` coordinate space for rotated images is an unverified assumption
      (stored orientation); `crs:CropAngle` not written.
- [x] Message Batches API mode (`--batch`).
- [x] Sequence grouping and best-of-set ranking (`--seq-gap`, `--seq-look`, `--keep-best`,
      `--outranked`, `cull rank`), replacing burst/near-duplicate grouping.
