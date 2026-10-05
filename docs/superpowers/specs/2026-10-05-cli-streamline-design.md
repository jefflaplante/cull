# CLI streamlining: design

**Goal:** fewer things to know before running `cull`. The commands map onto the workflow
well; the sprawl is in the flags. `judge --help` lists 56 flags A–Z. The 11 policy flags
repeat on judge, rank, decide and calibrate, and the 4 tag flags on offload, scan and
judge. After this change, the everyday session is:

```sh
cull offload /Volumes/LEICA\ M ~/Pictures --name "X"   # copy, verify, free scan
cull judge "$SHOOT"          # asks over $1, judges, ranks; re-run to continue
cull review "$SHOOT"         # your labels and stars, saved to sidecars
cull decide --sort "$SHOOT"  # before import
cull apply-c1 --run "$SHOOT" # after import
```

**Release:** v0.2.0. Everything retired here is *deprecated*, not deleted: it still works,
prints a one-line "use X instead" warning, and is gone from help. Deletion is the
following release's job (not in this plan).

**Decided with the user (2026-10-05):** deprecate for one release; keep the experimental
features but hide them; drop the offline review page; `--help` in sections with
`--help-all`; culls-only sorting uses `cull/`; sidecars on by default everywhere; replace
calibrate's sharpness sweep with a policy grid; re-record the site's session; finish with
a v0.2.0 tag, push and gh-pages deploy.

## 1. Command surface

### judge resumes by default
- An existing report is continued, as `--resume` did. `--fresh` replaces it; `-o` writes a
  separate report. The resume guard is unchanged: a different schema, backend, model,
  escalation or effort is refused, and the refusal names `--fresh` and `-o`.
- A report holding nothing paid for (a scan report) is continued too: its tags carry over
  and every frame is judged.
- **Backend and model come from the report** when one exists and the flag wasn't typed,
  as `rank` does today (`internal/cli/rank.go`): typed flag > report > `~/.cull` > built-in
  default. A `~/.cull` value doesn't mark a flag typed, so the report wins over it.
- `--resume` becomes a deprecated no-op. It leaves `notInDotfile` (deprecated instead).

### rank folds into judge
- Every judge run already ranks the sets that need it in `finishRun`
  (`internal/pipeline/pipeline.go`), so `cull judge <dir>` on a finished report = `cull rank`.
- New `judge --rerank`: re-rank every set of two or more rankable frames, even ones
  with a model order (= `rank --force`; `rankSets(..., force=true, ...)`).
- `judge --batch` on a judged report must rank through the Message Batches executor, as
  `rank --batch` does. Verify; fix if not.
- `cull rank` is deprecated (cobra `Deprecated`): it still runs, unchanged, and its
  message says to use `cull judge <dir>`, with `--rerank` to redo ranked sets.
- `judge --estimate` prices the frames still pending plus the ranking that will run.

### Sidecars on by default
- judge, decide and review keep cull-written sidecars current by default. `--no-xmp` turns
  it off on all three (review has it already).
- `--write-xmp` becomes a deprecated no-op on judge and decide.
- Unchanged: a sidecar cull didn't write is never touched without `--overwrite-xmp`.
- `cull tag`'s hint becomes `cull decide <dir>`.

### One way to move files
- `--sort` takes an optional value: `all` (the default when given bare) or `culls`.
  pflag `NoOptDefVal = "all"`.
  - `all`: keep/, review/, cull/, as today.
  - `culls`: culls into `cull/`, everything else stays home.
- `--move-culled` becomes a deprecated alias for `--sort=culls` on judge and decide.
- **Old `culled/` moves:** `reconcileMove` still finds frames in `culled/`. The next sort
  moves them into `cull/` (or home), and `restore` still undoes them. `CulledDir` stays as
  a read-only legacy location. Discover keeps skipping it.
- `review --sort` is unchanged. It sorts all; there is no `culls` form on review.

### Retired outright (deprecated, no replacement)
- `--xmp-develop` (judge, decide): a no-op. The warning says Capture One ignores those
  settings.
- `review --static` and `cull import-labels`: still work, with a deprecation warning.
  Deleted next release.
- Tag flags (`--project --event --location --keyword`) on scan and judge: still applied,
  with a warning to use `offload` or `cull tag`.

### calibrate's grid
- The `--review-below-sharpness` sweep is removed.
- In its place is a grid over the report's stored assessments and ranks:
  - axes: `--keep-best` 1–5 × `--outranked` review/cull × `--raw-clipped` review/ignore;
  - 20 rows, each showing false culls (n, %), culls caught, missed culls and review %;
  - other policy values come from the flags, or the stored policy.
- Rows under 1% false culls are marked. The row matching the current policy is marked
  "current".
- The grid prints only when labels exist; `--compare` is unchanged.
- The sets section's keep-best 1..5 sweep is now redundant with the grid, so it is removed.

### judge --estimate from real sets
- When the report holds sets from a scan (results without evaluations), ranking is priced
  from them. Each set's non-junk members count as rankable: an upper bound, because some
  will be culled. The count uses `callsFor`.
