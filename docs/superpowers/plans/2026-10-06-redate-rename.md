# Date fixing and bulk renaming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the capture dates written by a camera with a dead clock, and rename frames
in bulk, keeping cull's integrity guarantees. The pieces:

- `cull offload --set-date`;
- `cull redate`;
- `cull rename` and `cull rename --undo`.

**Architecture:**

- **`internal/dng` gains `PatchDates`:** it computes same-length byte patches for every
  capture-date field and writes nothing.
- **`internal/offload` gains the patch proof:** stream the source once, hashing it as read
  (`orig`) and with the patches applied (`want`), then re-read the patched file from the
  device and require `want`.
  - Offload applies this in stage B, on the temp, before naming.
  - `redate` (new package `internal/redate`) applies it via a patched temp that is
    atomically renamed over the original.
- **`cull rename`** (new package `internal/rename`) renames in two phases under a journal.
  The report, labels, manifest and sidecars follow each file.
- **A small `internal/journal` package** reports incomplete redate/rename journals. `judge`,
  `decide`, `review`, `redate` and `rename` refuse while one is incomplete.

**Tech Stack:** Go 1.26 and the existing packages. `golang.org/x/sys/unix` goes from
indirect to direct, for `Setattrlist`, the creation time. It is already in go.sum through
Bubble Tea; record the reason in CLAUDE.md's Dependencies.

**Spec:** `docs/superpowers/specs/2026-10-06-redate-rename-design.md`. Read it first; it is
binding.

## Global Constraints

- **The card is never written.** Card files are opened O_RDONLY. Any eviction uses the
  existing read-only `mapFile`.
- **A DNG changes only at the capture-date fields the spec lists, at the same length.** No
  file takes its final name, or replaces an original, until the proof passes: a
  device-read hash equal to `want`.
- **Nothing is ever replaced**, except `redate`'s atomic `rename(2)` of a proven temp over
  its original, in the same directory.
- **The manifest is append-only.** For any `(orig, size)` the last line is current. The
  `sha256` field always holds the card's hash. Patched files add `file_sha256`,
  `dates_set` and `patched_at`.
- **The labels log is append-only**, and the last line per file name wins.
- **After `redate` or `rename`, a following `judge` makes zero model calls**, because the
  report's keys follow the files.
- `--set-date`, `--time` and `--undo` are not settable from `~/.cull`: add them to
  `notInDotfile`.
- **Tests use synthetic fixtures only.** Live checks read the real card, read-only, and
  write only to your own temp folders, deleting them afterwards.
