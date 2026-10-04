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
the best few of a sequence. Before any of that, it copies the card with every file
verified. Afterwards it sorts frames into folders and writes sidecars with keywords, for
Capture One or Lightroom.

![cull judge running: per-frame verdicts, progress, and keep/review/cull tallies](docs/images/judge-running.png)

For a whole session with real output at every step, from the card to Capture One, see
[USAGE.md](USAGE.md). Project site: **[code.jefflaplante.com/cull](https://code.jefflaplante.com/cull/)**, with a [usage walkthrough](https://code.jefflaplante.com/cull/usage.html).

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

**Homebrew** (macOS):

```sh
brew install jefflaplante/tap/cull
```

**Pre-built for macOS** (one universal binary for Apple silicon and Intel), from the
[latest release](https://github.com/jefflaplante/cull/releases/latest):

```sh
curl -fsSL https://github.com/jefflaplante/cull/releases/latest/download/cull_macos_universal.tar.gz | tar -xz
sudo mv cull_macos_universal/cull /usr/local/bin/
cull version
```

Each release also has a `.sha256` file to check the download against.

**The binary isn't notarized by Apple.** Downloaded with `curl` as above, it runs as is.
Downloaded in a browser, macOS quarantines it ("cannot be opened"). Clear that once with
`xattr -d com.apple.quarantine cull`.

**From source** (Go 1.26+; Bubble Tea v2, the live progress view, needs it):

```sh
make build                                            # bin/cull, version stamped from git
make install                                          # $GOPATH/bin/cull
go install github.com/jefflaplante/cull/cmd/cull@latest
```

**Releases** are built by `.github/workflows/release.yml` when a `v*` tag is pushed:
1. A macOS runner runs vet and the tests, then `make dist`.
2. It publishes the release.
3. It updates `Formula/cull.rb` in
   [jefflaplante/homebrew-tap](https://github.com/jefflaplante/homebrew-tap), through a
   deploy key stored as the `TAP_DEPLOY_KEY` secret.

`make dist` also builds the same tarball locally.

- **Optional:** `exiftool`, which finds previews in unusual locations.
- **For the `claude-code` backend:** the `claude` CLI, logged in to your Claude
  subscription.
- **Name clash:** npm, crates.io and PyPI each have an unrelated package that installs
  a `cull` command, and npm's deletes files. If you have one of those installed, check
  that `which cull` is this one.

## Quick start

```sh
cull offload /Volumes/LEICA\ M ~/Pictures --name "Forest portraits"   # card → verified copy → free scan
S=~/Pictures/"2026-10-02 Forest portraits"   # the shoot folder offload created
cull judge --estimate "$S"                    # free: what judging would cost
cull judge "$S"                               # model assessment + decisions + set ranking
cull review "$S"                              # browser: check, label keep/review/cull, add stars
cull decide --write-xmp --sort "$S"           # sidecars; keep/ review/ cull/ for import
```

Everything is recorded in `cull-report.json` in the shoot folder. `cull restore "$S"`
undoes `--sort` and `--move-culled`.

**A real run, 17 frames:**

```
$ cull offload LEICA_M Pictures --name "Forest portraits" --location "Forest Park, Portland"
…
copied 17, skipped 0 (already there), failed 0: 1.1 GB in 2s (624 MB/s, verified)
all 17 files verified on Pictures/2025-12-28 Forest portraits: safe to format the card

$ cull judge --backend claude-code --write-xmp "Pictures/2025-12-28 Forest portraits"
…
[1/17] M1103817.DNG CULL  sharp 2.5(missed_focus) exp 7.0(+0.3EV) comp 6.5(good)  [model: Woman's eyes with glasses]
[3/17] M1103823.DNG KEEP  sharp 8.6(sharp) exp 6.5(+0.5EV) comp 6.5(croppable)  [face q=287]
…
ranked set 1 (2 frames): M1104115.DNG wins — … Sharpness at the eyes is the top priority, so Frame 2 wins narrowly.
results: map[cull:1 keep:14 review:2]

$ cull decide --write-xmp --sort "Pictures/2025-12-28 Forest portraits"
decided 17 frame(s); no decision changed; sorted 17 into keep/, review/ and cull/, 0 back into the shoot folder
```

Every step, with its full output and screenshots of the live progress view, is in
[USAGE.md](USAGE.md).

## Offload: card to shoot folder

![cull offload copying with a live progress bar](docs/images/offload-copying.png)

`cull offload <card>... <dest>` copies every DNG on the cards into one folder per run,
`<dest>/<YYYY-MM-DD> <name>/`. It then scans that folder, which is free.

- **The cards are only read.** Nothing on them is written, renamed or deleted.
- **The folder date** comes from the earliest capture date. Camera clocks get set wrong,
  so the plan says which file it came from; use `--date` to set it yourself.
- **One folder per event, if you ask.** A card holding several events can go into one
  numbered shoot folder each (`2026-10-02 Smith wedding 1`, `… 2`), so each imports into
  Capture One or Lightroom as its own set. Each event folder gets its own manifest, scan
  and report.
  - `--split` starts a new event wherever capture time jumps by more than `--split-gap`
    (default 2h), or the day changes. It refuses a card whose clock wasn't running:
    nearly every frame stamped alike.
  - `--split-at M1103426,M1103838` starts an event at each named file instead, in camera
    order, whatever the clock says. `--dry-run` shows the events first.
  - Free space is checked for all events together.
- **Planned first.** Every name, skip and refusal is decided before any byte is written:
  - a file name already in the folder with different content is refused (use `--rename`);
  - too little free space on either destination is refused.

  `--dry-run` prints the plan and stops.
- **Verified copies.** The card is read once:
  - the SHA-256 is taken during that read, and the same read feeds `--backup` too;
  - each copy goes to a hidden temp file, is synced, dropped from the page cache, and read
    back from the disk;
  - only a matching copy gets its real name, and nothing existing is ever replaced;
  - a read or write error retries the file twice, then lists it as failed.
- **"Safe to format"** prints only when all of these hold:
  - every file is verified on every destination;
  - each drive's own write cache was flushed (`F_FULLFSYNC`);
  - no file was merely "already there" without ever being checked against the card. Use
    `--checksum` to check those.
- **Re-running is safe** after a pulled card or Ctrl-C. Only what's missing is copied:
  `cull-offload.jsonl` records each verified file. `--verify <folder>` re-checks copies
  later against those checksums, for example before formatting the next day.
- **`--rename "{date}_{name}_{n:4}"`** renames as it copies. Tokens: `{date}` (YYYYMMDD),
  `{name}`, `{orig}` (camera name), `{n}` and `{n:W}`. The counter continues from the
  folder's largest number, so a second card carries on from the first. Frames are
  numbered in camera file order, not by capture time.
- **Speed (measured on an M11-P card over USB):** the card reads at about 280 MB/s. A
  verified offload runs at about 216 MB/s, against 264 MB/s for plain `cp` with no
  checks: about 5 minutes for a 63 GB card.

## Keywords and tags

Every judged frame gets up to 8 **content keywords** from the model, such as `portrait`,
`forest`, `red dress` or `laughing`. The model is asked for 3–8, and Go cleans them. Every frame also gets the **shoot's tags**:
- **Set them** with `--project`, `--event`, `--location` and `--keyword` (repeatable) on
  `offload`, `scan` or `judge`.
- **They're stored in the report,** so later runs keep them, including `judge --fresh`
  (tags describe the shoot). Flags given again replace them field by field.
- **Tags belong to the report,** not the folder: a run with `-o other.json` starts
  without them.
- **Values can't contain `|`** (it separates keyword levels) or start with `cull:`
  (cull's own markers).
- **`cull tag <dir>`** shows them or changes them (`--clear-location` and so on). Then
  run `cull decide --write-xmp <dir>` to rewrite the sidecars.

**Sidecars** carry them as plain keywords in `dc:subject` and as paths in
`lr:hierarchicalSubject`: `content|forest`, `project|Smith wedding`, `event|…`,
`location|…`. Capture One 16.7.2 nests them on import (checked), and so does Lightroom.

**`apply-c1 --keyword`** applies the same plain words to frames already in a catalog.
Capture One's scripting can't nest keywords.

**The review page** shows each frame's keywords and, in its header, the shoot's tags.
Junk frames get your tags but no content keywords, since the model never saw them.

## Commands

| Command | What it does | Calls a model |
|---|---|---|
| `offload <card>... <dest>` | Copy a card into a dated shoot folder, every file verified; then scan it (`--verify <folder>` re-checks later) | no |
| `scan <dir>` | Extract previews, EXIF, faces and look fingerprints, flag junk frames; write the report | no |
| `judge <dir>` | Assess every frame, decide keep/review/cull, then rank the sets | yes |
| `rank <dir>` | Rank the sets in an existing report (after `judge --no-rank`, or after new frames) | yes |
| `decide <dir>` | Re-apply the policy to stored assessments; regroup sets; write sidecars; sort into keep/ review/ cull/ (`--sort`) or move culls | no |
| `review <dir>` | Browser contact sheet for checking, labelling and rating frames | no |
| `calibrate <report>...` | Compare a report's decisions with your labels; sweep thresholds | no |
| `apply-c1 <dir>` | AppleScript that applies verdicts, stars and keywords in Capture One (dry run by default) | no |
| `tag <dir>` | Show or change the shoot's project, event, location and keywords | no |
| `restore <dir>` | Move frames that `--sort` or `--move-culled` moved back where they were | no |
| `status <dir>` | Where a shoot stands (judged, labelled, ranked, spent) and the next command to run | no |
| `version`, `completion` | Build version; shell completion | no |

Every command has `--help` with examples.

## How decisions are made

For each DNG:

1. **Preview.** The largest embedded JPEG preview is read directly from the file's
   TIFF structure; the raw data isn't read for this. EXIF orientation is applied.
   **Junk frames** are set aside here, with no model call and no cost: near-black (≥ 98%
   of pixels), blown white (≥ 95%), or uniform (thumbnail contrast below 3), such as a
   lens cap, a flash misfire, or a blank frame. `--junk` decides them (default cull;
   `review`, or `ignore` to judge them anyway). The thresholds come from 992 real frames:
   the darkest real frame was 74% near-black, the brightest 82% blown, and the flattest
   (low-key portraits in shade) had contrast 10. A filter on fine detail was tried and
   dropped, because sharp shallow-focus portraits have the least fine detail of all.
   Motion-blurred or pocket shots with some contrast left still go to the model.
2. **Focus target.** Face detection ([pigo](https://github.com/esimov/pigo)) finds the
   subject, centred on the eyes. With no confident face (`--face-min-q`, default 80),
   `judge` asks the model to locate the intended focus target (`--locate off` to skip).
3. **Assessment.** The model receives:
   - the full frame, downscaled to `--max-edge` (1024 px; it is context only);
   - the subject cropped at native resolution;
   - when there's no subject crop, a "where focus landed" tile (`--tiles`).

   It returns structured scores for sharpness, exposure, composition, and people
   (eyes, expression). Every answer is checked against a JSON Schema, with one retry.
4. **Decision in Go** (`eval.Policy`). The model only assesses; these rules decide:
   - `missed_focus` or `motion_blur` → **cull**, when the sharpness score agrees (at
     most `--cull-max-sharpness`, default 2.9, the top of the band the prompt gives those
     statuses; a higher score sends it to **review**);
     `soft` → **review**. Optionally,
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

**Ranking twice (`--rank-twice`, on `judge` and `rank`).** Models favour some positions
in a list. With this flag, each set of up to 8 frames is ranked a second time with its
frames shown in reverse, which doubles those calls.
- A frame inside `--keep-best` in both orders is best.
- A frame outside it in both orders is outranked.
- A frame the two orders disagree on is disputed: a keep goes to review (unless
  `--outranked ignore`), and it isn't marked best.
- Sets already ranked once are re-ranked only with `--force`.

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
  page that keeps labels in the browser, with JSONL export and import. Bring exported
  labels into the shoot's log with `cull import-labels <export.jsonl> <dir>`.
- **Files don't move while you review.** A label change rewrites only the labels log and
  the frame's sidecar, where the frame is now: a frame you rescue from `cull/` stays in
  `cull/` with a green label. To move frames into the folders that match your labels:
  - run `cull decide --sort <dir>` after the session; or
  - start the session with `cull review --sort <dir>`, which re-sorts once when you stop
    the server with Ctrl-C. If the server ends any other way (the terminal is closed, the
    process is killed), nothing moves; run `cull decide --sort` yourself.

  **Don't re-sort once `keep/` and `review/` are imported** into Capture One or
  Lightroom: they lose track of files that move under them. After import, push your
  changes in with `cull apply-c1` instead.

| Key | Action |
|---|---|
| ← → | previous / next frame |
| ↑ ↓ | one grid row (one frame in the detail view) |
| Enter / Esc | open the detail view / back to the grid |
| K / R / C | label keep / review / cull (in the detail view, then advance) |
| U | clear the label |
| 1–5 / 0 | set stars / clear them |
| Z | 100% view of the whole frame, opened on the subject; drag or arrow keys pan |
| S | compare the frame's set side by side at one scale; 1–9 keeps that frame |
| N | next unlabelled frame |
| [ / ] | previous / next set |
| - / = | one keeper fewer / more in the frame's set (top N by the model's rank keep, the rest cull) |
| , / . | exposure −/+0.1 EV (< / > for ±0.5; \ resets): previewed on the page, applied in Capture One by `apply-c1 --exposure` |

**Filters** combine three rows:
- **Verdict:** your label, else the model's.
- **Progress:** unlabelled, unrated, and where you disagree with the model.
- **Sets:** all, keepers (frames in a group that you or the model keep), outranked.

For example, *Keep + Unrated* shows the keepers you haven't starred yet.

**Sets in the grid.** Each set is drawn as one box around its frames, with a header
showing how many keepers it has, and **−** / **+** buttons (or **-** / **=**) to change
that number. Changing it labels the set's top N frames, by the model's rank, as keep and
the rest of its ranked frames as cull. These are your labels, so sidecars,
`decide --move-culled`, `apply-c1` and `calibrate` all follow them. Each frame in a set
also has a **✓ keeper** toggle that flips just that frame between keep and cull.
Keepers have a green border.

**On each card and in the detail view:**
- Your label is the outlined badge, top right; the model's verdict is in the card's
  bottom row.
- A frame in a group shows "group_N · #rank/of": groups are numbered 1, 2, … through
  the shoot, and the rank is the model's.
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
2. Run `cull calibrate <dir>` (or a report path). It reports:
   - the false-cull rate (you kept, it culled) and missed-cull rate;
   - the review rate;
   - a `--review-below-sharpness` sweep;
   - a `--keep-best` sweep for sets.
3. For stability, judge the same frames twice (`-o run1.json`, `-o run2.json`) and run
   `cull calibrate --compare run1.json run2.json`. It counts decision flips, keep↔cull
   crossings and sharpness status changes, and needs no labels.
4. Apply the settings you pick with `cull decide`. The report stores that policy, so
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
  model; `--escalate-on` picks the outcomes that escalate. When the two models
  disagree on whether a frame failed (one says sharp, the other missed focus), it goes
  to review instead of trusting either.
- Small local vision models judge focus poorly. Calibrate before trusting one.

## Cost control

- `judge --estimate` and `rank --estimate` print the cost without calling a model or
  needing a key.
  - `rank`'s figure is exact.
  - `judge`'s includes an approximate ranking cost that assumes 8-frame sets. It
    usually overestimates.
- On a terminal, `judge` and `rank` ask before spending when the estimate is over $1.
  `--yes` skips the question; scripts (no terminal) aren't asked.
- `--max-cost USD` stops the run once it has spent that much. Continue with `--resume`.
- A re-run without `--resume` refuses to replace a report that holds assessments:
  `--fresh` (on `judge` and `scan`) starts over, and `-o` writes a separate report.
  Even `--fresh` is refused while the report records frames moved into `culled/`.
- `--batch` (anthropic) uses the Message Batches API: half price, with results within
  minutes to hours. Ctrl-C is safe; `--resume` re-attaches to batches already paid for.
- Costs so far are recorded in the report; `judge` and `rank` print them when they
  finish.
- On the API the system prompt is cached, so frames after the first read it at a
  tenth of the input price. The report's cost includes cache writes (1.25× input) and
  reads.

### Cost experiments

Two levers can cut cost, but could also change verdicts, so they were measured first:
- **`--effort low`** (or `medium`), with **`--locate-effort`** for the locate call. Output
  is about 40% of a frame's cost.
- **`--max-edge`**: now 1024 by default, after this A/B (16% fewer input tokens,
  verdicts within run-to-run noise on 17 frames). `--max-edge 1568` restores the old size.

```sh
cull judge -o base.json ~/Pictures/shoot
cull judge --effort low -o low.json ~/Pictures/shoot
cull calibrate --compare base.json low.json
```

Adopt a lever only if `--compare` shows no keep↔cull crossings, and labels agree. On
`--backend claude-code` these runs cost quota, not money. The report records the
effort, and `--resume` refuses a different one.

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
| Capture One, via `apply-c1` | yes | yes, with `--exposure` / `--crop`, only over default exposure and crop |
| Lightroom Classic | ignores sidecars for DNG files | — |

**Exposure from review.** Set a frame's exposure in the review page (the slider in the
detail view, or `,` `.` `<` `>`). The page previews it from the camera JPEG, and it's
saved with your labels. The sidecar gets `crs:Exposure2012` for Adobe tools, but
Capture One 16.7.2 ignores that on import (tested). `apply-c1 --exposure` sets it in
Capture One instead. Your EV always overwrites Capture One's exposure, while the model's
suggestion applies only where it is still 0.

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

**Sorting for import.** `--sort` (on `judge` or `decide`) moves every judged frame, and
its sidecar, into `keep/`, `review/` or `cull/` beside it, by its verdict (your label
first).
- **Then import** `keep/` and `review/` into Capture One, Lightroom or anything else. No
  sidecar support is needed for that split.
- **Re-sorting:** run `decide --sort` again after changing labels in `review`, and frames
  move between the folders.
- **Do it before importing:** the catalog loses track of frames moved afterwards.
- **Safety:** frames that failed stay where they are. The same rules as `--move-culled`
  apply (no overwrites, recorded in the report, `cull restore` undoes it). The two
  options can't be combined.

## Flag reference

**Global** (every command): `-o/--report` sets the report path; `-r/--recursive`
includes subfolders.

**Verbosity** (every command): `-q/--quiet` prints only warnings, errors and the final
summary; the default adds progress and a line per frame; `-v/--verbose` adds per-stage
and per-call detail; `--debug` adds backend events and raw model answers (never
credentials). `--log-level quiet|normal|verbose|debug` is the long form.

On an interactive terminal, `scan`, `judge` and `rank` show a live view: a progress bar
per stage with an ETA, keep/review/cull tallies, spend and any warnings, with each
frame's line printed above it. Piped or redirected output (and `--plain`, and `-q`) stays
plain lines. Ctrl-C works as before: in-flight frames finish, the report is saved.

**Image** (`scan`, `judge`):
- `--max-edge` (1024) sets the size of the full frame sent to the model. It was
  1568 before 2026-10-01: 1024 cut input tokens by 16% with verdicts unchanged.
- `--min-preview-edge` (1500) sets the preview size below which a frame is flagged.
- `--face-min-q` (80) is the face-detection confidence needed.
- `--tiles` (1) sets how many "where focus landed" tiles to send. Add
  `--landed-with-subject` to send them even when there's a subject crop.
- `--save-inputs <dir>` writes exactly what the model sees.

**Grouping** (`scan`, `judge`, `rank`, `decide`, `calibrate`): `--seq-gap` (60 s) and
`--seq-look` (0.08).

**Policy** (`judge`, `rank`, `decide`, `calibrate`):
- `--review-below-sharpness` (0 = off);
- `--cull-max-sharpness` (2.9; 0 = the status alone culls);
- `--eyes-closed`, `--raw-clipped` and `--outranked` (each `ignore`, `review` or
  `cull`; default `review`);
- `--junk` (`cull`, `review` or `ignore`; default `cull`): blank frames, decided without
  a model call.
  - **Changing it:** `decide --junk review` (or `cull`) re-decides junk frames for free,
    and rewrites their sidecars with `--write-xmp`. `decide --junk ignore` takes back a
    junk cull and leaves the frame unjudged; `judge --resume --junk ignore` then has the
    model judge it.
  - **The thresholds come from one photographer's outdoor work.** A night sky, a concert or
    white-seamless product frames could cross them. `calibrate` lists any junk frame you
    labelled keep: check it on a new kind of shoot.
  - **Cost estimates** count every DNG, because junk is only found while frames are read.
    Junk frames are never charged;
- `--raw-clip-threshold` (0.5 % of raw samples);
- `--min-crop-area` (0.6);
- `--keep-best` (3).

**`judge`:**
- **Backend:** `--backend`, `--model`, `-j/--concurrency`, `--locate`, `--resume`,
  `--checkpoint`.
- **Cost:** `--estimate`, `--max-cost`, `--batch`, `--quota-stop`.
- **Ranking:** `--no-rank`.
- **Other assessment options:** `--escalate-*`, `--raw-clip` (on by default),
  `--second-opinion` (ask the model again about soft-or-worse frames; when the two
  answers disagree on whether the frame failed, it goes to review; not with `--batch`).
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
