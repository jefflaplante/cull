# cull

Go CLI that culls Leica M11-P DNGs by sending each file's embedded JPEG preview to a
vision model. Priority order: **sharpness gates; exposure gets fixed, not culled;
composition gets cropped, never culled.** Batches are 100–1000 images. The user's
editor is Capture One (macOS).

Scaffolded in a claude.ai chat; this file carries that context forward.

The binary is **`cull`** (renamed 2026-09-28); the model step is `cull judge`. Files and
tags it writes: `cull-report.json`, `cull-labels.jsonl`, `cull-review/`,
`cull-offload.jsonl`, `cull-redate.json` (while a redate runs), `cull-rename.json` (the
last rename, for `--undo`), the hidden folder lock `.cull.lock` / `.cull-holder-*`, keywords
`cull:<verdict>` / `cull:labeled`. npm, crates.io and PyPI each have an unrelated `cull`
package that installs a `cull` command (npm's deletes files); none is installed here.

## Commands

```sh
make build            # bin/cull, version stamped from git describe
make test             # all tests use synthetic fixtures; no network, no API key
                      # (eval tests bind loopback via httptest: under the Claude Code
                      # sandbox this needs sandbox.network.allowLocalBinding: true)
                      # slowest packages: rename (~110 s) and pipeline (~60 s); under -race,
                      # redate and rename need -timeout 60m
make vet
./bin/cull scan --save-inputs /tmp/in <dir>          # no model calls; previews, faces
./bin/cull review <dir>                              # browser: label/star; saves labels log + sidecars
./bin/cull judge <dir>                               # anthropic: spends API credits; re-run continues and ranks
./bin/cull judge --backend claude-code <dir>        # subscription quota
./bin/cull judge --backend openai --model <m> <dir>  # local OpenAI-compatible server (free)
./bin/cull judge --rerank <dir>                      # re-rank every set
./bin/cull decide --sort <dir>                       # keep/ review/ cull/ (--sort=culls: only culls); sidecars are written by default
./bin/cull offload <card> <dest> --set-date 2026-10-04   # fix copies' capture dates (proven); card untouched
./bin/cull redate <dir> --date 2026-10-04 --dry-run  # the same for a folder already offloaded
./bin/cull rename <dir> "{date}_{name}_{n:4}" --dry-run   # bulk rename; everything follows; --undo
```

## Layout

- `cmd/cull` — main; signal-aware context into cobra (binary `cull`; module and repo `github.com/jefflaplante/cull`, first called gophotocull)
- `internal/cli` — cobra tree: `offload`, `scan`, `judge` (model; code in cull.go; continues, ranks), `rank` (rank.go,
  deprecated in v0.2.0),
  `decide`, `review`, `calibrate`, `apply-c1`, `restore`, `status`, `tag`, `import-labels`
  (deprecated), `redate` (redate.go; also `holdShoot`: the folder lock and unfinished-journal
  refusal every frame-reading command takes), `rename` (rename.go),
  `version` (+ built-in `completion`); status.go also lists unfinished journals and hidden
  temps that may be a frame's only copy;
  help.go (sectioned `--help`, `--help-all`); sortflag.go (`--sort[=all|culls]`, `--move-culled` as its
  deprecated alias);
  backend.go (`--backend`/`--model`/credential flags shared by judge and rank); dotfile.go (`~/.cull` /
  `$CULL_CONFIG` flag defaults, applied in the root's PersistentPreRunE: typed flag > stored policy >
  dotfile > built-in default; `notInDotfile` refuses one-off/risky flags)
- `internal/dng` — pure-Go TIFF IFD/SubIFD walk for the largest reduced-resolution
  JPEG; reads IFDs + preview bytes only. `exiftool` fallback. patch.go: `PatchDates`
  returns the capture-date fields as same-length byte patches (`Patch{Off, Old, New}`) plus
  what it left alone, writing nothing; `ContentCredentials` (IFD0 0xCD41). `dngtest`: the
  synthetic DNG builder (`Build`, `Apply`) every package's tests share.
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
  quota stop, `--save-inputs`, `--sort` / `Restore` (move.go: `place` puts frames
  home or in `keep/` `review/` `cull/`; `--sort=culls` puts culls in `cull/`; `culled/` is legacy, read only (moved out on the next sort); `reconcileMove` searches them all; Discover skips them, and every hidden file: temps end in `.DNG`;
  with -r it skips hidden folders too, so `holdShoot -r` never locks `.Trashes`),
  stages.go (shared frame stages), decide.go, groups.go (decideAll: regroups sequences,
  reuses/applies stored ranks, marks best), batch.go (Message Batches driver with
  re-attachable `<report>.batch.json` state), escalation, cost budget; rank.go (`Rank`/
  `RankSets`: chunk-then-final calls per set, sync and batch executors), rank_batch.go
  (Message Batches ranking executor, re-attachable `<report>.rank-batch.json` state),
  rankplan.go (chunk/finalist/merge math), rankcalls.go (`RankCalls`: exact call count
  for `--estimate` without spending anything)
- `internal/group` — look fingerprint (8×8 mean RGB) + sequence grouping (time gap +
  look to the previous frame), score order; `Order` (capture time, then camera order, then
  path) is Sequences' order and rename's reorder check
- `internal/dcf` — camera order: `Of`/`Unify`/`Compare` on DCF names (4 chars + 4-digit
  counter, in `NNNXXXXX` folders): folder number (only when every frame's is known), then
  counter, then name; other names after, by name. Used by offload's scan order (numbering,
  `--split-at`), rename's numbering and `group.Order`
- `internal/rawclip` — pure-Go lossless-JPEG (SOF3) decoder; raw highlight clipping
- `internal/review` — HTML contact sheet (index.html from embedded page.html: labels, stars,
  filters; images in its assets/ folder, a cache: cache.go names each image after the DNG's size+mtime,
  the focus box and `assetVersion`, so review renders only what's missing or changed and sweeps stale ones;
  scan/judge pre-render thumbs and subject crops via `Prerender` while the preview is decoded,
  `--no-review-images` off; `review --prepare` / `--clear-cache` / `--force`) and
  the server `review` runs by default (serve.go: 127.0.0.1, Host/Origin/token checks, appends labels,
  optional sidecars); `CachedImages` gives rename the cache images to carry over
- `internal/labels` — the user's append-only JSONL labels log (last line per file wins),
  `Effective` verdict (label over model), and the one sidecar mapping (`Sidecar`,
  `WriteSidecar`) used by cull, decide, the server; apply-c1 mirrors it. An entry's `from`
  records the old name when rename carries a label over
- `internal/calib` — confusion matrix, rates, the policy grid (keep-best × outranked × raw-clipped)
- `internal/c1` — Capture One AppleScript generator, read-only probe, osascript runner
- `internal/offload` — `cull offload`: plan.go (hygienic card walk, one folder per run or per event with
  `--split` (capture-time gap or new day; refused when `clockBroken`) / `--split-at` (file names), `MakePlans`,
  files in camera order (`cameraOrder`, dcf),
  names/--rename counter, skips, clashes, free space; nothing written), copy.go (two stages:
  `writeStage` single-read tee, SHA-256 while reading, F_NOCACHE temps; `finishStage` fsync,
  evict + mincore check, uncached verify, link-based no-replace rename), run.go (pipeline: file
  N's `finishStage` overlaps N+1's card read, settled in plan order; `hooks.serial` is the old
  one-at-a-time engine for tests and cardbench; retries, each after evicting the card file; verified-only manifest
  `cull-offload.jsonl`, F_FULLFSYNC per destination, `Safe`, `Verify`), sys_darwin.go
  (fcntl/msync/mincore; no-ops elsewhere). `go test -tags cardbench` benchmarks against
  `cp` on the LEICA M card.
  Dates: patch.go (`streamPatched`: `orig` and `want` SHA-256s in one read; `applyPatches`;
  `proveFrom`: evict + uncached re-read must hash to `want`; `setFileTimes`: mtime and
  creation time). `--set-date` runs in stage B: verify against the card → patch → prove →
  times → link. Manifest entries gain `file_sha256`, `dates_set`, `patched_at`, and
  `camera_time` (the card's DateTimeOriginal + SubSec before the first patch, by
  `--set-date` or redate; never replaced, carried by every superseding line)
  (`CurrentManifest`, `AppendManifest`); a re-run finds copies by their recorded name.
  replace.go: `ReplacePatched`, redate's one replace (stream into `.cull-redate-<hex>.<name>`,
  prove, F_FULLFSYNC, `rename(2)` over the original, F_FULLFSYNC the folder), `RedateTemps`,
  `Adopt`, `RemoveStaleTemps`; xattr_unix.go copies xattrs onto the replacement.
  rename_temps.go: `.cull-rename-<hex>.<name>` temps, `MoveNoReplace`. No temp name starts
  `._` (macOS's AppleDouble companions on exFAT/FAT).
- `internal/redate` — `cull redate`: per frame `PatchDates` → `ReplacePatched` (file times
  only for Content Credentials or nothing to patch), proven against the manifest's checksum
  when one is recorded (a mismatch is refused, never "fixed"), else against the frame as
  read; then a manifest line, the report (`ModTime`, `DatesSet`, `CameraTime` when empty), cull's sidecar. The journal `cull-redate.json` records each frame (with its camera time) before its
  swap; `settleTemps` restores or removes temps an interruption left; refuses a sort folder,
  a parent of shoots, a pending batch. `Folders`/`ShootOf` are shared with rename.
- `internal/rename` — `cull rename` / `--undo`: plans every name and refuses before moving
  anything (clashes, case-insensitive; foreign files; companions such as a JPG, `.DNG.xmp`
  or Capture One `.cos`; a frame order change without `--reorder`; unusable names, locked
  frames, unwritable folders; batches), journals `cull-rename.json`, moves old → temp →
  new, then the report (two saves, `report_at`), labels (desired-state entries with `from`),
  manifest and review cache. Re-running finishes from any stop; `--undo` reverses it.
- `internal/journal` — journal.go: the redate and rename journals, `Unfinished`/
  `IncompleteBelow`, `Finish` (the exact command), `PendingBatch`. lock.go: the folder lock,
  exclusive flocks only. redate and rename hold the gate `.cull.lock` and check every
  `.cull-holder-<pid>-<hex>`; each reader (judge, decide, the review server, restore,
  offload, scan, tag, rank, import-labels) holds its own holder file and probes the gate.
  Released by close alone, never `LOCK_UN`; EACCES on the gate, or on a reader's own new
  holder file, counts as held (a stale or contended SMB lock). `cmd/cull` calls
  `RemoveHolders` on exit. `redate -r` and `rename -r` hold only the parent folder's gate: a
  reader run on a subfolder alone (`judge <sub>`) registers there and doesn't see them.
- `internal/report` — JSON source of truth (schema v4); `Tags` (the shoot's project/event/
  location/keywords, merged per run by `MergeTags`, changed by `cull tag`, cli/tags.go);
  `Result.DatesSet` (the date `--set-date`/redate set; the sidecar prefers it over EXIF);
  `Result.CameraTime` (the manifest's `camera_time`, filled by the pipeline, the manifest
  winning; also set by redate; `GroupFrame` uses it over Exif's time, so re-dating never
  regroups);
  `Result.CardName` (`100LEICA/M1103127.DNG`, from the manifest's `Entry.CardName`; judge
  refreshes it, rename leaves it) and `GroupFrame`/`CameraName`, the one frame key judge,
  decide and rename's reorder check group with;
  `RenamePaths` maps every path field (File, XMP, MovedTo, set members/order/notes)
- `internal/ui` — verbosity levels and progress events (`Sink`): plain lines, or the Bubble Tea
  live view on an interactive terminal (live.go); `-q`/`-v`/`--debug`/`--plain` in cli/output.go.
  Every step that can take a while is a stage (`ui.Track`): sidecars, sort/move, restore, looks,
  review's render, batch prepare/upload/wait, offload's plan/checksum/flush, `apply-c1 --run`. Active
  stages spin and show elapsed time, so an open-ended wait never looks hung; plain output announces
  a described stage at the normal level
- `internal/xmp` — sidecar writer, atomic, never clobbers by default
- `internal/config` — API key resolution; dotfile.go reads `~/.cull` (`LoadSettings`)
- `site/` — the GitHub Pages site (`index.html` overview, `usage.html` walkthrough, shared
  `style.css` + `site.js`: lens strip, shutter dial, rangefinder headline, and on the homepage and usage heroes the
  M11's 35/135 bright-line frames and a centred focusing patch, proportions measured from a 0.72x finder view; `img/`), deployed to the
  `gh-pages` branch with `git subtree split --prefix site`; https://code.jefflaplante.com/cull/

## Invariants — do not break

- Never modify or delete DNGs, with one exception: `offload --set-date` and `redate`
  rewrite only the capture-date fields listed in the spec, at the same length.
  - The card is never written.
  - Every patched file is proven byte-identical to its source outside those fields before
    it takes its name.
  - The manifest keeps both the card's hash and the patched file's hash.

  The spec is `docs/superpowers/specs/2026-10-06-redate-rename-design.md` (§2 lists the
  fields). Amended during the build:
  - **Frames with Content Credentials** (C2PA, IFD0 tag 0xCD41) are never byte-patched: the
    signature covers the date fields. Only their file times change, and `Result.DatesSet`
    carries the corrected date to cull's sidecar.
  - **`redate` never rewrites the report's `Result.Exif`**: sequence grouping reads it, so a
    rewrite would regroup and re-rank. The corrected date lives in `Result.DatesSet` (and
    `ModTime` follows the file).
  - **Grouping reads the camera's own time** (2026-10-07, final review): `Result.CameraTime`
    from the manifest's `camera_time`, else Exif's. After `--set-date` (or redate before
    the first judge) a scan reads patched `M…` frames at the target date while Content
    Credentials `L…` frames keep the camera's, which split or merged sets where they met.
    Limit: a folder with no manifest, redated before its first scan, is grouped by the new
    date, and its Content Credentials frames have nowhere to keep the corrected date
    (redate warns: scan first).
- Only `--sort` (judge, decide, review; `--move-culled` is its deprecated alias) moves DNGs
  (same-disk rename into `keep/` `review/` `cull/`, never overwriting, recorded as `moved_to`),
  and `restore` undoes it. `rename` (and `--undo`) renames them in their folders: journalled,
  through hidden temps, never replacing anything, with the report, labels log, manifest,
  sidecars and review cache following. `offload` only reads cards and never replaces a file.
- No command replaces a DNG, with one exception: `redate`'s `rename(2)` of a proven temp
  over its original, in the same directory, after the proof step. Never overwrite an
  existing `.xmp` unless `--overwrite-xmp`. (cull's own files — the report, its sidecars,
  journals — are saved by temp + rename, which does replace them.)
  - **Design assumption, not measured:** that rename is atomic on APFS. On exFAT, FAT32 and
    SMB it may not be: a crash can leave only the proven temp, which the next redate proves
    and adopts (`settleTemps`) and `status` reports. The tests simulate it; no real crash
    has been observed.
- `cull-offload.jsonl` and `cull-labels.jsonl` are append-only: the last line per
  `(orig, size)` / per file name is current. A manifest's `sha256` is always the card's hash.
- After `redate` or `rename`, a following `judge` makes zero model calls: the report's keys
  (path + size + mtime) follow the files. The exception is `rename --reorder`, whose
  regrouped sets are ranked again. Both refuse while a batch is pending (its state
  is keyed by path). judge, decide, review, restore, scan, tag, rank, import-labels,
  offload, redate and rename refuse a folder with an unfinished `cull-redate.json` or
  `cull-rename.json` (`holdShoot`, `journal.Unfinished`); `status` names it.
- Never print, log, or read the API key contents beyond `internal/config`.
- Keep/review/cull is decided in Go (`eval.Policy`), not by the model. The model
  only assesses. Keeps decisions deterministic, auditable, and tunable.
- Don't run `judge` on real photos without asking first: it spends money.
- Dependencies: stdlib plus cobra, and pigo `core` (face detection; justified in the
  2026-09-26 spec), and Charm's Bubble Tea v2 / Bubbles / Lip Gloss (+ `x/term`) for the
  live progress view on terminals (2026-10-02: a live view of a 1000-frame, multi-hour run
  was asked for; non-terminal output stays plain lines). Bubble Tea v2.0.10 requires
  Go 1.26. `golang.org/x/sys` is direct since 2026-10-06 (already in go.sum through Bubble
  Tea): `Setattrlist` for a file's creation time, xattr copying on redate's replace,
  `Access` in rename's checks. Justify anything else.
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
- **Capture One 16.7.2 builds keyword hierarchies from sidecars** (user-tested 2026-10-02,
  `photos/c1-kw-test/`, two clones of M1103823). Each test frame showed its keywords both
  flat and nested, and both kept their stars and green label.
  - KWTEST_A had paths straight in `dc:subject` (`content|forest`).
  - KWTEST_B had plain words in `dc:subject` and paths in `lr:hierarchicalSubject`.

  cull writes B, the Adobe convention: Lightroom reads it too, and would show A's paths as
  literal `content|forest` keywords. The AppleScript dictionary's `keyword` has a
  read-only `parent`, so `apply-c1` applies plain words only.
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
  6: one frame on the user's card (2026-10-02), not yet checked visually. 3 not yet seen.
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
- **A regression from the same day (3d450ff), found 2026-10-02:** `res.Stats = &p.stats`
  kept every frame's ~190 MB decoded preview alive through the report.
  - The 17-frame sample hid it. 60 frames peaked at 11.5 GB.
  - A 992-frame scan reached a 57.7 GB footprint and was killed (twice) near frame 290.
  - Fixed by copying the stats; `TestResultDoesNotRetainFrame` guards it.
  - Now, at the default `-j 4`: 60 frames 1.78 GB, 244 frames 1.84 GB (flat).
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

### Content keywords, live (2026-10-02, claude-code, user-approved)

`judge --backend claude-code --write-xmp` with tags, on clones of M1103823, M1104114,
M1103817 and M1103865 in `photos/c1-kw-test/live/`; the report went to a temp folder.
- **Verdicts matched earlier runs:** M1103817 cull (missed focus), the rest keep.
- **Keywords:** the model gave the full 8 every time, lowercase and descriptive (e.g.
  `portrait, woman, smile, glasses, curly hair, white cardigan, forest road, bokeh`).
  Normalization dropped nothing.
- **Synonyms vary across frames** (`smile` here, `smiling` there), so a catalog collects
  both forms.
- **Output tokens per frame:** 514 and 578 for face frames with one call; 1156 and 1285
  for frames that also needed a locate call. 8 short keywords are roughly 30–40 of those.
  The cost estimate's ~1k out per frame still covers it.
- **The sidecar was as designed:** `cull:keep`, then the 8 keywords and the 3 tag values
  in `dc:subject`; `content|…`, `project|…` and `location|…` in `lr:hierarchicalSubject`.
- **Still unchecked: whether structured output is written in schema property order.**
  The decoded answer doesn't show key order, so that unverified assumption stands.

### First calibration: 979 labelled frames (2026-10-05)

The user labelled 979 of the 992 frames from the LEICA M card (keep or cull only), in
`/Volumes/photos/2026/2025-12-27 Card-Offload` on the NAS. The frames were judged with
`--backend claude-code` (Sonnet → claude-sonnet-5-5), with `--keep-best 2` and otherwise
default policy; 174 sets were ranked. (`cull calibrate` now prints a grid of policy rows for any labelled report.)

`cull calibrate` (rows: the user's label; columns: cull's verdict):

| | keep | review | cull |
|---|---|---|---|
| **keep** (638) | 398 | 237 | 3 |
| **cull** (341) | 60 | 248 | 33 |

- **Safe to auto-cull.** False culls: 3 of 638 keeps (0.5%; 95% upper bound about 1.4%).
  Two are consecutive frames (M1104022/23, missed_focus 2 and 2.5); the third is M1103282
  (motion_blur 2.5). Of 35 frames the model called missed_focus or motion_blur, the user
  culled 32.
- **It catches few culls.** 33 of the user's 341 culls. 196 of those 341 are sharp or
  acceptable: the user culls for moment, duplicates and taste, which the policy never culls
  on by design. 52 of the 60 culls the model kept sit in sets; the user culled 19 of the 179
  sets entirely, and ranking always keeps the top keep-best.
- **Review splits evenly:** 485 frames (49.5%), of which the user kept 237 and culled 248.
  By reason (a frame can have several):

  | reason | kept | culled |
  |---|---|---|
  | clipping | 155 | 71 |
  | soft | 72 | 112 |
  | eyes | 59 | 79 |
  | outranked, and nothing else | 7 | 63 |

  The `--review-below-sharpness` sweep only grows review.
- **Ranking:** with keep-best 2, 7.6% of the user's keeps were ranked out of the best;
  outranked-only frames were 90% the user's culls.

**Policy tests.** These ran on copies of the report against the same labels; the NAS report
was not touched.

| settings | false culls | culls caught | missed culls | review |
|---|---|---|---|---|
| as judged (keep-best 2) | 3 (0.5%) | 33 | 60 | 49.5% |
| `--outranked cull` | 16 (2.5%) | 115 | 60 | 39.8% |
| `--keep-best 3 --outranked cull` | 6 (0.9%) | 80 | 95 | 39.8% |
| …and `--raw-clip-threshold 2` | 7 (1.1%) | 89 | 104 | 33.7% |
| …and `--raw-clipped ignore` | 7 (1.1%) | 91 | 127 | 22.3% |
| `--raw-clipped ignore` alone | 3 (0.5%) | 33 | 85 | 33.9% |

- Clipping only moves frames between keep and review, never to cull.
- `--keep-best 3 --outranked cull` is the candidate: it stays under 1% false culls and
  catches 2.4× as many culls.

**Limits.**
- One shoot, one model run and one photographer.
- The review page shows the model's verdict while labelling, which may bias labels towards it.
- The settings were tuned on the same frames they were measured on: confirm them on the
  next labelled shoot before changing any default.

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
  This didn't reproduce on 2026-10-06 (stage-A profile, same card and reader): 262 MB/s plain,
  ~210 (177–254) with F_NOCACHE.
- EXIF for all 992 frames reads in 1.7 s. Every capture time falls between 2025-12-27 23:56
  and 2025-12-28 00:06, and 969 of 991 consecutive pairs are 0 s apart.
  - The user says the card holds several shoots and a multi-day trip, and the camera's clock
    was set wrong at one point.
  - So EXIF times can't date or split folders, or order frames. Offload makes one folder
    per run (`--date` overrides) and numbers frames in camera order (the DCF file counter;
    see "One counter, two prefixes" below).
  - The file times match EXIF exactly, offset by the time zone, so they're no better.
- **A real offload, verified (2026-10-02):** the whole LEICA M card went to the user's `Grey` volume
  (an exFAT SSD over USB, `/Volumes/Grey/test_cull`), with no backup.
  - **Result:** 992 of 992 frames, 67.7 GB, in 8 min 5 s (140 MB/s), with "safe to format".
    Peak memory was 52 MB.
  - **The card was unchanged:** metadata of all 998 entries was identical before and after.
  - **The destination held exactly what was expected:** 992 DNGs, a 992-line manifest, no
    temp files, and the card's modification times. exFAT reports `rwx------` on card and
    copy alike.
  - **A re-run copied nothing:** all 992 skipped, still "safe to format".
  - **`--verify` matched all 992 from disk.**
  - **The rate (140 MB/s) is below the 216 MB/s measured to the internal SSD.** Two likely
    causes: the verify re-read also goes over USB to Grey, and Grey's write speed.
- **SMB share (2026-10-06, the user's NAS at `/Volumes/photos`, smbfs):** F_FULLFSYNC fails with ENOTSUP (45) on files and folders; plain fsync on both succeeds. `flushDrive` falls back to fsync there (every file was already fsync'd before its verify read) and prints a one-line note; 10 card frames (0.6 GB) offloaded at 15 MB/s, "safe to format", `--verify` 10/10.
- **SMB over 10 GbE (2026-10-06, `/Volumes/photos-1`, TrueNAS ZFS pool with a fast separate log device):**
  - **Link:** 10GBASE-T, full duplex, 0.4 ms round trips; SMB 3.1.1, encryption off.
  - **cardbench, 3 rounds × 10 card frames (1.8 GB per tool), durable MB/s:**
    - the cull engine (every byte re-read and compared): 91; its verify re-read took 21% of
      its time (46% over Wi-Fi);
    - ditto 75 (206 before sync), cp 52 (98), rsync 41 (66).
  - **A 2 GiB SMB probe:** buffered write 749 MB/s, write until fsync returned 159 MB/s,
    uncached read 829 MB/s.
  - **So the network isn't the limit:** waiting for the share to commit synced data is.
    Durable writes top out near 160 MB/s, under the card's ~270 MB/s read.
- **The pool and `logbias` (2026-10-06, user-supplied `zpool status` and `lsblk`):**
  - **Pool:** one mirror of two 21.8 TB HDDs. The log device is a 240 GB SATA data-centre
    SSD (IBM-branded Micron `MTFDDAK240MBP`, power-loss protection), and the cache is the
    same model. `sync=standard`, `logbias=latency` (both defaults).
  - **`logbias=throughput` was much worse:**

    | Measure | `latency` | `throughput` |
    |---|---|---|
    | 2 GiB probe, MB/s until committed | 159–209 | 55–58 (fsync 35–37 s) |
    | cardbench cull pipeline / serial | 107 / 92 | 74 / 70 |
    | ditto / cp / rsync | — | 65 / 39 / 36 |

    Reads were unchanged. Synced data written straight to the HDD mirror is slow; the
    SATA log SSD is the faster path and sets the committed-write ceiling.
  - **Keep the default (`latency`).** To go faster: an NVMe log device with power-loss
    protection, until the mirror's ~250 MB/s becomes the limit. Never `sync=disabled`.
- **NFS vs SMB to the same share (2026-10-07, user-approved; NFSv3 export
  `10.1.68.9:/mnt/tank/photos`, back to back with SMB at `/Volumes/photos`):**
  - **Access:** the dataset is owned by uid/gid 1000, mode 770. The Mac's NFSv3 identity is
    502/20, so it was refused until the user set the NFS share's Mapall user and group to
    `jeff`. After that, files written over NFS are owned by 1000, matching SMB. Only v3 is
    registered with rpcbind. A non-root `mount_nfs` works without `resvport`.
  - **2 GiB probe** (write until fsync returns / uncached read, MB/s):
    - SMB: 181–191 / 745–793.
    - NFS, 32 KB blocks (the macOS default): 217–233 / 282–390.
    - NFS, 64 KB: 227–229 / 417–529.
    - NFS commits about 20% faster and reads about half as fast.
  - **Blocks over 64 KB break the client.** At 128 KB, a `soft` mount failed the write with
    ENXIO ("Device not configured") and dropped the mount. A 1 MB attempt on the default
    `hard,nointr` mount froze the Mac until a restart. The cause is unknown (macOS client,
    TrueNAS, or a NIC offload); 64 KB works, so a jumbo-frame mismatch is unlikely.
  - **cardbench, 3 rounds × the card's 10 frames, durable MB/s, SMB → NFS at 64 KB:**
    - cull pipeline 124 → 111 (NFS rounds 99, 119, 120; one SMB round started with a
      frame cached);
    - cull serial 96 → 89.
    - The other tools' totals swung with `sync` time (SMB `ditto` rounds 109, 127, 19), so
      they rank nothing.
    - The faster commit is cancelled by the slower verify re-read.
  - **Verdict: stay on SMB.** Any NFS mount from a Mac in this project's tests must be
    `soft,intr,locallocks,rsize=65536,wsize=65536`, with every command time-limited.
  - **Tuning follow-up (same day): NFS isn't reliable here at any block size.**
    - Mount: `soft,intr,timeo=50,retrans=3,deadtimeout=60`.
    - Read blocks of 256 KB with writes at 64 KB raised plain reads from 370–420 to
      618–637 MB/s. Uncached reads (cull's verify path) stayed at 533–583, the same as
      at 64 KB.
    - But 6 of about 32 fresh mounts stalled. The server answered nothing for 60 s,
      a few to 16 MB into a write, and the client dropped the mount (ENXIO). That
      happened at 64, 128 and 256 KB read blocks, with fresh mount folders and 5 s
      pauses.
    - Stall times (Mac local, 2026-10-07): 14:19, 14:43, about 14:46 (twice), 14:48 and
      14:50. A `hard` mount, macOS's default, waits forever instead: that was the earlier
      freeze.
    - Read-ahead and `nfsiod_thread_max` weren't tried; there was no point while the
      mounts stall. Cause unknown. TrueNAS's `journalctl -u nfs-server` and `dmesg` at
      those times would be the next place to look.
    - SMB, on the same link and pool, never stalled in any session.
    - **Not the 10 GbE adapter** (15:06–15:22). The same loop was run over the Mac's 1 GbE
      Thunderbolt port (`en6`, MTU 1500) to the NAS's other address, `10.1.66.9`; the share
      needed `10.1.66.0/24` added to its networks first. Stalls:
      - 1 GbE: 3 of 60 fresh mounts (5%);
      - 10 GbE (`en8`, MTU 1500), a 20-mount control in between: 1 of 20;
      - 10 GbE, all day: 7 of 52.

      Each stall was the same 60 s silence, with about 9 MB written. Both paths stall, so
      neither the adapter nor jumbo frames explain it.
    - The user's Proxmox cluster (Linux clients) uses NFS from the same TrueNAS without
      trouble. That points at the macOS NFS client with this server, though no Linux
      client was tested in the same loop.
    - **Searched online (2026-10-07, macOS 26.4.1 here):** no report matches a macOS
      *client* stalling 60 s mid-write. The known Sequoia/Tahoe NFS defect is the other
      direction: a Mac *serving* NFS whose clients hang after 300 s idle (Bresink NFS
      Manager notes call it a macOS 15+ TCP/IP stack defect, unfixed). An unverified lead
      on this Mac: CrowdStrike Falcon's system extension is active, and endpoint network
      filters can stall kernel TCP flows. The test would be the same loop with it paused,
      which needs IT's say-so on a managed Mac.
- Offload benchmark, 20 real frames per tool, read from the card uncached: the `cull`
  engine runs at 216 MB/s (hash, uncached write, evict, verify from disk, F_FULLFSYNC);
  `cp` runs at 264 MB/s. The gap was the verify re-read, which wasn't overlapped with the
  next file's card read until the pipeline (below). A dry run over 992 frames plans in about 1 s.
- **Pipelined Run (2026-10-06): file N's fsync, verify and naming overlap file N+1's card read.**
  cardbench now times `offload.Run` itself, pipelined and with the serial seam, 3 rounds × the
  card's 10 frames (1.8 GB per arm), the card's pages evicted before every run (one frame
  sometimes stays cached; the bench logs it). Durable MB/s, serial → pipelined:
  - Mac SSD 190 → 194 (within round-to-round noise, 177–224; 1 of 3 pipelined runs started with
    a frame cached). **Not explained yet.** The serial split there was card read + hash + write +
    fsync 8.31 s and verify + link + manifest 1.16 s of 9.5 s. The fsync and verify are stage B and
    overlap, and the card-bound ceiling that session was about 240 MB/s (`ditto` before sync, 241).
    So some gain was expected. Candidates, unmeasured: stage B's fsync and verify read contending
    with stage A's uncached writes on the same SSD, or fsync being a small share there.
  - Grey (USB SSD, exFAT) 142 → 153 (+8%). All 3 pipelined runs started with one of the 10
    frames still cached on the card side (1 of 3 serial runs did), so part of the gain is that.
  - NAS over 10 GbE (`/Volumes/photos-1`) 92 → 107 (+16%); no cache bias against serial.
  - A per-stage probe on the NAS (serial, per 602 MB): card read + writes 2.1–3.3 s, fsync about
    2.5 s, verify + link + manifest 1.3 s. Pipelined, stage B (fsync then verify) bounds the run:
    111–114 MB/s. More would need two files in stage B at once (two concurrent writers measured
    229 MB/s), which the manifest's plan order would then have to reorder: not built.
  - **Live:** `cull offload "/Volumes/LEICA M" /Volumes/photos-1/cull-pipe-tmp --name pipetest
    --no-scan` with the card's pages evicted first: 10 of 10, 0.6 GB in 6.2 s (102 MB/s), "safe to
    format", peak memory 50 MB; `--verify` 10/10; every copy byte-identical to the card, manifest
    in card order. A run straight after other reads of the card said 138 MB/s: warm card pages.
- **Card read with read-ahead (2026-10-06): `openSource` no longer sets F_NOCACHE, and a retry
  evicts the card file first.** The stage-A profile (`profile_card_test.go`) found F_NOCACHE reads of
  the card (exFAT) slower than plain ones: ~210 MB/s (177–254) against 262, steady. F_NOCACHE +
  F_RDAHEAD didn't help, and F_NOCACHE still left 98% of the pages cached. Temps and the verify read keep F_NOCACHE. cardbench, main vs the change back to back,
  3 rounds × the 10 frames per arm, durable MB/s pipelined (serial in brackets):
  - Mac SSD: two sessions 209, 209 → 230, 239 (199, 207 → 210, 230). Pipelined runs that started
    with no frame cached, both sessions pooled: 208–216 → 220–252.
  - Grey (USB SSD): 147, 157 → 164, 168 (148, 152 → 168, 166). Clean pipelined runs, both sessions
    pooled: 144–151 → 155–171. In session 1 the after arm had 3 of its 6 cull runs start warm (one
    frame cached), against 1 of 6 before: a small upward bias on its 164 and 168. Grey stays bound
    by its own mixed write + verify read.
  - NAS over 10 GbE (`/Volumes/photos-1`), one session: 101 → 134 (89 → 101).
  - On both sides some runs started with one frame (3664 of 36780 pages) back in the cache after
    eviction; the ranges above leave those out.
  - mincore can't show whether the card was opened F_NOCACHE: on APFS an F_NOCACHE read left
    2048/2048 pages cached, as a plain read did. Writes do show it (F_NOCACHE 0/2048, normal
    2048/2048), which `TestTempCopiesStayUncached` guards for the temps.
  - **Retries:** before each retry (never try 0), `evictPasses` evicts the card file through a
    read-only mapping (msync MS_INVALIDATE, mincore, up to 5 passes), so the retry reads the card,
    not RAM. Best effort: a failure or pages left only add a verbose note. Retries run with nothing
    else in flight, so this never evicts while the card streams (which cost 262 → 189 in the
    profile). Not yet exercised live: no file has failed on the real card.
  - **Live:** `cull offload "/Volumes/LEICA M" /Volumes/Grey/cull-ra-tmp --name ratest --no-scan`:
    10 of 10, "safe to format", `--verify` 10/10; the card's listing and SHA-256s unchanged. Its
    300 MB/s means nothing: the card's pages were warm from the benches.
- Page cache (internal SSD, `mincore`):
  - a normal write leaves every page cached, so a re-read verifies RAM;
  - with `F_NOCACHE` on the write handle, 0 of 4097 pages stayed cached in a quiet probe;
  - but under load, the second 2 MiB of each 4 MiB write sometimes stayed cached.
  - `msync(MS_INVALIDATE)` on a mapping evicted every page of a fully cached 80 MB file
    (5/5 trials). Offload evicts and checks 0 resident before every verify read
    (`offload.dropCache`).
  - Under load, roughly 1 run in 20 had 2 pages back in the cache between that check
    and the read: pages [0 1] or [0 2047], a file's first and last. That's a scanner
    (Spotlight or XProtect) reading new files.
    - **This doesn't break the guarantee:** nothing writes the copy after eviction, so a
      re-cached page came from the device.
    - **The tests tolerate ≤ 1% resident** at verify start. Without eviction, 100% is
      resident, so they still catch a failure.

### One counter, two prefixes (2026-10-07, the user's M11-P card, read only)

- The camera names frames from **one shared 4-digit counter** under two prefixes:
  M1102767–M1102771 (shot 19:09 camera time), then L1002772–L1002776 (19:23; Content
  Credentials frames). Sorting by name put every `L…` before every `M…`: a live
  `cull rename <shoot> "{date}_{name}_{n:4}"` numbered L1002772 → `_0001` … and M1102767 →
  `_0006`, the wrong order.
- So camera order is DCF order (`internal/dcf`): the DCF folder number (`100LEICA`; it
  increments when the counter wraps past 9999) when every frame's is known, then the
  4-digit counter, then the name. Capture times can't stand in (the clock wasn't running).
  For a single-prefix shoot it equals name order, as long as all names are DCF-shaped and
  the counter doesn't wrap inside the shoot. Other names (edited copies such as
  `M1103127-Edit`) sort after all camera names, by name. offload orders card by card in
  argument order, DCF order within each card.
- Not yet checked: what the M11-P does at a wrap (folder number, prefix), and whether
  other bodies share a counter across prefixes.

### Junk filter (2026-10-02, the 992 frames copied to Grey and the 17 samples)

Each frame's luma is measured: the share near-black (< 16), the share blown (≥ 250), and
the contrast (std) of a 64 px thumbnail.

| Rule | Threshold | Real frames |
|---|---|---|
| black | ≥ 98% near-black | max 74% (an underground scene) |
| white | ≥ 95% blown | the blank M1103546 99.95%; next 82% (the high-key Smith Tower) |
| uniform | contrast < 3 | M1103546 0.31; every real frame ≥ 10.0 (low-key portraits in shade: M1103899, M1103879, M1104048) |

- **Fine detail doesn't work as a rule.** The 90th-percentile cell and the most-structured
  cell both rank sharp shallow-focus portraits (M1103914, M1103917, M1103568) and a
  soft-sky building (M1103502) below the blank frame. Don't reintroduce it.
- **`cull scan` on all 992 (read-only, `-o` to a temp folder):** 9 min 50 s at `-j 4`,
  1.88 GB peak. Junk: 1 (M1103546, white), decided cull. The 17 samples: 0.
- **One frame has EXIF orientation 6** (the first seen): 492 have 1, 499 have 8. Its
  display hasn't been checked visually.

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

### Date fixing and renaming (2026-10-06/07, built for a camera whose clock stopped)

The user reports that their M11-P's real-time clock is dead, so every frame carries the
same timestamp. The facts below were measured while building `--set-date`, `redate` and
`rename`.

- **The card's frames, read-only probe of `/Volumes/LEICA M` (10 frames, 2026-10-06):**
  - **5 `L…` frames carry Content Credentials:** a signed C2PA manifest in IFD0 tag 0xCD41,
    about 60 KB. It repeats the capture dates, and its hash binding covers the date fields,
    so any patch would invalidate the signature. `PatchDates` skips them.
  - **5 `M…` frames get exactly 5 patches each:** 3 EXIF ASCII dates (DateTimeOriginal in
    IFD0, DateTimeOriginal and DateTimeDigitized in the EXIF IFD) and 2 XMP attributes
    (`xmp:CreateDate`, `xmp:ModifyDate`). Nothing else was reported left alone.
  - **The embedded XMP** uses the literal prefix `xmp:` (namespace `xap/1.0`), values like
    `2026-10-05T19:09:00`, with no fraction and no zone. There are no `exif:` or
    `photoshop:` date properties.
- **File times on removable filesystems** (hdiutil images, 2026-10-07):
  - exFAT keeps mtimes to 10 ms, truncated (`.1234567` → `.12`);
  - FAT32 keeps 2 s, truncated to an even second (`:01.567` → `:00`).

  So redate counts ±2 s of the target as already set, and offload's quick check allows 2 s
  (`mtimeWindow`).
- **macOS writes `._<name>` AppleDouble companions** on exFAT and FAT32 for a file with
  extended attributes, such as a Finder tag (measured on the same images).
  - A temp named `.<name>.…` would read as one for a frame named `_DSC…`, `_IGP…` or `_MG_…`.
    Hence `.cull-redate-<hex>.<name>` and `.cull-rename-<hex>.<name>`.
  - The real-filesystem tests (`CULL_ADV_DEST`) passed on exFAT and FAT32 images, with
    companions present.
- **Replacing a file drops its extended attributes** (Finder tags, comments), because the
  replacement is a new inode. redate copies them (`copyXattrs`; `TestXattrKept`, and
  `TestXattrReadOnly` for a read-only original).
- **flock on the user's NAS** (`/Volumes/photos-1`, smbfs, SMB 3.1.1 to TrueNAS,
  2026-10-06/07):
  - **A shared lock held by one process blocks a shared lock from another:** smbfs treats
    shared as exclusive. Hence the exclusive-only scheme (holder files plus a gate).
  - **Within one process, locks never conflict.**
  - **Unlocking with `LOCK_UN` and then closing drops a lock another process took in
    between.** The next holder's lock was lost in 35–47 of 60 rounds, and the stress test
    found 2–6 cases of two writers at once. Releasing by close alone: 0 in 9 runs.
  - **Closing any other handle to a locked file in the same process drops that process's
    lock.** So cull never opens a lock file it holds.
  - **A lock nobody owns comes back as EACCES,** not EWOULDBLOCK. A `.cull.lock` left on
    the share couldn't be deleted from either mount ("Resource busy"), and still refused a
    fresh flock. That it came from a `LOCK_UN`-then-close run is inferred, not proven.
    EACCES may also be ordinary smbfs contention, not only a stale lock. Hence EACCES on
    the gate counts as held, and the advice (remount, or remove the file) applies only
    when no cull command is running.
  - **Exclusive flocks across processes work** there, and on APFS, exFAT and FAT32, with no
    "lock unavailable" note.
