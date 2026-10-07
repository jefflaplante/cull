# Date fixing and bulk renaming: design

**Goal:** fix capture dates written by a camera with a dead real-time clock, and rename
frames in bulk, without losing cull's guarantee that every copy is exactly what the card
held.

The user's Leica M11-P has a failed RTC: every frame carries the same timestamp, whatever
the menu was last set to. True capture times are gone. The dates must be corrected:

- in each DNG's metadata, as Capture One, Lightroom and Finder read it;
- on the file itself.

**Decided with the user (2026-10-06):**

| Question | Decision |
|---|---|
| Where corrected dates live | Patch the copies (never the card) |
| The corrected time | One fixed time on the date the user gives |
| When date fixing is available | During offload, and as a standalone command for folders already offloaded |
| Rename scope | Anywhere, before or after judging, keeping every reference linked |

**Out of scope:**

- recovering real capture times (from a phone or a GPS track);
- shifting by an offset, which doesn't apply to a stopped clock;
- renaming the shoot folder from `redate`;
- editing EXIF fields other than the capture dates;
- other raw formats.

## 1. Commands

| Command | What it does |
|---|---|
| `cull offload … --set-date YYYY-MM-DD [--time HH:MM:SS]` | Each copy's dates are fixed as it lands (§3). The shoot folder is dated with `--set-date` (it implies `--date`). `--time` defaults to `12:00:00`. |
| `cull redate <folder> --date YYYY-MM-DD [--time HH:MM:SS] [--dry-run]` | Fixes every frame of an already-offloaded shoot folder, including frames in `keep/`, `review/` and `cull/`. With `--recursive`, subfolders too. `--dry-run` prints per-file what would change and writes nothing. |
| `cull rename <folder> "<pattern>" [--dry-run]` | Bulk-renames frames. Before or after judging. |
| `cull rename --undo <folder>` | Restores the names from the last journal (§4). |

