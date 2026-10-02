# cull

Go CLI that culls Leica M11-P DNGs by sending each file's embedded JPEG preview to a
vision model. Priority order: **sharpness gates; exposure gets fixed, not culled;
composition gets cropped, never culled.** Batches are 100–1000 images. The user's
editor is Capture One (macOS).

Scaffolded in a claude.ai chat; this file carries that context forward.

The binary is **`cull`** (renamed 2026-09-28); the model step is `cull judge`. Files and
tags it writes: `cull-report.json`, `cull-labels.jsonl`, `cull-review/`, keywords
`cull:<verdict>` / `cull:labeled`. npm, crates.io and PyPI each have an unrelated `cull`
package that installs a `cull` command (npm's deletes files); none is installed here.

## Commands

```sh
make build            # bin/cull, version stamped from git describe
make test             # all tests use synthetic fixtures; no network, no API key
                      # (eval tests bind loopback via httptest: under the Claude Code
                      # sandbox this needs sandbox.network.allowLocalBinding: true)
make vet
./bin/cull scan --save-inputs /tmp/in <dir>          # no model calls; previews, faces
./bin/cull review <dir>                              # browser: label/star; saves labels log + sidecars
./bin/cull judge <dir>                               # anthropic: spends API credits
./bin/cull judge --backend claude-code <dir>        # subscription quota
./bin/cull judge --backend openai --model <m> <dir>  # local OpenAI-compatible server (free)
./bin/cull rank <dir>                                # rank sequences already judged (or after --no-rank)
```

## Layout

- `cmd/cull` — main; signal-aware context into cobra (binary `cull`; module and repo `github.com/jefflaplante/cull`, first called gophotocull)
- `internal/cli` — cobra tree: `scan`, `judge` (model; code in cull.go), `rank` (rank.go),
  `decide`, `review`, `calibrate`, `apply-c1`, `restore`, `version` (+ built-in `completion`);
  backend.go (`--backend`/`--model`/credential flags shared by judge and rank)
- `internal/dng` — pure-Go TIFF IFD/SubIFD walk for the largest reduced-resolution
  JPEG; reads IFDs + preview bytes only. `exiftool` fallback.
- `internal/imageprep` — `Frame`: decoder's YCbCr kept in stored orientation + display
  `[]uint8` luma; crops/downscale cut in stored coords, only results rotated; stats
- `internal/focus` — pigo face detection (cascades embedded, MIT), subject-crop
  geometry, "where focus landed" fine/coarse ratio tiles
- `internal/llm` — `Backend` interface (system + text/JPEG parts + JSON Schema →
  validated JSON, usage, quota) with `anthropic` (output_config json_schema),
  `claude-code` (`claude -p`, subscription), `openai` (OpenAI-compatible, streaming)
- `internal/eval` — prompts, schemas, `Evaluate`, `Locate`, **Policy** (types.go), the
  rank call's prompt/schema/request and permutation-checked decode (rank.go)
- `internal/pipeline` — detect → locate → crops → evaluate → decide; worker pool,
  resume (path+size+mtime; refuses a different backend/model/schema), checkpointing,
  quota stop, `--save-inputs`, `--move-culled` / `--sort` / `Restore` (move.go: `place` puts frames
  home, in `culled/`, or in `keep/` `review/` `cull/`; `reconcileMove` searches them all; Discover skips them),
  stages.go (shared frame stages), decide.go, groups.go (decideAll: regroups sequences,
  reuses/applies stored ranks, marks best), batch.go (Message Batches driver with
  re-attachable `<report>.batch.json` state), escalation, cost budget; rank.go (`Rank`/
  `RankSets`: chunk-then-final calls per set, sync and batch executors), rank_batch.go
  (Message Batches ranking executor, re-attachable `<report>.rank-batch.json` state),
  rankplan.go (chunk/finalist/merge math), rankcalls.go (`RankCalls`: exact call count
  for `--estimate` without spending anything)
- `internal/group` — look fingerprint (8×8 mean RGB) + sequence grouping (time gap +
  look to the previous frame), score order
- `internal/rawclip` — pure-Go lossless-JPEG (SOF3) decoder; raw highlight clipping
- `internal/review` — HTML contact sheet (index.html from embedded page.html: labels, stars,
  filters; images in its assets/ folder) and
  the server `review` runs by default (serve.go: 127.0.0.1, Host/Origin/token checks, appends labels,
  optional sidecars)
- `internal/labels` — the user's append-only JSONL labels log (last line per file wins),
  `Effective` verdict (label over model), and the one sidecar mapping (`Sidecar`,
  `WriteSidecar`) used by cull, decide, the server; apply-c1 mirrors it
- `internal/calib` — confusion matrix, rates, sharpness-threshold sweep
- `internal/c1` — Capture One AppleScript generator, read-only probe, osascript runner
- `internal/offload` — `cull offload`: plan.go (hygienic card walk, one folder per run,
  names/--rename counter, skips, clashes, free space; nothing written), copy.go (single-read
  tee, SHA-256 while reading, F_NOCACHE temp, evict + mincore check, uncached verify,
  link-based no-replace rename), run.go (retries, verified-only manifest
  `cull-offload.jsonl`, F_FULLFSYNC per destination, `Safe`, `Verify`), sys_darwin.go
  (fcntl/msync/mincore; no-ops elsewhere). `go test -tags cardbench` benchmarks against
  `cp` on the LEICA M card.
- `internal/report` — JSON source of truth (schema v4)
- `internal/ui` — verbosity levels and progress events (`Sink`): plain lines, or the Bubble Tea
  live view on an interactive terminal (live.go); `-q`/`-v`/`--debug`/`--plain` in cli/output.go
- `internal/xmp` — sidecar writer, atomic, never clobbers by default
- `internal/config` — API key resolution

## Invariants — do not break

- Never modify or delete DNGs. Only `--move-culled` / `--sort` (judge, decide) move them (same-disk
  rename into `culled/` or `keep/` `review/` `cull/`, never overwriting, recorded as `moved_to`),
  and `restore` undoes it. `offload` only reads cards and never replaces a file. Never overwrite an existing `.xmp` unless `--overwrite-xmp`.
- Never print, log, or read the API key contents beyond `internal/config`.
- Keep/review/cull is decided in Go (`eval.Policy`), not by the model. The model
  only assesses. Keeps decisions deterministic, auditable, and tunable.
- Don't run `judge` on real photos without asking first: it spends money.
- Dependencies: stdlib plus cobra, and pigo `core` (face detection; justified in the
  2026-09-26 spec), and Charm's Bubble Tea v2 / Bubbles / Lip Gloss (+ `x/term`) for the
  live progress view on terminals (2026-10-02: a live view of a 1000-frame, multi-hour run
  was asked for; non-terminal output stays plain lines). Bubble Tea v2.0.10 requires
  Go 1.26. Justify anything else.
- Tests use synthetic fixtures. Never commit real images.

## Verified facts

- Capture One scripting is AppleScript/JXA, macOS only. No pixel or preview access.
- Capture One reads XMP sidecar **metadata** (rating, color label, keywords via
  Image › Sync Metadata). It does **not** reliably apply Adobe `crs:` develop
  settings (exposure, crop) from sidecars. Edits into C1 must go through AppleScript.
- **Capture One 16.7.2 reads a DNG's `.xmp` sidecar on import** (user-tested 2026-09-27
  on a clone of M1103817 with a cull-written sidecar): `xmp:Rating` stars and
  the `xmp:Label` colour showed up. So write sidecars before import; after import,
  changes go through `apply-c1`.