- Tests run unsandboxed (the Bash tool's `dangerouslyDisableSandbox: true`). Prefix
  `rtk proxy` for raw output. In-sandbox builds need `GOCACHE=$TMPDIR/gocache`.
- Commit style: one descriptive sentence, no prefix. Work on branch `redate-rename`.

## Review Focus

1. **Re-running offload on the same card after `--set-date`**, three ways: with camera
   names, with `--rename`, and with `--checksum`. Each copies nothing and ends "safe to
   format", never "exists with different content". (Task 3)
2. **`redate` twice, or interrupted and re-run:** no double patching, the report's keys
   stay current, and a `judge` afterwards makes zero model calls. (Task 5)
3. **`rename` that swaps two frames' names**, and frames sorted into `keep/`, `review/`
   and `cull/`. Every one ends up where it was, under its new name, with its label.
   (Task 6)
4. **A DNG whose embedded XMP holds dates in another format**, such as
   `2025-12-28T00:05:59.00+01:00` or a date-only value. Lengths are kept, the time zone is
   kept, and an unparseable value is left alone and reported. (Task 1)
5. **`redate` on files no manifest records** (copied with Finder), and on a folder with no
   report: the files are still patched and proven, and the bookkeeping that exists is
   updated. (Task 5)

---

## Task 1: `dng.PatchDates`

**Files:**
- Modify: `internal/dng/preview.go`: `ifdEntry` gains `valueAt int64`, the file offset of
  the entry's 4-byte value field, set in `readIFD`. That is
  `int64(off)+2+int64(i)*12+8`.
- Create: `internal/dng/patch.go`, `internal/dng/patch_test.go`.
- Create: `internal/dng/fixture_test.go`, a synthetic DNG builder for tests in this
  package. Export a copy for other packages as `dngtest` in Step 1 of Task 2, or keep a
  builder per package; see Task 2.

**Interfaces:**
- Produces:

```go
// Patch is one same-length rewrite of a file's bytes.
type Patch struct {
	Off      int64  // file offset
	Old, New []byte // len(Old) == len(New)
	What     string // e.g. "EXIF DateTimeOriginal", "XMP xmp:CreateDate"
}

// PatchDates returns the same-length patches that set every capture date in the DNG
// read from r (size bytes) to t (sub-seconds zeroed), sorted by Off and
// non-overlapping. Fields already equal to the target are omitted, so a second call
// on a patched file returns none. skipped names embedded-XMP values it could not
// rewrite at the same length (left alone). It writes nothing.
func PatchDates(r io.ReaderAt, size int64, t time.Time) (patches []Patch, skipped []string, err error)
```

**The fields:**

| Where | Tag | Contents | Patch |
|---|---|---|---|
| IFD0 | 0x0132 DateTime | ASCII `YYYY:MM:DD HH:MM:SS\0` | first 19 bytes |
| EXIF IFD (pointer 0x8769) | 0x9003 DateTimeOriginal | ASCII `YYYY:MM:DD HH:MM:SS\0` | first 19 bytes |
| EXIF IFD | 0x9004 DateTimeDigitized | ASCII `YYYY:MM:DD HH:MM:SS\0` | first 19 bytes |
| EXIF IFD | 0x9290–0x9292 SubSecTime* | ASCII digits | every digit becomes `'0'` |
| IFD0 | 0x02BC XMLPacket | UTF-8 XMP, BYTE or UNDEFINED | date values, below |

- **ASCII date-times:** the new value is `t.Format("2006:01:02 15:04:05")`.
  - Patch only if the current 19 bytes are a well-formed date or all spaces/zeros.
    Otherwise skip, and add `"<What>: unparseable"` to `skipped`.
  - The value's offset is `valueAt` when `count <= 4`, else `bo.Uint32(value)`.
- **XMP date values:** `xmp:CreateDate`, `xmp:ModifyDate`, `xmp:MetadataDate`,
  `exif:DateTimeOriginal`, `exif:DateTimeDigitized` and `photoshop:DateCreated`, in
  attribute form `prop="VALUE"` or element form `<prop>VALUE</prop>`.
  - VALUE is ISO 8601: `YYYY-MM-DD`, then optionally `THH:MM`, `:SS`, `.f+`, and `Z` or
    `±HH:MM`.
  - **Rewrite:** the date digits become t's date. The `HH:MM[:SS]` digits become t's time.
    The fraction digits become `0`. The zone suffix is unchanged. A date-only value stays
    date-only.
  - The new value must be the same length; if it isn't, skip and report.

- [ ] **Step 1: Write the fixture builder.** A function writes a little-endian TIFF:

  - IFD0 with NewSubfileType (0), DateTime and the EXIF IFD pointer, plus optionally an
    XMLPacket;
  - an EXIF IFD with DateTimeOriginal, DateTimeDigitized, SubSecTimeOriginal (`"53"`,
    which is inline at count 3), SubSecTime (`"530"`, out of line at count 4 or more is
    fine), and optionally a 0x9010 OffsetTime;
  - padding bytes with a known pattern between structures, so a test can check that
    every other byte is unchanged.

```go
type fixture struct {
	DateTime, DTO, DTD string // 19-char values or "" to omit the tag
	SubSec, SubSecOrig    string
	XMP                   string // full packet, or "" for none
	BigEndian             bool
}

func buildDNG(t *testing.T, f fixture) []byte
```

  Write it in full in fixture_test.go, using `encoding/binary`. Entries go in tag order.
  ASCII values longer than 4 bytes go out of line, after the IFDs.

- [ ] **Step 2: Write the failing tests** (patch_test.go):

```go
func applyAll(b []byte, ps []Patch) []byte {
	out := append([]byte(nil), b...)
	for _, p := range ps {
		copy(out[p.Off:], p.New)
	}
	return out
}

func TestPatchDatesSetsEveryField(t *testing.T) {
	xmpPkt := `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmlns:exif="http://ns.adobe.com/exif/1.0/" xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" ` +
		`xmp:CreateDate="2025-12-28T00:05:59.53" xmp:ModifyDate="2025-12-28T00:05:59+01:00">` +
		`<exif:DateTimeOriginal>2025-12-28T00:05:59.530+01:00</exif:DateTimeOriginal>` +
		`<photoshop:DateCreated>2025-12-28</photoshop:DateCreated>` +
		`</rdf:Description></rdf:RDF></x:xmpmeta>`
	b := buildDNG(t, fixture{DateTime: "2025:12:28 00:05:59", DTO: "2025:12:28 00:05:59", DTD: "2025:12:28 00:05:59",
		SubSec: "530", SubSecOrig: "53", XMP: xmpPkt})
	target := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)
	ps, skipped, err := PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("err %v skipped %v", err, skipped)
	}
	got := applyAll(b, ps)
	if len(got) != len(b) {
		t.Fatal("length changed")
	}
	for _, want := range []string{"2026:10:04 12:00:00", `xmp:CreateDate="2026-10-04T12:00:00.00"`,
		`xmp:ModifyDate="2026-10-04T12:00:00+01:00"`, `<exif:DateTimeOriginal>2026-10-04T12:00:00.000+01:00</exif:DateTimeOriginal>`,
		`<photoshop:DateCreated>2026-10-04</photoshop:DateCreated>`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("missing %q", want)
		}
	}
	if bytes.Contains(got, []byte("2025:12:28")) || bytes.Contains(got, []byte("2025-12-28")) {
		t.Error("an old date survived")
	}
	// Only patched bytes differ.
	in := func(i int) bool {
		for _, p := range ps {
			if int64(i) >= p.Off && int64(i) < p.Off+int64(len(p.New)) {
				return true
			}
		}
		return false
	}
	for i := range b {
		if b[i] != got[i] && !in(i) {
			t.Fatalf("byte %d changed outside a patch", i)
		}
	}
	// Sorted, non-overlapping, same length, Old matches the file.
	for i, p := range ps {
		if len(p.Old) != len(p.New) || !bytes.Equal(b[p.Off:p.Off+int64(len(p.Old))], p.Old) {
			t.Fatalf("patch %d (%s) inconsistent", i, p.What)
		}
		if i > 0 && ps[i-1].Off+int64(len(ps[i-1].New)) > p.Off {
			t.Fatal("overlap or unsorted")
		}
	}
	// Idempotent: a patched file needs no patches.
	if again, _, _ := PatchDates(bytes.NewReader(got), int64(len(got)), target); len(again) != 0 {
		t.Fatalf("second pass returned %d patches", len(again))
	}
}

