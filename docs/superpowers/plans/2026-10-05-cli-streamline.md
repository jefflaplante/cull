# CLI streamlining Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `cull` simpler to drive, with the same capabilities:
- `judge` resumes and ranks by itself;
- one `--sort` replaces two move modes;
- sidecars are written by default;
- help comes in sections;
- `calibrate` sweeps the settings that matter;
- everything retired is deprecated for one release.

The docs, site and recordings move with the change, and it ships as v0.2.0.

**Architecture:**
- **CLI only:** most changes are in `internal/cli` (cobra flags, defaults and help).
- **Pipeline:** small changes in `internal/pipeline`:
  - `--sort=culls` goes to `cull/`;
  - a scan report no longer blocks a resume;
  - `Config.Rerank`;
  - `EstimateRanking`.
- **Calibrate:** `internal/calib` replaces the sharpness sweep with a policy grid.
- **Deprecation:** cobra's `MarkDeprecated` (flags) and `Deprecated` (commands). The old
  forms keep working, warn once, and drop out of help.

**Tech Stack:** Go 1.26, cobra v1.10.2, pflag. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-05-cli-streamline-design.md` (approved 2026-10-05).
Read it before starting.

## Global Constraints

- No new dependencies (CLAUDE.md "Dependencies").
- Never modify or delete DNGs.
- Moves only under `--sort` (and its deprecated alias `--move-culled`), never overwriting;
  `restore` undoes every move, including old `culled/` ones.
- Never overwrite a sidecar cull didn't write unless `--overwrite-xmp`.
- Report schema stays v4: no field added or removed.
- Tests use synthetic fixtures only; never commit real images.
- Deprecated forms still work in v0.2.0 and print one warning each. Deleting them is NOT in this plan.
- Running tests:
  - `make test` needs the sandbox off (httptest binds loopback; the Go build cache is
    outside the sandbox). Run test commands unsandboxed, alone.
  - Builds inside the sandbox: `GOCACHE=$TMPDIR/gocache make build`.
- Don't run `judge` on real photos without asking the user first (Task 12).
- Checking that no matches remain: use Python, not grep. The rtk hook can hide grep
  output.
- Commit style: one descriptive sentence, no conventional-commit prefix (see `git log`).
- Work on branch `cli-streamline`; Task 13 merges it into main.

## Review Focus

1. **`--sort culls <dir>` (a space, not `=`):** must fail naming `--sort=culls`, not
   "accepts 1 arg(s)", and must never treat `culls` as the folder. Test in Task 2.
2. **Old keys in `~/.cull`:** `sort = true`, `write-xmp = false`, `move-culled = true` and
   `resume = true` each warn that they're deprecated, map to the new behaviour
   (`--sort=all`, `--no-xmp`, `--sort=culls`, nothing), and never fail the run.
   Tests in Tasks 2 and 8.
3. **A report judged with claude-code while `~/.cull` says `backend = anthropic`:** judge
   continues with claude-code (the report wins over the dotfile) and prints that backend.
   Test in Task 4.
4. **Frames already in an old `culled/` folder, then `decide --sort`:** they move into the
   right sort folder with their sidecars, and `restore` brings them home. Test in Task 2.
5. **`cull judge --help-all` with no folder, and `cull --help-all`:** both print help and
   exit 0, with no "accepts 1 arg(s)". Test in Task 1.

---

## Task 0: Branch

- [ ] **Step 1:** `git checkout -b cli-streamline` (from an up-to-date, clean `main`).

---

## Task 1: Sectioned help and `--help-all`

**Files:**
- Create: `internal/cli/help.go`, `internal/cli/help_test.go`
- Modify: `internal/cli/root.go` (help flags, usage template, `registerPrep`, `registerSeqVars`)
- Modify: `internal/cli/policy.go` (`register`)
- Modify: `internal/cli/backend.go` (`register`)
- Modify: `internal/cli/cull.go`, `internal/cli/decide.go`, `internal/cli/review.go`,
  `internal/cli/offload.go` (section calls after flag registration)

**Interfaces:**
- **Produces:**
  - constants `secCommon = ""`, `secPolicy = "Policy"`, `secBackend = "Backend"`,
    `secSidecars = "Sidecars & folders"`, `secTuning = "Tuning"`,
    `secExperimental = "Experimental"`;
  - `func setSection(fs *pflag.FlagSet, sec string, names ...string)`, which panics on an
    unknown name;
  - `func sectionOf(f *pflag.Flag) string`.
- **Later tasks:** every new flag gets a section through `setSection`, unless it belongs
  in Common.

- [ ] **Step 1: Write the failing tests** (`internal/cli/help_test.go`)

```go
package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Plain --help shows the everyday sections in order and folds tuning and
// experimental flags into a count; --help-all shows them.
func TestJudgeHelpSections(t *testing.T) {
	out, err := run(t, "judge", "--help")
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"Flags:", "Policy flags", "Backend flags", "Sidecar and folder flags", "Global Flags:"}
	at := -1
	for _, h := range order {
		i := strings.Index(out, h)
		if i < 0 || i < at {
			t.Fatalf("section %q missing or out of order in:\n%s", h, out)
		}
		at = i
	}
	for _, hidden := range []string{"--second-opinion", "--rank-twice", "--escalate-backend", "--checkpoint", "Tuning flags", "Experimental flags"} {
		if strings.Contains(out, hidden) {
			t.Errorf("plain --help shows %s", hidden)
		}
	}
	if !strings.Contains(out, "more flags (tuning, experimental): cull judge --help-all") {
		t.Errorf("no --help-all pointer:\n%s", out)
	}
	all, err := run(t, "judge", "--help-all")
	if err != nil {
		t.Fatalf("--help-all without a folder must just print help: %v", err)
	}
	for _, want := range []string{"Tuning flags", "Experimental flags", "--second-opinion", "--checkpoint"} {
		if !strings.Contains(all, want) {
			t.Errorf("--help-all lacks %s", want)
		}
	}
	if _, err := run(t, "--help-all"); err != nil {
		t.Fatalf("root --help-all: %v", err)
	}
}

// Every flag on every command is in a known section, so a new flag can't land
// somewhere the template doesn't print.
func TestEveryFlagHasAKnownSection(t *testing.T) {
	known := map[string]bool{secCommon: true, secPolicy: true, secBackend: true, secSidecars: true, secTuning: true, secExperimental: true}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if !known[sectionOf(f)] {
				t.Errorf("%s --%s: section %q", c.CommandPath(), f.Name, sectionOf(f))
			}
		})
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(NewRootCmd())
}
```

- [ ] **Step 2: Run them and check they fail**

Run: `go test ./internal/cli -run 'TestJudgeHelpSections|TestEveryFlagHasAKnownSection' -count=1`
Expected: compile failure, because `secCommon` and `sectionOf` are undefined.

- [ ] **Step 3: Write `internal/cli/help.go`**

```go
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Flag sections: help prints each command's flags grouped, the everyday ones first.
// Tuning and Experimental appear only under --help-all; every flag still works and
// completes either way.
const (
	sectionKey      = "cull_section"
	secCommon       = ""
	secPolicy       = "Policy"
	secBackend      = "Backend"
	secSidecars     = "Sidecars & folders"
	secTuning       = "Tuning"
	secExperimental = "Experimental"
)

var sectionOrder = []struct{ sec, title string }{
	{secCommon, "Flags:"},
	{secPolicy, "Policy flags (stored in the report; tune them later with cull decide, free):"},
	{secBackend, "Backend flags:"},
	{secSidecars, "Sidecar and folder flags:"},
	{secTuning, "Tuning flags:"},
	{secExperimental, "Experimental flags (not yet measured against labels):"},
}

// folded are the sections plain --help leaves out.
var folded = map[string]bool{secTuning: true, secExperimental: true}

// showAllFlags is set by --help-all for the help being printed.
var showAllFlags bool

// setSection puts the named flags of fs in sec. An unknown name panics: it is a typo,
// and every test that builds the command tree catches it.
func setSection(fs *pflag.FlagSet, sec string, names ...string) {
	for _, n := range names {
		if err := fs.SetAnnotation(n, sectionKey, []string{sec}); err != nil {
			panic(fmt.Sprintf("setSection %q: %v", n, err))
		}
	}
}

func sectionOf(f *pflag.Flag) string {
	if v := f.Annotations[sectionKey]; len(v) == 1 {
		return v[0]
	}
	return secCommon
}

// sectionedFlags renders c's own flags by section, for the usage template.
func sectionedFlags(c *cobra.Command) string {
	sets := map[string]*pflag.FlagSet{}
	hidden := 0
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Deprecated != "" {
			return
		}
		s := sectionOf(f)
		if folded[s] && !showAllFlags {
			hidden++
			return
		}
		if sets[s] == nil {
			sets[s] = pflag.NewFlagSet(s, pflag.ContinueOnError)
		}
		sets[s].AddFlag(f)
	})
	var b strings.Builder
	for _, s := range sectionOrder {
		fs := sets[s.sec]
		if fs == nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s.title + "\n" + strings.TrimRight(fs.FlagUsages(), " \n"))
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "\n\n%d more flags (tuning, experimental): %s --help-all", hidden, c.CommandPath())
	}
	return b.String()
}

// helpAllValue is --help-all: it turns on the help flag too, so cobra prints help
// before checking arguments (a bare `cull judge --help-all` needs no folder).
type helpAllValue struct{ all, help *bool }

func (v helpAllValue) String() string   { return fmt.Sprint(*v.all) }
func (v helpAllValue) Type() string     { return "bool" }
func (v helpAllValue) IsBoolFlag() bool { return true }
func (v helpAllValue) Set(s string) error {
	on := s == "true"
	if !on && s != "false" {
		return fmt.Errorf("want true or false")
	}
	*v.all, *v.help = on, on
	return nil
}

// installHelp adds -h/--help and --help-all to root and swaps the usage template's
// flag list for the sectioned one.
func installHelp(root *cobra.Command) {
	showAllFlags = false
	var help bool
	pf := root.PersistentFlags()
	pf.BoolVarP(&help, "help", "h", false, "help for this command")
	pf.Var(helpAllValue{all: &showAllFlags, help: &help}, "help-all", "help listing every flag, tuning and experimental ones too")
	pf.Lookup("help-all").NoOptDefVal = "true"
	cobra.AddTemplateFunc("sectionedFlags", sectionedFlags)
	const plain = "Flags:\n{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}"
	t := root.UsageTemplate()
	if !strings.Contains(t, plain) {
		panic("cobra's usage template changed: update installHelp")
	}
	root.SetUsageTemplate(strings.Replace(t, plain, "{{sectionedFlags .}}", 1))
}
```

- [ ] **Step 4: Wire it in and annotate the flags**

In `root.go` `NewRootCmd`, right after `so.out.register(pf)`, add `installHelp(root)`.

In `root.go` `registerPrep`, at the end:

```go
	setSection(f, secTuning, "max-edge", "tiles", "min-preview-edge", "face-min-q", "save-inputs", "no-review-images")
	setSection(f, secExperimental, "landed-with-subject")
```

In `root.go` `registerSeqVars`, at the end: `setSection(f, secPolicy, "seq-gap", "seq-look")`.

In `root.go` `NewRootCmd`, after the `for _, c := range []*cobra.Command{scan, judge}` loop,
make `--save-inputs` common on scan (its main use):

```go
	setSection(scan.Flags(), secCommon, "save-inputs")
```

In `policy.go` `policyFlags.register`, at the end:

```go
	setSection(f, secPolicy, "min-crop-area", "review-below-sharpness", "cull-max-sharpness", "eyes-closed",
		"outranked", "raw-clipped", "raw-clip-threshold", "junk", "keep-best")