- **Capture One 16.7.2 ignores `crs:Exposure2012` in a sidecar on import** (user-tested
  2026-10-01, `photos/c1-ev-test/`). Two copies of M1103823 were imported:
  - A carried cull's exact sidecar output;
  - B also carried `crs:Version` and `crs:ProcessVersion`.

  Both got their stars, green label and keyword, and neither got the +1.00 EV. Exposure
  edits must go through `apply-c1` (AppleScript) after import.
- **AppleScript exposure works on Capture One 16.7.2** (user-run 2026-10-01,
  `photos/c1-ev-test/set-ev-A.applescript`): `set exposure of adjustments of v to 1.0`
  on an imported image's variant read back 0.0 → 1.0. An image's `name` includes the
  extension ("EVTEST_A.DNG").
- Lightroom Classic ignores sidecars for DNG files (uses embedded XMP).
- Sidecar naming convention is `L1000123.xmp`, not `L1000123.DNG.xmp`.

### M11-P previews (verified 2026-09-26 on 12 real frames in `./photos`, gitignored)

- DNG layout: IFD0 is the raw (lossless JPEG, 9536×6336, 14-bit). Its SubIFDs hold
  four baseline JPEG previews: 160×120, **9504×6320 (full resolution)**, 2112×1408,
  720×480. `internal/dng` correctly picks 9504×6320. Roadmap item "render tiles from
  raw" is unnecessary.