- The estimate line says "from N sets found by scan".
- Without sets, the current every-frame-in-an-8-frame-set worst case stays, and it says so.

## 2. Help output

- Each flag gets a section via a pflag annotation (`cull_section`):
  - Common
  - Policy (tune afterwards with `cull decide`, free)
  - Backend
  - Sidecars & folders
  - Tuning
  - Experimental: `--escalate-*`, `--second-opinion`, `--rank-twice`, `--landed-with-subject`
- One usage template on the root renders the sections in that order; a flag without an
  annotation falls into Common. Global flags stay last.
- `--help` shows Common, Policy, Backend and Sidecars & folders. It ends with
  "N more flags (tuning, experimental): --help-all".
- `--help-all` is a persistent root flag that shows every section.
- Flags are not `MarkHidden`, so completion still offers them.
- Deprecated flags *are* hidden (cobra does that), and appear in neither.
- **Common is per command.** For judge: `--backend`, `--model`, `--estimate`, `--batch`,
  `--max-cost`, `--sort`, `--no-xmp`, `--fresh`, `--rerank`, `-j`, `--yes`.

## 3. Compatibility and safety

- **Report schema:** no change (v4). Escalated reports and `culled/` moves keep loading.
- **Invariants hold:**
  - DNGs are never modified.
  - Moves happen only under `--sort` (and its deprecated alias).
  - Foreign sidecars are never overwritten without `--overwrite-xmp`.
  - Resume-by-default spends nothing new on frames already judged, and the guard still
    refuses mixing.
- **`~/.cull`:** `applyDotfile` sets values without parsing, so cobra never prints the
  deprecation. It must warn itself when a key names a deprecated flag
  (`f.Deprecated != ""`), then apply it as the alias would.
- **`status`:** its "next" hints drop `rank`, `--resume`, `--write-xmp` and
  `--move-culled`. An unranked judged report suggests `cull judge <dir>`.
- **Errors and hints:** every message that suggests a command is updated. These live in
  pipeline (`pipeline.go`, `batch.go`, `rank_batch.go`, `move.go`) and cli.

## 4. Tests

All synthetic, as now.
- **Deprecations:** a table over every deprecated flag and command, checking that it
  warns once, on stderr, and behaves as its replacement. Covers `--move-culled` →
  `--sort=culls`, `--resume`, `--write-xmp`, `--xmp-develop`, tag flags on scan/judge,
  `cull rank`, `review --static`, `import-labels`.
- **Resume by default:** a second judge run with a stub backend calls nothing for done
  frames. The backend comes from the report when not typed, and a typed one that differs
  is refused with `--fresh` in the message.
- **`--rerank`:** it re-ranks sets that already have a model order. Without it, they are
  left alone.
- **Sorting:** `--sort=culls` places culls in `cull/`. A frame recorded in `culled/` is
  moved to `cull/` on the next sort, and `restore` brings back both.
- **Sidecars:** judge and decide write them by default; `--no-xmp` writes none; a foreign
  sidecar is untouched.
- **Help:** a golden test of `judge --help` sections. `--help-all` shows Experimental, and
  plain help doesn't but counts it.
- **Calibrate:** the grid on synthetic labels and sets checks the counts in a known row
  and the <1% marking.
- **Estimate:** a scan report with sets gives `callsFor`'s count.
- `~/.cull` with a deprecated key warns and applies the alias.

## 5. Docs, examples and site

Every user-facing text moves to the new forms:
- **Cobra text:** `Long`, `Example` and flag usage strings on every command, and the
  root's intro.
- **Repo docs:**
  - README.md and USAGE.md: workflow, command reference, flag tables, `~/.cull`.
  - CLAUDE.md: the Commands block, Layout (rank.go and importlabels.go deprecated),
    Invariants (moves: `--sort`), and the policy-test table's flags.
  - Earlier plans and specs are history: left as they are.
- **Site:**
  - `site/usage.html`: contents table (rank and import-labels marked deprecated), each
    step, "Every command and flag", Policy flags, Global flags, `~/.cull`, help
    sections, `--help-all`.
  - `site/index.html`: pipeline and delivery copy.
- **Review page** (`internal/review/page.html`): any command text it shows.
- **Re-recorded session:** the 17 sample frames, from `photos/` (gitignored), with the
  new binary.
  - A folder stands in for the card, and the destination is under `$TMPDIR`.
  - The run is `offload` (free, includes the scan), `judge --estimate`, then
    `judge --backend claude-code`: subscription quota, about 5 minutes, and **the user
    confirms before it runs** (CLAUDE.md).
  - The output is transcribed into `site/site.js` (`SCAN`, `JUDGED`, `REPLAYS`), following
    the rules in its comment, and the usage page's terminal blocks are updated to match.
  - The PNG fallbacks (`site/img/*.png`, `docs/images/*.png`) are terminal screenshots
    the user captures.
- **Release:** `make test`, `make vet`, commit and push main, tag `v0.2.0` and push it (the
  release workflow builds and publishes), then deploy gh-pages with
  `git subtree split --prefix site`.
  - The brew tap formula update is whatever the release flow already does. Check it, and
    note any manual step.
