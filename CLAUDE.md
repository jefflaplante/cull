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
make vet
./bin/gophotocull scan <dir>          # no API calls; preview size + stats
./bin/gophotocull cull <dir> --csv x  # spends API credits
```

## Layout

- `cmd/gophotocull` — main; signal-aware context into cobra
- `internal/cli` — cobra tree: `scan`, `cull`, `version` (+ built-in `completion`)
- `internal/dng` — pure-Go TIFF IFD/SubIFD walk for the largest reduced-resolution
  JPEG; reads IFDs + preview bytes only. `exiftool` fallback.
- `internal/imageprep` — EXIF orientation, box downscale, luma/clipping stats,
  top-k Laplacian-variance tiles at native resolution
- `internal/eval` — Messages API client (forced tool call for structured output,
  retries on 429/529/5xx honouring retry-after), prompt, schema, **Policy**
- `internal/pipeline` — worker pool, resume (path+size+mtime), checkpointing
- `internal/report` — JSON source of truth + CSV
- `internal/xmp` — sidecar writer, atomic, never clobbers by default
- `internal/config` — API key resolution

## Invariants — do not break

- Never modify DNGs. Never overwrite an existing `.xmp` unless `--overwrite-xmp`.
- Never print, log, or read the API key contents beyond `internal/config`.
- Keep/review/cull is decided in Go (`eval.Policy`), not by the model. The model
  only assesses. Keeps decisions deterministic, auditable, and tunable.
- Don't run `cull` on real photos without asking first: it spends money.
- Dependencies: stdlib plus cobra. Justify anything else.
- Tests use synthetic fixtures. Never commit real images.

## Verified facts

- Capture One scripting is AppleScript/JXA, macOS only. No pixel or preview access.
- Capture One reads XMP sidecar **metadata** (rating, color label, keywords via
  Image › Sync Metadata). It does **not** reliably apply Adobe `crs:` develop
  settings (exposure, crop) from sidecars. Edits into C1 must go through AppleScript.
- Lightroom Classic ignores sidecars for DNG files (uses embedded XMP).
- Sidecar naming convention is `L1000123.xmp`, not `L1000123.DNG.xmp`.

## Unverified assumptions — check before building on them

- M11-P embedded preview dimensions. Everything about focus judgement depends on this.
- `crs:Crop*` coordinate space for rotated (orientation 6/8) images; code assumes
  stored orientation (`xmp.FromDisplay`).
- Capture One AppleScript property names for rating, keywords, exposure, crop.
  Dump the real dictionary with `sdef "/Applications/Capture One.app"` and read it
  before writing the applier.
- Whether C1 reads sidecars for DNGs or only embedded XMP.

## Roadmap (priority order)

1. **Validate previews.** `scan` a real shoot. If long edge < ~1500px, render
   detail tiles from raw (LibRaw `dcraw_emu` shell-out) instead of the preview.
2. **Calibrate before trusting.** User hand-labels ~50 frames keep/cull; add an
   `eval` subcommand that measures agreement (confusion matrix on the sharpness gate).
   Tune prompt/policy until false-cull rate is acceptable. Nothing should auto-apply
   at 1000-frame scale before this.
3. **`apply-c1` subcommand.** Read the report, generate JXA, run via `osascript`
   against the open catalog: rating/label/keyword, exposure for `fixable`, crop for
   `croppable`. Default to `--dry-run` that prints the script.
4. Raw-level clipping check so `clipped` can become a real cull gate.
5. Message Batches API mode for large runs (async, cheaper).
6. Burst / near-duplicate grouping: keep the best frame of a sequence.

## Working style

Be concise and precise; explain *why*. Push back candidly on weak ideas, no
flattery. Flag uncertainty explicitly and separate verified facts from assumptions.