- The full-res preview is ~IJG quality 60 equivalent, 4:2:2, 2–4 MB. At 100% it shows
  the same visible detail as a macOS ImageIO raw render (`sips`) of the same crop
  (checked on 2 frames); the raw's extra high-frequency energy is mostly noise.
- EXIF orientation handling is correct for 1 and 8 (matches Apple's raw render).
  6 and 3 not yet seen.
- **Top-k raw Laplacian-variance tiles do not find the subject.** They are
  contrast-driven: they picked sunlit out-of-focus foreground litter, bark, or fabric,
  and landed on the face in 1/12 frames. A contrast-normalized fine/coarse ratio did
  ~4/12. Pixel statistics answer "where is there fine detail", not "is the intended
  subject sharp". `peak_tile_sharpness` is therefore not a usable focus ranking.
- pigo (`github.com/esimov/pigo` v1.4.6, MIT; the `core` package imports only stdlib)
  at 2000px detection size, angles 0/±18°: every detection with Q ≥ 100 was a true
  face (6/12). Frames with turned/tilted/small faces topped out at Q 28–51 and those
  detections were background bokeh — except one true tilted face at Q 29. ~2 s/frame
  including the 60MP decode.

- pigo's pupil localizer (puploc) perturbs its search with `math/rand` (puploc.go:248),
  so pupils, the subject crop's centre and `subject_sharpness` vary run to run: on
  M1104119 main gave 0.107–0.117 over repeated scans (checked 2026-09-30). Compare
  such numbers across runs only beyond that spread.

### Built pipeline on 17 real frames (2026-09-26, `scan --save-inputs`)

- pigo in Go (`internal/focus`) found a confident face (Q ≥ 80) on 10/17; every face
  crop is the face, centred on the eyes (lowest true face: Q 85).
- "Where focus landed" tile: the raw fine/coarse ratio rated noisy bokeh (0.3–0.6)
  above sharp faces (~0.1). With the frame's noise variance subtracted and a
  top-quarter-structure gate, the tile sits in the subject's plane on 12/17 (fabric,
  hair, face, necklace) and on bokeh/out-of-focus foliage on 5. Advisory only.
- Measured peak RSS (`/usr/bin/time -l`, unsandboxed): 1.49 GB at `-j 1`, 2.32 GB with
  3 frames in flight: ~0.8–1 GB per concurrent 60MP frame. ~2.7 s/frame single-threaded.
