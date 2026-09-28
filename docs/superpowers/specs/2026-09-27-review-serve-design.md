# Review server, stars, and a JSONL labels log — design

Date: 2026-09-27 · Status: design agreed section by section in chat; this document
awaits the user's review before planning.

## Goal

The review sheet becomes the place the user judges frames, in two stages:

1. **Calibration (now).** Label keep/review/cull to measure the model (`calibrate`).
2. **Culling pass (later, once the model is trusted).** Confirm or override the model's
   verdicts and add star ratings before importing into Capture One. The user's
   judgement then drives sidecars, `--move-culled` and Capture One.

Today labels leave the page only through a manual "Export labels.csv" download that
the user must then point `calibrate` at. After this change every keypress is saved to
disk, with no export step, in any browser.

## Decisions made with the user

- The sheet's role is **both, in stages** (above). The design must serve stage 1 now
  without rework for stage 2.
- **Stars are the user's alone** and independent of keep/review/cull: `xmp:Rating` is
  the user's stars; the verdict is carried by the colour label and a keyword. The
  model never sets stars.
- **Approach A:** `gophotocull review --serve`, a local Go server. File writes and
  the sidecar safety rules stay in Go. (Rejected: the browser folder-access API,
  which is Chromium-only, needs a re-grant each session and would duplicate the
  safety rules in JavaScript. Also rejected: a static page plus an import command,
  which keeps the manual step.)
- Filters combine a **verdict** row with a **progress** row (e.g. Keep + Unrated).
- Labels are an **append-only JSONL log**. **CSV is removed everywhere**, including
  `cull --csv`. No backwards compatibility: no CSV reader is kept.
- Writing into the DNG's own EXIF/XMP is out: "never modify DNGs" stands. Metadata
  goes to `.xmp` sidecars, and to Capture One through `apply-c1`.

Non-goals: writing DNG metadata; a multi-user or remote server; moving or deleting
files from the page (`--move-culled` stays a CLI action); report schema changes (none
needed: labels live outside the report).

## 1. Labels log (`internal/labels`, new)

File: `gophotocull-labels.jsonl` in the report's directory (`labels.DefaultPath(report)`).
One log per shoot directory: labels describe photos, not a particular model run, so
several reports in one directory share it.

Each line holds a frame's **full current state** (not a delta):

```jsonl
{"file":"M1103817.DNG","label":"cull","stars":0,"at":"2026-09-27T20:14:03-07:00"}
{"file":"M1103823.DNG","label":"keep","stars":4,"at":"2026-09-27T20:14:09-07:00"}
```

- `file`: base name (frames are matched by base name, as today). `label`: `keep`,
  `review`, `cull`, or `""` (unlabelled). `stars`: 0–5 (0 = unrated). `at`: when the
  server received it.
- **Fold:** the last line per file wins. A frame whose final state is `""`/0 is absent
  from the folded map.
- **Torn write:** if the file does not end in `\n`, its final line is an interrupted
  append and is skipped. Any other malformed or invalid line is an error naming its
  line number: that is corruption or a bad hand edit, not a crash, and silently
  dropping labels would skew calibration.
- **Append:** one `write` of the line plus `\n` to a file opened `O_APPEND|O_CREATE`,
  then fsync. No rewrite, so the history is kept and a crash cannot lose earlier lines.
- The log grows with every change: tens of KB at 1000 frames. No compaction.

```go
type Entry struct {
	File  string    `json:"file"`
	Label string    `json:"label"` // keep | review | cull | ""
	Stars int       `json:"stars"` // 0-5
	At    time.Time `json:"at"`
}
func (e Entry) Validate() error
func DefaultPath(reportPath string) string
func Append(path string, e Entry) error
func Read(path string) (map[string]Entry, error) // missing file → empty map, nil
```

## 2. `gophotocull review --serve`

```
gophotocull review --serve [--port 0] [--open] [--write-xmp] [--overwrite-xmp] <dir>
```

- Builds the sheet exactly as `review` does (images cached in `gophotocull-review/`),
  then serves it on **127.0.0.1 only**, on a random free port by default. It prints
  the URL, `--open` launches it with macOS `open`, and it runs until Ctrl-C.
- `--write-xmp` updates the frame's sidecar on each change (section 4).
  `--overwrite-xmp` has its usual meaning. Without `--write-xmp` only the log is
  written.
