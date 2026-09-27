# gophotocull

Culls Leica M11-P DNGs using a vision model on the embedded JPEG preview.
Priority: **sharpness gates, exposure gets fixed, composition gets cropped.**
Go + [cobra](https://github.com/spf13/cobra).

## Quick start

```sh
make build
# 1. Verify previews first (no model calls): resolution, source, faces found.
#    --save-inputs shows exactly what the model would be sent.
./bin/gophotocull scan --save-inputs /tmp/inputs /path/to/shoot
# 2. Evaluate (pick a backend; see below)
./bin/gophotocull cull --csv /path/to/shoot/cull.csv /path/to/shoot
# 3. Write sidecars from a resumed run (never clobbers existing .xmp)
./bin/gophotocull cull --resume --write-xmp /path/to/shoot
# Optional, before importing into Capture One: move culls aside, and undo it
./bin/gophotocull cull --resume --move-culled /path/to/shoot
./bin/gophotocull restore /path/to/shoot
```

| Command | Purpose |
|---|---|
| `scan <dir>` | Extract + measure previews, detect faces; writes report only, calls no model |
| `cull <dir>` | Evaluate with the model, apply policy, optional sidecars; `--move-culled` moves culls (with their `.xmp`) into `culled/` beside them |
| `restore <dir>` | Move frames that `--move-culled` moved back to where they were (never overwrites) |
| `version` | Build version (set via `make build`) |
| `completion <shell>` | Shell completion (cobra built-in) |

Global flags: `-o/--report`, `-r/--recursive`, `--max-edge`, `--tiles` ("where focus
landed" tiles, default 1), `--face-min-q` (default 80), `--save-inputs <dir>`,
`--min-preview-edge`. Run `gophotocull cull --help` for the rest.

### Backends (`cull --backend`)

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
skipped by later runs, and `gophotocull restore` puts everything back. Until the
sharpness gate is calibrated, look through `culled/` before deleting anything.

## Pipeline

1. `internal/dng` — walks TIFF IFDs/SubIFDs for the largest reduced-resolution JPEG
   (reads IFDs + preview bytes only, never raw data). On the M11-P that is a
   full-resolution 9504×6320 preview. Falls back to `exiftool` below `--min-preview-edge`.
2. `internal/imageprep` — applies EXIF orientation; downsizes the full frame to
   `--max-edge`; crops at native resolution; measures luma percentiles and clipping.
3. `internal/focus` — what should be sharp, and where focus landed:
   - pigo face detection (embedded cascades); the most confident face with
     Q ≥ `--face-min-q` is the target, centred on the eyes when found;
   - otherwise `cull` asks the model to locate the intended focus target on a small
     frame (`--locate off` to skip);
   - the subject crop (768–1536 px, native) plus `--tiles` regions with the most fine
     detail relative to local contrast — raw Laplacian variance rewards contrast, not
     focus, and picks sunlit out-of-focus foreground.
4. `internal/llm` — one structured call (system prompt, text + JPEG parts, JSON
   Schema) implemented by the `anthropic`, `claude-code` and `openai` backends; every
   answer is schema-validated in Go, with one retry.
5. `eval.Policy` — **deterministic decision in Go**, not the model's call:
   - `missed_focus` / `motion_blur` → cull; `soft` → review
   - exposure `clipped` → review (preview clipping overstates raw clipping)
   - composition never culls; invalid or < `-min-crop-area` crops are dropped
6. `internal/report` — JSON is the source of truth (schema v2, with `backend` and a
   per-frame `focus_target`; checkpointed every `-checkpoint` results; `-resume` keys
   on path+size+mtime). Optional CSV.

## Write-back

| Target | Rating / label / keyword | Exposure / crop |
|---|---|---|
| XMP sidecar → Capture One (Image › Sync Metadata) | yes | **no** — C1 doesn't reliably translate `crs:` settings |
| XMP sidecar → Lightroom Classic | ignored for DNG (LR uses embedded XMP) | ignored |
| Capture One AppleScript (TODO) | yes | yes |

`-xmp-develop` writes `crs:Exposure2012` and `crs:Crop*` for ACR/Bridge-style consumers.

## Known gaps / TODO

- [x] Verify M11-P preview dimensions: full resolution (9504×6320), no raw rendering needed.
- [ ] Calibrate the sharpness gate and `--face-min-q` against hand labels (`eval` subcommand).
- [ ] Raw-level clipping check (LibRaw) so `clipped` can become a real cull gate.
- [ ] `apply-c1` subcommand: generate AppleScript/JXA from the report to set rating,
      exposure, and crop per variant. Verify property names against the C1 scripting
      dictionary before running against a catalog.
- [ ] `crs:Crop*` coordinate space for rotated images is an unverified assumption
      (stored orientation); `crs:CropAngle` not written.
- [ ] Message Batches API mode (async, cheaper) for large runs.
- [ ] Burst/near-duplicate grouping so only the best frame of a sequence is kept.
