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

### `cull offload <card>... <dest>`

```
cull offload /Volumes/LEICA [/Volumes/M11-P] ~/Pictures --name "Smith wedding" --backup /Volumes/Backup/Pictures \
     [--date 2026-10-02] [--rename "{date}_{name}_{n:4}"] [--checksum] [--dry-run] [--verify]
     [--project …] [--event …] [--location …] [--keyword …] [--no-scan]
```

Held to rsync's standard and beyond it where a card that is about to be formatted needs
more. The sources are only ever read: never written, renamed or deleted.

**1. Plan before copying** (like rsync's file list):
- list every regular DNG under each source, recursively;
- skip AppleDouble `._*`, `.Trashes` and `.Spotlight-*`;
- never follow a symlink;
- read each frame's capture date from EXIF, falling back to the file's modification time;
- work out the shoot folder `<dest>/<YYYY-MM-DD>[ <name>]/`: **one per run**, dated by
  the earliest frame's EXIF capture date, or by `--date`. A run is never split by day: a
  shoot that crosses midnight stays together, and a camera clock set wrong can't scatter
  frames. The plan prints the date it chose, so a wrong clock is visible before any copy.
  Later cards of the same shoot go into that folder by re-running with the same `--name`
  and date;
- work out each destination name (the camera name, or the `--rename` pattern);
- detect collisions;
- check free space on the destination and the backup with `statfs`: the bytes to copy plus
  a margin.

Every refusal (a collision, too little space, an unreadable source) happens here, before
any byte is written. `--dry-run` prints the plan: files, GB, folders, skips.

**2. Skip what is already there:**
- **The quick check, as rsync's default:** the same size and modification time as a file
  already in the shoot folder means it's skipped. The time comparison allows 2 s, because
  exFAT and FAT32 timestamps are coarse and can be stored in local time (rsync's
  `--modify-window` for FAT).
- **`--checksum`** compares by SHA-256 instead.
- **Renamed files** are matched through the manifest's checksums, so re-offloading a card
  never copies a frame twice.
- **The same camera name with different content** is refused in the plan, with a pointer
  to `--rename`.

**3. Copy, for each file in the card's own order:**
- **Read the card once.** A reader goroutine fills large buffers (4 MB), and a writer
  goroutine streams them to every destination at once (the main folder and `--backup`), so
  reading and writing overlap.
- **Hash while streaming.** The SHA-256 is computed during that single read of the card.
- **Write to a temp file:** each destination writes `.<name>.cull-<random>.tmp` in the
  final folder, as rsync does, with `F_NOCACHE` set on the write handle, then:
  - `fsync` it;
  - set its modification time to the card file's, like rsync `-t`;
  - set permissions to `0644`, not FAT's `0777`.
- **Verify the disk, not the cache.** Each temp file is re-read with `F_NOCACHE` (macOS),
  so the bytes come from the device and not the page cache, and is hashed. A match with
  the card's hash is required.
  - Measured 2026-10-02 on the internal SSD (64 MB, `mincore`): a normal write leaves all
    4097 pages cached, so a naive re-read checks RAM. With `F_NOCACHE` on the write
    handle, none stay cached, so the verify read has to reach the device.
  - The drive's own cache can still answer, and only `F_FULLFSYNC` empties it. This is the
    limit of every source-against-copy check, and it's stated, not hidden.
- **Rename into place** only after verifying, then sync the folder entry.
- **Retry:** a mismatch or a read error deletes the temp file and retries from the card
  (up to 2 retries, with backoff). If it still fails, the file is recorded as failed and
  the run continues; the exit status is non-zero at the end.
- **The manifest** `cull-offload.jsonl` in the shoot folder gets a line only after a file
  verifies, synced line by line. It holds the source path, the destination names, size,
  SHA-256 and time.

**4. Concurrency:**
- **One file at a time, no `--jobs` flag.** On the user's card and reader (exFAT over USB,
  measured 2026-10-02), uncached reads gave 282 MB/s with 1 file at a time, 271 MB/s with 2
  and 272 MB/s with 4.
- **The rest of the pipeline is not the bottleneck.** Hashing and the verify re-read from
  internal SSD are far faster than the card. A 63 GB card is one read pass of about 4
  minutes.

**5. Interruption:**
- **Ctrl-C** finishes or abandons the current file (its temp files are removed), keeps
  everything already verified, and exits non-zero.
- **A card pulled mid-run** (EIO/ENXIO) is handled the same way.
- **A re-run** resumes: the quick check or the manifest skips what's done, and stale
  `.cull-*.tmp` files are removed.

**6. "Safe to format":**
- At the end, one `F_FULLFSYNC` per destination volume flushes the drive's own write cache.
  Plain `fsync` on macOS doesn't. The shoot folders' entries are synced too.
- Only when every planned file is verified on every destination, and these syncs
  succeeded, does it print: **"all N files verified on <dest> and <backup>: safe to format
  the card"**.
- Otherwise it lists what failed and never says that.
- **`cull offload --verify <shoot-folder>`** re-hashes every manifest entry later, for
  example before reformatting the card the next day.

**7. Progress and summary:**
- progress in bytes, with throughput and ETA (Part 1's view);
- the summary gives copied, skipped as already present, failed, total GB and MB/s per
  destination.

**Names:**
- **Kept by default.**
- **`--rename <pattern>` tokens:**
  - `{date}` is the capture date as YYYYMMDD;
  - `{name}` is `--name`, with spaces as `_`;
  - `{orig}` is the camera's file stem;
  - `{n}` or `{n:W}` is the counter, zero-padded to W digits.
- **The counter continues across cards:** the next `n` is 1 + the largest `n` among the
  shoot folder's files matching the pattern.
- **Frames are numbered by camera file name, then source path, not capture time.** File
  numbers are the camera's own order, and they survive a clock set wrong.
- So card 2, 3 and so on carry on where the last stopped.

**Not done:** rsync's rolling-checksum delta transfer and compression. Neither helps a
local copy of new files; rsync itself uses whole-file copies for local disks.

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

**Offload** (against a fake card directory):
- **Plan:**
  - one folder per run, dated by the earliest capture date or `--date`;
  - a collision is refused before any write;
  - too little free space is refused before any write (a test hook for `statfs`);
  - `--dry-run` writes nothing.
- **Copy:**
  - injected corruption between write and verify is caught: retried, then failed, with
    nothing left under the real name;
  - a source read error mid-file is injected, and must be retried and recovered;
  - an interruption leaves no partial file under a real name, and the re-run resumes;
  - the quick check honours the 2 s window;
  - `--checksum` skips identical files;
  - the manifest skips renamed duplicates;
  - the rename counter continues across two "cards";
  - backup verified separately;
  - "safe to format" appears only when everything verified;
  - modification time and `0644` are kept;
  - the card tree is byte-for-byte unchanged, shown by hashing it before and after.
- **`--verify`** finds a copy corrupted later.
- **Benchmark:** offload against `cp` on 2 GB of large files, with the result recorded.

**Sorting:**
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
