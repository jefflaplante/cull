# Part 2: Offload and Folder Sorting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `cull offload <card>... <dest>` copies DNGs off a card into one shoot folder per
run. Every copy is verified against the card from the disk, not the page cache, and the
run says "safe to format" only when that is true. `judge --sort` / `decide --sort` then
place each frame in `keep/`, `review/` or `cull/` for import.

**Architecture:**
- **A new `internal/offload` package:**
  - `plan.go` scans the sources and decides every name, skip and refusal before writing.
  - `copy.go` holds the single-read tee copy engine (hash while reading; temp file
    written with `F_NOCACHE`; plain fsync; uncached verify re-read; no-replace rename).
  - `manifest.go` reads and writes `cull-offload.jsonl`.
  - `run.go` executes a plan with retries, progress events and the closing
    `F_FULLFSYNC`, then decides "safe to format".
  - `verify.go` implements `--verify`.
  - The macOS cache calls live in `sys_darwin.go`, with no-op fallbacks in
    `sys_other.go`.
- **Sorting** generalizes `internal/pipeline/move.go`: each frame has a wanted place
  (home, `culled/`, or a sort folder) and is moved there. `reconcileMove` searches every
  place a frame can be. `Discover` skips all of them.

**Tech Stack:** Go 1.26 stdlib only: `crypto/sha256`, the `syscall` package's
`F_NOCACHE`, `F_FULLFSYNC` and `Statfs`, and `os.Link` for no-replace renames.

**Spec:** `docs/superpowers/specs/2026-10-02-preingest-workflow-design.md`, Part 2. It
was updated 2026-10-02 with one folder per run, `--date`, no `--jobs`, and the measured
card facts in CLAUDE.md.

## Global Constraints

- **Sources are read only:** never written, renamed or deleted. Tests hash the source tree
  before and after.
- **Writes never replace an existing file.** A file appears under its final name only after
  it has been verified.
- **"Safe to format"** prints only when every planned file is verified on every
  destination, and the final `F_FULLFSYNC` per destination volume succeeded.
- **Nothing is written before every refusal is known:** collisions, free space, unreadable
  sources.
- **Card hygiene:** regular `.dng` files only (any case); skip `._*`, `.Trashes`,
  `.Spotlight-V100`, `.fseventsd` and any dot-directory; never follow symlinks.
- **Copied files** are mode `0644`, with the source's modification time.
- **The quick check** matches on size, and on modification time within 2 s.
- **Sorting:**
  - folders are `keep/`, `review/` and `cull/` beside the frame's home;
  - `--sort` and `--move-culled` are mutually exclusive;
  - moves are same-disk renames, never overwriting, recorded in `moved_to`;
  - `restore` undoes them.
- **No new dependencies.** Tests use synthetic files only. The real-card check is read-only
  (`--dry-run`), unless the user approves a copy.
- **Running Go:**
  - tests run with `GOCACHE=$TMPDIR/gocache`;
  - the `cli` package runs unsandboxed;
  - the darwin cache-residency test runs unsandboxed.
- Branch `feat/offload-sort`; one commit per task.

## Review Focus

- **The card is pulled mid-copy.** A source read fails partway: the temp file is removed,
  nothing appears under the real name, the file is retried and then failed, and the run
  never says "safe to format". *Task 3: `TestSourceReadErrorRetriesThenFails`.*
- **A destination fills up mid-run, despite the free-space check** (another process
  writes). A write fails with ENOSPC: the same handling, then later files are attempted
  and fail fast. *Task 3: `TestWriteErrorFailsFileNotRun`* (a write hook returns ENOSPC).
- **Re-running on the same card after a partial run** copies only what's missing. A stale
  `.cull-*.tmp` from a crash is removed. *Task 3: `TestRerunResumes`.*
- **Two cards from one body,** whose file numbers overlap after a folder reset: without
  `--rename`, the clash is refused in the plan with nothing written; with `--rename`, the
  counter keeps going. *Task 1: `TestPlanRefusesClash`, `TestRenameCounterContinues`.*
- **Re-sorting after labels change, including from `cull/` back to `keep/`:** the frame and
  its sidecar move together, nothing is overwritten, and `restore` returns everything
  home. *Task 5: `TestResortFollowsLabels`, `TestRestoreFromSortFolders`.*