- **Rename tokens:** the same as offload's `--rename`: `{date}` (YYYYMMDD from the frame's
  capture date: the fixed date once redated), `{name}` (the shoot name from the folder,
  `<date> <name>`), `{orig}` (the camera's file name), `{n}`, `{n:W}`.
- **Numbering:** in camera file order (the camera's own name order), never by capture time.
  Times are untrustworthy here by definition.
- **Pattern validation:** as offload's: `{n}` or `{orig}` is required.
- Every command fits the help sections, the dotfile rules (`--set-date`, `--time` and
  `--undo` are one-off: not settable from `~/.cull`), `status` hints and the docs.

## 2. The fields that change

**Inside each DNG.** All values are rewritten at the same length, digit for digit, so no
byte outside them changes:

- **TIFF/EXIF ASCII date-times** (`YYYY:MM:DD HH:MM:SS\0`, 20 bytes), wherever they sit. The
  M11-P stores DateTimeOriginal in IFD0:
  - DateTime (0x0132), in IFD0;
  - DateTimeOriginal (0x9003) and DateTimeDigitized (0x9004), in the EXIF IFD.
- **SubSecTime, SubSecTimeOriginal, SubSecTimeDigitized** (0x9290–0x9292), where present:
  every digit set to `0`, same length.
- **The embedded XMP packet** (tag 0x02BC in IFD0, where present). The date values
  `xmp:CreateDate`, `xmp:ModifyDate`, `xmp:MetadataDate`, `exif:DateTimeOriginal`,
  `exif:DateTimeDigitized`, `photoshop:DateCreated`, as attributes or elements:
  - only their date and time digits are rewritten;
  - their format and length are kept, including any fractional seconds (zeroed) and any
    time-zone suffix (kept);
  - a value cull can't parse, or one that would change length, is left alone, and the
    file's report says so.
- **Left alone:** time-zone offsets (OffsetTime*, 0x9010–0x9012) and every other byte.
- **Values that need no change:** if a field already holds the target value, it isn't
  rewritten. That makes a re-run of `redate` idempotent.

**Frames with Content Credentials (C2PA)** (amended 2026-10-06, user decision). Leica
writes a signed C2PA manifest (IFD0 tag 0xCD41) into some frames: the `L…` files on the
M11-P. The manifest repeats the capture dates, and its signature covers the date fields,
so any patch would invalidate it. These frames are therefore never byte-patched:

- they're listed as skipped, with the reason;
- their file times are still set;
- cull's sidecar still carries the corrected date. The report records it as
  `Result.DatesSet`, which the sidecar uses in place of the file's own EXIF date.

**GPS dates** (GPSDateStamp, UTC, from a paired phone) are out of scope and never patched.

**On the file:**

- **Modification time:** set to the corrected date and time, in the Mac's local time zone,
  as the camera's local time would be.
- **Creation time** (Finder's "Created", via `setattrlist`): set best effort; filesystems
  without it skip with one note.

**In the sidecar:** the `.xmp` sidecar cull writes (`labels.Sidecar`) gets
`exif:DateTimeOriginal`, `xmp:CreateDate` and `photoshop:DateCreated` with the corrected
value. Foreign sidecars are never touched (the existing rule).

**Unverified:** whether Capture One 16.7 takes the capture date from the DNG's EXIF or from
the sidecar. The plan's live check imports a redated test frame to find out, and the
result goes into CLAUDE.md.

## 3. Patching safely

A new `internal/dng` function `PatchDates(r io.ReaderAt, size int64, t time.Time) ([]Patch, error)`
returns the exact byte ranges and their new contents (`Patch{Off int64; Old, New []byte}`)
without writing anything. `Patch.Old` is what the file holds there now. Every writer below
applies exactly these patches and then proves the result.

**Proof (shared): hash equality against "source with exactly these patches".**

- While cull reads the source anyway, it computes two SHA-256s in the same pass:
  - `orig`: the bytes as read;
  - `want`: the same bytes with the patches applied in-stream.
- After writing the patches and fsyncing, it evicts the file and re-reads it from the
  device (F_NOCACHE, the existing verify path). That hash must equal `want`.
- This proves the file is the source byte-for-byte except at exactly the patched ranges,
  which hold `Patch.New`.
- A mismatch removes the temp and fails the file; the retry rules are offload's.

**Offload with `--set-date`.** Per file, in stage B of the pipeline:

1. **Compute the patches** from the temp's metadata. A DNG's IFDs and XMP packet are read
   with ReadAt, so no full read is needed.
2. **Verify, extended:** fsync the temp, evict it, and re-read it from the device as today.
   - `orig` must equal the card's hash, which is today's check.
   - `want` is computed in the same pass.
3. **Patch:** `pwrite` the patches into the temp, fsync, evict, and re-read it from the
   device. The hash must equal `want`.
4. **Set the file times,** then link-no-replace to the final name, as today.

Cost: one extra re-read of the copy per file (the proof). Measure it in the plan's bench.

The manifest entry (`cull-offload.jsonl`) keeps `sha256` = the card's hash, plus new
fields:

| Field | Meaning |
|---|---|
| `file_sha256` | the patched file's hash |
| `dates_set` | the corrected value |
| `patched_at` | when the patch was applied |

- `--verify` checks `file_sha256` when present, `sha256` otherwise.
- The skip logic for a re-run of offload must treat a patched entry as already copied when
  the card file's size and modification time and its `orig` match the entry, rather than
  comparing against the destination file's (now changed) modification time. A re-run
  copies nothing and stays "safe to format". This needs a test.

**`redate` on an existing folder.** Per file:

1. **Compute the patches.**
2. **Stream the original** into a hidden temp beside it (`.<name>.cull-*.tmp`, the existing
   temp convention), applying the patches in-stream. In the same pass, compute:
   - `orig`, which must equal the manifest's checksum for this file (`file_sha256`, else
     `sha256`) when one exists. A mismatch means the file was already damaged or changed:
     refuse that file and report it, never "fix" a corrupted file.
   - `want`, the hash of the patched bytes.
3. **Prove the temp:** fsync it, evict it, and re-read it from the device. Its hash must
   equal `want`.
4. **Set the times on the temp.**
5. **Atomically rename it over the original** (`rename(2)`, same directory), then fsync the
   directory.
   - This is the one deliberate exception to "never replace a file", allowed only here.
   - A crash leaves either the old or the new file, never a torn one, plus at most a
     hidden temp that the next run removes.

Then the bookkeeping, per file, in this order:

- the manifest entry gets `file_sha256`, `dates_set` and `patched_at`;
- the report's `Result.ModTime` and `Exif.DateTimeOriginal` are updated, and the report is
  saved;
- the sidecar is rewritten (if cull's).

**The report must be updated.** Its key is path + size + modification time, and
`judge`/`scan` resume on that key. Without the update, the next judge would re-judge and
re-bill every frame. `redate` therefore:

- saves the report at least every N files (the checkpoint convention) and at the end;
- writes a journal, `cull-redate.json` in the shoot folder: the target, the per-file state
  (planned, swapped, recorded) and the old size/mtime/sha. A re-run with the same target
  finishes an interrupted run: files whose dates already match are recorded without
  re-patching.
- While a journal is incomplete:
  - `status` says so and names the command to finish;
  - `judge` refuses to run, so it can't re-bill frames whose report entries are stale.
- The journal is removed when complete.

**After import:** both `redate` and `rename` print one warning that Capture One and
Lightroom may lose track of files changed after import (as `--sort` does). Use `apply-c1`
for imported frames; `apply-c1` doesn't set dates, which is out of scope.

## 4. Renaming safely

1. **Plan.** List every frame (with sort folders), compute every new name, and refuse
   before touching anything when:
   - two frames would get the same name;
   - a new name exists and isn't one of the frames being renamed;
   - the pattern is invalid;
   - an offload or redate journal is incomplete.

   `--dry-run` prints the plan: `old → new`, with folders.
2. **Journal** (`cull-rename.jsonl` in the shoot folder): written and fsynced before any
   move, with every `old → temp → new` triple for the DNG and its sidecar.
3. **Phase 1:** rename each DNG and sidecar to a unique hidden temp name in the same
   folder, link-no-replace style.
4. **Phase 2:** temp to the final new name, never replacing. Phases 1–2 let frames safely
   swap names (A→B and B→A).
5. **Then every reference follows the file, saved atomically (temp + rename):**
   - **Report:** `Result.File`, `XMP` and `MovedTo` paths; `Sets` member and order paths.
   - **Labels log** (append-only, keyed by base name): for each renamed frame with an
     entry, append its latest entry under the new name, with `At` = now and an optional new
     field `from` = the old name. Last line wins, so the new name carries the label;
     nothing is rewritten.
   - **Offload manifest:** append a superseding entry per renamed file. The manifest
     reader takes the last entry per `orig` as current, so `--verify` and offload's skip
     find files under their new names.
   - **Review cache:** images are named from size, modification time and focus box, not
     the file name, so nothing changes there. Confirm in code; if any cache key includes
     the name, rename those images too.
6. **Finish:** mark the journal complete and keep the last one, so `--undo` can reverse it.
   - `--undo` runs the same two phases in reverse and appends label and manifest entries
     back.
   - An interrupted rename is finished by re-running the same command, or reversed with
     `--undo`.
   - `status` reports an incomplete journal, and `judge`, `decide`, `review` and `redate`
     refuse while one is incomplete.
7. **No overwriting.** Nothing is ever replaced, and DNG contents are untouched by rename.

## 5. Invariants: the change to CLAUDE.md

"Never modify or delete DNGs" becomes:

> Never modify or delete DNGs, with one exception: `offload --set-date` and `redate`
> rewrite only the capture-date fields listed in the spec, at the same length.
> - The card is never written.
> - Every patched file is proven byte-identical to its source outside those fields before
>   it takes its name.
> - The manifest keeps both the card's hash and the patched file's hash.

"Never replacing a file" gains one exception: `redate`'s atomic rename over the original,
after the proof step. Everything else is unchanged.

## 6. Testing

**Synthetic DNG fixtures** (extend the existing test builders) with:

- all three ASCII dates;
- sub-second fields;
- an embedded XMP packet, with attribute and element forms, a fractional value and a
  time-zone value;
- one fixture with no EXIF IFD;
- one with an unparseable date;
- one where a field already has the target value.

**Tests:**

- `PatchDates`: exact offsets, lengths kept, idempotence, refusal on a length change.
- **The proof:** catches a stray byte changed outside a patch, a patch not applied, and
  a damaged original (`orig` doesn't match the manifest), which redate refuses.
- **Offload `--set-date`:**
  - the card is unchanged (tree hash);
  - each patched copy differs from its card file only at the patch ranges;
  - the manifest has both hashes, and `--verify` passes;
  - a re-run copies nothing and is "safe to format";
  - crashes injected at every step leave only hidden temps.
- **`redate`:**
  - each file differs from its original only at the patch ranges, and its times are set;
  - the report's keys are updated, and a following `judge` (stub backend) makes **zero**
    model calls;
  - interrupted at each step, then re-run, it completes;
  - `judge` refuses while the journal is incomplete;
  - frames in sort folders are handled.
- **`rename`:**
  - a swap (A↔B) works;
  - a collision is refused before any move;
  - a name owned by a foreign file is refused;
  - the report, labels (effective labels unchanged per frame), manifest (`--verify`
    passes) and sidecars all follow;
  - `judge` after a rename makes zero model calls;
  - an interrupted run, then a re-run, completes; `--undo` restores everything;
  - `--dry-run` writes nothing.
- **The usual suite:** `-race` on offload, the full suite and vet.

**Live checks:**

- From the real card (read-only): `offload --set-date` to a temp folder, then `--verify`
  and `exiftool -time:all` on a copy (if exiftool is installed; otherwise cull's own
  reader). Then `redate` and `rename` on that folder.
- The user imports one redated frame into Capture One to confirm which date it shows.
  That result is recorded in CLAUDE.md.

## 7. Docs

- `README`, `USAGE`, the site (a short section in the usage walkthrough's offload step,
  plus reference entries for `redate` and `rename`) and `CLAUDE.md` (Layout, Invariants,
  Verified facts after the live checks).
- The help texts and `status` hints.
