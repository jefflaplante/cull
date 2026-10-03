# Part 4: Keywords Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** every judged frame carries content keywords from the model, and every frame
carries your project, event, location and extra keywords. All of them go into sidecars
(Capture One builds the hierarchy on import) and into `apply-c1`.

**Architecture:**
- `eval.Evaluation.Keywords` is the last field of the evaluation schema, normalized in Go.
- `report.Report.Tags` is set by `--project`, `--event`, `--location` and `--keyword` on
  offload, scan and judge, and changed later with `cull tag`.
- `xmp.Sidecar.Hierarchy` writes `lr:hierarchicalSubject`.
- `labels.Sidecar` takes the report's tags, and every sidecar writer passes them.
- `apply-c1` applies the same plain words; the review page shows them.

**Tech Stack:** Go stdlib. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-02-preingest-workflow-design.md`, Part 4. The
open item it named is settled: see CLAUDE.md, "Capture One 16.7.2 builds keyword
hierarchies from sidecars".

## Global Constraints

- **The sidecar format is the Adobe convention.**
  - `dc:subject` holds plain words: `cull:<verdict>`, `cull:labeled`, `cull:best` (as
    today), then each content keyword, the project, event and location values, and each
    `--keyword`.
  - `lr:hierarchicalSubject` holds the paths: `content|<kw>`, `project|<v>`, `event|<v>`
    and `location|<v>`.
  - User-tested on Capture One 16.7.2: it nests them, and stars and labels still apply.
- **Content keywords:**
  - 3–8 short lowercase phrases, asked for in the prompt;
  - normalized in Go: trimmed, lowercased, deduped, at most 8, each at most 3 words and
    30 characters;
  - no `|`, no `cull:` prefix, nothing empty.
  - The schema has no `minItems`/`maxItems`, as other constraints are kept out of what
    the APIs accept.
- **Junk frames get no content keywords** (no model call), but do get your tags.
- **Tags:**
  - **Precedence:** tags given on a command replace the stored ones, field by field.
    `--keyword` replaces the whole stored list when given.
  - **No flags means the stored tags stand,** including on `judge --resume` and `decide`.
  - **`cull tag <dir>`** sets fields, with `--clear-project`, `--clear-event`,
    `--clear-location` and `--clear-keywords`. With no flags it prints the stored tags.
    It never touches sidecars: `decide --write-xmp` rewrites them.
  - **No IPTC location fields.**
- **The report's schema version is unchanged.** The new fields are optional; old reports
  load with none.
- **No real judge run without the user's go-ahead** (it spends quota).
- **Running Go:** `GOCACHE=$TMPDIR/gocache`, with the cli tests unsandboxed. Branch
  `feat/keywords`.

## Review Focus

- **A model answer with junk keywords is cleaned:** an empty string, `"Cull:keep"`,
  `"a|b"`, a 12-word sentence, 15 keywords, duplicates differing in case. *Task 1:
  `TestNormalizeKeywords`.*
- **A sidecar is written for the same frame by judge, decide, and the review server:** all
  three carry the tags and the content keywords identically. *Task 3:
  `TestSidecarWritersAgree`.*
- **`cull tag` changes tags, and the next `decide --write-xmp` rewrites every sidecar.**
  *Task 2/3: `TestTagThenDecideRewrites`.*
- **Re-running `judge --resume` without tag flags keeps the stored tags; with
  `--project X`, only the project changes.** *Task 2: `TestTagPrecedence`.*
- **A value with XML metacharacters** (`Smith & Jones <2026>`) is escaped in the sidecar
  and quoted in the AppleScript. *Task 3, Task 4.*

---

### Task 1: Content keywords from the model

**Files:** `internal/eval/prompt.go` (schema: `keywords` after `notes`; prompt
paragraph), `internal/eval/types.go` (`Evaluation.Keywords []string
json:"keywords,omitempty"`, `NormalizeKeywords`), `internal/eval/evaluate.go` (normalize
after decode), tests in `eval_test.go`.

- [ ] **Step 1: Write the failing tests.**
  - `TestEvaluationSchemaHasKeywordsLast`: required ends with `"keywords"`, and the
    property is an array of strings.
  - `TestNormalizeKeywords`: covers the cases in the Review Focus. The result is ≤ 8 and
    in order of first appearance.
  - `TestEvaluateDecodesKeywords`: a fake backend answer with
    `"keywords":["Portrait","forest ","forest","cull:keep"]` gives `[portrait forest]`.
- [ ] **Step 2: Run them.** Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Add a prompt line after section 4: `5. KEYWORDS (description, not evidence): 3-8
    short lowercase keywords a photographer would search a catalog by: subject, setting,
    notable objects, mood (e.g. portrait, woman, forest, red dress, laughing). Write them
    last.`
  - Then `Evaluate`, and the batch decode (find where `Evaluation` is unmarshalled for
    batch results), normalize.
- [ ] **Step 4: Run** `go test ./internal/eval ./internal/pipeline`. Expected: PASS.
- [ ] **Step 5: Commit.** `Model content keywords: schema (last), prompt, normalized in Go`

### Task 2: Tags: storage, flags, `cull tag`

**Files:** `internal/report/report.go` (`Tags` type: `Project, Event, Location string;
Keywords []string`, with `IsZero` and `Keywords() (plain, paths []string)`;
`Report.Tags *Tags json:"tags,omitempty"`), `internal/pipeline/pipeline.go` (`Config.Tags
*report.Tags` and the merge rule at run start: resume carries the stored tags, flags
override), `internal/cli/tags.go` (new: flag registration for offload/scan/judge; `cull
tag` command), `internal/cli/offload.go` (pass to the scan), tests.

**Interfaces (Produces):**

```go
// report
type Tags struct {
	Project  string   `json:"project,omitempty"`
	Event    string   `json:"event,omitempty"`
	Location string   `json:"location,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
}
func (t *Tags) Plain() []string // values for dc:subject: project, event, location, keywords (nil-safe)
func (t *Tags) Paths() []string // project|v, event|v, location|v (nil-safe)
// MergeTags: stored with each non-empty field of flags applied (flags.Keywords non-nil replaces)
func MergeTags(stored, flags *Tags) *Tags
```

- [ ] **Step 1: Write the failing tests.**
  - `TestMergeTags` (report): field by field, keywords replaced only when given, and a
    nil result for empty input.
  - `TestTagPrecedence` (pipeline):
    1. a run with `Tags{Project:"A", Location:"L"}`, then resume with no tags: the report
       keeps both;
    2. a resume with `Project:"B"`: B and L.
  - `TestTagCommand` (cli):
    - `cull tag dir --event Ceremony` on a judged report → the stored event is Ceremony,
      and the project is unchanged;
    - `--clear-location` clears the location;
    - no flags prints the tags.
  - `TestScanTagsFlags` (cli): `scan --project P --keyword a --keyword b` stores them.
- [ ] **Step 2: Run them.** Expected: FAIL.
- [ ] **Step 3: Implement.** `finishRun` stores `rep.Tags`, and `startRun` carries the
  stored tags over on resume. A tag value with `|` is refused at the flag, because it
  would split the hierarchy.
- [ ] **Step 4: Run.** Expected: PASS.
- [ ] **Step 5: Commit.** `Tags: --project/--event/--location/--keyword stored in the report; cull tag`

### Task 3: Sidecars carry keywords and tags

**Files:** `internal/xmp/xmp.go` (`Sidecar.Hierarchy []string` → `lr:hierarchicalSubject`
bag, with the `xmlns:lr` declared), `internal/labels/sidecar.go` (`Sidecar(r, l,
orientation, develop, tags *report.Tags)` and `WriteSidecar(..., tags)`), every call site
(`pipeline/stages.go`, `pipeline/decide.go` via `DecideOptions.Tags`, `review/serve.go`
with the report's tags), tests.

- [ ] **Step 1: Write the failing tests.**
  - `TestRenderHierarchy` (xmp):
    - the bag is present only when there are paths;
    - values are escaped (`Smith & Jones <2026>`);
    - the result parses as XML.
  - `TestSidecarKeywordsAndTags` (labels):
    - `dc:subject` order: `cull:keep`, content keywords, then the tag values;
    - the paths;
    - a junk frame: tags, but no content keywords.
  - `TestSidecarWritersAgree` (pipeline): judge with `--write-xmp` and tags, then `Decide`
    with `WriteXMP` and the same labels. The bytes are identical for a frame whose
    decision didn't change.
  - `TestTagThenDecideRewrites` (pipeline): change the tags in the report, then
    `Decide(WriteXMP)`, and the sidecar holds the new project.
- [ ] **Step 2: Run them.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./...`. Expected: PASS.
- [ ] **Step 5: Commit.** `Sidecars: content keywords and your tags (dc:subject words + lr:hierarchicalSubject paths)`

### Task 4: `apply-c1`, review page, docs

**Files:** `internal/c1/c1.go` (apply content keywords and tag values as plain keywords
via `ensureKeyword`; `Options` gets `Tags`), `internal/cli/applyc1.go` (pass the
report's tags), `internal/review/review.go` and `page.html` (show the keywords and tags in
the detail view), README (Keywords section), CLAUDE.md (layout).

- [ ] **Step 1: Write the failing tests.**
  - `TestScriptAppliesKeywordsAndTags` (c1): the script contains `ensureKeyword(doc,
    "forest")` and `ensureKeyword(doc, "Smith & Jones")`, quoted correctly.
  - Review: the card JSON carries `keywords`, and the page's pure function (if one is
    added) or the template renders them. Follow the existing review tests' pattern.
- [ ] **Step 2: Run them.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./...`. Expected: PASS.
- [ ] **Step 5: Commit.** `apply-c1 and the review page show keywords and tags; docs`

### Task 5: Real check (needs the user's go-ahead)

- [ ] Ask before `cull judge --backend claude-code` on 3–4 sample frames (subscription
  quota, about 2 minutes). With approval: check the keywords are sensible and the token
  delta, then write a sidecar to a copy, ready for the user to import.

### Finish

- [ ] `make vet`, and `go test -count=1 ./...` unsandboxed.
- [ ] Final review (fresh reviewer, most capable model), one fix pass, then the finishing
  menu.