```

In `backend.go` `backendFlags.register`, at the end:
- Check the flag names first with `sed -n 25,41p internal/cli/backend.go`.
- `backend` and `model` stay in Common.

```go
	setSection(f, secBackend, "api-key-file", "base-url", "openai-key-file", "effort", "quota-stop")
	setSection(f, secTuning, "openai-stream", "claude-bin", "locate-effort")
```

In `cull.go`, after `f.IntVar(&o.checkpoint, ...)`:

```go
	setSection(f, secSidecars, "overwrite-xmp", "labels", "no-labels", "write-xmp", "xmp-develop", "move-culled")
	setSection(f, secTuning, "locate", "raw-clip", "checkpoint", "batch-poll", "no-rank")
	setSection(f, secExperimental, "escalate-backend", "escalate-model", "escalate-on", "second-opinion", "rank-twice")
```

In `decide.go`, after the `f.BoolVar(&noLabels, ...)` line:

```go
	setSection(f, secSidecars, "write-xmp", "xmp-develop", "overwrite-xmp", "labels", "no-labels", "move-culled")
```

In `review.go`, after the `f.IntVar(&port, ...)` line:

```go
	setSection(f, secSidecars, "no-xmp", "overwrite-xmp", "labels")
	setSection(f, secTuning, "out", "concurrency", "force", "port")
```

In `offload.go`, after its flags are registered: `setSection(cmd.Flags(), secTuning, "checksum")`.
Use the local flag-set variable name the file already uses.

In `rank.go`, after `o.policy.register(f)`:

```go
	setSection(f, secTuning, "batch-poll")
	setSection(f, secExperimental, "rank-twice")
```

- [ ] **Step 5: Run the tests and check they pass**

Run: `go test ./internal/cli -count=1` (unsandboxed)
Expected: PASS, the whole package included. A `setSection` panic names a flag that doesn't
exist on that command: fix the name.

- [ ] **Step 6: Look at it by eye**

Run: `GOCACHE=$TMPDIR/gocache make build && ./bin/cull judge --help && ./bin/cull judge --help-all | tail -30 && ./bin/cull --help-all | head -5`
Expected:
- the sections appear in order;
- the last line before Global Flags is `N more flags (tuning, experimental): cull judge --help-all`;
- `cull --help-all` exits 0.

- [ ] **Step 7: Commit**

```bash
git add internal/cli
git commit -m "Help in sections: everyday flags first, tuning and experimental ones under --help-all"
```

---

## Task 2: One `--sort` (`all` | `culls`); `--move-culled` deprecated; culls-only uses `cull/`

**Files:**
- Create: `internal/cli/sortflag.go`, `internal/cli/sortflag_test.go`
- Modify: `internal/pipeline/move.go`, `internal/pipeline/pipeline.go` (finishRun note,
  guardOverwrite message), `internal/pipeline/decide.go` (error text)
- Modify: `internal/cli/cull.go`, `internal/cli/decide.go`, `internal/cli/review.go`,
  `internal/cli/status.go`, `internal/cli/restore.go`
- Modify: `internal/report/report.go` (comments that say `--move-culled` / `culled/`)
- Test: `internal/pipeline/sort_test.go`, `internal/pipeline/move_test.go` (expectations
  that `--move-culled` lands in `culled/` become `cull/`), `internal/cli/cli_test.go`,
  `internal/cli/status_test.go`

**Interfaces:**
- **Produces:**
  - `type sortMode string` with `sortNone = ""`, `sortAll = "all"` and `sortCulls = "culls"`;
  - `func registerSort(f *pflag.FlagSet, m *sortMode, usage string)`;
  - `func (m sortMode) flags() (moveCulled, sort bool)`, mapping to the pipeline's
    `MoveCulled` / `Sort`;
  - `func sortArgs(n int) cobra.PositionalArgs`.
- **Pipeline:** `Config.MoveCulled` and `DecideOptions.MoveCulled` keep their names but now
  mean "culls into `cull/`". `CulledDir` stays as a legacy read-only folder in `placeDirs`.

- [ ] **Step 1: Write the failing pipeline tests** (append to `internal/pipeline/sort_test.go`)

```go
// Culls-only placement (--sort=culls) uses cull/, the same folder full sorting uses.
func TestCullsOnlyUsesCullDir(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir) // MoveCulled: culls only
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, CullDir, "L1000001.DNG")) || exists(filepath.Join(dir, CulledDir)) {
		t.Fatal("the cull must be in cull/, and culled/ must not exist")
	}
	if !exists(filepath.Join(dir, "L1000002.DNG")) || !exists(filepath.Join(dir, "L1000003.DNG")) {
		t.Fatal("keep and review frames stay home")
	}
}

