# gophotocull

Go CLI that culls Leica M11-P DNGs by sending each file's embedded JPEG preview to a
vision model. Priority order: **sharpness gates; exposure gets fixed, not culled;
composition gets cropped, never culled.** Batches are 100–1000 images. The user's
editor is Capture One (macOS).

Scaffolded in a claude.ai chat; this file carries that context forward.

## Commands

```sh
make build            # bin/gophotocull, version stamped from git describe
make test             # all tests use synthetic fixtures; no network, no API key
                      # (eval tests bind loopback via httptest: under the Claude Code
                      # sandbox this needs sandbox.network.allowLocalBinding: true)
make vet
./bin/gophotocull scan --save-inputs /tmp/in <dir>   # no model calls; previews, faces
./bin/gophotocull cull <dir> --csv x                  # anthropic: spends API credits
./bin/gophotocull cull --backend claude-code <dir>    # subscription quota
./bin/gophotocull cull --backend openai --model <m> <dir>  # local OpenAI-compatible server (free)
```

## Layout

- `cmd/gophotocull` — main; signal-aware context into cobra
- `internal/cli` — cobra tree: `scan`, `cull`, `decide`, `review`, `calibrate`, `apply-c1`,
  `restore`, `version` (+ built-in `completion`)
- `internal/dng` — pure-Go TIFF IFD/SubIFD walk for the largest reduced-resolution
  JPEG; reads IFDs + preview bytes only. `exiftool` fallback.
- `internal/imageprep` — `Frame`: decoder's YCbCr kept in stored orientation + display
  `[]uint8` luma; crops/downscale cut in stored coords, only results rotated; stats
- `internal/focus` — pigo face detection (cascades embedded, MIT), subject-crop
  geometry, "where focus landed" fine/coarse ratio tiles
- `internal/llm` — `Backend` interface (system + text/JPEG parts + JSON Schema →
  validated JSON, usage, quota) with `anthropic` (output_config json_schema),
  `claude-code` (`claude -p`, subscription), `openai` (OpenAI-compatible, streaming)
- `internal/eval` — prompts, schemas, `Evaluate`, `Locate`, **Policy**
- `internal/pipeline` — detect → locate → crops → evaluate → decide; worker pool,
  resume (path+size+mtime; refuses a different backend/model/schema), checkpointing,
  quota stop, `--save-inputs`, `--move-culled` / `Restore` (move.go; Discover skips `culled/`),
  stages.go (shared frame stages), decide.go, groups.go (decideAll), batch.go (Message
  Batches driver with re-attachable `<report>.batch.json` state), escalation, cost budget
- `internal/group` — dHash + burst grouping (time gap + hash), best-of-burst
- `internal/rawclip` — pure-Go lossless-JPEG (SOF3) decoder; raw highlight clipping
- `internal/review` — offline HTML contact sheet (embedded page.html) + labeling
- `internal/calib` — labels CSV, confusion matrix, rates, sharpness-threshold sweep
- `internal/c1` — Capture One AppleScript generator, read-only probe, osascript runner
- `internal/report` — JSON source of truth (schema v3) + CSV
- `internal/xmp` — sidecar writer, atomic, never clobbers by default
- `internal/config` — API key resolution

## Invariants — do not break

- Never modify or delete DNGs. Only `cull --move-culled` moves them (same-disk
  rename into `culled/`, never overwriting, recorded as `moved_to`), and `restore`
  undoes it. Never overwrite an existing `.xmp` unless `--overwrite-xmp`.
- Never print, log, or read the API key contents beyond `internal/config`.
- Keep/review/cull is decided in Go (`eval.Policy`), not by the model. The model
  only assesses. Keeps decisions deterministic, auditable, and tunable.
- Don't run `cull` on real photos without asking first: it spends money.
- Dependencies: stdlib plus cobra, and pigo `core` (face detection; justified in the
  2026-09-26 spec). Justify anything else.
- Tests use synthetic fixtures. Never commit real images.

## Verified facts

- Capture One scripting is AppleScript/JXA, macOS only. No pixel or preview access.
- Capture One reads XMP sidecar **metadata** (rating, color label, keywords via
  Image › Sync Metadata). It does **not** reliably apply Adobe `crs:` develop
  settings (exposure, crop) from sidecars. Edits into C1 must go through AppleScript.
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

### Built pipeline on 17 real frames (2026-09-26, `scan --save-inputs`)

- pigo in Go (`internal/focus`) found a confident face (Q ≥ 80) on 10/17; every face
  crop is the face, centred on the eyes (lowest true face: Q 85).
- "Where focus landed" tile: the raw fine/coarse ratio rated noisy bokeh (0.3–0.6)
  above sharp faces (~0.1). With the frame's noise variance subtracted and a
  top-quarter-structure gate, the tile sits in the subject's plane on 12/17 (fabric,
  hair, face, necklace) and on bokeh/out-of-focus foliage on 5. Advisory only.
