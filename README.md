# gophotocull

Culls Leica M11-P DNGs using a vision model on the embedded JPEG preview.
Priority: **sharpness gates, exposure gets fixed, composition gets cropped.**
Stdlib-only Go (no module downloads).

## Quick start

```sh
make build
# 1. Verify previews first (no API calls): resolution, source, peak sharpness
./bin/gophotocull -dry-run /path/to/shoot
# 2. Evaluate
./bin/gophotocull -csv /path/to/shoot/cull.csv /path/to/shoot
# 3. Optionally write sidecars (never clobbers existing .xmp)
./bin/gophotocull -resume -write-xmp /path/to/shoot
```

API key resolution: `-api-key-file` > `$ANTHROPIC_API_KEY` > `~/.anthropic/api_key`,
`~/.config/anthropic/api_key`, `~/.anthropic_api_key`, `~/.anthropic`. Warns if the
file is group/world readable.

## Pipeline

1. `internal/dng` — walks TIFF IFDs/SubIFDs for the largest reduced-resolution JPEG
   (reads IFDs + preview bytes only, never raw data). Falls back to `exiftool` if the
   preview is below `-min-preview-edge`.
2. `internal/imageprep` — applies EXIF orientation; downsizes full frame to
   `-max-edge`; selects the `-tiles` highest-Laplacian-variance regions at native
   resolution (where focus actually landed); measures luma percentiles and clipping.
3. `internal/eval` — one Messages API call per image, structured output via a forced
   tool call. Retries 429/529/5xx with backoff, honours `retry-after`.
4. `eval.Policy` — **deterministic decision in Go**, not the model's call:
   - `missed_focus` / `motion_blur` → cull; `soft` → review
   - exposure `clipped` → review (preview clipping overstates raw clipping)
   - composition never culls; invalid or < `-min-crop-area` crops are dropped
5. `internal/report` — JSON is the source of truth (checkpointed every
   `-checkpoint` results; `-resume` keys on path+size+mtime). Optional CSV.

## Write-back

| Target | Rating / label / keyword | Exposure / crop |
|---|---|---|
| XMP sidecar → Capture One (Image › Sync Metadata) | yes | **no** — C1 doesn't reliably translate `crs:` settings |
| XMP sidecar → Lightroom Classic | ignored for DNG (LR uses embedded XMP) | ignored |
| Capture One AppleScript (TODO) | yes | yes |

`-xmp-develop` writes `crs:Exposure2012` and `crs:Crop*` for ACR/Bridge-style consumers.

## Known gaps / TODO

- [ ] Verify M11-P preview dimensions with `-dry-run`. If too small for focus
      judgement, render tiles from raw (LibRaw `dcraw_emu` shell-out).
- [ ] Raw-level clipping check (LibRaw) so `clipped` can become a real cull gate.
- [ ] Capture One applier: generate AppleScript/JXA from the report to set rating,
      exposure, and crop per variant. Verify property names against the C1 scripting
      dictionary before running against a catalog.
- [ ] `crs:Crop*` coordinate space for rotated images is an unverified assumption
      (stored orientation); `crs:CropAngle` not written.
- [ ] Message Batches API mode (async, cheaper) for large runs.
- [ ] Burst/near-duplicate grouping so only the best frame of a sequence is kept.