// A frame an older version moved into culled/ is found, re-placed by the next sort,
// and restore brings it home from wherever it is.
func TestLegacyCulledFolderIsResortedAndRestored(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled = false
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	// Simulate v0.1.x: L1 (cull) moved into culled/ with its sidecar, recorded.
	legacy := filepath.Join(dir, CulledDir)
	os.MkdirAll(legacy, 0o755)
	for _, ext := range []string{".DNG", ".xmp"} {
		if err := os.Rename(filepath.Join(dir, "L1000001"+ext), filepath.Join(legacy, "L1000001"+ext)); err != nil {
			t.Fatal(err)
		}
	}
	rep, _ := report.Load(c.ReportPath)
	for i := range rep.Results {
		if r := &rep.Results[i]; filepath.Base(r.File) == "L1000001.DNG" {
			r.MovedTo, r.XMP = filepath.Join(legacy, "L1000001.DNG"), filepath.Join(legacy, "L1000001.xmp")
		}
	}
	rep.Save(c.ReportPath)

	if _, err := Decide(context.Background(), c.ReportPath, DecideOptions{Policy: c.Policy, Sort: true, WriteXMP: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".DNG", ".xmp"} {
		if !exists(filepath.Join(dir, CullDir, "L1000001"+ext)) {
			t.Fatalf("L1000001%s not moved culled/ → cull/", ext)
		}
	}
	if exists(legacy) {
		t.Fatal("the emptied culled/ folder should be removed")
	}
	if _, err := Restore(c.ReportPath, dir, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"L1000001", "L1000002", "L1000003"} {
		if !exists(filepath.Join(dir, n+".DNG")) {
			t.Fatalf("%s not restored home", n)
		}
	}
}
```

Update the existing pipeline tests that assert a culls-only run lands in `CulledDir`
(`move_test.go`, `sort_test.go`): they now expect `CullDir`. List them with:

```bash
python3 - <<'EOF'
import glob,re
for f in sorted(glob.glob('internal/pipeline/*_test.go')):
    for i,l in enumerate(open(f),1):
        if re.search(r'CulledDir|"culled"',l): print(f'{f}:{i}: {l.rstrip()}')
EOF
```

Change each hit to `CullDir` / `"cull"`, except where a test sets up a legacy folder or
checks that `Discover` skips `culled/` (`TestDiscoverSkipsPlaceDirs`,
`TestOffloadKnowsThePlaceDirs`, and the new legacy test).

- [ ] **Step 2: Write the failing CLI tests**

Create `internal/cli/sortflag_test.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSortModeValues(t *testing.T) {
	for in, want := range map[string]sortMode{"all": sortAll, "true": sortAll, "culls": sortCulls, "false": sortNone, "": sortNone} {
		var m sortMode
		if err := m.Set(in); err != nil || m != want {
			t.Errorf("Set(%q) = %q, %v; want %q", in, m, err, want)
		}
	}
	var m sortMode
	if err := m.Set("keep"); err == nil {
		t.Error("an unknown value must fail")
	}
}

// `--sort culls <dir>` (a space) would take "culls" as the folder: say what to type.
func TestSortWithSpaceNamesTheEqualsForm(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	for _, args := range [][]string{{"decide", "--sort", "culls", dir}, {"judge", "--estimate", "--sort", "culls", dir}} {
		_, err := run(t, args...)
		if err == nil || !strings.Contains(err.Error(), "--sort=culls") {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

// The deprecated --move-culled still works, as --sort=culls, into cull/.
func TestMoveCulledIsSortCulls(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", "--move-culled", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "--move-culled has been deprecated") || !strings.Contains(out, "--sort=culls") {
		t.Errorf("no deprecation warning naming --sort=culls:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "cull", "L1000001.DNG")); err != nil {
		t.Fatalf("not in cull/:\n%s", out)
	}
}
```

In `cli_test.go`:
- Rename `TestCullMoveCulledThenRestoreEndToEnd` to `TestSortCullsThenRestoreEndToEnd`.
- Change its flags to `"--sort=culls"`. Leave `--write-xmp` alone; Task 3 removes it.
- Change its folder to `"cull"`.
- Delete `TestCullHasMoveCulledFlag`; `TestSortModeValues` replaces it.

- [ ] **Step 3: Run them and check they fail**

Run: `go test ./internal/pipeline ./internal/cli -run 'CullsOnly|LegacyCulled|SortMode|SortWithSpace|MoveCulledIsSortCulls|SortCullsThenRestore' -count=1`
Expected: the pipeline tests FAIL (the cull lands in `culled/`), and the CLI tests fail to compile (`sortMode`).

- [ ] **Step 4: Implement the pipeline side** (`internal/pipeline/move.go`)
  - Rename `placeCulled` to `placeCulls`, with the comment `// --sort=culls: culls into cull/, everything else home`.
  - In `want`, the `placeCulls` case returns `CullDir`, not `CulledDir`.
  - Change the constants comment to say:
    - `CulledDir` is where v0.1 `--move-culled` put culls;
    - it is only read now: `reconcileMove` and `Restore` find frames there, and the next
      sort moves them out;
    - `Discover` still skips it.
  - In `placeShown`, the `mode == placeCulls` text becomes `"moving culls into cull/"`.
  - `moveCulled` keeps its name; fix its comment.
  - `internal/pipeline/pipeline.go` finishRun: the `else if n := moveCulled(...)` note becomes
    `"moved %d culled frame(s) into %s/ (undo: cull restore %s)", n, CullDir, cfg.Dir`.
  - `guardOverwrite`: the moved>0 message stops naming `CulledDir`:
    `"%s records %d frame(s) moved into sort folders: run `cull restore %s` first, or use -o for a separate report"`.
  - `internal/pipeline/decide.go`: the Sort+MoveCulled error becomes
    `"--sort=all and --sort=culls can't be combined"`. The `case o.MoveCulled: mode = placeCulls`
    stays.

- [ ] **Step 5: Implement the CLI flag** (`internal/cli/sortflag.go`)

```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// sortMode is --sort: all (keep/, review/, cull/), culls (only cull/), or none.
// A bare --sort means all; true/false are accepted so a ~/.cull "sort = true" keeps
// working.
type sortMode string

const (
	sortNone  sortMode = ""
	sortAll   sortMode = "all"
	sortCulls sortMode = "culls"
)

func (m *sortMode) String() string { return string(*m) }
func (m *sortMode) Type() string   { return "all|culls" }
func (m *sortMode) Set(s string) error {
	switch s {
	case "all", "true":
		*m = sortAll
	case "culls":
		*m = sortCulls
	case "", "false":
		*m = sortNone
	default:
		return fmt.Errorf("want all or culls")
	}
	return nil
}

// flags maps the mode onto the pipeline's two placements.
func (m sortMode) flags() (moveCulled, sort bool) { return m == sortCulls, m == sortAll }

func registerSort(f *pflag.FlagSet, m *sortMode, usage string) {
	f.Var(m, "sort", usage)
	f.Lookup("sort").NoOptDefVal = string(sortAll)
}

// sortArgs is cobra.ExactArgs(n) that catches `--sort culls <dir>`: without the "="
// the value is taken as an argument.
func sortArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == n+1 && cmd.Flags().Changed("sort") && (args[0] == "culls" || args[0] == "all") {
			return fmt.Errorf("write --sort=%s (with \"=\"): a bare --sort means all, so %q was read as the folder", args[0], args[0])
		}
		return cobra.ExactArgs(n)(cmd, args)
	}
}
```

- [ ] **Step 6: Use it in judge, decide and review**

`cull.go`:
- Change the field `sort bool` to `sort sortMode`.
- Replace the `--sort` registration with:
  `registerSort(f, &o.sort, "move judged frames (with their .xmp) into folders beside them: --sort or --sort=all into keep/, review/, cull/; --sort=culls only culls into cull/. Undo with 'cull restore'")`
- Keep `--move-culled` registered, then deprecate it:
  `f.MarkDeprecated("move-culled", "use --sort=culls (culls now go into cull/)")`.
- Set `Args: sortArgs(1)`.
- In PreRunE, replace the sort/move-culled conflict check with:

```go
			if o.moveCulled {
				if o.sort == sortAll {
					return fmt.Errorf("--sort and --move-culled can't be combined: --sort already puts culls in cull/")
				}
				o.sort = sortCulls
			}
```

- In RunE, replace `cfg.MoveCulled, cfg.Sort = o.moveCulled, o.sort` with
  `cfg.MoveCulled, cfg.Sort = o.sort.flags()`.
- Replace the `if o.moveCulled || o.sort || o.writeXMP {` label-loading condition with
  `if o.sort != sortNone || o.writeXMP {`.

`decide.go`:
- Change the `sortF bool` variable to `sortF sortMode`.
- Register it with `registerSort(f, &sortF, "sync the folders with the current verdicts (your labels first): --sort or --sort=all keep/, review/, cull/; --sort=culls only cull/. Undo with 'cull restore'")`.
- Deprecate `--move-culled` the same way.
- Set `Args: sortArgs(1)`.
- At the top of RunE:

```go
			if moveC {
				if sortF == sortAll {
					return fmt.Errorf("--sort and --move-culled can't be combined: --sort already puts culls in cull/")
				}
				sortF = sortCulls
			}
			moveCulls, sortAllF := sortF.flags()
```

- Pass `MoveCulled: moveCulls, Sort: sortAllF` to `DecideOptions`.
- Call `describeDecide(sum, sortAllF)`.
- In `describeDecide`, the default branch becomes
  `"; moved %d into cull/, %d back into the shoot folder"`.

`review.go`:
- Change the `sortAfter bool` variable to `sortAfter sortMode`.
- Register it with `registerSort(f, &sortAfter, "when the server stops (Ctrl-C), re-sort the frames by your labels, as decide --sort does (--sort=culls: only culls into cull/); don't use it once the folders are imported")`.
- Set `Args: sortArgs(1)`.
- Every `sortAfter` truth test becomes `sortAfter != sortNone`.
- `serveSheet` keeps its `bool` parameter, called with `sortAfter != sortNone`.
- `sortAfterReview` takes `mode sortMode` and passes `MoveCulled, Sort` from `mode.flags()`.
  Its `describeDecide` gets `mode == sortAll`.

- [ ] **Step 7: Status and restore**

`status.go` `writeStatus`:
- Replace the `sorted, culledDir` counting with `inCull` (folder `cull` or `culled`) and
  `inKeepReview` (folder `keep` or `review`):

```go
	var inCull, inKeepReview int
	for _, r := range rep.Results {
		if r.MovedTo == "" || !exists(r.MovedTo) {
			continue
		}
		switch filepath.Base(filepath.Dir(r.MovedTo)) {
		case pipeline.CullDir, pipeline.CulledDir:
			inCull++
		default:
			inKeepReview++
		}
	}
	where := ""
	if n := inCull + inKeepReview; n > 0 {
		where = fmt.Sprintf(" (%d sorted into folders)", n)
	}
```

- The total becomes `len(files)+inCull+inKeepReview`.
- In the default `next:` case, `move := "--sort"`, or `"--sort=culls"` when
  `inCull > 0 && inKeepReview == 0`. Task 3 drops the `--write-xmp` from that hint.
- Update `status_test.go`'s expectations for a culls-only shoot: `--sort=culls`, and
  "sorted into folders".

`restore.go`:
- Short: `"Move frames that --sort moved back to where they were"`.
- Long: `"(into keep/, review/ or cull/ by --sort, or into the culled folder older versions used)"`.
  It avoids the literal `culled/` and `--move-culled`, which Task 9's help test forbids.

`report.go`: the `MovedTo` comment becomes `// set by --sort; cleared by restore`.

- [ ] **Step 8: Run the tests and check they pass**

Run: `go test ./internal/pipeline ./internal/cli ./internal/report -count=1` (unsandboxed)
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal
git commit -m "One --sort: all (keep/ review/ cull/) or culls (cull/ only); --move-culled deprecated as --sort=culls; old culled/ moves still found and restored"
```

---

## Task 3: Sidecars by default; `--write-xmp` and `--xmp-develop` deprecated

**Files:**
- Modify: `internal/cli/cull.go`, `internal/cli/decide.go`, `internal/cli/tags.go`, `internal/cli/status.go`
- Test: `internal/cli/cli_test.go`, `internal/cli/status_test.go`

**Interfaces:**
- **Consumes:** `setSection`, `secSidecars` (Task 1).
- **Produces:**
  - a `--no-xmp` bool on judge and decide;
  - `--write-xmp` still registered on both, default **true**, deprecated. An explicit
    `--write-xmp=false` (typed or from `~/.cull`) means `--no-xmp`.
  - `--xmp-develop`: deprecated, ignored.

- [ ] **Step 1: Write the failing tests** (`cli_test.go`)

```go
// judge and decide write cull's sidecars by default; --no-xmp writes none.
func TestSidecarsByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)

	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "L1000001.xmp")); err != nil {
		t.Fatal("judge wrote no sidecar by default")
	}

	quiet := t.TempDir()
	tinyDNG(t, filepath.Join(quiet, "L1000001.DNG"))
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", "--no-xmp", quiet); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(quiet, "L1000001.xmp")); err == nil {
		t.Fatal("--no-xmp wrote a sidecar")
	}
	if out, err := run(t, "decide", "--no-xmp", quiet); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(quiet, "L1000001.xmp")); err == nil {
		t.Fatal("decide --no-xmp wrote a sidecar")
	}
	if out, err := run(t, "decide", quiet); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(quiet, "L1000001.xmp")); err != nil {
		t.Fatal("decide wrote no sidecar by default")
	}
}

// A sidecar cull didn't write is never touched by the new default.
func TestDefaultSidecarsLeaveForeignOnes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte(fakeClaudeCull), 0o755)
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	foreign := []byte("<x:xmpmeta>someone else's</x:xmpmeta>")
	os.WriteFile(filepath.Join(dir, "L1000001.xmp"), foreign, 0o644)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run(t, "decide", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "L1000001.xmp")); string(b) != string(foreign) {
		t.Fatalf("foreign sidecar changed:\n%s", b)
	}
}