- Measured peak RSS (`/usr/bin/time -l`, unsandboxed): 1.49 GB at `-j 1`, 2.32 GB with
  3 frames in flight: ~0.8–1 GB per concurrent 60MP frame. ~2.7 s/frame single-threaded.
- Local `cull --backend openai --model <local-4b-vision-model>`: works end to
  end, ~2.5 frames/min at `-j 4`. The 4B model is a poor judge: its locate calls
  returned "woman" boxes, swapped top/bottom (rejected as invalid), or "no clear
  subject" on a frame with one; it called `missed_focus` on 3 frames whose eyes are
  acceptably sharp at 100% (the knit dress in the landed tile looks crisper than
  skin). 2/17 frames ended in incomplete JSON (now retried once). Streaming with
  early hang-up: 45 s vs 95 s non-streaming for the same 2 frames at `-j 1`; at
  temperature 0 the two runs still disagreed on both verdicts (the 4B is unstable
  near decision boundaries).
- Subscription `cull --backend claude-code` (Sonnet 5): 17 frames in 105 s at `-j 2`,
  150k input / 12k output tokens. Located the eyes on all 7 no-face frames. Culled 2,
  both genuinely soft at 100% (M1103817 missed focus; M1104110 moving subject); kept
  the 3 frames the 4B model false-culled. 17 unlabelled frames: a first signal, not
  calibration. Compare backends and `--tiles 0` vs `1` against hand labels.
- API `cull --backend anthropic` (claude-sonnet-5), 1 frame (M1104114, no face),
  run with the user's approval: locate + evaluate both succeeded on the first try
  with `output_config.format` json_schema. 7.4k input / 1.2k output tokens (~$0.03),
  26 s. Locate boxed the eyes of a tilted face pigo missed; verdict matched the
  subscription run (acceptable 5.5, keep). Output stayed far under the 8192 cap.

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
  - One 1568px frame + three 768px tiles ≈ 5.4k input tokens (~$0.02 list-price
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
- Live `cull --batch` (claude-sonnet-5, 3 frames, with the user's approval): two rounds
  as designed. Round 1 = 1 evaluate (face frame) + 2 locates, round 2 = 2 evaluates;
  ~2 min per round, 4.2 min total. 21.2k in / 2.3k out, $0.033 at batch price
  (estimate said $0.04). State file removed on completion. Peak RSS 1.18 GB (3 frames
  prepared concurrently, raw clip on). Verdicts: M1103823 keep 8.5, M1104114 keep 8.0
  (the pre-feature-batch sync run said 5.5 with a landed tile), M1103817 review 3.5
  "soft" (the subscription run called it missed_focus → cull; it is soft at 100%).

## Unverified assumptions — check before building on them

- Capture One runtime details not in its dictionary: color-tag numbering (code assumes
  1 red, 3 yellow), whether image `name` includes the extension (script tries both),
  orientation of `dimensions`/`crop`, `make new keyword`. Run `apply-c1 --probe` first.
- The review sheet's look (only its behaviour was tested, in jsdom).

- pigo Q threshold (~80 separated true/false on 12 frames) needs calibration on more
  shoots.
- Whether heavy scripted `claude -p` use is within subscription usage policy and how
  much of the 5-hour window a 1000-frame run uses. `--quota-stop` bounds the impact.
- `crs:Crop*` coordinate space for rotated (orientation 6/8) images; code assumes
  stored orientation (`xmp.FromDisplay`).
- Capture One AppleScript property names for rating, keywords, exposure, crop.
  Dump the real dictionary with `sdef "/Applications/Capture One.app"` and read it
  before writing the applier.
- Whether C1 reads sidecars for DNGs or only embedded XMP.

## Roadmap (priority order)

0. ~~Validate previews.~~ Done 2026-09-26: full-resolution preview, see above.
1. ~~Focus-target localization + model backends.~~ Done 2026-09-26 (branch
   `focus-target`; spec/plan in `docs/superpowers/`): pigo face → model locate
   fallback → native subject crop; noise-corrected "where focus landed" tile;
   `anthropic` / `claude-code` / `openai` backends; `--save-inputs`; scan summary.
2. **Calibrate before trusting.** Tooling done 2026-09-27 (`review` → `labels.csv` →
   `calibrate`, tune with `decide`). Waiting on the user's labeled sample set.
   Tune prompt/policy until false-cull rate is acceptable. Nothing should auto-apply
   at 1000-frame scale before this.
3. ~~`apply-c1`~~ built (dry run default); confirm with `--probe` on a real catalog.
4. ~~Raw-level clipping~~ built (pure Go).
5. ~~Message Batches~~ built (`--batch`); verified live 2026-09-27 on 3 frames.
6. ~~Burst grouping~~ built.

## Working style

Be concise and precise; explain *why*. Push back candidly on weak ideas, no
flattery. Flag uncertainty explicitly and separate verified facts from assumptions.
