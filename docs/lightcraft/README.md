# LightCraft preset: Leica M10-R STD

A corrective develop preset for [LightCraft](https://getartcraft.com/apps/lightcraft)
(v0.4.0) that closes the gap between LightCraft's default rendering of Leica M10-R
DNGs and the camera's own JPEG rendering. LightCraft has no M10-R camera profile
(the known `LR-IMP-CAMERA-COVERAGE` gap), so its default output renders flat:
low midtone saturation and a dark tone curve relative to Leica's embedded previews.

Built 2026-10-08 by iterative render-and-measure against the ground truth every
M10-R DNG carries: the camera's own embedded JPEG preview. Named **STD** to match
the in-camera preset the target previews were shot with (M10-R JPEG setting:
Standard) — the goal is to reproduce that specific look, not a generic "Leica look."

## Files

- `leica-m10r-std.lcpreset` — import via `preset.import` (GUI or CLI)
- `leica-m10r-std.json` — bare settings for `lightcraft-cli render --settings`

## Method

Two M10-R frames (L1001525, L1001534; 75mm Apo-Summicron, ISO 200, 16-bit
lossless-JPEG DNG). Each candidate preset rendered at 1600px and compared with
the Leica preview (rotated to display orientation) on four measures: per-channel
means, midtone saturation (mean HSV-S for luma 80–160), luma percentiles
(1/50/75/95), and structural correlation. Five iterations; the committed preset
is v6.

## Validation

| frame | render | RGB means | midsat | p1/p50/p75/p95 | corr |
|---|---|---|---|---|---|
| L1001525 | Leica preview | 82/91/98 | 64.3 | 2/95/126/220 | 1.000 |
| | LightCraft default | 78/83/87 | 45.6 | 12/83/102/185 | 0.946 |
| | **with preset** | **85/91/98** | **62.0** | 1/93/116/218 | 0.945 |
| L1001534 | Leica preview | 72/77/76 | 58.1 | 1/47/120/222 | 1.000 |
| | LightCraft default | 73/76/74 | 43.5 | 13/59/98/189 | 0.930 |
| | **with preset** | **79/82/80** | **58.2** | 1/63/113/230 | 0.928 |

The preset closes the midtone-saturation gap on both frames (45→62 vs Leica 64;
43→58 vs Leica 58) and lifts the tone curve onto Leica's, without disturbing
structure (correlation unchanged). Not a colorimetric match — Leica's profile
remains proprietary — but the "flat" default is gone.

## Scope and limits

- Two frames, one camera, one lens. Validate on a fuller shoot before client
  delivery; adjust `light.exposure` per shoot if needed.
- Tuned against Leica's JPEG profile, which is itself a look, not a reference.
- LightCraft's `develop.auto` and WB were NOT applied in these tests; the preset
  assumes default settings as its base.
- M11-P DNGs (14-bit, same DNG lineage) are expected to benefit but were not
  available to test.

## Use in the cull → LightCraft bridge

For one frame by hand:

```sh
lightcraft-cli render KEEP.DNG -o CLIENT.jpg \
  --settings leica-m10r-std.json --quality 95
```

cull's XMP sidecars (exposure + crop) are read by LightCraft on import and
render, and crop coordinates pass through in the oriented frame — see the
`crs:Crop*` orientation note in the root CLAUDE.md.

## A whole shoot: `cull develop`

`cull develop <shoot>` runs this bridge over a decided shoot's keep set. cull stays
the decision layer (which frames, their exposure and crop, all in the sidecars);
LightCraft renders. cull only execs `lightcraft-cli`.

```sh
cull develop --dry-run <shoot>        # keeps, recipe, presets per camera, estimate; writes nothing
cull develop <shoot>                  # asks, then renders <shoot>/export/<frame>.jpg
cull develop --yes -j 2 <shoot>       # no question (scripts need --yes); 2 processes, ~5.8 GB
```

**The flow:**

1. **Keep set.** Every frame whose verdict is keep: your label from
   `cull-labels.jsonl`, else the model's decision from `cull-report.json`. It is
   developed where it is now (`keep/` after `decide --sort`). A shoot with labels
   but no report works too (`-r` searches subfolders such as `raw/`).
2. **Plan.** For each keep, cull reads its camera model (EXIF) and its sidecar's
   `crs:Exposure2012` and crop. It fingerprints what the JPEG would be made from
   (commands, preset, sidecar, DNG size and mtime) and skips keeps whose recorded
   export still matches. Then it prints the estimate (~25 s and ~2.9 GB per 60 MP
   frame, CPU only; once a shoot has run, its own measured time) and asks.
3. **Chunks.** Frames go to LightCraft 10 at a time (`--chunk`), one process per
   chunk, `-j` processes at once (default 1). Each chunk gets a throwaway library
   in `.cull-develop/`, so every attempt starts from the sidecars as they are now:
   - run 1: `library.xmpPreferences autoWrite=false` (LightCraft never writes
     beside the DNGs), `library.import mode=add` (reads the sidecars), and
     `catalog.query` for the photo ids;
   - run 2, a `--script`: `preset.import` of the presets it needs, then per frame:

     | command | why |
     |---|---|
     | `library.select ids=[id] active=id` | auto, wb and straighten act on the active photo only |
     | `develop.wb mode=auto` | `--wb`; the preset doesn't touch white balance |
     | `develop.auto` | **only for bodies without a preset**: the preset sets every control auto sets |
     | `crop.autoStraighten` | levels the horizon; cull's crop rectangle stands |
     | `preset.apply id=leica-m10r-std ids=[id]` | the camera's look, keyed by EXIF Model |
     | `develop.set light.exposure = preset + EV` | auto and the preset overwrite the sidecar's exposure, so cull's EV goes back last |
     | `app.export path=<temp> format=jpeg longEdge=3000 quality=95` | `--long-edge`, `--quality` |

4. **Exports.** Each JPEG lands as a hidden `.cull-develop-<hex>.<frame>.jpg` in the
   export folder and is checked (a JPEG, of the size LightCraft reported) before it
   takes its name. A JPEG cull didn't export is never replaced: that frame fails.
5. **State.** `cull-develop.json` (beside the report) records the recipe, LightCraft's
   version and each frame's export and SHA-256. It is saved after every frame, so
   Ctrl-C or a crash loses only the frame in progress: the same command picks up
   from there. Changing a sidecar, a flag or the DNG redoes just those frames.
6. **Report:** frames developed, failed, up to date, the JPEGs and their size, the
   time. Failed frames are listed with LightCraft's error; their chunk's logs stay
   in `.cull-develop/` and the command exits non-zero.

**Why `develop.auto` doesn't run on an M10-R.** All of LightCraft's stages set values
outright. Measured on the two frames: `develop.auto` replaced the sidecar's +0.50 EV
with +0.92 (and wanted +1.71 on L1001534, whose camera JPEG is dark by intent), and
`preset.apply` then replaced all eight controls auto had set. Running auto before the
preset is wasted work. Running it after would undo the tone calibration above. cull's
EV is relative to the camera's rendering, which the preset reproduces, so it is added
to the preset's own +0.3. A body with no preset gets `develop.auto` and a warning; its
EV then replaces auto's exposure.

**`develop.matchExposure` isn't in the recipe.** It would override cull's per-frame EV,
and with chunks its reference photo would differ from chunk to chunk.

**Validated** on the two M10-R frames (2026-10-08). From labels alone, with no
sidecars, it developed 2 of 2 in 46 s: `L1001525.jpg` 2,996,168 B and `L1001534.jpg`
3,672,648 B, both 1984×3000. Both were pixel-identical to a hand-written
`lightcraft-cli run` of the same stages. A copy with cull sidecars (+0.50 EV; a 10%
crop on each side) was interrupted with Ctrl-C after the first JPEG. LightCraft
stopped in 0.05 s, and the re-run developed only the second frame.