- Local `judge --backend openai --model <local-4b-vision-model>`: works end to
  end, ~2.5 frames/min at `-j 4`. The 4B model is a poor judge: its locate calls
  returned "woman" boxes, swapped top/bottom (rejected as invalid), or "no clear
  subject" on a frame with one; it called `missed_focus` on 3 frames whose eyes are
  acceptably sharp at 100% (the knit dress in the landed tile looks crisper than
  skin). 2/17 frames ended in incomplete JSON (now retried once). Streaming with
  early hang-up: 45 s vs 95 s non-streaming for the same 2 frames at `-j 1`; at
  temperature 0 the two runs still disagreed on both verdicts (the 4B is unstable
  near decision boundaries).
- Subscription `judge --backend claude-code` (Sonnet 5): 17 frames in 105 s at `-j 2`,
  150k input / 12k output tokens. Located the eyes on all 7 no-face frames. Culled 2,
  both genuinely soft at 100% (M1103817 missed focus; M1104110 moving subject); kept
  the 3 frames the 4B model false-culled. 17 unlabelled frames: a first signal, not
  calibration. Compare backends and `--tiles 0` vs `1` against hand labels.
- API `judge --backend anthropic` (claude-sonnet-5), 1 frame (M1104114, no face),
  run with the user's approval: locate + evaluate both succeeded on the first try
  with `output_config.format` json_schema. 7.4k input / 1.2k output tokens (~$0.03),
  26 s. Locate boxed the eyes of a tilted face pigo missed; verdict matched the
  subscription run (acceptable 5.5, keep). Output stayed far under the 8192 cap.
- **Default model is now `claude-sonnet-5-5`** (2026-09-30; same price as `claude-sonnet-5`).
  Every live anthropic run above used `claude-sonnet-5`: treat 5.5 as a new model for
  calibration. `--resume` on a 5.0 report needs `--model claude-sonnet-5`.

### Model access via a Claude subscription (checked 2026-09-26)

- Reusing Claude Code's subscription OAuth token to call the Messages API from this
  tool is not permitted (consumer terms; enforced since early 2026). The Agent SDK
  docs also disallow claude.ai login for third-party products. Don't build this.
- `claude -p` (Claude Code headless) works with subscription login. `--bare` forces
  API-key auth and never reads OAuth, so a subscription backend cannot use `--bare`.
- claude 2.1.283 supports `--tools ""` (no tools), `--json-schema` (result in
  `structured_output`), `--input-format stream-json`, `--no-session-persistence`,
  `--system-prompt`, `--strict-mcp-config`, `--setting-sources`.
- Probed 2026-09-26 (synthetic images only). This invocation, run in an empty temp
  dir with `CLAUDECODE`/`CLAUDE_CODE_ENTRYPOINT` unset, is clean and works on the
  subscription:
  `claude -p --input-format stream-json --output-format stream-json --verbose
  --model sonnet --tools "" --no-session-persistence --strict-mcp-config
  --setting-sources "" --system-prompt <prompt> --json-schema <schema>`
  - Context overhead ~525 tokens (no hooks/plugins/CLAUDE.md leak in).
  - Base64 `image` blocks in the stream-json user message work.
  - One 1568px frame (the default until 2026-10-01) + three 768px tiles ≈ 5.4k input tokens (~$0.02 list-price
    equivalent, not billed on the subscription).
  - The init event's `apiKeySource` is `none` on subscription auth. If
    `ANTHROPIC_API_KEY` is set in the child env, claude would bill the API instead:
    strip it and check `apiKeySource`.
  - `rate_limit_event.rate_limit_info.unifiedWindows.{five_hour,seven_day}.utilization`
    reports quota use (0–1) per call; `overageStatus` was `rejected`.
  - Structured output arrives via a synthetic `StructuredOutput` tool (2 turns).

### Feature batch (verified 2026-09-27 on the 17 real frames)

- Lean decoding: peak RSS 1.49 GB → 0.47 GB at `-j1`, 2.32 → 0.66 GB with 3 in flight;
  same speed. Frame luma is now the JPEG Y (BT.601), which is what pigo expects.