---

### Task 1: The offload plan

**Files:** create `internal/offload/plan.go` and `internal/offload/plan_test.go`.

**Interfaces (Produces):**

```go
type Options struct {
	Sources  []string // card roots (read only)
	Dest     string   // the root shoot folders go under
	Backup   string   // optional second root, same layout
	Name     string   // shoot name: folder "<date> <name>", {name} token
	Date     string   // YYYY-MM-DD; "" = the earliest capture date in the run
	Rename   string   // pattern with {date} {name} {orig} {n} {n:W}; "" = camera names
	Checksum bool     // skip by SHA-256 instead of size+mtime
	// test hooks
	freeSpace func(path string) (uint64, error) // nil = statfs
	now       func() time.Time
}

type File struct {
	Src     string    // absolute source path
	Size    int64
	ModTime time.Time
	Capture time.Time // EXIF capture time, or ModTime when absent
	Name    string    // destination base name
	Skip    string    // "" = copy; else why it's skipped ("already copied", "in manifest")
}

type Plan struct {
	Folder  string   // shoot folder name, e.g. "2025-12-27 Smith wedding"
	Dests   []string // absolute shoot folders: Dest/Folder and, with Backup, Backup/Folder
	Files   []File
	Bytes   int64    // bytes to copy (files not skipped)
	Dated   string   // where the date came from: "EXIF of M1103127.DNG" or "--date"
}

func MakePlan(o Options) (*Plan, error) // refuses: no DNGs, clashes, not enough space, unreadable source
func (p *Plan) Summary() string        // the --dry-run text
```