- **Live on the user's card (2026-10-07, 10 frames, read only; every destination a temp
  folder):**
  - **`offload --set-date`:** 10 of 10, "safe to format"; `--verify` 10/10. The 5 `M…`
    copies had their 5 date fields at the target (15 bytes differ from the card); the 5
    `L…` copies were byte-identical to the card, with their file times set. The card's
    listing and SHA-256s were unchanged.
  - **`redate`** on a plain offload: the dry run matched the real run (patched 5, times
    only 5); `--verify` 10/10; a re-run said "already set 10".
  - **`rename`** then `--verify` 10/10; offload re-run after the rename copied 0, "safe to
    format"; `--undo` then `--verify` 10/10.
  - **After the DCF fix,** a rename dry run numbered the `M…` frames 0001–0005 and the
    `L…` frames 0006–0010 (camera order).
  - **Capture One takes a Content Credentials frame's date from cull's sidecar**
    (user-tested 2026-10-07; `DATETEST_C`, a byte-identical `L…` copy whose EXIF keeps
    the camera's 2026-10-05 19:23, with a cull sidecar giving 2026-10-03 12:00, Green,
    4 stars). On import it showed 2026-10-03, the green label and 4 stars. So the
    sidecar's `exif:DateTimeOriginal` / `xmp:CreateDate` / `photoshop:DateCreated` win
    over the file's EXIF there. The `--set-date` copy (A, 2026-10-04) and the redated
    copy (B, 2026-10-03), both without sidecars, showed their patched EXIF dates.