func TestPatchDatesSubSecZeroed(t *testing.T) {
	b := buildDNG(t, fixture{DTO: "2025:12:28 00:05:59", SubSec: "530", SubSecOrig: "53"})
	ps, _, _ := PatchDates(bytes.NewReader(b), int64(len(b)), time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local))
	got := applyAll(b, ps)
	ex, _ := ReadExifFrom(bytes.NewReader(got), int64(len(got))) // see Step 3
	if ex.SubSec != "00" {
		t.Fatalf("subsec %q", ex.SubSec)
	}
}

func TestPatchDatesBigEndianAndMissingTags(t *testing.T) {
	b := buildDNG(t, fixture{DTO: "2025:12:28 00:05:59", BigEndian: true}) // no DateTime, no DTD, no XMP
	ps, _, err := PatchDates(bytes.NewReader(b), int64(len(b)), time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local))
	if err != nil || len(ps) != 1 || ps[0].What != "EXIF DateTimeOriginal" {
		t.Fatalf("%v %+v", err, ps)
	}
}

func TestPatchDatesSkipsUnparseable(t *testing.T) {
	b := buildDNG(t, fixture{DTO: "not a date at all!!", XMP: `<x:xmpmeta><rdf:Description xmp:CreateDate="yesterday"/></x:xmpmeta>`})
	ps, skipped, err := PatchDates(bytes.NewReader(b), int64(len(b)), time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local))
	if err != nil || len(ps) != 0 || len(skipped) != 2 {
		t.Fatalf("err %v patches %d skipped %v", err, len(ps), skipped)
	}
}
```

- [ ] **Step 3:** Add `ReadExifFrom(r io.ReaderAt, size int64) (Exif, error)`. Refactor
  `ReadExif` to open the file and call it; behaviour is unchanged.
- [ ] **Step 4: Run them and check they fail.**
  `rtk proxy go test ./internal/dng -run PatchDates -count=1`: undefined `PatchDates`.
- [ ] **Step 5: Implement `patch.go`.**
  - Parse IFD0 with `newTIFFReader` and `readIFD`, then follow 0x8769.
  - For the ASCII tags, read the 19 bytes at the value offset. Validate them with
    `^\d{4}:\d\d:\d\d \d\d:\d\d:\d\d$` (or all spaces/NUL), and emit a Patch when they
    differ from the target.
  - For SubSec, map each `'0'..'9'` to `'0'`, and emit a Patch only if it changed.
  - For XMP, read `count` bytes at the offset (cap 1 MiB). For each property name, find
    every attribute or element occurrence with `regexp`. Rewrite the value by position
    (digits only), assert the length is equal, and emit a Patch at
    `xmpOffset + matchStart` if it changed.
  - Sort by Off and check there's no overlap; return an error on overlap, which would be a
    bug.
- [ ] **Step 6: Run** `rtk proxy go test ./internal/dng -count=1`. Expected: PASS, and the
  existing exif/preview tests still pass.
- [ ] **Step 7: Commit:** "dng: PatchDates computes same-length patches for every capture-date field (TIFF, EXIF, sub-seconds, embedded XMP)".

## Task 2: The patch proof, the manifest's patched fields, and `--verify`

**Files:**
- Create: `internal/offload/patch.go`, `internal/offload/patch_test.go`.
- Modify: `internal/offload/manifest.go` (Entry fields, `current`).
- Modify: `internal/offload/run.go` (`Verify` uses `current` and `FileSHA256`).
- Modify: `internal/offload/sys_darwin.go` and `sys_other.go` (`setFileTimes`).
- Modify: `go.mod` (`golang.org/x/sys` becomes direct).

**Interfaces:**
- Consumes: `dng.Patch`, `dng.PatchDates`.
- Produces:

```go
// streamPatched reads r to EOF, returning the SHA-256 of the bytes as read (orig) and
// of the bytes with ps applied in-stream (want). If w is non-nil the patched bytes are
// written to it. It fails if the bytes at a patch's range don't equal Patch.Old (the
// file isn't the one the patches were computed from) or ps is unsorted/overlapping.
func streamPatched(ctx context.Context, r io.Reader, ps []dng.Patch, w io.Writer) (orig, want [32]byte, err error)

