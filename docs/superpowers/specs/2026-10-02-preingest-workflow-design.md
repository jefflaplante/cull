# Pre-ingest workflow: design

**Goal:** `cull` does the culling before a shoot reaches Capture One. It takes the card
from offload to sorted, tagged folders ready to import:

1. **Offload:** card to a dated shoot folder, verified, optionally backed up.
2. **Scan:** junk frames are culled for free.
3. **Judge:** the model assesses each frame and gives content keywords.
4. **Review:** you label and adjust.
5. **Sort:** `keep/`, `review/`, `cull/`.
6. **Import** `keep/` and `review/` into Capture One.

Every frame carries your project, event and location as keywords.

**Out of scope (deferred by the user, 2026-10-02):** Lightroom support, other raw formats,
watch/tethered mode, learning your taste.

**Order:** four parts, each its own plan, built in this order. Output comes first because
the others report through it.

| Part | What |
|---|---|
| 1 | Output: log levels and progress display |
| 2 | Offload and folder sorting |
| 3 | Junk pre-filter |
| 4 | Keywords: content from the model, and your tags |

Invariants unchanged: DNGs are never modified; moves are same-disk renames that never
overwrite and are recorded in the report; decisions stay in Go.

---

## Part 1: Output

**Verbosity** (persistent flags, valid on every command):

| Flag | Shows |
|---|---|
| `-q, --quiet` | Errors, warnings and the final summary only |
| default | Progress, plus a one-line result per frame (as today) |
| `-v, --verbose` | Per-stage detail: face found or located, landed tile, tokens and cost per call, timings per stage |
| `--debug` | Also request sizes, backend events (`claude -p` init and rate limits, HTTP retries) and the raw model JSON for each call |

`--log-level quiet|normal|verbose|debug` is the long form.

**Display:**
- **On an interactive terminal (stderr):** a Bubble Tea view with:
  - a progress bar per stage (offload bytes, frames, ranking sets) with an ETA;
  - live tallies (keep / review / cull / junk / errors);
  - spend (dollars, or subscription quota %);
  - the last few frame lines.

  Ctrl-C keeps today's meaning: finish in-flight work, save, exit.
- **Elsewhere** (pipes, scripts, `--quiet`, or `--plain`): plain lines, as today. Any
  output that isn't a terminal stays line-oriented, so scripts and logs work.

**Shape:**
- A new `internal/ui` package holds:
  - a `Progress` interface (stage start/advance/done, frame result, notes at a level);
  - a plain implementation, writing to an `io.Writer` with a level filter;
  - a Bubble Tea implementation.
- `pipeline.Config` gets `Progress` beside `Log`. Existing `Log` writes become notes at
  the normal or verbose level.

**Dependencies:** `charmbracelet/bubbletea`, `bubbles` (progress, spinner) and
`lipgloss`. These are the first non-trivial additions since pigo. The justification goes
in CLAUDE.md: a live view of a 1000-frame, two-hour run is the request, and plain output
remains for anything that isn't a terminal.

**Testing:**
- the plain implementation with golden output at each level;
- the Bubble Tea model by feeding messages and checking `View()` (no terminal needed);
- each command's terminal detection is a test hook.

---

## Part 2: Offload and folder sorting

### `cull offload <card> <dest>`

```
cull offload /Volumes/LEICA ~/Pictures --name "Smith wedding" --backup /Volumes/Backup/Pictures \
     [--rename "{date}_{name}_{n:4}"] [--project …] [--event …] [--location …] [--keyword …] [--no-scan]
```

**Finding frames:** every DNG under `<card>`, recursively (DCIM). The card is only read,
never written, renamed or deleted.

**Shoot folders:** `<dest>/<YYYY-MM-DD>[ <name>]/`, dated by each frame's capture time
(local date). A card spanning days fills one folder per day.

**Copy and verify, for each frame:**
1. Write to a temporary name in the shoot folder, sync it, then rename it into place.
2. Verify by re-reading the copy: same size, and SHA-256 equal to the card file's.
3. With `--backup`, do the same into the same layout under the backup root.
4. A failed verification removes the copy and reports an error. The run continues, and
   exits non-zero at the end.

**Names:** camera filenames are kept by default.
- A name already in the folder with the **same checksum** was already copied, and is
  skipped. This makes offload resumable, and safe to re-run on the same card.
- The same name with **different content** (two bodies, or a counter rollover) is
  refused for that file. The error names both files and suggests `--rename`.

**`--rename <pattern>`:**
- **Tokens:**
  - `{date}` is the capture date as YYYYMMDD;
  - `{name}` is `--name`, with spaces as `_`;
  - `{orig}` is the camera's file stem;
  - `{n}` or `{n:W}` is the counter, zero-padded to W digits.
- **The counter continues across cards:** the next `n` is 1 + the largest `n` among files
  in the shoot folder that match the pattern.
  - Within one card, frames are numbered by capture time, then camera filename.
  - So card 2, 3 and so on carry on where the last one stopped.
- **The manifest** `cull-offload.jsonl` in the shoot folder records one line per copied
  file: card path, original name, new name, size, SHA-256, time.
  - A re-run skips any frame whose checksum is already in the manifest. That holds even
    when renamed, so re-offloading a card never copies a frame twice.

**After copying:** it runs `scan` on each shoot folder, which is free: junk pre-filter,
previews, faces, sets. Then it prints the next step, `cull judge --estimate <folder>`.
`--no-scan` stops after verification.