### LightCraft bridge (2026-10-08, two M10-R frames in the user's Photos share)

[LightCraft](https://getartcraft.com/apps/lightcraft) (storytold/lightcraft v0.4.0, built from
source) is a pure-Rust Lightroom reimplementation whose CLI reads cull's `crs:` XMP sidecars
on import and render. Tested as a cull → client-JPEG pipeline:

- **`crs:Exposure2012` and `crs:Crop*` are both applied on render.** A hand-written
  sidecar in cull's exact format (+0.50 EV, 60% centered crop) rendered 3120x4718 from a
  5200x7864 source (exactly 60% both axes) with the expected luma lift.
- **`crs:Crop*` are oriented-frame edges.** Proven on an orientation-6 M10-R DNG: crops
  written in display coordinates render at correlation 0.9989/0.9973 at two positions;
  transposed interpretations score 0.29 / -0.26 or fail dimension checks. This closed the
  "ASSUMPTION TO VERIFY" that used to sit on `FromDisplay`: **cull's old transposition was
  wrong for LightCraft and Lightroom consumers, and is removed** (`DisplayCrop` passes
  display coordinates through; `internal/xmp/crop_orientation_test.go` guards it).
  Evidence: NAS `/Jules/lightcraft-test/crop-orientation-test/`.