- Plain `review` (no `--serve`) still writes a static, offline `index.html`.

**Endpoints.** The page's images are served from the sheet directory under the same
relative names, so one page works in both modes.

| Method, path | Behaviour |
|---|---|
| `GET /` | the sheet, with `"serve": true` in its embedded data |
| `GET /<image>.jpg` | files from the sheet directory; nothing outside it |
| `GET /api/labels` | folded log: `{"labels": {file: {label, stars}}}` |
| `POST /api/labels` | body `{file, label, stars}`; validate, append, optional sidecar; reply `{"entry": …, "sidecar": "written" \| "off" \| "skipped: <reason>"}` |

**Access control.**
- The listener is bound to 127.0.0.1.
- A random 128-bit token is carried in the URL fragment (`#token=…`), so it never
  appears in request lines, logs or Referer headers. The page sends it as the
  `X-Gophotocull-Token` header on every `/api` call.
- Every request's `Host` must be `127.0.0.1:<port>` or `localhost:<port>`, which
  blocks DNS rebinding. An `Origin` header, when present, must be
  `http://127.0.0.1:<port>` or `http://localhost:<port>`.
- A wrong token returns 403, and a wrong Host or Origin returns 403.

**What it may write:** only the labels log, and the `.xmp` of frames listed in the
report. Never a DNG. A `file` not in the report returns 400. Requests are handled one
at a time behind a mutex.

**Duplicate names.** Labels are keyed by base name. If two frames in the report share
a base name (possible with `-r`), `--serve` refuses to start and lists them: a label
could not say which frame, or which sidecar, it means. Static `review` and
`calibrate` keep today's behaviour, where the names collide (a known deferred minor).

**Recording sidecar ownership.** When the server creates a sidecar, it re-reads the
report, sets only that frame's `xmp` field, and writes the report back atomically
(temp file and rename).
- Known race: running `cull` on the same report while serving can lose one of the two
  writes. Losing an ownership record fails safe: the sidecar is then treated as
  foreign and never overwritten.
- The README says not to run `cull` on a report that is being reviewed. No locking.

## 3. The page

- **Keys:** K / R / C set keep / review / cull and U clears the label (unchanged).
  **1–5 set stars and 0 clears them.** Stars show on cards and in the detail view.
- **Filters:** two rows, combined with AND.
  - **Verdict:** All / Keep / Review / Cull. "Verdict" means the *effective* verdict:
    the user's label if set, else the model's decision.
  - **Progress:** All / Unlabeled / Unrated / Disagreements (label ≠ model decision).
  - Example: Keep + Unrated = keepers not yet starred.
- **Stay on screen:** a frame changed while it is shown stays visible until the user
  navigates away. The filter re-applies on navigation.
- **Auto-advance** (detail view): the key that finishes the frame advances.
  - K / R / C advance, as today.
  - 1–5 advance only while the Unrated progress filter is on. Elsewhere stars stay put,
    so "4 then K" rates and labels in one visit.
- **Saving (serve mode):**
  - Each change is POSTed, and the frame shows "saved" or "not saved".
  - Failed changes queue in localStorage (per report), are counted in the header, and
    are replayed in order after the next successful save or on reload.
  - On load, the server's folded log wins, except for frames with queued changes.
  - A sidecar skip reason (e.g. "sidecar not written: exists and isn't gophotocull's")
    is shown on that frame.
- **Static mode:** labels live in localStorage. Export and import switch to JSONL
  (`gophotocull-labels.jsonl`, same format). The export writes one line per labelled
  or rated frame, with `at` set to the export time. The import folds lines as `Read`
  does and ignores `at`. The CSV export and import are removed.

## 4. Sidecars and Capture One

One mapping for `cull --write-xmp`, `decide --write-xmp` and `review --serve --write-xmp`:

| Field | Value |
|---|---|
| `xmp:Rating` | the user's stars; **omitted when unrated** (the model never sets it) |
| `xmp:Label` | effective verdict: cull **Red**, review **Yellow**, keep **Green** |
| `dc:subject` | `gophotocull:<verdict>`, plus `gophotocull:labeled` when the verdict is the user's |