**Tags:** `--project`, `--event`, `--location` and `--keyword` go into the scan report
(Part 4), so every later sidecar and `apply-c1` run carries them.

### Folder sorting: `--sort`

- **The option:** `judge --sort` and `decide --sort` move each frame, with its sidecar,
  into `keep/`, `review/` or `cull/` inside the shoot folder, by its effective verdict:
  your label, otherwise the model's.
  - Running it again re-sorts after label changes, moving frames between folders.
  - `restore` moves everything back.
- **Safety:** same-disk renames that never overwrite, each recorded in the report
  (`moved_to`). `reconcileMove` learns the sort folders, so a crash mid-sort still leaves
  nothing it can't find again.
- **Discovery:** `Discover` skips `keep/`, `review/`, `cull/` and `culled/`. Sorted frames
  are tracked through `moved_to`, as culled frames are today.
- **Relation to `--move-culled`:** it stays, and moves only culls into `culled/`. The two
  can't be combined: `--sort` is the general form.
- **For Capture One:** import `keep/` and `review/`. Frames in `cull/` aren't imported, and
  can be deleted by hand once you've looked.

### Testing

- **Offload, against a fake card directory:**
  - layout by date;
  - the checksum is verified, and a corrupted copy (a test hook) is removed and reported;
  - a re-run skips everything;
  - a same name with different content is refused;
  - the rename counter continues across two "cards";
  - the manifest skips renamed duplicates;
  - backup;
  - the card is unchanged, shown by hashing the card tree before and after.
- **Sorting:**
  - keep, review and cull land in their folders;
  - re-sorting after a label change moves the frame;
  - `restore` puts everything back;
  - reconcile after a simulated crash;
  - never overwrites.

---

## Part 3: Junk pre-filter

**Where:** in frame preparation (scan and judge), from measurements already taken (luma
statistics, fine/coarse detail). It needs no model call.

**What counts as junk:**

| Kind | Rule (starting values, recorded in the report) |
|---|---|
| near-black (lens cap, misfire) | ≥ 98% of preview pixels at luma < 16 |
| near-white (blown or flash misfire) | ≥ 95% at luma ≥ 250 |
| featureless (pocket shot, ground blur) | the frame's best detail cell is below a floor, *and* coarse structure is nearly flat |

**What happens:**
- A junk frame records `junk: <kind>` with the measured values. `judge` doesn't send it to
  the model, which saves the call, and the policy decides it.
- The new policy action `--junk cull|review|ignore` defaults to `cull`, and is stored in
  the report's policy like the other actions.

**Safety:**
- The starting thresholds must flag **none** of the 17 real sample frames. Low-key frames
  like M1103865 (dark woodshed) must pass. Synthetic black, white and blank frames must be
  flagged.
- `calibrate` reports junk frames you labelled keep, so a false junk shows up.
- `scan` prints a junk count.

**Testing:** synthetic frames of each kind are flagged; low-key, high-key and
low-contrast synthetic portraits aren't; the real-sample check is recorded in CLAUDE.md.

---

## Part 4: Keywords

### Content keywords from the model

- **What:** the evaluation schema gets `keywords`: 3–8 short, lowercase words for subject,
  setting, notable objects and mood (e.g. `portrait, woman, forest, red dress, laughing`).
  It's listed after the verdict fields, since it is description, not evidence. Stored as
  `Evaluation.Keywords`.
- **Where it goes:**
  - into sidecar `dc:subject`, under a `content|` hierarchy (`content|forest`), so they
    don't mix with your own keywords, and Capture One shows them grouped;
  - into `apply-c1` keywords;
  - into the review page detail view.
- **Not for junk frames.** The model wasn't asked about them, so they get none.
- **Cost:** a few dozen output tokens per frame.
- **Old reports:** frames judged before this have no keywords. `judge --resume` doesn't
  re-judge for them.

### Your tags

- **Flags** `--project`, `--event`, `--location` (one each) and `--keyword` (repeatable),
  on `offload`, `scan` and `judge`.
- **Storage:** they're stored in the report (`Report.Tags`), so later `decide`, `review`
  and `apply-c1` runs use them without repeating the flags.
- **`cull tag <dir> [--project …] [--clear-location] …`** changes them later. Run
  `decide --write-xmp` afterwards to rewrite the sidecars.
- **Written as keywords** in sidecars and `apply-c1`: `project|Smith wedding`,
  `event|Ceremony`, `location|Forest Park, Portland`, and each `--keyword` as given.
  Capture One reads sidecar keywords on import (verified).
- **No IPTC location fields yet.** Whether Capture One reads them from sidecars is
  unverified; one import test would settle it. EXIF GPS is ignored: the M11-P writes none
  without the phone app, and Capture One reads GPS from the raw itself.

### Testing

- a schema test;
- a decode of a fake model answer with keywords;
- the sidecar carries the `content|` keywords and your tags;
- the `apply-c1` script carries them;
- `cull tag` round-trip;
- report storage survives `decide` and `--resume`.

---

## Open items, checked during implementation

- **Hierarchical keywords:** whether Capture One shows `content|forest` as a hierarchy
  from sidecars. If not, fall back to flat keywords with no prefix. Settled by one import
  test, like the earlier ones.
- **Bubble Tea and the `claude-code` backend:** the backend reads the child process's
  stdout, not the terminal, so there should be no conflict. Checked with a real run.