- **Keywords and rating also flow** (`docs/xmp-interop.md` in the LightCraft repo maps
  them), but Capture One remains the only target for the AppleScript path.
- **Rendering is CPU-only headless, ~25 s and ~2.9 GB per 60 MP frame.** A 300-frame keep
  set is an overnight job, not a coffee break.
- **M10-R corrective preset:** `docs/lightcraft/leica-m10r-standard.lcpreset` (plus bare
  `.json` for `--settings`), tuned against the camera's own embedded JPEG previews; closes
  the flat-color/contrast gap (validation table in `docs/lightcraft/README.md`).
- LightCraft never *writes* `crs:` — the bridge is one-way (fine for DNG→JPEG delivery;
  round-trips back to Capture One keep only what apply-c1 put there).

## Unverified assumptions — check before building on them

- What makes the M11-P write Content Credentials on some frames (`L…`) and not others
  (`M…`). It was seen on one card only.
- The cost of `--set-date`'s proof: one extra full re-read of every patched copy, and one
  extra fsync. This hasn't been benched; expect it to show on the NAS, where stage B
  already bounds the run.
- The smbfs flock findings come from one TrueNAS share. Other SMB servers are unchecked.

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
- Capture One AppleScript property names for rating, keywords, exposure, crop.
  Dump the real dictionary with `sdef "/Applications/Capture One.app"` and read it
  before writing the applier.