// The retired sidecar flags still parse, warn, and change nothing harmful.
func TestDeprecatedSidecarFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	for _, flag := range []string{"--write-xmp", "--xmp-develop"} {
		out, err := run(t, "judge", "--estimate", flag, dir)
		if err != nil || !strings.Contains(out, flag+" has been deprecated") {
			t.Errorf("%s: err=%v\n%s", flag, err, out)
		}
	}
}
```

In `TestFlagValidation`:
- Remove the two cases `"xmp-develop without write-xmp"` and `"overwrite-xmp without write-xmp"`.
- Add `"overwrite-xmp with no-xmp": {"judge", "--overwrite-xmp", "--no-xmp", dir}`.

In `TestSortCullsThenRestoreEndToEnd`, drop `"--write-xmp"` from the args. The sidecar is
written by default.

- [ ] **Step 2: Run them and check they fail**

Run: `go test ./internal/cli -run 'SidecarsByDefault|DefaultSidecarsLeave|DeprecatedSidecar|FlagValidation' -count=1`
Expected: FAIL. There's no `--no-xmp` flag yet, and no sidecar is written by default.

- [ ] **Step 3: Implement judge** (`cull.go`)
- Add field `noXMP bool`.
- Register:
  - `f.BoolVar(&o.noXMP, "no-xmp", false, "don't write sidecars (by default cull keeps its own sidecars current: rating, label, keywords; never over ones it didn't write)")`;
  - change `write-xmp`'s default to `true`, with usage `"write sidecars (the default; --no-xmp turns them off)"`, then
    `f.MarkDeprecated("write-xmp", "sidecars are written by default; --no-xmp turns them off")`;
  - `f.MarkDeprecated("xmp-develop", "Capture One ignores Adobe develop settings in sidecars; set exposure and crop with cull apply-c1 --exposure --crop")`.
- Add `"no-xmp"` to the `secSidecars` `setSection` call.
- In PreRunE:
  - delete the two `requires --write-xmp` checks;
  - add `if o.overwriteXMP && (o.noXMP || !o.writeXMP) { return fmt.Errorf("--overwrite-xmp can't be used with --no-xmp") }`.
- In RunE:
  - `cfg.WriteXMP = o.writeXMP && !o.noXMP`;
  - `cfg.XMPDevelop = false` (delete the old assignment);
  - the label-loading condition becomes `if o.sort != sortNone || cfg.WriteXMP {`.

- [ ] **Step 4: Implement decide** (`decide.go`)
- Add `noXMP bool` and register `f.BoolVar(&noXMP, "no-xmp", false, "don't rewrite sidecars (by default decide rewrites cull's own sidecars and creates missing ones)")`.
- `write-xmp`: default `true`, deprecated with the same message as judge.
- `xmp-develop`: deprecated, same message as judge.
- Add `"no-xmp"` to the sections call.
- At the top of RunE, replace the two `requires --write-xmp` checks with:

```go
			write := writeXMP && !noXMP
			if overwrite && !write {
				return fmt.Errorf("--overwrite-xmp can't be used with --no-xmp")
			}
```

- Pass `WriteXMP: write, XMPDevelop: false`.
- Update the Long text: `--write-xmp rewrites…` becomes "Sidecars cull wrote are rewritten
  (and missing ones created) unless --no-xmp; other sidecars are never touched unless
  --overwrite-xmp."

- [ ] **Step 5: Hints**
- `tags.go`: Long says "run 'cull decide <dir>' afterwards to rewrite the sidecars". The
  saved hint is `"saved; rewrite the sidecars with: cull decide %q\n"`.
- `status.go`: the default `next:` becomes `"cull calibrate " + dir + ", then cull decide " + move + " " + dir`.
- `status_test.go`: update the expected strings to match.

- [ ] **Step 6: Run the tests and check they pass**

Run: `go test ./internal/cli ./internal/pipeline -count=1` (unsandboxed)
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal
git commit -m "Sidecars by default on judge and decide (--no-xmp turns them off); --write-xmp and --xmp-develop deprecated"
```

---

## Task 4: judge resumes by default; backend, model and effort come from the report

**Files:**
- Modify: `internal/pipeline/pipeline.go` (`startRun`, the resume guard's messages, `guardOverwrite`, `Run`'s batch-state message)
- Modify: `internal/pipeline/batch.go` (messages), `internal/pipeline/rank_batch.go` (`rerunJudgeBatch`)
- Modify: `internal/cli/backend.go` (`fromReport`), `internal/cli/rank.go` (use `fromReport`)
- Modify: `internal/cli/cull.go` (restructured RunE; `--resume` deprecated), `internal/cli/dotfile.go`, `internal/cli/status.go`
- Test: `internal/pipeline/pipeline_test.go`, `internal/cli/cli_test.go`, `internal/cli/status_test.go`, and the existing batch tests whose expected messages change

**Interfaces:**
- **Produces:** `func (o *backendFlags) fromReport(backendTyped, modelTyped bool, rep *report.Report)`.
- **Behaviour:**
  - judge sets `cfg.Resume = !o.fresh`;
  - `startRun` treats a scan report (`Backend == ""`) as nothing to continue.

- [ ] **Step 1: Write the failing pipeline test** (`pipeline_test.go`)

```go
// With Resume on, a scan report is nothing to continue: judging starts, and the
// scan's tags carry over.
func TestResumeOverAScanReportJudges(t *testing.T) {
	dir := t.TempDir()
	minimalDNG(t, filepath.Join(dir, "L1000001.DNG"))
	s := cfg(dir)
	s.DryRun, s.Tags = true, &report.Tags{Project: "p"}
	if _, _, err := Run(context.Background(), s, nil); err != nil {
		t.Fatal(err)
	}
	c := cfg(dir)
	c.Resume, c.Backend, c.Model = true, "fake", "m"
	b := &fakeBackend{status: "sharp"}
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatalf("resume over a scan report: %v", err)
	}
	if b.calls != 1 || rep.Backend != "fake" || rep.Tags == nil || rep.Tags.Project != "p" {
		t.Fatalf("calls %d, backend %q, tags %+v", b.calls, rep.Backend, rep.Tags)
	}
}
```

- [ ] **Step 2: Write the failing CLI tests** (`cli_test.go`)

```go
// countingClaude is fakeClaudeCull that also appends a line to calls per run.
func countingClaude(t *testing.T) (bin, calls string) {
	t.Helper()
	calls = filepath.Join(t.TempDir(), "calls")
	bin = filepath.Join(t.TempDir(), "claude")
	body := strings.TrimPrefix(fakeClaudeCull, "#!/bin/sh\n")
	os.WriteFile(bin, []byte("#!/bin/sh\necho x >> '"+calls+"'\n"+body), 0o755)
	return bin, calls
}

func lineCount(path string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), "\n")
}

// Running judge again continues the report, on the backend it was judged with,
// without --resume or --backend; a different typed backend is refused with --fresh named.
func TestJudgeResumesByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CULL_CONFIG", "/dev/null")
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin, calls := countingClaude(t)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := lineCount(calls); n != 1 {
		t.Fatalf("first run: %d calls", n)
	}
	out, err := run(t, "judge", "--claude-bin", bin, "--locate", "off", dir)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	if n := lineCount(calls); n != 1 || !strings.Contains(out, "backend: claude-code") || !strings.Contains(out, "1 already done") {
		t.Fatalf("second run made %d calls in total:\n%s", n, out)
	}
	_, err = run(t, "judge", "--backend", "openai", "--model", "m", dir)
	if err == nil || !strings.Contains(err.Error(), "--fresh") {
		t.Fatalf("a different typed backend must be refused naming --fresh: %v", err)
	}
}

// The report's backend wins over ~/.cull (which only replaces built-in defaults).
func TestReportBackendBeatsDotfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin, _ := countingClaude(t)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	dotfile(t, "backend = anthropic\n") // dotfile_test.go's helper
	out, err := run(t, "judge", "--claude-bin", bin, "--locate", "off", dir)
	if err != nil || !strings.Contains(out, "backend: claude-code") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

// --batch on a report judged with another backend says why, rather than a bare
// "anthropic only".
func TestJudgeBatchOnClaudeCodeReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CULL_CONFIG", "/dev/null")
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	bin, _ := countingClaude(t)
	if out, err := run(t, "judge", "--backend", "claude-code", "--claude-bin", bin, "--locate", "off", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	_, err := run(t, "judge", "--batch", dir)
	if err == nil || !strings.Contains(err.Error(), "judged with claude-code") {
		t.Fatalf("got %v", err)
	}
}
```

In `TestFlagValidation`, keep `"fresh with resume"`. It still errors: the flag is
deprecated, but the contradiction check stays.

- [ ] **Step 3: Run them and check they fail**

Run: `go test ./internal/pipeline ./internal/cli -run 'ResumeOverAScan|ResumesByDefault|ReportBackendBeats|BatchOnClaudeCode' -count=1` (unsandboxed)
Expected: all FAIL.
- The second judge run refuses with "holds 1 assessed frame(s)".
- The scan-report resume refuses with a backend mismatch.

- [ ] **Step 4: Implement the pipeline side** (`pipeline.go` `startRun`)

Replace

```go
	if !cfg.Resume {
		if err := guardOverwrite(cfg); err != nil {
			return nil, nil, err
		}
	}
```

with

```go
	resume := cfg.Resume
	if resume && !cfg.DryRun {
		// A scan report holds nothing judged: judging replaces it (its tags carry over
		// below). Tests judge with an empty backend name, so "nothing assessed" decides,
		// not the name alone.
		if prev, err := report.Load(cfg.ReportPath); err == nil && prev.Backend == "" {
			if evaluated, _ := prev.PaidWork(); evaluated == 0 {
				resume = false
			}
		}
	}
	if !resume {
		if err := guardOverwrite(cfg); err != nil {
			return nil, nil, err
		}
	}
```

Then change `if cfg.Resume {` (the block that loads `prev` and checks it) to `if resume {`.
Inside that block:
- Delete the `scan` variable, and the `if prev.Backend == ""` branch of `fix`. They can't
  happen now.
- Schema message: `"run `cull decide %s` (free; it upgrades the report in place), then judge again"`.
- Backend/model mismatch message:

```go
				return nil, nil, fmt.Errorf("%s was judged with backend %q, model %q, escalation %q (schema v%d); "+
					"this run is backend %q, model %q, escalation %q (schema v%d): drop --backend/--model to continue it "+
					"(give the same --escalate-* flags), use --fresh to replace it, or -o for a separate report",
					cfg.ReportPath, prev.Backend, prev.Model, prev.Escalation, prev.SchemaVersion,
					cfg.Backend, cfg.Model, rep.Escalation, report.SchemaVersion)
```

- Effort message ends with `"drop --effort/--locate-effort to continue it with those, use --fresh to replace it, or -o for a separate report"`.

Other message changes:
- `guardOverwrite`, the evaluated>0 message:
  `"%s holds %d assessed frame(s) ($%.2f): judge continues it unless --fresh; use -o for a separate report"`.
- `Run`, the batch-state message:
  `"an unfinished batch run is recorded in %s: finish it with cull judge --batch (or delete that file to start over)"`.
- `batch.go`:
  - every `"rerun with --batch --resume to re-attach"` becomes `"rerun cull judge with --batch to re-attach"`;
  - the `rankBatchStatePath ... && !cfg.Resume` message ends `"rerun cull judge with --batch (without --fresh) to re-attach"`;
  - the comment "Without --resume every frame…" becomes "With --fresh every frame…".
- `rank_batch.go`: `rerunJudgeBatch = "rerun cull judge with --batch to re-attach"`.
  `rerunRankBatch` stays, because the deprecated `cull rank` still uses it.

Then update every test that asserted the old wording. Run
`go test ./internal/pipeline -count=1` and fix each failing `strings.Contains`; there are
about 6 in `batch_test.go`, `rank_batch_test.go` and `pipeline_test.go`.

- [ ] **Step 5: Implement `fromReport`** (`backend.go`, and rank.go's `applyBackendModel` uses it)

```go
// fromReport defaults --backend and --model to what rep was judged with, unless
// typed. A ~/.cull value isn't typed, so the report wins over it, as a stored policy
// does. The model follows the report only while the backend matches it.
func (o *backendFlags) fromReport(backendTyped, modelTyped bool, rep *report.Report) {
	if !backendTyped && rep.Backend != "" {
		o.backend = rep.Backend
	}
	if !modelTyped && rep.Model != "" && o.backend == rep.Backend {
		o.model = rep.Model
	}
}
```

In `rank.go` `applyBackendModel`, replace its first two `if`s with
`o.backendFlags.fromReport(backendChanged, modelChanged, rep)`.

- [ ] **Step 6: Restructure judge's RunE** (`cull.go`)
- Change the `--resume` flag to `f.BoolVar(&o.resume, "resume", false, "continue the existing report (the default now)")`,
  then `f.MarkDeprecated("resume", "judge continues an existing report by default; --fresh replaces it")`.
- Change `--fresh` to `f.BoolVar(&o.fresh, "fresh", false, "replace an existing report that holds assessments (default: continue it)")`.
- In PreRunE, delete the batch/backend check (`if o.batch && o.backend != "anthropic"`); it
  moves into RunE below. Keep `if o.batch && o.second` and the escalation checks.
- Replace the RunE body from its start down to just before `b, auth, err := o.newBackend(cmd)` with:

```go
			cfg, err := so.base(args[0])
			if err != nil {
				return err
			}
			fl := cmd.Flags()
			// The report this run continues (nil: none, or --fresh). A scan report
			// holds nothing judged, so it fixes no backend and no policy.
			var prev *report.Report
			if !o.fresh {
				if r, err := report.Load(cfg.ReportPath); err == nil {
					prev = r
				}
			}
			judged := prev != nil && prev.Backend != ""
			if judged {
				o.backendFlags.fromReport(fl.Changed("backend"), fl.Changed("model"), prev)
				if !fl.Changed("effort") {
					o.effort = prev.Effort
				}
				if !fl.Changed("locate-effort") {
					o.locateEffort = prev.LocateEffort
				}
				if err := o.backendFlags.validate(); err != nil {
					return err
				}
			}
			if o.batch && o.backend != "anthropic" {
				if judged && !fl.Changed("backend") {
					return fmt.Errorf("--batch uses the Message Batches API (anthropic), and %s was judged with %s: use -o for a separate report, or --fresh to replace it", cfg.ReportPath, o.backend)
				}
				return fmt.Errorf("--batch uses the Message Batches API: --backend anthropic only")
			}
			if o.model == "" {
				o.model = backendDefaults[o.backend].model
			}
			cfg.Policy, _ = o.policy.policy() // validated in PreRunE
			if judged { // continuing: keep the policy and grouping its decisions came from
				var notes, seqNotes []string
				if cfg.Policy, notes, err = o.policy.resolve(fl, prev.Policy); err != nil {
					return err
				}
				cfg.Seq, seqNotes = resolveSeq(fl, cfg.Seq, prev.Seq)
				if err := validSeq(cfg.Seq); err != nil {
					return fmt.Errorf("the report's stored grouping: %w", err)
				}
				noteStoredPolicy(cmd.ErrOrStderr(), append(notes, seqNotes...))
			}
			if err := checkPriced(cmd, o.backend, o.model, o.maxCost); err != nil {
				return err
			}
			if o.escalateBackend != "" {
				if err := checkPriced(cmd, o.escalateBackend, o.escalateModel, o.maxCost); err != nil {
					return fmt.Errorf("escalation: %w", err)
				}
			}
			price, priced := llm.PriceFor(o.backend, o.model)
			var escPrice llm.Price
			escPriced := false
			if o.escalateBackend != "" {
				escPrice, escPriced = llm.PriceFor(o.escalateBackend, o.escalateModel)
			}
			if o.estimate || priced || escPriced {
				files, err := pipeline.Discover(cfg.Dir, cfg.Recursive)
				if err != nil {
					return err
				}
				pending := files
				if prev != nil { // only what's left costs anything
					pending = pipeline.Pending(files, prev, cfg.Policy)
					if judged {
						fmt.Fprintf(cmd.ErrOrStderr(), "resume: %d already judged, %d to go\n", len(files)-len(pending), len(pending))
					}
				}
				usd := printEstimate(cmd, len(pending), o.backend, o.model, price, priced, o.batch, !o.noRank, o.rankTwice)
				if escPriced {
					// Only frames the first pass doubts escalate, and which those are isn't
					// known before judging: price the bound, so the question and --max-cost
					// advice aren't blind to it.
					eusd, _, _ := llm.Estimate(len(pending), escPrice, false)
					fmt.Fprintf(cmd.ErrOrStderr(), "escalation to %s/%s ≤ $%.2f at list price, if every frame escalates (only frames the first pass calls %s do)\n",
						o.escalateBackend, o.escalateModel, eusd, o.escalateOnList)
					usd += eusd
				}
				if o.second {
					fmt.Fprintln(cmd.ErrOrStderr(), "second opinions: one more evaluation per soft-or-worse frame, on top of the estimate")
				}
				if o.estimate {
					return nil
				}
				if err := confirmSpend(cmd, usd, o.yes); err != nil {
					return err
				}
			}
```

After `newBackend`:
- delete the old `cfg.Policy, _ = o.policy.policy()` line;
- delete the whole `// Continuing a report: keep the policy…` `if o.resume {…}` block;
- set `cfg.Resume = !o.fresh`.

Change the two stop messages at the end:
- `"stopped at --max-cost; run the same command again (with a higher --max-cost) to continue"`;
- `"stopped early to protect your subscription quota; run the same command later to continue"`.

Update the Long and Example text:
- remove `--resume` from the examples;
- the claude-code backend line ends "…; run judge again later to continue.";
- add a paragraph: "An existing report is continued: frames already judged are skipped,
  and the backend, model and effort default to the report's. --fresh replaces it."

- [ ] **Step 7: Dotfile and status**
- `dotfile.go`: remove `"resume": true` from `notInDotfile`. The dotfile's
  deprecated-flag warning comes in Task 8.
- `status.go`:
  - `judgeBatch` hint: `"cull judge --batch " + dir + "   (re-attaches; already paid for)"`;
  - `unjudged > 0` hint: `"cull judge " + dir`.
- `status_test.go`: update to match.

- [ ] **Step 8: Run the tests and check they pass**

Run: `go test ./internal/... -count=1` (unsandboxed)
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal
git commit -m "judge continues an existing report by default, on the backend, model and effort it was judged with; --resume deprecated, --fresh replaces"
```

---

## Task 5: `judge --rerank`; `cull rank` deprecated; batch ranking goes through judge

**Files:**
- Modify: `internal/pipeline/pipeline.go` (`Config.Rerank`, `finishRun`)
- Modify: `internal/cli/cull.go` (`--rerank`), `internal/cli/rank.go` (`Deprecated`), `internal/cli/status.go`, `internal/cli/dotfile.go`
- Test: `internal/pipeline/rank_test.go`, `internal/pipeline/rank_batch_test.go`, `internal/cli/status_test.go`

**Interfaces:**
- **Produces:**
  - `Config.Rerank bool`: at the end of a run, re-rank every set of two or more rankable
    frames, even ones that already have a model order;
  - `--rerank` on judge.

- [ ] **Step 1: Write the failing pipeline tests**

In `rank_test.go`:

```go
// A judge run over a finished report ranks nothing new; with Rerank it ranks every
// set again.
func TestJudgeRerank(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 3; i++ {
		texturedDNG(t, filepath.Join(dir, fmt.Sprintf("L%07d.DNG", i)))
	}
	c := moveCfg(dir)
	c.MoveCulled, c.WriteXMP, c.Rank = false, false, true
	c.Seq = group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}
	c.Policy.KeepBest, c.Policy.Outranked = 1, eval.ActionReview
	b := &judgeRankBackend{fakeBackend: fakeBackend{status: "sharp"}, rank: rankBackend{order: reverse}}
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	c.Resume = true
	if _, _, err := Run(context.Background(), c, b); err != nil || b.rank.calls != 1 {
		t.Fatalf("plain re-run: err %v, %d rank calls (want 1)", err, b.rank.calls)
	}
	c.Rerank = true
	if _, _, err := Run(context.Background(), c, b); err != nil || b.rank.calls != 2 {
		t.Fatalf("--rerank: err %v, %d rank calls (want 2)", err, b.rank.calls)
	}
}
```

In `rank_batch_test.go`:

```go
// judge --batch over a judged report whose set is unranked ranks it through the
// Message Batches API, and submits no evaluations: what cull rank --batch did.
func TestJudgeBatchRanksAJudgedReport(t *testing.T) {
	c, _ := seqShoot(t, 3) // judged, Rank off: the set is ordered by scores
	p := llm.Price{In: 2, Out: 10}
	c.Price, c.Batch, c.Rank, c.Resume = &p, true, true, true // same (empty) backend and model as seqShoot judged with
	fb := &fakeBatch{}
	rep, _, err := RunBatch(context.Background(), c, fb)
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 1 || len(fb.submitted[0]) != 1 {
		t.Fatalf("want one batch holding one rank request, got %d batches", len(fb.submitted))
	}
	if rep.Sets[0].By != "model" {
		t.Fatalf("set not ranked: %+v", rep.Sets[0])
	}
}
```

If `RunBatch` submits an empty evaluation batch when nothing is pending, this test catches
it (`len(fb.submitted) != 1`). Fix that in `batch.go`: skip `submit` when `reqs` is empty,
after checking whether `prepareRound` or `submit` already does.

- [ ] **Step 2: Run them and check they fail**

Run: `go test ./internal/pipeline -run 'JudgeRerank|JudgeBatchRanksAJudgedReport' -count=1`
Expected: `TestJudgeRerank` fails to compile (`Rerank`). `TestJudgeBatchRanksAJudgedReport`
passes or fails depending on the empty-batch behaviour; record which.

- [ ] **Step 3: Implement**

`pipeline.go`:
- Add to `Config`, after `RankTwice`:
  `Rerank bool // at the end of the run, re-rank every set of two or more rankable frames, even ones with a model order (judge --rerank)`
- In `finishRun`, change `rankSets(ctx, rep, cfg, cfg.rankWith, false, budget)` to
  `rankSets(ctx, rep, cfg, cfg.rankWith, cfg.Rerank, budget)`.

`cull.go`:
- Add field `rerank bool`.
- Register `f.BoolVar(&o.rerank, "rerank", false, "rank every set again, even ones the model already ranked (after re-judging, or adding frames); a policy change alone needs only cull decide, free")`.
- PreRunE: `if o.rerank && o.noRank { return fmt.Errorf("--rerank and --no-rank contradict each other") }`.
- RunE: `cfg.Rerank = o.rerank`.
- `--no-rank`'s usage: `"after judging, don't rank the sets that need it (judge ranks them on its next run); until then each set is ordered by scores"`.

`rank.go`:
- Set `Deprecated: "use 'cull judge <dir>': it ranks the sets that need it at the end of every run (--rerank re-ranks them all)"`.
- Start its Short with "(deprecated) ".

`dotfile.go`: add `"rerank": true` to `notInDotfile`, because it spends money.

`status.go`:
- `rankBatch` hint: `"cull judge --batch " + dir + "   (re-attaches; already paid for)"`;
- `byScores > 0` hint: `fmt.Sprintf("cull judge --estimate %s   (then cull judge: it ranks the %d unranked set(s))", dir, byScores)`.

`status_test.go`: update to match.

- [ ] **Step 4: Run the tests and check they pass**

Run: `go test ./internal/... -count=1` (unsandboxed)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal
git commit -m "judge --rerank re-ranks every set; cull rank deprecated (judge ranks what needs it on every run, batch included)"
```

---

## Task 6: `judge --estimate` prices ranking from the report's real sets

**Files:**
- Modify: `internal/pipeline/rankcalls.go` (`EstimateRanking`, `setCalls`; `callsFor` uses `setCalls`)
- Modify: `internal/cli/cull.go` (`printEstimate`)
- Test: `internal/pipeline/rankcalls_test.go` (create it if absent), `internal/cli/cli_test.go`

**Interfaces:**
- **Consumes:** `Config.Rerank`, `Config.RankTwice`, `chunks`, `maxRankFrames`,
  `cloneForRankCalls`, `decideAll`.
- **Produces:**
  - `type RankEstimate struct { Sets, Calls int; Grouped bool }`;
  - `func EstimateRanking(rep *report.Report, pending []string, cfg Config) RankEstimate`;
  - `printEstimate(cmd, n, backend, model, p, priced, batch bool, rank *pipeline.RankEstimate) float64`.

- [ ] **Step 1: Write the failing tests** (`internal/pipeline/rankcalls_test.go`)

```go
package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/group"
)