- M11-P EXIF: no `FNumber` (no aperture coupling); APEX `ApertureValue` is the camera's
  estimate. `DateTimeOriginal` has 1 s resolution, no SubSec. Coded lenses report
  focal length and `LensModel`. On this sample all 17 timestamps fall within 2 s
  (they look rewritten), so burst grouping relied on dHash alone: 2 bursts found.
- Raw: IFD0, lossless JPEG SOF3, 2 components × 4768 = 9536 wide, 14-bit,
  BlackLevel 1023, WhiteLevel 16383, CFA RGGB. Pure-Go decode 0.76 s, measure 0.82 s.
  Preview overstates clipping 4–1000× (M1103821: 3.89 % preview vs 0.50 % raw).
- "Where focus landed" tiles bias the model: with a subject crop present, Sonnet
  compared skin with in-plane fabric/hair and culled 4 sharp faces. Default now sends
  landed tiles only when there is no subject crop (+ prompt caveat); a re-run kept
  those 4 and still culled the 2 genuine misses. Model verdicts vary between runs on
  borderline frames: calibrate before trusting.
- Capture One 16.7.2 dictionary (bundle `Contents/Resources/CaptureOne.sdef`; the `sdef`
  tool needs Xcode): variant rw `rating`, `color tag` (integer), `adjustments` (has
  `exposure`, `rotation`), `crop` = {centerX, centerY, width, height}; image `name`,
  `path`, `dimensions`; `apply keyword <existing keyword> to {variants}`. Generated
  scripts compile with `osacompile`.
- Subscription run, 17 frames with all features: 165k in / 35k out tokens, 4.6 min.
- Review sheet: the user viewed it on the 17 frames (subject crops good, verdicts clear,
  reasons sometimes terse). `review --serve` used live by the user 2026-09-27: works.
  Follow-ups done: ↑/↓ move by grid row, your badge outlined + legend, header shows the
  folder and labels log, and `review <dir>` serves + opens + writes sidecars by default.