// applyPatches pwrites ps into the file at path (opened O_WRONLY, never O_TRUNC) and
// fsyncs it.
func applyPatches(path string, ps []dng.Patch) error

// proveFrom evicts path from the page cache, re-reads it from the device and
// requires its SHA-256 to equal want (the existing dropCache + hashUncached path).
func proveFrom(ctx context.Context, path string, want [32]byte) error

// setFileTimes sets path's access and modification times to t and, on darwin, its
// creation time (Setattrlist ATTR_CMN_CRTIME) best effort: crtimeErr reports a
// creation-time failure (the caller notes it once); err is a mtime failure.
func setFileTimes(path string, t time.Time) (crtimeErr, err error)

// current returns, for each (orig, size), the last manifest entry: later lines
// supersede earlier ones (redate and rename append superseding lines).
func current(es []Entry) []Entry
```

- Manifest `Entry` gains:

```go
	FileSHA256 string    `json:"file_sha256,omitempty"` // the file as it is now, when its dates were patched
	DatesSet   string    `json:"dates_set,omitempty"`   // "2026-10-04T12:00:00" local, when patched
	PatchedAt  time.Time `json:"patched_at,omitempty"`
```

- [ ] **Step 1: Write the failing tests** (patch_test.go). Reuse a fixture builder. Copy
  Task 1's `buildDNG` into `internal/offload/dngfixture_test.go`; it's test-only
  duplication, acceptable and documented in a comment there.
  - `TestStreamPatchedHashes`: `orig` equals the SHA-256 of the input, `want` equals the
    SHA-256 of `applyAll`, and with `w` set the written bytes equal `applyAll`.
  - `TestStreamPatchedRejectsWrongOld`: with one patch's `Old` changed, it errors.
  - `TestProveCatchesStrayByte`: write the patched file, then flip one byte outside any
    patch; `proveFrom(want)` errors. Also leave one patch out; it errors.
  - `TestVerifyUsesFileSHAAndLastEntry`:
    - a manifest with an entry, plus a superseding entry for the same orig and size with a
      new Name and a FileSHA256 matching the file on disk;
    - Verify reports OK=1, Bad=0, and isn't confused by the old name;
    - then corrupt the file, and Verify reports Bad=1.
  - `TestSetFileTimes`: mtime equals t; on darwin, if `crtimeErr == nil`, the birthtime
    equals t. Read it with `unix.Stat_t` `Birthtimespec`.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
  - `streamPatched`: read in 4 MiB chunks, update `h1` with the raw chunk, and copy the
    chunk. For each patch overlapping the chunk, check that the bytes equal `Old` and copy
    `New` over them (patches can span chunk boundaries, so track the offset). Update `h2`
    and write the patched chunk to `w`. Check `ctx` between chunks.
  - `applyPatches`: `os.OpenFile(path, os.O_WRONLY, 0)`, then `WriteAt` per patch, then
    `plainSync`.
  - `proveFrom`: `dropCache(path)`, then `hashUncached(ctx, path)`, then compare.
  - `setFileTimes`: `os.Chtimes(path, t, t)`. On darwin, `unix.Setattrlist` with
    `Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_CRTIME}` and
    a `Timespec` buffer.
  - `current`: iterate in order and keep the last per `lower(orig)+size`, preserving the
    order of first appearance.
  - `Verify`: iterate `current(man)`, and compare against
    `e.FileSHA256` if it isn't empty, else `e.SHA256`. Its message, "differs from the
    card", becomes "differs from the recorded checksum" when FileSHA256 is used.
- [ ] **Step 4: Run** `rtk proxy go test ./internal/offload -count=1`. Expected: all pass,
  existing included.
- [ ] **Step 5: Commit:** "offload: the patch proof (orig and patched hashes in one read, a device re-read must match), patched manifest fields, --verify follows the current entry and checksum".

## Task 3: `offload --set-date`

**Files:**
- Modify: `internal/offload/plan.go`: `Options.SetDate` and `SetDateSet`. The folder date
  comes from SetDate. Fix the skip logic for patched entries in `cameraNames`/`same` and
  `--checksum`.
- Modify: `internal/offload/copy.go`: `written` carries `setDate *time.Time`, and
  `finishStage` patches.
- Modify: `internal/offload/run.go`: `record` writes the patched fields; notes for skipped
  XMP values and for creation-time failures.
- Modify: `internal/cli/offload.go`: the `--set-date` and `--time` flags.
- Modify: `internal/cli/dotfile.go` (`notInDotfile`).
- Test: `internal/offload/setdate_test.go`, `internal/cli/offload_test.go`.

**Interfaces:**
- Consumes: Task 2's `streamPatched`, `applyPatches`, `proveFrom`, `setFileTimes`,
  `current`, and the Entry fields; Task 1's `dng.PatchDates`.
- Produces: `Options.SetDate time.Time` and `Options.SetDateSet bool`. The CLI parses
  `--set-date YYYY-MM-DD` and `--time HH:MM:SS` (default `12:00:00`) in `time.Local`.
  `--set-date` implies `Options.Date` = that date, and an explicit different `--date` is
  an error.

**Stage B with SetDate** (copy.go `finishStage`; every existing step keeps its order):

1. fsync, close, chmod, as now. Skip `os.Chtimes(card mtime)` when setDate is set.
2. dropCache, then open the temp `O_RDONLY` with F_NOCACHE.
   - Compute `ps, skipped := dng.PatchDates(f, size, *setDate)` (ReadAt).
   - Then `orig, want := streamPatched(ctx, f, ps, nil)`.
   - `orig` must equal `w.sum` (the card's hash). This replaces today's
     `hashUncached == sum` check with an equivalent, and keeps its error text.
3. If `len(ps) > 0`: `applyPatches(temp, ps)`, then `proveFrom(ctx, temp, want)`.
4. `setFileTimes(temp, *setDate)`.
5. Link-no-replace, as now.
6. `written` records `fileSum = want`, `skipped` and `crtimeErr` for `record`.

- [ ] **Step 1: Write the failing tests** (setdate_test.go), using a synthetic card folder
  of fixture DNGs with known dates (the offload tests' card helpers plus `buildDNG`):
  - **`TestSetDatePatchesCopiesNotCard`:**
    - offload with SetDate 2026-10-04 12:00 is Safe;
    - the card's tree hash is unchanged;
    - each copy equals its card file with `PatchDates` applied (byte-compare against
      `applyAll`);
    - mtime equals the target;
    - the manifest has `sha256` = the card's hash, `file_sha256` = the copy's hash, and
      `dates_set`;
    - `Verify` passes.
  - **`TestSetDateRerunSkips`:** after the run above, plan and run again with the same
    options in three modes. The camera-names and `--rename` modes give `Copied == 0`, all
    skipped, Safe. `--checksum` gives the same, not "exists with different content".
    *(Review Focus 1)*
  - **`TestSetDateProofFailureFailsFile`:** a test seam corrupts the temp after
    `applyPatches` on file k, on the first attempt. Expected: k is retried (from the card,
    as offload retries); it ends correct; no file is named unproven; the run is Safe.
  - **`TestSetDateSkippedXMPNoted`:** a fixture with an unparseable XMP date gives one
    note naming the file and the field, and the run is still Safe.
  - **`TestSetDateConflictsWithDate`:** a different `--date` gives a CLI error.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement stage B** as above. `written` keeps the pipeline's single-stage-B
  semantics; nothing changes in pipeline.go except `record` using `w.fileSum`.
- [ ] **Step 4: Implement the skip logic** (plan.go):
  - **Camera names** (`same`): before the mtime comparison, look up the destination's
    `current(manifest)` entry for this name. If it records `FileSHA256`, and its
    `Size`/`ModTime` (the card's) and `Orig` match the card file within `mtimeWindow`,
    treat the file as the same.
  - **`--checksum`:** compare `fileSum(card)` with `entry.SHA256`, and
    `hashFromDisk(dest)` with `entry.FileSHA256`.
  - **`--rename`:** this path already matches on the manifest. Confirm it with the test.
- [ ] **Step 5: CLI.** `--set-date` and `--time` go in offload's Common help section, and
  `set-date` and `time` are added to `notInDotfile`. Help text:
  `"fix every copy's capture date (EXIF, embedded XMP, file times) to this date, YYYY-MM-DD; the card is never changed"`.
  The `cli offload_test` asserts the flag parses, and that `--time` without `--set-date`
  is an error.
- [ ] **Step 6: Run everything.**
  `rtk proxy go test ./internal/offload ./internal/cli -count=1`, then
  `rtk proxy go test -race -count=5 ./internal/offload`. Expected: PASS.
- [ ] **Step 7: Commit:** "offload --set-date: patch each verified copy's capture dates in stage B, prove it against the card, keep both checksums; re-runs skip patched copies".

## Task 4: The capture date in cull's sidecars

**Files:**
- Modify: `internal/xmp/xmp.go`: `Sidecar.DateTaken string` (ISO
  `2006-01-02T15:04:05`). When it isn't empty, Render writes `exif:DateTimeOriginal`,
  `xmp:CreateDate` and `photoshop:DateCreated` with that value, and declares the
  namespaces.
- Modify: `internal/labels/sidecar.go`: `Sidecar(r, ...)` sets DateTaken from
  `r.Exif.DateTimeOriginal`, converting `YYYY:MM:DD HH:MM:SS` to ISO, when it parses.
- Test: `internal/xmp/xmp_test.go`, `internal/labels/sidecar_test.go`.

- [ ] **Step 1: Failing tests.**
  - xmp: Render with DateTaken contains the three properties with the exact value. Without
    it, none appear.
  - labels: a Result with an Exif date gives a sidecar with DateTaken. With no Exif, it's
    empty.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.** Keep the XML valid: run the existing round-trip or parse test,
  if there is one.
- [ ] **Step 4: Run** `rtk proxy go test ./internal/xmp ./internal/labels ./internal/review -count=1`.
- [ ] **Step 5: Commit:** "Sidecars carry the capture date (exif:DateTimeOriginal, xmp:CreateDate, photoshop:DateCreated) from the report".

## Task 5: `cull redate`

**Files:**
- Create: `internal/journal/journal.go`, `journal_test.go`.
- Create: `internal/redate/redate.go`, `redate_test.go`.
- Create: `internal/cli/redate.go`.
- Modify: `internal/cli/root.go` (add the command); `internal/cli/cull.go`,
  `internal/cli/decide.go` and `internal/cli/review.go` (journal guard);
  `internal/cli/status.go` (hint).
- Modify: `internal/report/report.go` if a helper is needed: `(*Report).ResultFor(path)`.

**Interfaces:**
- Consumes: Tasks 1, 2 and 4.
- Produces:

```go
// package journal
const RedateName = "cull-redate.json"
const RenameName = "cull-rename.json" // Task 6
// Incomplete reports an unfinished redate or rename journal in dir: which, and the
// command that finishes it.
func Incomplete(dir string) (which, finish string, ok bool)