**Rules:**
- **Walk each source** with `filepath.WalkDir`.
  - Skip any dot-directory, and `._*` and other dot-files.
  - Accept only regular files with extension `.dng` in any case (`d.Type().IsRegular()`;
    a symlink is `ModeSymlink`, so it's skipped).
  - Order files by base name, then full path.
- **Capture time:** `dng.ReadExif(src).CaptureTime()`, falling back to `ModTime`.
- **Folder:** `Date` when set (validated with `time.Parse("2006-01-02")`), otherwise the
  earliest `Capture`'s date. With a name, the folder is `<date> <name>`, else `<date>`.
  `Dated` records which.
- **Names without `--rename`:** the camera base name.
  - Within the run, two sources with the same base name are refused: "M1103127.DNG is on
    two cards".
  - If the destination already has that name: the quick check (size equal, and mtime
    within 2 s), or with `Checksum` equal SHA-256, means skip ("already copied").
    Otherwise refuse: "<dest>/M1103127.DNG exists with different content; use --rename
    to keep both".
- **Names with `--rename`:**
  - Each file is first matched against the manifest by source base name, size and mtime
    (2 s window). A match is skipped ("in manifest as <name>").
  - The rest are numbered in order, starting at 1 + the largest `n` found by matching the
    pattern against every file name already in `Dests[0]` (the pattern compiles to a
    regexp with `(\d+)` for `{n}`).
  - `{date}` is the folder date as YYYYMMDD; `{name}` is `Name` with spaces as `_`;
    `{orig}` is the camera stem. The extension is the source's, uppercased.
  - A pattern without `{n}` and without `{orig}` is refused, since it can't give unique
    names.
- **Backup:** every rule above is checked against `Dests[1]` as well. A file is skipped only
  if both destinations hold it. If only one does, it's recopied to both, because the tee
  writes all destinations at once. This is simpler, and the extra copy is acceptable:
  `Ruling` if changed.
- **Free space:** for each destination root, `Bytes` + 1% + 512 MB must be ≤ the free
  bytes, else refuse with both numbers.

- [ ] **Step 1: Write the failing tests.** The helper `card(t, files map[string]fileSpec)`
  writes synthetic DNGs: `tinyDNG`-style bytes, padded to the given size, each with an
  mtime.
  - `TestPlanWalksCardHygienically`: these are skipped: `DCIM/100LEICA/._M1.DNG`,
    `.Trashes/x.DNG`, `.fseventsd/f`, a symlink `L.DNG → elsewhere`, `M2.jpg`. Included:
    `DCIM/100LEICA/M1.DNG` and `DCIM/101LEICA/m3.dng`, in name order.
  - `TestPlanFolderDate`: with no `--date`, the earliest capture's date. With `Date`, that
    date. `Dated` says which.
  - `TestPlanSkipsAlreadyCopied`: the destination holds M1 with the same size and mtime
    (and also with mtime +1 s): skipped. With mtime +3 s: refused as a clash.
  - `TestPlanRefusesClash`:
    - an existing name with different content: refused, and nothing is created (the
      destination directory doesn't even exist after the call);
    - the same base name on two sources: refused.
  - `TestRenameCounterContinues`: the folder holds `20251227_Smith_0007.DNG`; two new files
    become `_0008` and `_0009`, in camera-name order.
  - `TestRenameSkipsManifestEntries`: a manifest line for M1 (size, mtime) means M1 is
    skipped, and M2 is numbered.
  - `TestRenamePatternNeedsCounterOrOrig`: `{date}_{name}` is refused.
  - `TestPlanChecksFreeSpace`: the `freeSpace` hook returns `Bytes`: refused, naming the
    root and both sizes.
  - `TestPlanBackupDestination`: with `Backup`, `Dests` has two entries, and a clash on
    the backup is refused.
- [ ] **Step 2: Run them and check they fail.**
  Run `go test ./internal/offload`. Expected: a build failure (package missing).
- [ ] **Step 3: Implement `plan.go`.**
- [ ] **Step 4: Run them.**
  Run `go test ./internal/offload`. Expected: PASS.
- [ ] **Step 5: Commit.** `offload: plan (hygienic walk, one folder per run, names, rename counter, skips, clashes, free space)`

### Task 2: The copy engine

**Files:** create `internal/offload/copy.go`, `sys_darwin.go`, `sys_other.go`,
`copy_test.go` and `nocache_darwin_test.go`.

**Interfaces (Produces):**

```go
// copyFile reads src once and writes it to every dst at the same time; returns src's SHA-256.
// Each dst appears only after its temp copy is fsynced, re-read uncached and matched.
func copyFile(ctx context.Context, src string, dsts []string, mtime time.Time, h hooks) (sum [32]byte, err error)

type hooks struct {
	open       func(string) (io.ReadCloser, error) // nil = os.Open + noCache
	afterWrite func(tmp string)                    // test: corrupt between write and verify
	write      func(f *os.File, p []byte) (int, error) // test: inject ENOSPC
}

func noCache(f *os.File) error   // darwin: fcntl F_NOCACHE 1; else nil
func fullSync(f *os.File) error  // darwin: fcntl F_FULLFSYNC; else f.Sync()
func plainSync(f *os.File) error // syscall.Fsync: on darwin, os.File.Sync is already F_FULLFSYNC, which is too costly per file
```

**Engine:**
1. Open the source (with `noCache`).
2. A reader goroutine fills buffers from a pool of 4 × 4 MiB and sends `(buf, n)` on a
   channel. It hashes on the reader side: `sha256.New()`.
3. For each destination, create the temp file `.<name>.cull-<8 hex>.tmp` with
   `O_CREATE|O_EXCL|O_WRONLY`, mode `0644`, then `noCache` it.
4. The writer loop writes each buffer to every temp file in turn, then returns the buffer
   to the pool. The writes are sequential; the overlap is between reading and writing.
5. At EOF: `plainSync` each temp; `os.Chtimes(tmp, mtime, mtime)`; `os.Chmod(tmp, 0644)`;
   close.
6. `afterWrite` hook.
7. **Verify:** reopen each temp, `noCache`, hash with the same 4 MiB reads, and compare.
   A mismatch is an error naming the destination.
8. **Rename** with `os.Link(tmp, final)`, then `os.Remove(tmp)`. Link refuses an
   existing name. A filesystem without links (exFAT backup drives) falls back to
   Lstat-then-Rename, as `pipeline.renameNoReplace` does.
9. Open the folder and `plainSync` it.
10. **On any error,** remove every temp file it created, and return the error.
11. **Cancellation:** a cancelled `ctx` is checked between buffers, and gives the same
    cleanup.

- [ ] **Step 1: Write the failing tests.**
  - `TestCopyFileTwoDestinations`: 9 MiB + 123 bytes of random data, copied to two folders.
    Checks:
    - both copies are byte-equal to the source;
    - the sum equals `sha256.Sum256(src)`;
    - mode `0644`, and mtime equals the given time;
    - no `.cull-*.tmp` is left.
  - `TestCopyFileCatchesCorruption`: `afterWrite` flips one byte in the first temp file.
    The error names that destination, neither final name exists, and no temp is left.
  - `TestCopyFileNeverReplaces`: the final name already exists → an error, and the existing
    file is unchanged.
  - `TestCopyFileSourceErrorCleansUp`: `open` returns a reader that fails after 5 MiB →
    an error; no final name, no temp.
  - `TestCopyFileCancel`: ctx is cancelled after the first buffer → `context.Canceled`,
    and no files are left.
  - `nocache_darwin_test.go` (`//go:build darwin`), `TestCopiedPagesNotResident`:
    - copy 32 MiB into `t.TempDir()`;
    - `mmap` and `mincore` the final file;
    - expect under 5% of its pages resident.

    This proves the verify read can't be served from the page cache. It's skipped when
    the temp dir is on tmpfs, which can't happen on macOS.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement** `copy.go` and the sys files. `sys_darwin.go` uses
  `syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_NOCACHE, 1)` and
  `syscall.F_FULLFSYNC`. `sys_other.go` has the `//go:build !darwin` fallbacks.
- [ ] **Step 4: Run** `go test ./internal/offload`, unsandboxed so the residency test runs.
  Expected: PASS.
- [ ] **Step 5: Commit.** `offload: single-read tee copy engine (hash while reading, F_NOCACHE write + uncached verify, no-replace rename)`

### Task 3: Run, manifest, "safe to format", `--verify`

**Files:** create `internal/offload/manifest.go`, `run.go`, `verify.go` and `run_test.go`.

**Interfaces (Produces):**

```go
type Entry struct { // one manifest line, written only after a file verified everywhere
	Src     string    `json:"src"`
	Orig    string    `json:"orig"` // source base name
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	SHA256  string    `json:"sha256"`
	At      time.Time `json:"at"`
}
const ManifestName = "cull-offload.jsonl"
func readManifest(folder string) ([]Entry, error)
func appendManifest(folder string, e Entry) error // O_APPEND, one write, plainSync

type Result struct {
	Plan     *Plan
	Copied   int
	Skipped  int
	Failed   []string // "<name>: <error>"
	Bytes    int64
	Elapsed  time.Duration
	Safe     bool // every planned file verified on every destination, and the final syncs succeeded
}
func Run(ctx context.Context, p *Plan, sink ui.Sink) (*Result, error)
func Verify(ctx context.Context, folder string, sink ui.Sink) (ok, bad int, err error)
```

**Run:**
1. `MkdirAll` each destination folder.
2. Remove stale `.*.cull-*.tmp` files.
3. Emit `Stage{Name:"offload", Unit:"bytes", Total:p.Bytes}`.
4. **For each file not skipped:** `copyFile`, with up to 2 retries after 1 s and 3 s.
   - Context cancellation isn't retried.
   - Progress is `Stage{Add:size}` after each file, so per-file granularity.
   - A file that still fails goes into `Failed`, and the run continues.
5. **After a file succeeds,** `appendManifest` in every destination folder (the manifest
   lives in both).
6. **At the end,** `fullSync` once per destination: open the shoot folder and
   `F_FULLFSYNC` it. On error, Safe is false and the error is noted.
7. **Safe** = no Failed, ctx not cancelled, and every fullSync succeeded.
8. A cancelled ctx returns the result so far, with `ctx.Err()`.

**Verify:** for each manifest entry in `folder`, hash `folder/Name` uncached and compare.
A mismatch or a missing file is counted bad, with a warning per file.

- [ ] **Step 1: Write the failing tests.**
  - `TestRunCopiesAndIsSafe`: a fake card with 3 files and a backup. Checks:
    - both folders hold 3 verified files;
    - both manifests have 3 lines with correct SHA-256;
    - `Safe` is true;
    - the card tree's hash is unchanged (hash every file and dir listing before and after).
  - `TestSourceReadErrorRetriesThenFails`: an `open` hook fails for M2 every time:
    - `Failed` has M2, and `Safe` is false;
    - M1 and M3 are copied and in the manifest, M2 isn't;
    - the hook was called 3 times for M2.
  - `TestTransientErrorRecovers`: M2 fails once, then succeeds: `Safe` is true and nothing
    fails. Use 1 ms retry delays through a package variable.
  - `TestWriteErrorFailsFileNotRun`: the `write` hook returns `syscall.ENOSPC` for M2:
    M2 fails, the others are copied, and `Safe` is false.
  - `TestRerunResumes`: run once, cancelled after the first file. Then:
    - a stale temp file is planted in the folder;
    - `MakePlan` and `Run` are run again;
    - the plan marks the first file skipped, all files end up present, the temp is gone,
      and `Safe` is true.
  - `TestVerifyFindsLaterCorruption`: after a run, flip a byte in one copy: `Verify`
    reports 1 bad and 2 ok.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.** The `MakePlan` manifest lookup from Task 1 now uses
  `readManifest`.
- [ ] **Step 4: Run** `go test ./internal/offload`. Expected: PASS.
- [ ] **Step 5: Commit.** `offload: run with retries, verified-only manifest, F_FULLFSYNC before "safe to format", --verify`

### Task 4: `cull offload`, then scan

**Files:** create `internal/cli/offload.go`; edit `internal/cli/root.go` (register the
command) and `internal/cli/cli_test.go`; README (a new "Offload" section and the flag
reference); CLAUDE.md (layout entry, plus the benchmark result).

**Command:**
- **Usage:** `cull offload <source>... <dest>`, with at least two arguments.
- **Flags:** `--name`, `--date`, `--backup`, `--rename`, `--checksum`, `--dry-run`,
  `--no-scan`, and `--verify <shoot-folder>` (which takes no positional arguments).
- **Flow:**
  1. `MakePlan`; print `Summary()`. It covers the folder and where its date came from, the
     files to copy and their GB, the skips by reason, each destination with its free
     space, and the first and last destination names.
  2. `--dry-run` stops there.
  3. Otherwise `Run` with a live output (`so.out.newOutput(cmd, true)`).
  4. Print the summary line: copied, skipped, failed, GB, MB/s.
  5. Print either `all N files verified on <dest> and <backup>: safe to format the card`,
     or `NOT safe to format: <n> failed` with the list.
  6. Exit non-zero unless Safe.
  7. Unless `--no-scan`, run the scan (as `scan` does, `cfg.DryRun = true`) on `Dests[0]`,
     then print `next: cull judge --estimate "<folder>"`.

- [ ] **Step 1: Write the failing tests** (cli, against a fake card in `t.TempDir()`):
  - `TestOffloadDryRunWritesNothing`;
  - `TestOffloadCopiesThenScans`: output has `safe to format` and `1 DNGs found`, and the
    report exists in the shoot folder;
  - `TestOffloadRefusesClash`: exits non-zero, `--rename` is suggested, and nothing is
    copied;
  - `TestOffloadVerify`: corrupt a copy, then `--verify` exits non-zero and names it.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/cli` unsandboxed. Expected: PASS.
- [ ] **Step 5: Benchmark and real-card dry run.**
  - **Benchmark:** a 2 GiB synthetic source tree of 25 × 80 MiB files on the internal disk,
    copied by `cull offload --no-scan` to a fresh folder, timed against `cp -R` of the same
    tree, both uncached (`purge` needs sudo, so the tree is written with `F_NOCACHE`, or a
    fresh tree per run). Record both MB/s in CLAUDE.md. The copy also verifies, so expect
    it slower than `cp` but within about 2×, since the re-read comes from SSD.
  - **Real card:** `cull offload --dry-run "/Volumes/LEICA M" $TMPDIR/dest --name test`
    (read only). Record the plan's date source, file count and GB.
  - **A real copy of the 63 GB card** needs the user's go-ahead (disk space and time):
    ask.
- [ ] **Step 6: Docs.**
- [ ] **Step 7: Commit.** `cull offload: plan, copy, verify, scan; docs`

### Task 5: Folder sorting

**Files:** `internal/pipeline/move.go` (generalize), `pipeline.go` (Discover skips,
finishRun), `decide.go` (`Sort` option), `internal/cli/cull.go` and `decide.go`
(`--sort`), `status.go` (wording), tests in `internal/pipeline/move_test.go` and the cli,
README.

**Interfaces:**

```go
const (KeepDir = "keep"; ReviewDir = "review"; CullDir = "cull")
var placeDirs = []string{CulledDir, KeepDir, ReviewDir, CullDir} // Discover skips these

type placement int
const (placeCulled placement = iota + 1; placeSorted)

// want is where r belongs: "" = home, else the folder beside home.
func want(r report.Result, lab labels.Entry, mode placement) string
// place moves every frame to where it belongs (with its sidecar), returning moved and
// restored counts. Frames that can't move stay put, with a Fixup and a warning.
func place(rep *report.Report, lab map[string]labels.Entry, mode placement, log io.Writer) (moved, home int)
```

**Rules:**
- **`placeCulled`:** an effective cull goes to `culled/`; anything else goes home. This is
  today's behaviour of `moveCulled` plus decide's un-cull restore, unified.
- **`placeSorted`:** the effective verdict goes to its folder: keep, review or cull.
  - Frames with an error or no evaluation stay home.
  - **The policy already maps every frame without a verdict to review,** so a measured but
    unjudged frame (from scan) stays home. The CLI refuses `--sort` on a report with no
    judged frames.
- **Moves:** from where the frame is now (`MovedTo`, or home) to where it belongs:
  `relocate(cur, dst)`. `MovedTo` is set to the destination, or cleared when it goes
  home. Afterwards, remove the old folder if it's empty.
- **`reconcileMove`:** if the recorded location doesn't exist, look for a file whose size
  and mtime match (`sameVersion`) at home and in each of `placeDirs`. With exactly one
  match, adopt it; with none or several, change nothing.
- **Discover** skips every name in `placeDirs`.
- **`judge --sort`** applies `placeSorted` in `finishRun`, after saving.
  **`decide --sort`** replaces the separate restore loop and `moveCulled` with `place`.
- **Mutual exclusion:** `--sort` together with `--move-culled` is an error.
- **`restore`** already handles any `MovedTo`. Its folder cleanup tries every recorded
  parent folder.

- [ ] **Step 1: Write the failing tests.**
  - `TestSortPlacesByVerdict`: four frames judged keep, review, cull and error land in
    `keep/`, `review/`, `cull/` and home, each with its sidecar. `MovedTo` is recorded.
  - `TestResortFollowsLabels`: after sorting, label the cull frame keep, then
    `Decide(Sort)`: it moves `cull/` → `keep/` with its sidecar, and `cull/` is removed
    once empty.
  - `TestSortNeverOverwrites`: `keep/L1.DNG` already exists (a stranger): L1 stays home,
    with a Fixup and a warning line.
  - `TestReconcileFindsSortedFrame`: L1 renamed into `review/` by hand, with no record:
    reconcile adopts it. Two copies (`keep/` and `review/`): unchanged.
  - `TestRestoreFromSortFolders`: everything goes home, and the folders are removed.
  - `TestDiscoverSkipsPlaceDirs`.
  - Existing `moveCulled` and decide tests pass unchanged; the `placeCulled` semantics
    match.
  - CLI:
    - `judge --sort --move-culled` → error;
    - `decide --sort` on a scan-only report → error ("nothing judged");
    - `judge --sort` with the fake claude: the frame ends up in `cull/`.
- [ ] **Step 2: Run them and check they fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/pipeline ./internal/cli` (cli unsandboxed).
  Expected: PASS.
- [ ] **Step 5: Docs.**
  - README: the sort folders.
  - **Sort before importing:** Capture One loses track of frames moved after import. Then
    import `keep/` and `review/`.
  - Status shows "sorted" or "moved" counts.
- [ ] **Step 6: Commit.** `--sort: keep/ review/ cull/ placement (judge, decide), re-sort, reconcile, restore`

### Finish

- [ ] `make vet`, and `go test -count=1 ./...` unsandboxed.
- [ ] Final whole-branch review (fresh reviewer, most capable model), one fix pass, then
  the finishing menu.