- `xmp.Sidecar.Rating` becomes optional (0 = omit).
- `cull --write-xmp` has no labels: it writes the model's verdict and no rating.
- `decide --write-xmp` rewrites sidecars the report records as ours to the new mapping,
  replacing the old 3/2/1-star ones. Foreign sidecars are untouched unless
  `--overwrite-xmp`, as today.
- The sidecar is otherwise what `decide` writes today. Develop fields come only with
  `--xmp-develop`.

`apply-c1` follows the same rules:
- It sets a rating **only** where the user gave stars, so it never resets ratings made
  in Capture One.
- Colour tags: cull 1, review 3, keep **4** (green). All three numbers are unverified;
  `--probe` shows them.
- The keyword is the same as in the sidecar.

## 5. Consumers of the log

- **`calibrate --labels`** reads the JSONL log. It defaults to `DefaultPath` of the
  first report. Entries with stars but no label don't count.
- **`decide --labels <log>`** and **`apply-c1 --labels <log>`**: the effective verdict
  (user's label, else the model's) drives sidecars, `--move-culled` (including
  restoring frames whose effective verdict is no longer cull) and Capture One colour
  tags and keywords. Stars drive the rating.
  - The report's `decision` field is **never** replaced by a label: the report stays
    the model's record, or `calibrate` would compare the user with themself.
  - `--labels` is explicit on these commands because they can move files.
- `review --serve` always uses its directory's log.

## 6. Removing CSV

- Page: the CSV export and import go (replaced per section 3).
- `calib.ReadLabels` (CSV) is replaced by `labels.Read`. `calibrate`'s flag text and
  help are updated.
- `cull --csv` and `report.WriteCSV` are removed. The JSON report is the only run
  output; filter it with `jq`.
- `.gitignore`: drop `gophotocull-report.csv`, add `gophotocull-labels.jsonl` and
  `gophotocull-review/`.
- README and CLAUDE.md: remove CSV mentions and document the log and `--serve`.

## 7. Testing

All tests use synthetic fixtures. The server tests use `httptest` (loopback).

- **labels:**
  - last line wins, and clearing works;
  - a torn final line (no trailing `\n`) is skipped;
  - a malformed middle line is an error naming its line;
  - an invalid label or stars value is rejected on Append and on Read;
  - frames are matched by base name;
  - a missing file gives an empty map.
- **calibrate:** reads the log; star-only entries don't count.
- **Sidecar mapping:**
  - unrated frames have no `xmp:Rating`;
  - the colour per verdict is right;
  - `gophotocull:labeled` appears only for the user's verdicts;
  - `decide --write-xmp` rewrites an old 3/2/1-star sidecar that is ours and leaves a
    foreign one alone.
- **Server:**
  - requests without the token, or with a wrong Host or Origin, get 403;
  - an unknown file gets 400;
  - POST appends one line and returns the state, and GET returns the fold;
  - `--write-xmp` creates a missing sidecar and records it in the report;
  - a foreign sidecar is skipped and the reason returned;
  - **DNG bytes are identical before and after** a session;
  - images outside the sheet directory are not served.
- **`decide --labels` / `apply-c1 --labels`:**
  - the effective verdict drives sidecars, `--move-culled` and colour tags;
  - report decisions are unchanged;
  - a rating is set only where there are stars.
- **CLI:** `--csv` is gone; `review --serve` flag validation.
- **Page behaviour** (combined filters, stay-on-screen, auto-advance rules, the
  unsaved queue, JSONL export and import) is checked with a jsdom harness **outside
  the repo**, as for the feature batch. Node stays out of `make test` under the
  dependency rule, so these checks don't rerun automatically.
- **Live:** `review --serve` on the 17 real frames, used by the user.

## Open items and dependencies

- **Capture One reads sidecars for DNGs on import: verified 2026-09-27** by the user.
  The test file was `photos/c1-sidecar-test/M1103817.DNG`, with a gophotocull-written
  sidecar giving 2 stars, a Yellow label and the keyword `gophotocull:review`; the
  stars and colour showed up. So sidecars are the main route into Capture One
  (write them before import), and `apply-c1 --labels` is the route for changes after
  import.
- The Capture One colour-tag number for green (assumed 4) and the other two are
  unverified: `apply-c1 --probe`.
- Page behaviour is verified outside `make test` (above).