// scanShoot scans n identical textured frames: one set, nothing judged.
func scanShoot(t *testing.T, n int) (Config, []string) {
	t.Helper()
	dir := t.TempDir()
	var files []string
	for i := 1; i <= n; i++ {
		f := filepath.Join(dir, fmt.Sprintf("L%07d.DNG", i))
		texturedDNG(t, f)
		files = append(files, f)
	}
	c := moveCfg(dir)
	c.MoveCulled, c.DryRun = false, true
	c.Seq = group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}
	if _, _, err := Run(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
	return c, files
}

func TestEstimateRankingFromScanSets(t *testing.T) {
	c, files := scanShoot(t, 9)
	rep := loadReport(t, c.ReportPath)
	e := EstimateRanking(rep, files, c)
	if !e.Grouped || e.Sets != 1 || e.Calls != 3 { // 9 frames: 2 chunks + a final
		t.Fatalf("got %+v", e)
	}
	if e := EstimateRanking(nil, files, c); e.Grouped || e.Calls != 2 { // no report: ⌈9/8⌉
		t.Fatalf("no report: %+v", e)
	}
	c3, f3 := scanShoot(t, 3)
	c3.RankTwice = true
	if e := EstimateRanking(loadReport(t, c3.ReportPath), f3, c3); e.Calls != 2 { // one call + its reverse
		t.Fatalf("twice: %+v", e)
	}
}
```

`loadReport` is `report.Load` plus a `t.Fatal` on error. Add it to this file if the
package has no such helper.

- [ ] **Step 2: Run it and check it fails**

Run: `go test ./internal/pipeline -run TestEstimateRankingFromScanSets -count=1`
Expected: compile failure (`EstimateRanking`).

- [ ] **Step 3: Implement** (`rankcalls.go`)

```go
// RankEstimate is the ranking a judge run is expected to do.
type RankEstimate struct {
	Sets, Calls int
	Grouped     bool // from the report's own sets (a scan or an earlier judge); false: the every-frame-in-an-8-frame-set guess
}