- Live `judge --batch` (claude-sonnet-5, 3 frames, with the user's approval): two rounds
  as designed. Round 1 = 1 evaluate (face frame) + 2 locates, round 2 = 2 evaluates;
  ~2 min per round, 4.2 min total. 21.2k in / 2.3k out, $0.033 at batch price
  (estimate said $0.04). State file removed on completion. Peak RSS 1.18 GB (3 frames
  prepared concurrently, raw clip on). Verdicts: M1103823 keep 8.5, M1104114 keep 8.0
  (the pre-feature-batch sync run said 5.5 with a landed tile), M1103817 review 3.5
  "soft" (the subscription run called it missed_focus → cull; it is soft at 100%).

### Run-to-run stability after the section-3 changes (2026-09-30, user-approved)

Two `judge --backend claude-code` runs (Sonnet via subscription, ranking on, defaults) on
the 17 sample frames, compared with `calibrate --compare`:
- decisions agreed on 17/17 (14 keep, 2 review, 1 cull), with 0 keep↔cull crossings;
- the sharpness status differed on 2 frames, both sharp↔acceptable (both keep);
- mean |Δ sharpness score| was 0.32.
- **Scores sat inside the prompt's bands** every time: sharp ≥ 8, acceptable 6.5–7,
  soft 4.5–5 (M1104110), missed_focus 2.0–2.3 (M1103817). This suggests the bands and
  the evidence-first order are being followed.
- M1103817 was culled both times; before the section-3 changes it flipped between cull
  and review. M1103821's review comes from raw clipping (deterministic), not the model.
- Each run used about 140k input and 11k output tokens, roughly 5 min at `-j 2`.
- **Limits:** one pair of runs on 17 mostly easy frames, with no labels. This shows
  stability, not correctness. Calibrate on a labelled sample before trusting culls.

### Cost A/B on claude-code (2026-10-01, user-approved)

`judge --backend claude-code` on the 17 sample frames: a fresh baseline, then one run
per lever, each compared with `calibrate --compare`. The noise floor is 3 default runs,
compared pairwise.

| vs baseline | decisions | crossings | status flips | mean \|Δ score\| | frame input | frame output |
|---|---|---|---|---|---|---|
| noise floor | 17/17 | 0 | 1–3 | 0.25–0.36 | 132k–141k | 9.6k–10.4k |
| `--effort low` | 17/17 | 0 | 5 | 0.42 | 146k | 10.5k |
| `--max-edge 1024` | 17/17 | 0 | 3 | 0.30 | 110k (−16%) | 9.5k |

- **`--max-edge` default is now 1024.** It is within noise and cuts input 16%. The cost
  estimate is lowered to 6k input per frame to match.
- **`--effort low` not adopted.** It saved no output tokens on claude-code, and flipped
  statuses slightly more often.
- **Limits:** 17 easy frames, one run each, no labels. claude-code token counts include
  its own overhead. Re-check on hard, faceless frames, where the full frame carries
  more of the judgement.

### White balance from the model: probed and dropped (2026-10-01)

A throwaway probe sent Sonnet a white-balance-only question (claude-code backend): the full
frame plus the face crop, with the status good, cast or intentional, and a direction.
- On the 17 sample frames (open-shade forest): 16 good; M1104119 a slight magenta cast.
- Casts added in linear light:
  - about 1000 K wrong: 0/4 caught (warm, cool, green, magenta);
  - about 2000 K wrong: 3/4 caught in the right direction, all called "slight". Green was
    missed, put down to foliage bounce.

It catches only gross casts and is blind to green in foliage, so it isn't worth wiring in.
Capture One's dictionary does expose `temperature`, `tint` and `autoadjust … adjust white
balance`, but no run has tested them yet.

### Camera support (tested 2026-09-29 on 22 raw.pixls.us samples with `cull scan --raw-clip`)

- `cull` judges only the embedded JPEG preview, so support depends on the camera's DNG
  writer. Previews ≥ 97% of the raw's width (the rest is masked border): Leica M10,
  M10-R, Q2, SL2, CL; Pentax K-1 II, K-3 III; Ricoh GR III; Sigma fp; iPhone 12 Pro
  ProRAW; Galaxy S23 Ultra; Adobe DNG Converter full-size output.
- Pixel 8 Pro: 2560 px (31% of 8160). Small (17–24%, flagged): iPhone XS, Pixel 4a,
  DJI Mini 2 / Mavic 3. No usable preview: Leica M9 (320×216 *uncompressed* RGB
  thumbnail, not JPEG), M (Typ 240) and Monochrom (Typ 246) (160×120), OnePlus 6T (none).
- Raw clipping (striped lossless JPEG only) worked on M10, M10-R, Typ 240, Typ 246,
  K-1 II, K-3 III, GR III. Fails, falling back to the preview: tiled LJPEG (phones,
  Sigma fp, Adobe converter), uncompressed (Q2, SL2, CL, DJI), lossy DNG (34892),
  Samsung (restart intervals).
- EXIF: Leica M10 also lacks `FNumber`; Pentax, Ricoh, Sigma, DJI and phones mostly lack
  `LensModel`. iPhone 12 Pro previews have orientation 6 (extraction fine; display not
  checked visually).
- The evaluate and locate prompts name each frame's camera from EXIF (`eval.Camera`,
  `Describe`: model, or maker's first word + model); Leica M bodies (`LEICA M…`) add
  "rangefinder (manual focus, often fast lenses shot wide open)". Checked on all 22
  samples via `scan --save-inputs`.

### Card offload measurements (2026-10-02, the user's `LEICA M` card, read only)

- exFAT over a USB reader: 992 DNGs, 63 GB, in `DCIM/100LEICA`, file names `M1103127`–`M1104130`
  (the 17 sample frames come from this card). macOS adds `.fseventsd` on mount.
- Uncached sequential read speed (`F_NOCACHE`): 282 MB/s with 1 file at a time, 271 MB/s
  with 2, 272 MB/s with 4. Parallel reads don't help, so offload reads one file at a time.
- EXIF for all 992 frames reads in 1.7 s. Every capture time falls between 2025-12-27 23:56
  and 2025-12-28 00:06, and 969 of 991 consecutive pairs are 0 s apart.
  - The user says the card holds several shoots and a multi-day trip, and the camera's clock
    was set wrong at one point.
  - So EXIF times can't date or split folders, or order frames. Offload makes one folder
    per run (`--date` overrides) and numbers frames in file-name order.
  - The file times match EXIF exactly, offset by the time zone, so they're no better.
- Offload benchmark, 20 real frames per tool, read from the card uncached: the `cull`
  engine runs at 216 MB/s (hash, uncached write, evict, verify from disk, F_FULLFSYNC);
  `cp` runs at 264 MB/s. The gap is the verify re-read, which isn't overlapped with the
  next file's card read. A dry run over 992 frames plans in about 1 s.
- Page cache (internal SSD, `mincore`):
  - a normal write leaves every page cached, so a re-read verifies RAM;
  - with `F_NOCACHE` on the write handle, 0 of 4097 pages stayed cached in a quiet probe;
  - but under load, the second 2 MiB of each 4 MiB write sometimes stayed cached.
  - `msync(MS_INVALIDATE)` on a mapping evicted every page of a fully cached 80 MB file
    (5/5 trials). Offload evicts and checks 0 resident before every verify read
    (`offload.dropCache`).

### Sequences: look distances on the 17 sample frames (2026-09-28)

`scan -o <tmp>` looks, `group.LookDistance` between consecutive frames in `Sequences`
order (capture time, then file name; all 17 times fall within 00:05:59–00:06:00, so
effectively file-name order and the time gap never splits). Set-ups judged from thumbnails.

| pair | distance | what changes |
|---|---|---|
| 3813→3817 | 0.206 | headshot → seated on bridge (landscape) |
| 3817→3821 | 0.174 | **same pose**, zoomed out ~1.5× |
| 3821→3823 | 0.285 | new outfit and place |
| 3823→3865 | 0.337 | new place |
| 3865→3880 | 0.406 | new outfit and place |
| 3880→3902 | 0.203 | standing full length → seated on a stump |
| 3902→3971 | 0.232 | → headshot, green bokeh |
| 3971→3979 | 0.155 | headshot → seated on a log |
| 3979→4110 | 0.172 | new outfit and place |
| 4110→4112 | 0.127 | same spot: twirl → look back (landscape) |
| 4112→4114 | 0.152 | same spot, new pose (portrait) |
| **4114→4115** | **0.034** | known pair: link |
| 4115→4116 | 0.167 | same spot, new pose |
| **4116→4117** | **0.022** | known pair: link |
| 4117→4118 | 0.162 | same spot, new pose |
| 4118→4119 | 0.118 | **same pose**, stepped back, hands moved |

- **`group.DefaultLook = 0.08`** (`--seq-look` default). Any value in (0.034, 0.118)
  gives the same sets here, exactly {4114, 4115} and {4116, 4117} (checked with
  `Sequences` and end to end through `scan`). 0.08 sits near the midpoint, leaning
  up because takes of one set-up drift more than these near-identical pairs; synthetic
  10% shift, +1 stop and 5% zoom all measure ≤ 0.045.
- Across all 136 pairs, the known pairs are the only ones under 0.118. Other minima:
  4110↔4114 0.120 and 4110↔4115 0.122 (same spot, twirl vs posed).
- Limit: the look links near-identical framing only. Reframed takes of the same pose
  (3817→3821 at 0.174, 4118→4119 at 0.118) aren't linked by any threshold that keeps
  different poses at the same spot apart (4110→4112 at 0.127). A portrait↔landscape
  switch is always far, because the grid is in display orientation.

## Unverified assumptions — check before building on them

- Whether `--rank-twice` disagreement (reversed-order ranking) tracks real ranking
  uncertainty, and how often sets are disputed: measure on a labeled sample.
- `eye_sharpness` / `face_sharpness` (advisory): whether a low eye-to-face ratio flags
  front/back focus. On the 17-frame sample (9 faces, none labelled soft) the ratio
  ranged 0.75–1.17 (2026-09-30), and puploc's randomness moves it run to run. No
  rule may use it before it's compared with labels.
- Structured output is written in schema property order. The evidence-first schemas
  (`llm.Schema` marshals properties in `required` order: focus_target, status, score)
  rely on it; not yet checked on a live call.

- Capture One runtime details not in its dictionary: color-tag numbering (code assumes
  1 red, 3 yellow, 4 green), orientation of `dimensions`/`crop`, `make new keyword`.
  (`name` includes the extension and `exposure` is settable: verified 2026-10-01.) Run
  `apply-c1 --probe` first.

- pigo Q threshold (~80 separated true/false on 12 frames) needs calibration on more
  shoots.
- Whether heavy scripted `claude -p` use is within subscription usage policy and how
  much of the 5-hour window a 1000-frame run uses. `--quota-stop` bounds the impact.
- `crs:Crop*` coordinate space for rotated (orientation 6/8) images; code assumes
  stored orientation (`xmp.FromDisplay`).
- Capture One AppleScript property names for rating, keywords, exposure, crop.
  Dump the real dictionary with `sdef "/Applications/Capture One.app"` and read it
  before writing the applier.
- How good and how stable the model's side-by-side ranking is: verdicts stay
  `review` (`--outranked` default) until the calibrate sets section shows it's
  trustworthy on a labeled sample.
- M11-P capture-time spacing: the user's card has 992 frames within 10 minutes of camera
  time, from a clock set wrong (see Card offload measurements), so `--seq-gap` never split
  anything there. Grouping relied on look distance alone. Whether gaps split sequences on
  a correctly set clock is still unconfirmed.
- Batch ranking (`cull rank --batch` / `judge --batch` with ranking on) has not been
  run against the live Message Batches API; only `judge --batch`'s evaluate/locate
  calls have (2026-09-27, above).

## Roadmap (priority order)

0. ~~Validate previews.~~ Done 2026-09-26: full-resolution preview, see above.
1. ~~Focus-target localization + model backends.~~ Done 2026-09-26 (branch
   `focus-target`; spec/plan in `docs/superpowers/`): pigo face → model locate
   fallback → native subject crop; noise-corrected "where focus landed" tile;
   `anthropic` / `claude-code` / `openai` backends; `--save-inputs`; scan summary.
2. **Calibrate before trusting.** Tooling done 2026-09-27 (`review` →
   `cull-labels.jsonl` → `calibrate`, tune with `decide`). Waiting on the user's labeled sample set.
   Tune prompt/policy until false-cull rate is acceptable. Nothing should auto-apply
   at 1000-frame scale before this.
3. ~~`apply-c1`~~ built (dry run default); confirm with `--probe` on a real catalog.
4. ~~Raw-level clipping~~ built (pure Go).
5. ~~Message Batches~~ built (`--batch`); verified live 2026-09-27 on 3 frames.
6. ~~Burst grouping~~ built, then superseded 2026-09-28 by **sequences and
   best-of-set ranking**: look fingerprint + capture-time/look-distance grouping
   (`internal/group`), side-by-side model ranking of each set (`eval.Rank`,
   `pipeline.Rank`/`RankSets`), `--keep-best`/`--outranked` policy, `cull rank`
   (sync and `--batch`), `cull:best` keyword, review-sheet set badges/filter/
   filmstrip, and a calibrate sets section. Replaces `--burst-gap`, `--burst-hash`,
   `--duplicates`. Ranking quality/stability still unverified (see above).

## Working style

Be concise and precise; explain *why*. Push back candidly on weak ideas, no
flattery. Flag uncertainty explicitly and separate verified facts from assumptions.
