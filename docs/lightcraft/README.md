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

```sh
lightcraft-cli render KEEP.DNG -o CLIENT.jpg \
  --settings leica-m10r-std.json --quality 95
```

cull's XMP sidecars (exposure + crop) are read by LightCraft on import and
render, and crop coordinates pass through in the oriented frame — see the
`crs:Crop*` orientation note in the root CLAUDE.md.