// EstimateRanking projects, without spending anything, the rank calls a judge run
// makes once pending is judged. With a report holding measured frames it groups them
// as the run will (cfg.Seq, cfg.Policy) and counts, for each set that will need a
// model order, every member that is rankable now or not judged yet: an upper bound,
// since frames judged cull leave their sets. Pending frames the report doesn't hold
// are priced with the old guess, ⌈n/8⌉ calls. cfg.Rerank counts sets that already
// have a model order; cfg.RankTwice adds the reversed call.
func EstimateRanking(rep *report.Report, pending []string, cfg Config) RankEstimate {
	var e RankEstimate
	unknown := len(pending)
	if rep != nil && measured(rep) {
		e.Grouped = true
		cp := cloneForRankCalls(rep)
		decideAll(cp, cfg.Policy, cfg.Seq)
		held := make(map[string]*report.Result, len(cp.Results))
		for i := range cp.Results {
			held[cp.Results[i].File] = &cp.Results[i]
		}
		for _, s := range cp.Sets {
			unjudged := 0
			for _, f := range s.Members {
				if r := held[f]; r != nil && r.Evaluation == nil {
					unjudged++
				}
			}
			n := s.Of + unjudged
			if n < 2 || (unjudged == 0 && s.By == "model" && !cfg.Rerank) {
				continue
			}
			e.Sets++
			e.Calls += setCalls(n, cfg.RankTwice)
		}
		unknown = 0
		for _, f := range pending {
			if held[f] == nil {
				unknown++
			}
		}
	}
	if unknown > 0 {
		calls := (unknown + maxRankFrames - 1) / maxRankFrames
		if cfg.RankTwice {
			calls *= 2
		}
		e.Calls += calls
	}
	return e
}

func measured(rep *report.Report) bool {
	for _, r := range rep.Results {
		if r.Preview != nil {
			return true
		}
	}
	return false
}

// setCalls is the calls ranking one set of n rankable frames takes: its chunks, plus
// a final merging call when there is more than one, or a reversed call with twice.
func setCalls(n int, twice bool) int {
	parts := len(chunks(n))
	if parts > 1 || twice {
		return parts + 1
	}
	return parts
}
```

Rewrite `callsFor`'s loop body to `calls += setCalls(rep.Sets[i].Of, twice)`.

- [ ] **Step 4: Use it in judge** (`cull.go`)

Change `printEstimate`'s last two parameters (`rank, twice bool`) to
`rank *pipeline.RankEstimate`. Its ranking block becomes:

```go
	if rank != nil && rank.Calls > 0 {
		rusd, _, _ := llm.EstimateRank(rank.Calls, p, batch)
		if rank.Grouped {
			fmt.Fprintf(w, "ranking ≈ %d call(s) for %d set(s) already found, $%.2f at %s (at most: frames judged cull leave their sets)\n", rank.Calls, rank.Sets, rusd, rate(batch))
		} else {
			fmt.Fprintf(w, "ranking ≈ %d call(s), $%.2f at %s, if every frame lands in an 8-frame set (pairs cost more per frame; frames in no set cost nothing)\n", rank.Calls, rusd, rate(batch))
		}
		usd += rusd
	}
```

Update its doc comment to match. In RunE, the call becomes:

```go
				var rankEst *pipeline.RankEstimate
				if !o.noRank {
					ecfg := cfg
					ecfg.Rerank, ecfg.RankTwice = o.rerank, o.rankTwice
					e := pipeline.EstimateRanking(prev, pending, ecfg)
					rankEst = &e
				}
				usd := printEstimate(cmd, len(pending), o.backend, o.model, price, priced, o.batch, rankEst)
```

Add a CLI check to `TestCullEstimateNeedsNoKeyAndCallsNothing`:
- after the existing assertions, run `scan` on a fresh dir of 2 `tinyDNG` copies;
- run `judge --estimate` on it;
- assert the output contains `"already found"`.

Two identical `tinyDNG`s form one set at the default `--seq-look`. If they don't, use 3 and
assert only on `"already found"` or on `"set(s)"`.

- [ ] **Step 5: Run the tests and check they pass**

Run: `go test ./internal/pipeline ./internal/cli -count=1` (unsandboxed)
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal
git commit -m "judge --estimate prices ranking from the sets a scan or earlier judge found, not a worst case"
```

---

## Task 7: calibrate's policy grid replaces the sharpness and keep-best sweeps

**Files:**
- Modify: `internal/calib/calib.go` (add `GridRow`, `Grid` and `FormatGrid`; delete
  `SweepRow`, `Sweep`, `FormatSweep`, `KeepBestRow` and `SweepKeepBest`; `FormatSets` loses
  its sweep parameter)
- Modify: `internal/cli/calibrate.go`
- Test: `internal/calib/calib_test.go` (delete the sweep tests, add the grid test), `internal/cli/cli_test.go` (`TestCalibrateCommand`)

**Interfaces:**
- **Produces:**
  - `type GridRow struct { KeepBest int; Outranked, RawClipped eval.Action; Matrix Matrix; Current bool }`;
  - `func Grid(labels map[string]string, base eval.Policy, decide func(eval.Policy) *report.Report) []GridRow`;
  - `func FormatGrid(w io.Writer, rows []GridRow)`;
  - `func FormatSets(w io.Writer, stats SetStats)`.

- [ ] **Step 1: Write the failing test** (`calib_test.go`)

```go
// The grid tries every keep-best × outranked × raw-clipped combination through the
// caller's decide, marks the current one, and stars rows under 1% false culls.
func TestGrid(t *testing.T) {
	labels := map[string]string{"a.DNG": "keep", "b.DNG": "cull"}
	decide := func(p eval.Policy) *report.Report {
		// b is outranked at keep-best 1; a is clipped. Each setting decides one frame.
		b, a := eval.Keep, eval.Keep
		if p.KeepBest < 2 && p.Outranked == eval.ActionCull {
			b = eval.Cull
		}
		if p.RawClipped == eval.ActionReview {
			a = eval.Review
		}
		ev := &eval.Evaluation{}
		return &report.Report{Results: []report.Result{
			{File: "/s/a.DNG", Decision: a, Evaluation: ev},
			{File: "/s/b.DNG", Decision: b, Evaluation: ev},
		}}
	}
	base := eval.Policy{KeepBest: 3, Outranked: eval.ActionReview, RawClipped: eval.ActionReview}
	rows := Grid(labels, base, decide)
	if len(rows) != 20 {
		t.Fatalf("%d rows", len(rows))
	}
	current := 0
	for _, r := range rows {
		if r.Current {
			current++
			if r.KeepBest != 3 || r.Outranked != eval.ActionReview || r.RawClipped != eval.ActionReview {
				t.Fatalf("wrong current row %+v", r)
			}
		}
		caught := r.Matrix.Counts["cull"]["cull"]
		if want := r.KeepBest == 1 && r.Outranked == eval.ActionCull; (caught == 1) != want {
			t.Errorf("row %+v caught %d", r, caught)
		}
	}
	if current != 1 {
		t.Fatalf("%d current rows", current)
	}
	var b strings.Builder
	FormatGrid(&b, rows)
	if !strings.Contains(b.String(), "policy grid") || !strings.Contains(b.String(), "← current") || !strings.Contains(b.String(), "*") {
		t.Fatalf("format:\n%s", b.String())
	}
}
```

Check the `eval.Evaluation` type name with `grep -n "type Evaluation" internal/eval/types.go`.
`compare` only needs `Evaluation != nil`.

- [ ] **Step 2: Run it and check it fails**

Run: `go test ./internal/calib -run TestGrid -count=1`
Expected: compile failure (`Grid`).

- [ ] **Step 3: Implement** (`calib.go`)

Delete `SweepRow`, `Sweep`, `FormatSweep`, `KeepBestRow` and `SweepKeepBest`. Remove the
`sweep` parameter from `FormatSets` and the "keep-best sweep" block inside it. Add:

```go
// GridRow is the agreement under one combination of the three settings that moved
// the first calibration most (2026-10-05): keep-best, outranked and raw-clipped.
type GridRow struct {
	KeepBest              int
	Outranked, RawClipped eval.Action
	Matrix                Matrix
	Current               bool // the policy the report's decisions came from (or the flags given)
}

// Grid re-decides the report under every keep-best 1-5 × outranked review/cull ×
// raw-clipped review/ignore combination, the rest of the policy as base. decide is
// the pipeline's (sets regrouped, stored ranks applied), so each row is what
// 'cull decide' with those flags would give.
func Grid(labels map[string]string, base eval.Policy, decide func(eval.Policy) *report.Report) []GridRow {
	var rows []GridRow
	for k := 1; k <= 5; k++ {
		for _, o := range []eval.Action{eval.ActionReview, eval.ActionCull} {
			for _, rc := range []eval.Action{eval.ActionReview, eval.ActionIgnore} {
				p := base
				p.KeepBest, p.Outranked, p.RawClipped = k, o, rc
				rows = append(rows, GridRow{KeepBest: k, Outranked: o, RawClipped: rc, Matrix: Compare(decide(p), labels),
					Current: k == base.KeepBest && o == base.Outranked && rc == base.RawClipped})
			}
		}
	}
	return rows
}

// FormatGrid writes one line per combination: false culls (you said keep, it
// culled), culls caught, missed culls (you said cull, it kept) and the review rate.
func FormatGrid(w io.Writer, rows []GridRow) {
	fmt.Fprintln(w, "  policy grid (re-decided from stored assessments and ranks; * = false culls under 1%):")
	fmt.Fprintf(w, "      %-9s %-9s %-11s %14s %7s %7s %7s\n", "keep-best", "outranked", "raw-clipped", "false culls", "caught", "missed", "review")
	for _, r := range rows {
		m := r.Matrix
		fc, _, rr := m.Rates()
		mark, cur := " ", ""
		if fc < 0.01 {
			mark = "*"
		}
		if r.Current {
			cur = "  ← current"
		}
		fmt.Fprintf(w, "    %s %-9d %-9s %-11s %6d (%4.1f%%) %7d %7d %6.1f%%%s\n", mark, r.KeepBest, r.Outranked, r.RawClipped,
			m.Counts["keep"]["cull"], 100*fc, m.Counts["cull"]["cull"], m.Counts["cull"]["keep"], 100*rr, cur)
	}
}
```

`calibrate.go`: replace the `FormatSweep` and `FormatSets` lines with

```go
				calib.FormatGrid(w, calib.Grid(verdicts, p, redecide))
				calib.FormatSets(w, calib.Sets(rep, verdicts, p.KeepBest))
```

The Long text replaces "It also re-decides the stored assessments across
--review-below-sharpness values…" and the "keep-best 1..5 sweep" sentence with:

"It also re-decides the stored assessments and ranks under every --keep-best 1-5 ×
--outranked review/cull × --raw-clipped review/ignore combination (the other settings from
the report's policy or your flags), marking rows with false culls under 1%; apply the one
you choose with 'cull decide'. Reports with multi-frame sets get a sets section: how often
a labeled keep was ranked out of the keep-best cut, and how often a labeled cull or review
was ranked into it."

`TestCalibrateCommand`: assert the output contains `"policy grid"` and doesn't contain
`"review-below-sharpness sweep"` or `"keep-best sweep"`.

Delete the tests of the removed functions from `calib_test.go`. Find them with
`grep -n "Sweep" internal/calib/*_test.go`.

- [ ] **Step 4: Run the tests and check they pass**

Run: `go test ./internal/calib ./internal/cli -count=1` (unsandboxed)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal
git commit -m "calibrate: a keep-best × outranked × raw-clipped grid replaces the sharpness and keep-best sweeps"
```

---

## Task 8: Remaining deprecations, deprecated keys in `~/.cull`, and a deprecation table test

**Files:**
- Modify: `internal/cli/root.go` (tag flags on scan/judge), `internal/cli/review.go`
  (`--static`), `internal/cli/importlabels.go` (`Deprecated`), `internal/cli/dotfile.go`
- Test: `internal/cli/deprecations_test.go` (create), `internal/cli/dotfile_test.go`

**Interfaces:**
- **Consumes:** the deprecations from Tasks 2–5.
- **Produces:** `applyDotfile` warns on deprecated keys and still applies them.

- [ ] **Step 1: Write the failing tests**

`internal/cli/deprecations_test.go`:

```go
package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every retired form still runs and says what replaced it.
func TestDeprecations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CULL_CONFIG", "/dev/null")
	dir := t.TempDir()
	tinyDNG(t, filepath.Join(dir, "L1000001.DNG"))
	if out, err := run(t, "scan", dir); err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	export := filepath.Join(t.TempDir(), "labels.jsonl")
	os.WriteFile(export, nil, 0o644)
	cases := []struct {
		args []string
		hint string
	}{
		{[]string{"judge", "--estimate", "--resume", dir}, "--fresh"},
		{[]stringAdd `"os"` to the test file's imports.

In `dotfile_test.go`, use its `dotfile` helper, and `judgedShoot` from `cli_test.go`
(L1 sharp → keep, L2 soft → review, L3 missed_focus → cull; no sidecars yet):

```go
// Deprecated keys warn and still apply: write-xmp = false means no sidecars,
// sort = true means --sort=all, and resume = true changes nothing.
func TestDotfileDeprecatedKeys(t *testing.T) {
	dir := judgedShoot(t)
	dotfile(t, "write-xmp = false\nresume = true\nsort = true\n")
	out, err := run(t, "decide", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "write-xmp: deprecated") {
		t.Errorf("no deprecation warning for write-xmp:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep", "L1.DNG")); err != nil {
		t.Fatalf("sort = true did not sort into keep/ review/ cull/:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep", "L1.xmp")); err == nil {
		t.Fatal("write-xmp = false still wrote a sidecar")
	}
	out, err = run(t, "judge", "--estimate", dir)
	if err != nil || !strings.Contains(out, "resume: deprecated") {
		t.Fatalf("resume key: %v\n%s", err, out)
	}
}
```

ile(export, nil, 0o644)`.

In `dotfile_test.go`, follow the file's existing pattern for writing a dotfile and running
a command:

```go
// Deprecated keys warn and still apply: write-xmp = false means no sidecars, and
// sort = true means --sort=all.
func TestDotfileDeprecatedKeys(t *testing.T) {
	// dotfile: "write-xmp = false\nresume = true\nsort = true\n"
	// run: judge --estimate <empty dir>
	// want: the output names write-xmp and resume as deprecated, err == nil
}
```

Write the body using the helper `dotfile_test.go` already has. Read the file first, then
assert:
- `err == nil`;
- the output contains `"write-xmp"` and `"deprecated"`;
- for `sort = true`: run `decide` on a judged temp shoot (use `fakeClaudeCull`) and assert
  the frame is in `cull/`. The fake culls everything.

- [ ] **Step 2: Run them and check they fail**

Run: `go test ./internal/cli -run 'TestDeprecations|TestDotfileDeprecatedKeys' -count=1` (unsandboxed)
Expected: FAIL. The tag flags on scan/judge, `--static` and `import-labels` aren't
deprecated yet, and the dotfile doesn't warn.

- [ ] **Step 3: Implement**

`root.go`, in the `for _, c := range []*cobra.Command{scan, judge}` loop, after `so.tags.register(c.Flags())`:

```go
		for _, n := range []string{"project", "event", "location", "keyword"} {
			c.Flags().MarkDeprecated(n, "set the shoot's tags with cull offload or cull tag <dir>")
		}
```

`review.go`, after registering `--static`:
`f.MarkDeprecated("static", "the offline page goes in the next release: use cull review (served), which saves every change itself")`.
Remove the `--static` example and the `--static…import-labels` sentence from Long.

`importlabels.go`:
- `Deprecated: "the offline review page goes in the next release, and with it this command: label with cull review (served)"`;
- start Short with "(deprecated) ".

`dotfile.go` `applyDotfile`, right after the `f == nil || f.Changed` check:

```go
			if f.Deprecated != "" {
				fmt.Fprintf(w, "warning: %s: deprecated, %s\n", at, f.Deprecated)
			}
```

The value is still `Set` below. The alias mapping in each RunE (moveCulled → sortCulls,
`write-xmp=false` → no sidecars, resume ignored) does the rest.

- [ ] **Step 4: Run the tests and check they pass**

Run: `go test ./internal/... -count=1` (unsandboxed)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal
git commit -m "Deprecated: tag flags on scan/judge, review --static, import-labels; ~/.cull warns on deprecated keys and still applies them"
```

---

## Task 9: Every cobra text uses the new forms

**Files:**
- Modify: `internal/cli/root.go` (Long), and the `Long`/`Example`/flag usage strings in
  `cull.go`, `decide.go`, `review.go`, `scan.go`, `offload.go`, `status.go`, `tags.go`,
  `restore.go`, `applyc1.go`, `calibrate.go`
- Modify: `internal/pipeline` messages that suggest commands (anything left after Tasks 2–5)
- Test: `internal/cli/help_test.go`

- [ ] **Step 1: Write the failing test** (append to `help_test.go`)

```go
// No help text of a current command teaches a retired form.
func TestHelpTeachesNoRetiredForms(t *testing.T) {
	retired := []string{"--move-culled", "--write-xmp", "--resume", "--xmp-develop", "cull rank", "import-labels", "--static", "culled/"}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Deprecated == "" && !c.Hidden {
			out, err := run(t, append(strings.Fields(strings.TrimPrefix(c.CommandPath(), "cull")), "--help-all")...)
			if err != nil {
				t.Fatalf("%s: %v", c.CommandPath(), err)
			}
			for _, r := range retired {
				if strings.Contains(out, r) {
					t.Errorf("%s --help-all mentions %s", c.CommandPath(), r)
				}
			}
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(NewRootCmd())
}
```

- [ ] **Step 2: Run it and check it fails**

Run: `go test ./internal/cli -run TestHelpTeachesNoRetiredForms -count=1`
Expected: FAIL. The output lists every leftover, which is the worklist for Step 3.

- [ ] **Step 3: Rewrite the texts**

Work through the failures. Specific rewrites:

`root.go` Long:

```
cull extracts the embedded JPEG preview from each DNG, measures it, and asks a
vision model to assess sharpness, exposure, and composition. A deterministic policy
turns those assessments into keep / review / cull decisions.

A shoot, start to finish:
  cull offload <card> <dest> --name "…"   copy and verify the card (then a free scan)
  cull judge <dir>                         the model; re-run it to continue
  cull review <dir>                        you label and rate
  cull decide --sort <dir>                 before import
  cull apply-c1 --run <dir>                after import
cull status <dir> says where a shoot stands and what to run next.

Your own defaults for any flag go in ~/.cull ($CULL_CONFIG to use another file), one
"name = value" per line, with optional [command] sections:

  keep-best = 3
  outranked = cull
  [review]
  sort = all

A flag you type wins, then a shoot's stored policy and backend (decide, judge), then
~/.cull, then the built-in default. Each run prints the values it took from the file;
CULL_CONFIG=/dev/null turns it off for one run. One-off and risky flags (yes, fresh,
force, rerank, run, overwrite-xmp, -q/-v) can't be set there.
```

Elsewhere:
- **judge**:
  - Long: the `--no-rank skips ranking (run it later with 'cull rank')` sentence becomes
    "--no-rank skips ranking until judge runs again; --rerank ranks every set again.".
  - Example:

```
  cull judge ~/Pictures/2026-09-26
  cull judge --backend claude-code ~/Pictures/2026-09-26
  cull judge --backend openai --model <model> ~/Pictures/2026-09-26
  cull judge --estimate ~/Pictures/2026-09-26
  cull judge --sort ~/Pictures/2026-09-26
```

- **decide**:
  - Long: "later runs (decide, calibrate, judge) start from that stored policy".
  - Example:

```
  cull decide --keep-best 3 --outranked cull ~/Pictures/2026-09-26
  cull decide --sort ~/Pictures/2026-09-26
```

- **review**: no `--static` text is left after Task 8.
- **scan**: Long gets "offload runs it for you after copying." Its tags now come from
  offload or tag.
- **status**: all hints are already updated in Tasks 2–5.

- [ ] **Step 4: Run the tests and check they pass**

Run: `go test ./internal/... -count=1` (unsandboxed)
Expected: PASS.

- [ ] **Step 5: Review by eye**

Run: `GOCACHE=$TMPDIR/gocache make build && for c in offload scan judge review decide calibrate apply-c1 restore status tag; do ./bin/cull $c --help; done | less`
Read each one through as a new user would.

- [ ] **Step 6: Commit**

```bash
git add internal
git commit -m "Help texts and hints teach the new forms: judge continues and ranks, --sort, sidecars by default"
```

---

## Task 10: README.md, USAGE.md and CLAUDE.md

**Files:** `README.md`, `USAGE.md`, `CLAUDE.md`

**Old → new forms** (apply everywhere except inside the renames section described below):

| Old | New |
|---|---|
| `judge --resume …` | `judge …` (re-run continues) |
| `--fresh` (explain) | "replace the existing report" |
| `cull rank <dir>` | `cull judge <dir>` (ranks what needs it) |
| `cull rank --force` | `cull judge --rerank` |
| `cull rank --batch` | `cull judge --batch` |
| `--move-culled`, `culled/` | `--sort=culls`, `cull/` |
| `--write-xmp` | (drop; default) / `--no-xmp` to turn off |
| `decide --write-xmp --move-culled` | `decide --sort=culls` |
| `--xmp-develop` | (drop; say Capture One ignores it, use `apply-c1 --exposure --crop`) |
| tag flags on `scan`/`judge` | on `offload`, or `cull tag` |
| `review --static` + `import-labels` | (drop; served review only) |
| `--review-below-sharpness` sweep in calibrate | the policy grid |
| "`--help` lists every flag" | `--help` shows sections; `--help-all` everything |

**Renames section:** README and the usage page each get exactly one section listing every
old → new form above, for v0.1 users:
- README: `## Changed in v0.2.0`, wrapped in `<!-- v0.2.0 renames -->` …
  `<!-- /v0.2.0 renames -->`.
- `site/usage.html`: the same section, in Task 11.

Each row says the old form still works in v0.2.0, with a warning, and goes in the next release.

- [ ] **Step 1: README.md.** Edit, by section (line numbers as of `6a9d6aa`):
  - **Quick start (≈116–146):** the five-command session from the spec, with no
    `--write-xmp` and no `--project` on judge.
  - **Your defaults / Precedence (≈200–264):**
    - the example uses `sort = all`;
    - precedence says "a shoot's stored policy and backend";
    - remove `--resume` from it;
    - the not-allowed list matches `notInDotfile` (with `rerank`, without `resume`).
  - **Keywords and tags (≈271–290):** tags are set on offload or with `cull tag`; sidecars
    pick them up on the next `decide` (by default).
  - **Commands (≈296–313):**
    - drop the `rank` and `import-labels` rows (they appear in the renames section);
    - judge's row: "continues and ranks".
  - **How decisions are made / Sequences and best of set (≈315–427):**
    - ranking happens at the end of every judge run;
    - `--rerank`;
    - remove `cull rank` and `--no-rank → cull rank`.
  - **Reviewing and labelling (≈430–512):** remove `--static` and `import-labels`; the
    calibrate text describes the grid.
  - **Cost control (≈532–569):**
    - "run the same command again" instead of `--resume`;
    - the estimate prices ranking from the sets scan found.
  - **Output: sidecars, Capture One and moving culls (≈571–621):**
    - sidecars by default, `--no-xmp`;
    - `--sort` / `--sort=culls` into `cull/`;
    - old `culled/` is still restored;
    - drop `--xmp-develop`.
  - **Flag reference (≈625–680):** regenerate the tables from `./bin/cull <cmd> --help-all`,
    grouped by the same sections.
  - Add the `## Changed in v0.2.0` section, just before `## Limitations`.
- [ ] **Step 2: USAGE.md.** Go through sections 0–8 and "Things to know":
  - §0: the setup mentions `~/.cull` with `backend = claude-code`.
  - §3:
    - the judge command has no `--write-xmp`;
    - "Ctrl-C, then run the same command" replaces `--resume`;
    - remove the `culled/` mention, or make it `cull/`.
  - §4: remove `--static`.
  - §5: tags via offload or `cull tag`.
  - §6: the calibrate grid.
  - §7: `cull decide --sort` (or `--sort=culls`); sidecars are already there.
  - "Ranking on its own" becomes "Re-ranking": `cull judge --rerank`, and when you'd want it.
- [ ] **Step 3: CLAUDE.md.**
  - Commands block: `./bin/cull judge <dir>` continues; replace the `rank` line with
    `./bin/cull judge --rerank <dir>   # re-rank every set`.
  - Layout:
    - `internal/cli`: `rank` (rank.go, deprecated in v0.2.0) and `import-labels`
      (deprecated); add help.go (sections, `--help-all`) and sortflag.go;
    - `internal/pipeline`: `--sort=culls` puts culls in `cull/`; `culled/` is legacy, read only;
    - `internal/calib`: the policy grid.
  - Invariants: "Only `--sort` (judge, decide, review; `--move-culled` is its deprecated
    alias) moves them … into `keep/` `review/` `cull/`, never overwriting …".
  - First calibration section: leave the measured table as it is (history); add one line
    saying `cull calibrate` now prints this grid.
  - Roadmap item 2: "Next: judge the next shoot with the candidate settings…" is unchanged.
- [ ] **Step 4: Verify with Python** (not grep):

```bash
python3 - <<'EOF'
import re,sys
pats=[r'--resume',r'cull rank\b',r'--move-culled',r'--write-xmp',r'--xmp-develop',r'import-labels',r'--static',r'culled/',r'review-below-sharpness sweep']
bad=0
for f in ['README.md','USAGE.md']:
    s=open(f).read()
    s=re.sub(r'<!-- v0\.2\.0 renames -->.*?<!-- /v0\.2\.0 renames -->','',s,flags=re.S)
    for i,l in enumerate(s.split('\n'),1):
        for p in pats:
            if re.search(p,l): print(f'{f}:{i}: {p}: {l.strip()[:100]}'); bad+=1
print('hits:',bad); sys.exit(1 if bad else 0)
EOF
```

Expected: `hits: 0`. CLAUDE.md is exempt: its verified-facts history keeps the old names.
Check its Commands, Layout and Invariants sections by eye.
- [ ] **Step 5: Commit**

```bash
git add README.md USAGE.md CLAUDE.md
git commit -m "Docs: README, USAGE and CLAUDE.md for v0.2.0 (judge continues and ranks, --sort, sidecars by default, help sections, calibrate grid); a 'Changed in v0.2.0' section"
```

---

## Task 11: Site pages and the review page's text

**Files:** `site/usage.html`, `site/index.html`, `internal/review/page.html`

- [ ] **Step 1: `site/usage.html`.** Edit by anchor (`id=`):
  - **`commands`, the contents table:** remove the `cull rank` and `cull import-labels`
    rows. The `cull judge` row reads "A vision model assesses each frame; Go rules decide the
    verdict; re-run to continue".
  - **`u-judge`, `u-est`:**
    - the command is `cull judge --backend claude-code "$SHOOT"`;
    - "After Ctrl-C, run the same command again";
    - the estimate text says ranking is priced from the sets the scan found.
  - **`u-review`:** nothing about `--static`.
  - **`u-cal`:** the grid (Task 12 records a real calibrate grid only if labels exist;
    otherwise describe it).
  - **`u-dec`, `u-sort`:** `cull decide --sort "$SHOOT"`, sidecars already written;
    `--sort=culls` as the alternative.
  - **`cmd-judge`, `cmd-decide`, `cmd-review`, `cmd-scan`, `cmd-policy`, `cmd-tag`,
    `cmd-status`, `cmd-restore`, `cmd-global`, `cmd-dotfile`:**
    - regenerate each flag list from `./bin/cull <cmd> --help-all`, keeping the page's
      existing markup per flag;
    - add `--help-all` under global;
    - add a line under `cmd-global` explaining the help sections.
  - **`cmd-rank`, `cmd-import-labels`:** replace each section's body with one paragraph:
    "Deprecated in v0.2.0: use … (it still works, with a warning, until the next release)".
    Keep the anchors, so old links land somewhere.
  - Add a `Changed in v0.2.0` section (anchor `changed`), wrapped in the same
    `<!-- v0.2.0 renames -->` markers, at the end of the reference. Link it from the
    contents table's footer.
  - Lines ≈101, 139: remove `cull rank` / `import-labels` from the hero step list and the
    contents.
- [ ] **Step 2: `site/index.html`** line ≈633: drop `--write-xmp` from the delivery copy, and
  say sidecars are written by default.
- [ ] **Step 3: `internal/review/page.html`** line ≈573: remove the text that tells static-page
  users to run `import-labels`, or reword it as "this offline page goes in the next release".
  The export button stays until then. Run `go test ./internal/review -count=1`.
- [ ] **Step 4: Verify with Python** (the same script as Task 10 Step 4), with
  `files=['site/usage.html','site/index.html']`. Exempt the renames markers and the two
  deprecated-command stubs: add `r'id="cmd-rank".*?</section>'` and the import-labels
  equivalent to the stripped patterns.
  Expected: `hits: 0`.
- [ ] **Step 5: Look at it by eye.** Run `python3 -m http.server -d site 8000` in the
  background. Ask the user to open http://127.0.0.1:8000/usage.html (the sandbox blocks
  `open`) and check:
  - the contents table;
  - the judge step;
  - the reference at phone width.
- [ ] **Step 6: Commit**

```bash
git add site internal/review/page.html
git commit -m "Site: usage and overview pages for v0.2.0; rank and import-labels marked deprecated; Changed in v0.2.0 section"
```

---

## Task 12: Re-record the 17-frame session

The replays in `site/site.js` (`SCAN`, `JUDGED`, `REPLAYS`) and the usage page's terminal
blocks are verbatim output from 2026-10-03. Re-record them with the new binary.

**Files:** `site/site.js`, `site/usage.html` (terminal blocks, figures), `site/img/*.png` and
`docs/images/*.png` (only those showing a retired form)

- [ ] **Step 1: Set up** (free):

```bash
GOCACHE=$TMPDIR/gocache make build
cd photos/demo
mv Pictures "Pictures.2026-10-03"     # keep the old recording; photos/ is gitignored
ls LEICA_M/DCIM/*/ | head            # the 17 DNGs the card stand-in holds
```

- [ ] **Step 2: Offload and scan** (free). Capture plain output (a pipe is not a terminal, so
  the live view is off):

```bash
cd photos/demo && ../../bin/cull offload LEICA_M Pictures --name "Forest portraits" --location "Forest Park, Portland" 2>&1 | tee $TMPDIR/rec-offload.txt
SHOOT="Pictures/2025-12-28 Forest portraits"
../../bin/cull judge --estimate "$SHOOT" 2>&1 | tee $TMPDIR/rec-estimate.txt
```

- [ ] **Step 3: Ask the user before judging.** The run uses the claude-code subscription
  quota, about 5 minutes for 17 frames. Ask them, and go on only after they say yes.
- [ ] **Step 4: Judge** (with approval):

```bash
cd photos/demo && ../../bin/cull judge --backend claude-code "$SHOOT" 2>&1 | tee $TMPDIR/rec-judge.txt
../../bin/cull status "$SHOOT" 2>&1 | tee $TMPDIR/rec-status.txt
```

- [ ] **Step 5: Transcribe** into `site/site.js`, following its header comment:
  - verbatim, except that local paths are shortened to `Pictures/…`, and each ranking reason
    is cut after its first sentence plus ` …`;
  - `SCAN` and `JUDGED` take the per-frame lines;
  - `REPLAYS.offload` and `REPLAYS.judge` take the commands (judge with no `--write-xmp`),
    the `next:` line, `results:` and tokens;
  - update the header comment's date to the recording date.
- [ ] **Step 6: Update `site/usage.html`'s terminal blocks and the facts in the prose** to
  match the recording:
  - the estimate output;
  - the run time;
  - the Keep/Review/Cull counts;
  - which frames were culled or sent to review, and why.

  If verdicts changed from 2026-10-03, say what the page now says; don't keep old claims.
- [ ] **Step 7: Screenshots.** Read each PNG in `site/img` and `docs/images` with the Read
  tool, and list the ones that show a retired form or numbers that no longer match. Ask the
  user to recapture those from a terminal running the same commands, at the same window size
  (the width and height are in the `<img>` tags). Leave the rest.
- [ ] **Step 8: Commit**

```bash
git add site docs/images
git commit -m "Site: replays and terminal blocks re-recorded with v0.2.0 on the 17 sample frames"
```

---

## Task 13: Release v0.2.0

- [ ] **Step 1: Full verification** (unsandboxed, alone): `make vet && make test`.
  Expected: no output from vet, every package `ok`.
- [ ] **Step 2: Whole-branch review.** Dispatch a fresh reviewer, using
  superpowers:requesting-code-review, on `main..cli-streamline` against the spec and this
  plan's Review Focus. Fix what it finds and re-run Step 1.
- [ ] **Step 3: Merge and push:**

```bash
git checkout main && git merge --ff-only cli-streamline && git push origin main
```

If the checkout fails on `.claude/settings.json`, run that step unsandboxed (see the
sandbox-go-test memory).
- [ ] **Step 4: Tag and push the tag** (the user approved the v0.2.0 tag on 2026-10-05):

```bash
git tag -a v0.2.0 -m "cull v0.2.0: judge continues and ranks, one --sort, sidecars by default, help in sections, calibrate grid"
git push origin v0.2.0
```

- [ ] **Step 5: Watch the release workflow:** `gh run watch "$(gh run list --workflow release.yml --limit 1 --json databaseId -q '.[0].databaseId')"`.
  Expected: success, which publishes the GitHub release and updates the Homebrew tap. Then
  check:
  - `gh release view v0.2.0` lists the tarball and its .sha256;
  - `gh api repos/jefflaplante/homebrew-tap/contents/Formula/cull.rb -q .content | base64 -d | grep -E 'version|url'`
    names v0.2.0.
- [ ] **Step 6: Release notes.** `--generate-notes` lists commits only. Prepend the "Changed in
  v0.2.0" table with `gh release edit v0.2.0 --notes-file <file>`. Build the file from
  README's renames section plus the generated notes, so the generated part stays.
- [ ] **Step 7: Deploy the site:**

```bash
git push origin "$(git subtree split --prefix site main)":refs/heads/gh-pages --force
```

Ask the user to check that https://code.jefflaplante.com/cull/usage.html serves the new
page (a hard refresh may be needed).
- [ ] **Step 8: Clean up.** Delete the branch (`git branch -d cli-streamline`); the sandbox
  may warn about `.git/config`, which is harmless.

  Update CLAUDE.md if anything learned during the release belongs there, such as a verified
  fact about the tap. Commit and push.
