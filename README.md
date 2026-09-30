# cull

`cull` sorts a folder of DNG raw files into **keep**, **review** and **cull**. A vision
model assesses each frame's embedded JPEG preview, and deterministic Go rules make
the decision. Its priorities:

- **Sharpness gates.** A missed-focus or motion-blurred frame is culled.
- **Exposure gets fixed, not culled.** Fixable exposure only produces a suggested
  correction.
- **Composition gets cropped, never culled.** A weak composition only produces a
  suggested crop.

It also groups similar frames into sets and ranks each set side by side, so you keep
the best few of a sequence. Results go to XMP sidecars and to Capture One.

For a step-by-step walkthrough of a shoot, from folder to Capture One, see
[WORKFLOW.md](WORKFLOW.md).

- [Camera support](#camera-support)
- [Install](#install)
- [Quick start](#quick-start)
- [Commands](#commands)
- [How decisions are made](#how-decisions-are-made)
- [Sequences and best of set](#sequences-and-best-of-set)
- [Reviewing and labelling](#reviewing-and-labelling)
- [Model backends](#model-backends)
- [Cost control](#cost-control)
- [Output: sidecars, Capture One and moving culls](#output-sidecars-capture-one-and-moving-culls)
- [Flag reference](#flag-reference)
- [Limitations](#limitations)

## Camera support

`cull` never renders the raw data. It judges sharpness on the **JPEG preview embedded
in the DNG**, so a camera works only if its DNGs embed a large preview. Full
resolution is best: focus is judged on a crop at the preview's native resolution.
The brand doesn't matter; the camera's DNG writer does.

| Result | Cameras tested |
|---|---|
| **Works: full-resolution preview** | Leica M11-P, M10, M10-R, Q2, SL2, CL; Pentax K-1 Mark II, K-3 Mark III; Ricoh GR III; Sigma fp; Apple iPhone 12 Pro (ProRAW); Samsung Galaxy S23 Ultra; Adobe DNG Converter output with a full-size preview |
| **Usable: reduced preview** (2560 px, about a third of the sensor width) | Google Pixel 8 Pro. Focus is judged on less detail than the sensor recorded. |
| **Unreliable: small preview** (≤ 960 px, flagged "focus judgement unreliable") | Apple iPhone XS, Google Pixel 4a, DJI drones (Mini 2, Mavic 3 / Hasselblad L2D-20c) |
| **Doesn't work: no usable preview** | Leica M9 (320 px uncompressed thumbnail only), Leica M (Typ 240) and M Monochrom (Typ 246) (160 px thumbnail only), OnePlus 6T (no preview) |

Tested on 22 sample files from [raw.pixls.us](https://raw.pixls.us) with
`cull scan`, which extracts previews, EXIF and raw clipping without calling a model.

- **Check your own camera** with `cull scan <dir>`. It reports each preview's size,
  and flags any preview smaller than `--min-preview-edge` (1500 px). When the file's
  own structure yields nothing large enough, `cull` also tries `exiftool` if it's
  installed.
- **Workaround for cameras without a large preview:** convert the files with Adobe
  DNG Converter, with *JPEG Preview: Full Size*. That output embeds a full-resolution
  preview. This wasn't tested on the failing cameras above.
- **Raw highlight clipping** (see [How decisions are made](#how-decisions-are-made))
  needs a raw that is stored as **striped lossless JPEG**. That covers the Leica M10,
  M10-R, M11, M (Typ 240) and Monochrom (Typ 246), and the Pentax and Ricoh bodies
  tested. Other layouts (tiled, uncompressed, lossy DNG) fall back to judging clipping
  on the preview, which overstates it.
- **The prompts name each frame's camera** from its EXIF ("a Canon EOS 5D Mark III",
  "an Apple iPhone 12 Pro"). Leica M bodies are also described as manual-focus
  rangefinders, where missed focus is a common failure.

## Install

Requires Go 1.22+.

```sh
make build                                            # bin/cull, version stamped from git
make install                                          # $GOPATH/bin/cull
go install github.com/jefflaplante/cull/cmd/cull@latest
```

- **Optional:** `exiftool`, which finds previews in unusual locations.
- **For the `claude-code` backend:** the `claude` CLI, logged in to your Claude
  subscription.
- **Name clash:** npm, crates.io and PyPI each have an unrelated package that installs
  a `cull` command, and npm's deletes files. If you have one of those installed, check
  that `which cull` is this one.

## Quick start

```sh
cull scan ~/Pictures/shoot                # free: previews, faces, EXIF; no model calls
cull judge --estimate ~/Pictures/shoot    # free: what judging would cost
cull judge ~/Pictures/shoot               # model assessment + decisions + set ranking
cull review ~/Pictures/shoot              # browser: check, label keep/review/cull, add stars
cull decide --write-xmp --move-culled ~/Pictures/shoot   # sidecars; culls into culled/
```

Everything is recorded in `cull-report.json` beside the photos. `cull restore <dir>`
undoes `--move-culled`.

## Commands

| Command | What it does | Calls a model |
|---|---|---|
| `scan <dir>` | Extract previews, EXIF, faces and look fingerprints; write the report | no |
| `judge <dir>` | Assess every frame, decide keep/review/cull, then rank the sets | yes |
| `rank <dir>` | Rank the sets in an existing report (after `judge --no-rank`, or after new frames) | yes |
| `decide <dir>` | Re-apply the policy to stored assessments; regroup sets; write sidecars or move culls | no |
| `review <dir>` | Browser contact sheet for checking, labelling and rating frames | no |
| `calibrate <report>...` | Compare a report's decisions with your labels; sweep thresholds | no |
| `apply-c1 <dir>` | AppleScript that applies verdicts, stars and keywords in Capture One (dry run by default) | no |
| `restore <dir>` | Move frames that `--move-culled` moved back where they were | no |
| `version`, `completion` | Build version; shell completion | no |

Every command has `--help` with examples.

## How decisions are made

For each DNG:

1. **Preview.** The largest embedded JPEG preview is read directly from the file's
   TIFF structure; the raw data isn't read for this. EXIF orientation is applied.
2. **Focus target.** Face detection ([pigo](https://github.com/esimov/pigo)) finds the
   subject, centred on the eyes. With no confident face (`--face-min-q`, default 80),
   `judge` asks the model to locate the intended focus target (`--locate off` to skip).
3. **Assessment.** The model receives:
   - the full frame, downscaled to `--max-edge` (1568 px);
   - the subject cropped at native resolution;
   - when there's no subject crop, a "where focus landed" tile (`--tiles`).

   It returns structured scores for sharpness, exposure, composition, and people
   (eyes, expression). Every answer is checked against a JSON Schema, with one retry.
4. **Decision in Go** (`eval.Policy`). The model only assesses; these rules decide:
   - `missed_focus` or `motion_blur` → **cull**; `soft` → **review**. Optionally,
     sharpness below `--review-below-sharpness` → review.
   - Closed eyes → `--eyes-closed` (default review).
   - Raw highlight clipping at or above `--raw-clip-threshold` → `--raw-clipped`
     (default review). When the raw shows headroom, a "clipped" preview isn't held
     against the frame.
   - Composition never culls. A suggested crop is dropped if it keeps less than
     `--min-crop-area` of the frame.
   - Frames ranked below `--keep-best` in their set → `--outranked` (default review).
5. **Report.** `cull-report.json` is the source of truth: assessments, decisions,
   reasons, sets, cost, and the policy used. It is checkpointed as `judge` runs, and
   `--resume` skips files already done. `jq` works on it, e.g.
   `jq '.results[] | select(.decision=="cull") | .file' cull-report.json`.

## Sequences and best of set

Similar frames (the same subject or scene over seconds to minutes) are grouped into
**sets**. Each set is ranked by one side-by-side model call, and Go keeps the best few.

**Grouping.** Frames are ordered by capture time, then file name. A frame joins the
previous frame's set when both hold:

- **Time:** it was taken within `--seq-gap` (default 60 s) of the previous frame. The
  gap is ignored when either frame has no capture time. `--seq-gap 0` turns grouping
  off.
- **Look:** its look distance to the previous frame is at most `--seq-look` (default
  0.08). The look is an 8×8 colour grid of the preview, levelled for exposure and
  compared over small shifts. Small reframing and exposure changes stay close; a new
  scene or subject doesn't.

A set holds at most 40 frames. At the default threshold, only nearly identical
framings link, so retakes of a pose after reframing usually form separate sets. Raise
`--seq-look` to group more loosely.

**Ranking.** Each frame is sent as a 768 px full frame plus its subject crop, with no
file names and no scores. The rubric, in order:

1. subject sharpness where it matters (the eyes);
2. eyes and expression;
3. gesture and moment;
4. composition and background, judged on the full frame;
5. exposure only if it can't be fixed.

A set of more than 8 frames is split into chunks of at most 8. The top frames of each
chunk then meet in one final call; a 40-frame set takes 6 calls. Frames that failed
the sharpness gate stay in the set but aren't ranked.

**Keeping the best.**
- `--keep-best` (default 3, range 0–5; 0 ranks without demoting) sets how many of
  each set stay keep. The rest get `--outranked` (`ignore`, `review` or `cull`;
  default `review`).
- Ranking only demotes keep; it never promotes a frame.
- A set that isn't ranked (`--no-rank`, a failed call, the cost limit) is ordered by
  the frames' own scores instead, and the reason says so.
- The best frames get the keyword `cull:best`.

**Reuse.** A set's ranking is reused, with no new call, while every rankable frame in
it is still covered. A frame dropping out keeps the order; a new frame makes the set
unranked until the next `cull rank`. `cull decide` regroups and re-applies stored
rankings for free, so changing `--keep-best` or `--outranked` costs nothing.
The report also stores the grouping (`--seq-gap`, `--seq-look`): later `decide`, `rank`
and `judge --resume` runs regroup with it unless you type those flags again.
Regrouping can lose rankings:
- when two ranked sets merge, only the first one's order is kept;
- `--seq-gap 0` drops them all.

A split set keeps its order. What was paid always stays in the report's cost total.

**Ranking separately.**
- `cull rank <dir>` ranks sets that have no current ranking. `--force` re-ranks all of
  them, `--estimate` gives the exact call count and cost, and `--batch` runs it at
  half price.
- It uses the backend and model the report was judged with, unless you pass others.
  `--batch` implies anthropic.
- An interrupted `--batch` ranking leaves `cull-report.json.rank-batch.json`.
  Re-running `cull rank --batch` re-attaches without paying twice; deleting the file
  abandons it.

## Reviewing and labelling

`cull review <dir>` builds a contact sheet and serves it on `127.0.0.1`, with a
per-session token, then opens your browser.
- **Saving:** every change is saved at once, to `cull-labels.jsonl` beside the report
  and to the frame's XMP sidecar.
- **Other modes:** `--no-xmp` saves only the labels log. `--static` writes an offline
  page that keeps labels in the browser, with JSONL export and import.

| Key | Action |
|---|---|
| ← → | previous / next frame |
| ↑ ↓ | one grid row (one frame in the detail view) |
| Enter / Esc | open the detail view / back to the grid |
| K / R / C | label keep / review / cull, and advance |
| U | clear the label |
| 1–5 / 0 | set stars / clear them |

**Filters** combine three rows:
- **Verdict:** your label, else the model's.
- **Progress:** unlabelled, unrated, and where you disagree with the model.
- **Sets:** all, best, outranked.

For example, *Keep + Unrated* shows the keepers you haven't starred yet.

**On each card and in the detail view:**
- The model's verdict is outlined, top right; your label sits bottom right.
- A frame in a set shows "set N · #rank/of", with a *best* marker on the best frames.
- The detail view shows the model's reasons, the frame's rank with its strengths and
  weaknesses, and a filmstrip of its set.

**The labels log** is append-only; the last line for a file wins:

```jsonl
{"file":"L1000123.DNG","label":"keep","stars":4,"at":"2026-09-27T20:14:09-07:00"}
```

Your labels never change the report's decisions: the report stays the model's
record. Everything that writes sidecars or moves files uses them by default, so your
verdict wins and your stars become the rating. That covers `decide`, `apply-c1`, and
`judge --write-xmp` / `--move-culled`. `--labels <path>` reads the log from somewhere
else; `--no-labels` ignores it. `calibrate` reads the same log.

**Calibrating.**
1. Label a sample in `review`.
2. Run `cull calibrate <dir>/cull-report.json`. It reports:
   - the false-cull rate (you kept, it culled) and missed-cull rate;
   - the review rate;
   - a `--review-below-sharpness` sweep;
   - a `--keep-best` sweep for sets.
3. Apply the settings you pick with `cull decide`. The report stores that policy, so
   later `decide`, `rank`, `calibrate` and `judge --resume` runs start from it. A flag
   you type overrides only its own setting.

## Model backends

| `--backend` | Default model | Auth and billing |
|---|---|---|
| `anthropic` (default) | `claude-sonnet-5-5` | API key, billed per token. Looked up in `--api-key-file`, `$ANTHROPIC_API_KEY`, `~/.anthropic/api_key`, `~/.config/anthropic/api_key`, `~/.anthropic_api_key` |
| `claude-code` | `sonnet` | Your Claude subscription, via `claude -p`. Never uses an API key. Stops at `--quota-stop` (default 0.9 of the 5-hour or 7-day window); continue later with `--resume` |
| `openai` | required (`--model`) | Any OpenAI-compatible server at `--base-url` (default `http://127.0.0.1:8000/v1`); key optional. Streams, and stops as soon as the JSON is complete |

- A model missing from `cull`'s price table is refused with `--max-cost` (it would count as $0), and warned about otherwise.
- `--resume` refuses a report written by a different backend or model. Use `-o` to
  keep one report per backend when comparing them.
- `--escalate-backend` / `--escalate-model` re-assess doubtful frames on a stronger
  model; `--escalate-on` picks the outcomes that escalate.
- Small local vision models judge focus poorly. Calibrate before trusting one.

## Cost control

- `judge --estimate` and `rank --estimate` print the cost without calling a model or
  needing a key.
  - `rank`'s figure is exact.
  - `judge`'s includes an approximate ranking cost that assumes 8-frame sets. It
    usually overestimates.
- `--max-cost USD` stops the run once it has spent that much. Continue with `--resume`.
- A re-run without `--resume` refuses to replace a report that holds assessments:
  `--fresh` (on `judge` and `scan`) starts over, and `-o` writes a separate report.
  Even `--fresh` is refused while the report records frames moved into `culled/`.
- `--batch` (anthropic) uses the Message Batches API: half price, with results within
  minutes to hours. Ctrl-C is safe; `--resume` re-attaches to batches already paid for.
- Costs so far are recorded in the report; `judge` and `rank` print them when they
  finish.

## Output: sidecars, Capture One and moving culls

**XMP sidecars** (`L1000123.xmp` beside `L1000123.DNG`) are written:
- by `review`, unless `--no-xmp`;
- by `judge` and `decide`, with `--write-xmp`.

Sidecars that `cull` didn't write are never overwritten without `--overwrite-xmp`.

| Field | Value |
|---|---|
| Rating | your stars only; left out when you haven't rated (the model never sets stars) |
| Label | the verdict (yours if you labelled, else the model's): keep **Green**, review **Yellow**, cull **Red** |
| Keywords | `cull:<verdict>`; `cull:labeled` when the verdict is yours; `cull:best` for a set's best frames |

`--xmp-develop` also writes `crs:Exposure2012` and `crs:Crop*` for Adobe Camera Raw
and Bridge.

| Target | Rating, label, keywords | Exposure, crop |
|---|---|---|
| Capture One, from sidecars | read on import; use Image › Sync Metadata after import | not applied |
| Capture One, via `apply-c1` | yes | yes, with `--exposure` / `--crop` |
| Lightroom Classic | ignores sidecars for DNG files | — |

**`apply-c1`** writes an AppleScript for the open Capture One document. Run
`--probe` first (read-only), read the dry-run output, then use `--run` to apply it.

**Moving culls.** `--move-culled` (on `judge` or `decide`) moves each culled DNG and
its sidecar into a `culled/` folder beside it.
- **When:** do it before importing into Capture One. Moving files the catalog
  already references makes them show as missing.
- **Safety:** moves are same-disk renames that never overwrite, and each is recorded
  in the report.
- **Later runs** skip `culled/`, and `cull restore` puts everything back.
- Look through `culled/` before deleting anything.

## Flag reference

**Global** (every command):
- `-o/--report` sets the report path; `-r/--recursive` includes subfolders.
- `--max-edge` (1568) sets the size of the full frame sent to the model.
- `--min-preview-edge` (1500) sets the preview size below which a frame is flagged.
- `--face-min-q` (80) is the face-detection confidence needed.
- `--tiles` (1) sets how many "where focus landed" tiles to send. Add
  `--landed-with-subject` to send them even when there's a subject crop.
- `--seq-gap` (60 s) and `--seq-look` (0.08) control grouping.
- `--save-inputs <dir>` writes exactly what the model sees.

**Policy** (`judge`, `rank`, `decide`, `calibrate`):
- `--review-below-sharpness` (0 = off);
- `--eyes-closed`, `--raw-clipped` and `--outranked` (each `ignore`, `review` or
  `cull`; default `review`);
- `--raw-clip-threshold` (0.5 % of raw samples);
- `--min-crop-area` (0.6);
- `--keep-best` (3).

**`judge`:**
- **Backend:** `--backend`, `--model`, `-j/--concurrency`, `--locate`, `--resume`,
  `--checkpoint`.
- **Cost:** `--estimate`, `--max-cost`, `--batch`, `--quota-stop`.
- **Ranking:** `--no-rank`.
- **Other assessment options:** `--escalate-*`, `--raw-clip` (on by default).
- **Output:** `--write-xmp`, `--xmp-develop`, `--overwrite-xmp`, `--move-culled`,
  `--no-labels`.

## Limitations

- **Calibrate before trusting it at scale.** Model verdicts vary between runs on
  borderline frames. Keep culls going to review until `calibrate` on your own labels
  shows an acceptable false-cull rate.
- **Ranking quality and stability** are not yet measured beyond a few sets.
- **Grouping** links nearly identical framings only; reframed retakes of one pose
  form separate sets.
- **Capture One:** `apply-c1` assumes Capture One's colour-tag numbering (1 red,
  3 yellow, 4 green) and image naming; confirm with `--probe`.
- **Crop coordinates** in `crs:Crop*` for rotated images follow the stored
  orientation. This hasn't been verified in Adobe tools, and `crs:CropAngle` isn't
  written.