- How good and how stable the model's side-by-side ranking is. On the first labelled shoot
  (2026-10-05) frames outranked and nothing else were 90% the user's culls, and
  `--keep-best 3 --outranked cull` gave 0.9% false culls. That is one shoot, tuned on the
  frames it was measured on: `--outranked` stays `review` by default until a second labelled
  shoot confirms it.
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
   `cull-labels.jsonl` → `calibrate`, tune with `decide`).
   - **Safety: settled (2026-10-05).** False culls were 0.5% on 979 labelled frames (see "First
     calibration"), so the model's culls can be auto-applied for shoots like that one.
   - **Usefulness: open.** It caught 33 of 341 culls, and review is a coin flip at 49.5%.
     Candidate settings: `--keep-best 3 --outranked cull` (0.9% false culls, 80 culls caught).
   - **Next:** judge the next shoot with the candidate settings, label it, and calibrate again
     before changing any default.
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
7. ~~Date fixing and bulk renaming~~ built 2026-10-06/07 (`offload --set-date`, `redate`,
   `rename`; spec and plan in `docs/superpowers/`) for a camera whose clock stopped. Checked
   live on the card 2026-10-07; Capture One shows a Content Credentials frame's corrected
   date from the sidecar.

## Working style

Be concise and precise; explain *why*. Push back candidly on weak ideas, no
flattery. Flag uncertainty explicitly and separate verified facts from assumptions.
