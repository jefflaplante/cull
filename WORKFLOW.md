# Workflow: culling a shoot with `cull`

The order you'd work through a shoot, from a folder of DNGs to Capture One. Every
command has `--help` with examples (`cull judge --help`). Replace
`~/Pictures/2026-10-04` with your shoot folder. For the full flag reference, see the
[README](README.md).

## 0. Setup (once)

```sh
make build      # bin/cull
make install    # optional: $GOPATH/bin/cull, so `cull` is on your PATH
```

- **API key:** `judge` and `rank` read it from `~/.anthropic/api_key`,
  `$ANTHROPIC_API_KEY`, or `--api-key-file`.
- **Subscription instead:** `--backend claude-code` uses your Claude login and needs
  no key.
- **Local model:** `--backend openai --model <name>` talks to an OpenAI-compatible
  server (free, but small models judge poorly).

Lost along the way? `cull status <dir>` shows where the shoot stands and the next command.

## 1. Check the folder (free)

```sh
cull scan ~/Pictures/2026-10-04
```

It reads every DNG's embedded preview, finds faces and groups similar frames. There
are no model calls, so it's a quick sanity check. `--save-inputs <dir>` writes out
exactly what the model would be sent.

## 2. See what judging would cost (free)

```sh
cull judge --estimate ~/Pictures/2026-10-04
```

It prints the per-frame cost plus an approximate ranking cost. The ranking figure
assumes every frame lands in a full 8-frame set, so it tends to run high: on the first
live run, a pair cost about a quarter of it.

## 3. Judge (this spends money or quota)

```sh
cull judge ~/Pictures/2026-10-04                         # API, list price
cull judge --batch ~/Pictures/2026-10-04                 # API, half price, slower
cull judge --backend claude-code ~/Pictures/2026-10-04   # subscription quota
```

- **What it does:** the model assesses each frame (sharpness at the eyes, exposure,
  composition, eyes and expression). Go, not the model, decides keep, review or cull.
  Then similar frames are grouped into sets, and each set is ranked side by side. Of
  each set, the best 3 stay keep and the rest go to review.
- **Useful flags:**
  - `--max-cost 5` stops at $5.
  - `--no-rank` skips ranking, so you can run `cull rank` later.
  - `--keep-best 1` keeps only each set's single best frame.
  - `--quota-stop 0.9` (claude-code) stops before the 5-hour or 7-day window runs out.
- **Interrupted (Ctrl-C, budget, quota):** rerun with `--resume`. With `--batch`, the
  rerun reconnects to batches you've already paid for.
- **Re-running without `--resume`** is refused once the report holds assessments, so a
  stray re-run can't throw away what you paid for. `--fresh` starts over; it is still
  refused while frames sit in `culled/` (run `cull restore` first).
- **Output:** `cull-report.json` in the shoot folder.

## 4. Review in your browser

```sh
cull review ~/Pictures/2026-10-04
```

It opens a contact sheet. Each click or key saves straight to `cull-labels.jsonl` and
to the frame's `.xmp` sidecar (`--no-xmp` saves only the labels log; `--static` writes
an offline page instead of serving).

| key | action |
|---|---|
| ← → ↑ ↓ | move (↑/↓ go by grid row) |
| Enter / Esc | open the detail view / back to the grid |
| K / R / C / U | your verdict: keep, review, cull, clear |
| 1–5, 0 | stars, or no stars |
| Z | 100% view of the whole frame, opened on the subject; drag or arrow keys pan |
| S | compare the frame's set side by side at one scale; 1–9 keeps that frame |
| N | next unlabelled frame |
| [ / ] | previous / next set |
| - / = | one keeper fewer / more in the frame's set (top N by the model's rank keep, the rest cull) |

- **Sets:** each set is boxed, with its keeper count in the header. Use **−** / **+**
  (or **-** / **=**) to keep the top N by the model's rank and cull the rest, or the
  **✓ keeper** toggle on a frame to flip just that frame. All of these are your labels.
- **Filters:** by verdict (keep, review or cull), by what you haven't labelled or
  rated yet, where you disagree with the model, and by **Sets: All / Keepers /
  Outranked**.
- **Detail view:** shows the model's reasons. For a frame in a set, it also shows
  the frame's rank, why it won or lost, and a filmstrip of the set.
- **Badges:** your label is the outlined badge, top right; the model's verdict is in
  the card's bottom row.
- **Your labels always win** over the model's for sidecars, moves and Capture One.

## 5. Check the tool against your labels, and tune it (free)

```sh
cull calibrate ~/Pictures/2026-10-04
cull decide --review-below-sharpness 7 ~/Pictures/2026-10-04
```

- **`calibrate`** reports false culls (you said keep, it culled), missed culls, and
  the review rate. It also sweeps `--review-below-sharpness`, and its sets section
  compares `--keep-best` values from the stored ranks.
- **`calibrate --compare a.json b.json`** shows how often two runs over the same frames
  disagree (judge twice with `-o run1.json` / `-o run2.json`; that spends twice). No
  labels needed. Verdicts can't be more trustworthy than they are stable.
- **`decide`** re-applies the policy with no model calls. The policy is **saved in the
  report**: later `decide`, `rank`, `calibrate` and `judge --resume` runs start from
  it, name the non-default settings they reuse, and let a flag you type override only
  its own setting. The grouping (`--seq-gap`, `--seq-look`) is saved the same way.

## 6. Before importing into Capture One

```sh
cull decide --write-xmp --move-culled ~/Pictures/2026-10-04
```

- **Sidecars:** each frame's `.xmp` gets your stars, a colour label (green keep,
  yellow review, red cull), and the keywords `cull:<verdict>` and `cull:best`.
- **Culls:** they move into `culled/` in the shoot folder; `cull restore <dir>` puts
  them back.
- **Capture One** reads these sidecars on import (verified with Capture One 16.7.2).

## 7. After importing: push changes into Capture One

```sh
cull apply-c1 --probe ~/Pictures/2026-10-04 | osascript -   # read-only check, run it first
cull apply-c1 ~/Pictures/2026-10-04 > apply.applescript     # dry run: read the script
cull apply-c1 --run ~/Pictures/2026-10-04                   # apply it
```

It sets colour tags, keywords and your stars on the images already in your catalog.
`--exposure` and `--crop` also apply the suggested fixes, but only to images whose
exposure and crop are still at their defaults: edits you made in Capture One stay.

## Ranking on its own

```sh
cull rank --estimate ~/Pictures/2026-10-04    # exact cost, free
cull rank ~/Pictures/2026-10-04               # rank the sets that need it
cull rank --batch ~/Pictures/2026-10-04       # half price
```

`rank` uses the backend and model the report was judged with, and the policy stored
in the report. It skips sets whose saved order is still valid, so re-running it costs
nothing for them. `--force` re-ranks everything. It also upgrades an older (schema v3)
report in place.

## Things to know

- **Nothing touches your DNGs** except `--move-culled`, which moves them and can be
  undone with `cull restore`. Sidecars that cull didn't write are never overwritten
  unless you pass `--overwrite-xmp`.
- **Verdicts are only as good as calibration.** Label a sample in `review`, check
  `calibrate`, and keep `--outranked review` (the default) until you trust the
  ranking.
- **Sets:** only frames with nearly identical framing get grouped into a set.
  Retakes of the same pose after reframing usually aren't. `--seq-look` widens the
  match; `--seq-gap` (60s) splits sets by capture time.