// package redate
type Options struct {
	Dir        string
	ReportPath string        // "" = <Dir>/cull-report.json
	Recursive  bool
	Target     time.Time     // local
	DryRun     bool
	UI         ui.Sink
}
type Result struct {
	Patched, AlreadySet, Refused int
	Refusals []string // "<name>: <why>"
	Skipped  []string // XMP values left alone, "<name>: <field>"
	CrtimeFailed int
}
func Run(ctx context.Context, o Options) (Result, error)
```

**Per-file algorithm** (redate.go). The files are the DNGs under Dir, including
`keep/`, `review/`, `cull/` and `culled/`, and subfolders with Recursive.

1. Compute `ps := dng.PatchDates`.
   - If there are no patches and the file's mtime equals Target: count it `AlreadySet`,
     still do the bookkeeping in steps 6–7 if it's missing (idempotent), then continue.
   - If there are no patches but the mtime differs: only `setFileTimes`.
2. Find the expected hash. If the folder's `current(manifest)` has an entry with this
   name: `FileSHA256`, else `SHA256`.
3. Stream the original into a temp (the existing hidden temp convention, same directory)
   with `streamPatched(ctx, f, ps, temp)`.
   - If an expected hash exists and `orig != expected`: Refused ("no longer matches its
     offload checksum: not fixing a changed or damaged file"). Remove the temp and
     continue.
4. fsync the temp, then `proveFrom(ctx, temp, want)`, then `setFileTimes(temp, Target)`.
5. `os.Rename(temp, original)`, then fsync the directory.
6. **Manifest:** if this name is recorded, append a superseding entry: the same `Src`,
   `Orig`, `Name`, `Size` and `ModTime` (the card's) and `SHA256` (the card's), plus
   `FileSHA256 = want`, `DatesSet` and `PatchedAt`. Use `appendManifest`; expose it from
   offload as `offload.AppendManifest` and `offload.CurrentManifest(dir)`.
7. **Report:** if it holds this file (by path, or by `MovedTo`), set `ModTime` to the
   file's new mtime and `Exif.DateTimeOriginal` to Target in EXIF format, `SubSec` to the
   zeroed digits. Save the report every 25 files and at the end. Then rewrite cull's
   sidecar if `xmp.Ours` (via `labels.WriteSidecar` with the current label).

**Journal** (`cull-redate.json`, written atomically before the first file and updated at
each checkpoint):

```json
{"target":"2026-10-04T12:00:00","started":"…","done":["path",…],"complete":false}
```

- A re-run with the same target resumes; files already patched come back `AlreadySet`
  through step 1.
- A re-run with a different target while one is incomplete is refused.
- On success the journal is deleted.

**Guards:**
- `journal.Incomplete(dir)` makes `judge`, `decide`, `review`, `redate` (a different
  target) and `rename` refuse, with the finish command in the message.
- `status` prints the incomplete journal and its finish command as `next:`.

**Already imported:** print one warning: "if these frames are already in a Capture One or
Lightroom catalogue, it may lose track of the changed files".

**CLI:** `cull redate <folder> --date YYYY-MM-DD [--time HH:MM:SS] [--dry-run] [-r]`.

- `--dry-run` prints per file: `name: N fields → 2026-10-04 12:00:00`, or "already set",
  or "would refuse: …". It writes nothing.
- At the end it prints a summary line: patched, already set, refused, and XMP values left.
- `notInDotfile` is one list by flag name, shared by every command. Add `date` to it: a
  fixed date makes no sense as a default for any command, including offload's folder
  `--date`. `time` and `set-date` were added in Task 3, and `dry-run` is already there.

- [ ] **Step 1: Failing tests** (redate_test.go):
  - **`TestRedatePatchesAndProves`:** after a folder offloaded with the Task 3 helpers
    (without set-date), redate gives:
    - each file = original with `applyAll` (byte compare);
    - mtime = Target;
    - manifest `current` has FileSHA256, and `offload.Verify` passes;
    - `report.Load` shows the updated ModTime and Exif date.
  - **`TestRedateThenJudgeMakesNoCalls`** *(Review Focus 2)*:
    - judge with a counting stub backend first, via the pipeline test helpers, which
      makes N calls;
    - then redate, then judge again (`Resume`): 0 new calls.
  - **`TestRedateIdempotentAndResumes`:**
    - Run, then Run again: Patched=0, AlreadySet=N.
    - Inject a stop after file k (seam), which leaves an incomplete journal. `judge`
      refuses (the CLI test). Re-run: it completes, the journal is gone, and every file is
      proven.
  - **`TestRedateRefusesChangedFile`:** a file whose bytes differ from its manifest
    checksum is Refused, and left untouched (byte-identical).
  - **`TestRedateUnrecordedFolder`** *(Review Focus 5)*: DNGs with no manifest and no
    report are patched and proven, with no error.
  - **`TestRedateSortFolders`:** files in `keep/` and `cull/` are patched, and the report
    is updated by `MovedTo`.
  - **`TestRedateDryRunWritesNothing`:** the tree hash and mtimes are unchanged.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement** journal, redate, the CLI command, the guards and the status
  hint.
- [ ] **Step 4: Run** `rtk proxy go test ./internal/... -count=1`, plus `-race` on
  redate and offload.
- [ ] **Step 5: Commit:** "cull redate: fix an offloaded folder's capture dates (proven patch, atomic swap, manifest and report follow, journal; judge refuses mid-run)".

## Task 6: `cull rename` and `--undo`

**Files:**
- Create: `internal/rename/rename.go`, `rename_test.go`.
- Create: `internal/cli/rename.go`.
- Modify: `internal/labels/labels.go` (`Entry.From string json:"from,omitempty"`).
- Modify: `internal/report/report.go`: `(*Report).RenamePath(old, new string) int`
  rewrites every stored path field equal to `old` and returns how many. That covers:
  - `Result.File`, `XMP` and `MovedTo`;
  - `Set.Members`, `Order` and `Reversed`;
  - `Set.Notes[].File`;
  - any other path-holding field. Find them all: walk the `Result` and `Set` struct
    definitions.
- Modify: `internal/journal` (rename journal reader); the CLI guards from Task 5 already
  call `journal.Incomplete`.

**Interfaces:**
- Consumes: `offload.CurrentManifest`, `offload.AppendManifest`, `journal`, `labels`,
  `report`.
- Produces:

```go
type Options struct {
	Dir, ReportPath, Pattern string
	DryRun, Undo bool
	UI ui.Sink
}
type Move struct{ Old, Tmp, New string } // absolute; the sidecar moves alongside when it exists
type Result struct{ Renamed int; Moves []Move }
func Run(ctx context.Context, o Options) (Result, error)
```

**Algorithm:**

1. **Frames:** the DNGs in Dir and its sort folders.
   - The order is camera order: by `orig` from `CurrentManifest` when recorded, else by
     current name.
   - `{date}` = the frame's EXIF capture date (`dng.ReadExif`) as YYYYMMDD, falling back
     to the folder's date prefix.
   - `{name}` = the folder name after its date.
   - `{orig}` = the stem of the camera name.
   - `{n}` counts from 1 in camera order.
   - Validate the pattern exactly as offload does: reuse offload's tokenizer, exported as
     `offload.ExpandName(pattern string, date, name, orig string, n int) (string, error)`
     and `offload.ValidatePattern`.
2. **Plan:**
   - The new name keeps the extension, uppercased as offload does, in the same folder as
     the frame.
   - Refuse, before any change, if two frames map to one name in the same folder, or a
     new name exists and isn't one of the frames being renamed.
   - Frames whose name doesn't change are omitted.
3. **`--dry-run`** prints `old → new` (relative paths) and stops.
4. **Journal** `cull-rename.json`: `{pattern, moves:[{old,tmp,new,sidecar_old,sidecar_tmp,sidecar_new}], phase, complete:false}`,
   written atomically and fsynced.
5. **Phase 1:** for each move, link-no-replace or rename `old→tmp` and
   `sidecar_old→sidecar_tmp`, where tmp is `.<name>.cull-rename-<rand>`. Then set
   `phase:2`.
6. **Phase 2:** `tmp→new` and `sidecar_tmp→sidecar_new`, never replacing (`linkNoReplace`
   with a rename fallback, as offload does). Then the bookkeeping.
7. **Bookkeeping:**
   - `rep.RenamePath(old,new)` and the sidecar path for each move; save the report.
   - Labels: for each move whose old base has an effective entry, `labels.Append` a copy
     with `File=new base`, `From=old base` and `At=now`.
   - Manifest: for each recorded move, append a superseding entry with
     `Name = new base`, everything else unchanged.
   - Review cache: `review.assetName` builds image names from the DNG's base name, size,
    modification time and focus box (internal/review/cache.go). Rename every file in
    `cull-review/assets` whose name was built from the old base to the one built from the
    new base, using the same function so the names match exactly. A failure here is a
    note, not an error: review re-renders missing images and sweeps stale ones.
  - Mark the journal complete; keep it for `--undo`.
8. **Resume:** if a journal is incomplete, re-running the same pattern completes each move
   from wherever its files are, per move (old, tmp or new exists). Any other pattern is
   refused.
9. **`--undo`:** with a complete journal, apply the moves in reverse (`new→tmp→old`)
   through the same two phases, journalled as an undo journal, with the bookkeeping
   reversed: the labels appended back under the old names with `From`, and the manifest
   supersedes appended. Then delete the journal.
10. **Already imported:** print the same warning as Task 5.

- [ ] **Step 1: Failing tests** (rename_test.go), on a synthetic shoot with a report,
  labels, a manifest, cull sidecars and two frames sorted into `keep/` and `cull/`:
  - **`TestRenameFollowsEverything`:**
    - the files and sidecars are renamed, in the same folders;
    - `report.RenamePath` leaves no old path anywhere: marshal the report JSON and assert
      no old path string remains;
    - `labels.Read` gives the same effective label per frame, under its new name;
    - `offload.Verify` reports OK for all;
    - judge with a counting stub makes 0 calls.
  - **`TestRenameSwap`** *(Review Focus 3)*: with a pattern and fixture where frame A's
    new name is B's old name and vice versa, it succeeds and the contents are swapped
    correctly. Check this by hash: each file's content follows its frame.
  - **`TestRenameCollisionRefusedBeforeAnyMove`:** a foreign file holds one target name.
    The error names it, and the tree is unchanged.
  - **`TestRenameInterruptedResumesOrUndoes`:** a seam stops after phase 1 for half the
    files.
    - Status and judge refuse.
    - Re-running completes it, and everything follows.
    - Separately, `--undo` after completion restores the original names, labels effective
      under the old names, and Verify OK.
  - **`TestRenameDryRun`:** nothing is written.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement** the rename package, `RenamePath`, `labels.From`, the exported
  offload tokenizer, and the CLI:
  `cull rename <folder> "<pattern>" [--dry-run]` and `cull rename --undo <folder>`.
  `undo` is added to `notInDotfile`.
- [ ] **Step 4: Run** `rtk proxy go test ./internal/... -count=1`,
  `rtk proxy go test -race -count=5 ./internal/rename ./internal/redate ./internal/offload`
  and `go vet ./...`.
- [ ] **Step 5: Commit:** "cull rename: bulk-rename frames in two journalled phases; report, labels, manifest and sidecars follow; --undo".

## Task 7: Docs, help and the invariant change

**Files:** `CLAUDE.md`, `README.md`, `USAGE.md`, `site/usage.html` and `site/index.html`
(one line in the offload feature list), and the help texts.

- [ ] **Step 1: CLAUDE.md.** Replace the invariant with the spec's §5 text, verbatim.
  Layout gets `internal/dng/patch.go`, `internal/offload/patch.go`, `internal/redate`,
  `internal/rename` and `internal/journal`. Dependencies: `golang.org/x/sys` is now direct
  (for `Setattrlist`; already in go.sum through Bubble Tea).
- [ ] **Step 2: README.** Offload gets `--set-date`. Add new sections for `cull redate`
  and `cull rename`, covering what changes and the proof, in plain words. Add the commands
  to the table.
- [ ] **Step 3: USAGE.md and site/usage.html.** In the offload step, a short "If your
  camera's clock is wrong" note with `--set-date`, plus reference entries for `redate`
  and `rename`, in the site's existing article markup.
  - Keep the photographer-friendly voice of the 2026-10-06 rewrite.
  - Explain that only the date fields change, and that the card is never touched.
  - Check the pages with Task 10's verification approach from the 2026-10-05 plan: HTML
    tag balance, no duplicate ids.
- [ ] **Step 4:** `TestHelpTeachesNoRetiredForms` and `TestEveryFlagHasAKnownSection`
  still pass. Build, then read `cull redate --help`, `cull rename --help` and
  `cull offload --help` by eye.
- [ ] **Step 5: Commit:** "Docs: offload --set-date, cull redate, cull rename; the DNG invariant's one exception".

## Task 8: Live checks (controller, with the user)

- [ ] **Step 1:** Build, then run
  `cull offload "/Volumes/LEICA M" $TMPDIR/live --name livetest --set-date 2026-10-04 --no-scan`.
  Expected: "safe to format", then `--verify` OK. Check the dates on one copy (`exiftool
  -time:all` if installed, else `cull scan` on the folder and its report's Exif). The
  card's tree hash must be unchanged.
- [ ] **Step 2:** Copy a second shoot folder without `--set-date`. Run `cull redate` on it
  with a different date, then `--verify`, then `cull rename … "{date}_{name}_{n:4}"`, then
  `--verify`, then `--undo`, then `--verify`.
- [ ] **Step 3:** Ask the user to import one redated frame and one `--set-date` frame into
  Capture One, and report which date it shows: Image › Metadata › Date. Record the answer
  in CLAUDE.md's Verified facts.
- [ ] **Step 4:** Delete the temp folders, then merge, push and deploy the site. Ask before
  tagging a release.
