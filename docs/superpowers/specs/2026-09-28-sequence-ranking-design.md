# Sequences and best-of-set ranking — design

Date: 2026-09-28 · Status: design agreed section by section in chat; this document
awaits the user's review before planning.

## Goal

In a run of similar frames, find the strongest few by what makes a photograph good:
subject sharpness first, then the moment (expression, eyes, gesture), then
composition. Keep those; send the rest to review, marked with their rank. The user
shoots **sequences** (the same subject or scene over tens of seconds to minutes, with
small changes in pose, expression, framing or distance) far more than bursts.

Today's burst grouping fails that case. It needs frames within 2 s and a strict dHash
match, keeps one "best" chosen from per-frame scores the model gave each frame in
isolation, and the model never compares frames.

## Decisions made with the user

- Similar sets are **mostly sequences**, sometimes bursts. One mechanism covers both.
- The frames outside the best few go to **review** (with their rank) by default;
  `cull` can be chosen once calibration shows the ranking is trustworthy.
- "Best" is decided by a **side-by-side model call per sequence** (approach B), not
  by per-frame scores (A) or a pairwise tournament (C).
- The model only ranks; **Go keeps the top N** (`KeepBest`, default 3). This is the
  project invariant that keep/review/cull comes from `eval.Policy`.
- No backwards compatibility: `--burst-gap`, `--burst-hash` and `--duplicates` go.

Non-goals:
- regrouping sets by hand in the sheet (later, if thresholds aren't enough);
- promoting frames;
- per-set keywords (set IDs aren't stable across runs);
- guaranteeing ranking quality on small local models.

## 1. Grouping frames into sequences (`internal/group`)

**Look fingerprint.**
- For every frame, an 8×8 grid of mean RGB (192 bytes) from the display-oriented
  preview, computed where `DHash` is computed today (scan and judge).
- Stored in the report as `look` (base64). It replaces `dhash`, which is removed.

**Look distance.**
1. Divide each grid by its frame's mean luma, which levels exposure differences of
   about ±1 stop.
2. For shifts dx, dy ∈ {-1, 0, 1} (about 12% of the frame), take the mean absolute
   difference over the overlapping cells.
3. The distance is the minimum over those shifts, scaled to [0, 1].

Small reframing or zoom and exposure changes stay close; a different scene or subject
is far apart.

**Linking.**
- Frames are ordered by capture time, then file name. That covers the 1 s EXIF
  resolution; Leica numbering is sequential. Frames without a capture time sort by
  file name among their neighbours.
- A frame joins the previous frame's sequence when both hold:
  - the gap to the previous frame is ≤ `--seq-gap` (default **60 s**). The gap is
    ignored when either frame has no capture time. Rewritten, near-identical times
    therefore leave the look to decide.
  - the look distance to the **previous** frame (not the first) is ≤ `--seq-look`.
- A sequence is capped at **40** frames (`MaxSequence`, replacing `MaxBurst = 15`),
  so a slow pan can't chain a whole walk.

**The `--seq-look` default** is set in the plan's grouping task from the distances
measured on the 17 sample frames. The two known pairs (M1104114/15 and M1104116/17)
must link. The distances and the chosen value are recorded in CLAUDE.md. Timestamps
straight off the card are still needed to confirm `--seq-gap`.

Every frame stays in its set. Frames that failed the technical gate are shown but not
ranked (section 2).

## 2. Ranking a sequence (`internal/eval` rank call, `internal/pipeline`)

**Rankable frames** have an evaluation and a policy decision other than cull; a
missed-focus or motion-blur cull is already decided. A set with at least 2 rankable
frames is ranked. A set with exactly 1 gets rank 1 without a call.

**Input.**
- Frames are labelled "Frame 1…N" in capture order, never by file name.
- Each frame gets:
  - a full frame downscaled to 768 px on the long edge;
  - the subject or face crop `judge` used (its focus-target box), at native preview
    resolution, capped at 512 px.
- The call does not include `judge`'s scores, so the model can't anchor on them.
- Images are re-extracted from the DNGs at rank time.

**Rubric, in priority order:**
1. subject sharpness where it matters (the eyes);
2. eyes and expression (open, engaged, natural; not mid-blink or mid-word);
3. gesture and moment;
4. composition and background (framing, horizon, edge distractions, cropped limbs);
5. exposure only if it can't be fixed. Fixable exposure never counts against a frame.

**Output (JSON Schema).**

```json
{
  "ranking": [{"frame": 3, "strength": "…", "weakness": "…"}],
  "summary": "one sentence on why the top frame wins"
}
```

- `ranking` lists every frame exactly once, best first.
- Go checks that it is a permutation of 1…N, retrying once on a schema mismatch or a
  non-permutation, like the other calls.

**Chunking.**
- One call covers at most **8** frames.
- A larger set is split in capture order into ⌈n/8⌉ nearly equal chunks.
- Each chunk sends max(⌊8/chunks⌋, ⌈KeepBest/chunks⌉) finalists to one final round.
  For 40 frames with KeepBest 3 that is 5 chunks of 8, 1 finalist each, and a final
  round of 5: 6 calls.
- The merged order is the final round's order, then the non-finalists by their chunk
  rank, with ties broken by capture order.

**When it runs.**
- At the end of `cull judge`, after every frame is judged and grouped. `--no-rank`
  skips it.
- With `--batch`, ranking is one more batch round (all chunk calls, then all finals).
- Ranking uses the same backend and model as judge; escalation doesn't apply.
- Ranking calls count toward `--max-cost`. Once the budget is reached, remaining sets
  stay unranked and fall back to scores.

**`cull rank <dir>`** ranks an existing report without re-judging. It takes
`--estimate`, `--max-cost`, `--batch` and `--force` (re-rank all). It computes any
missing `look` from the DNGs where they now live (`moved_to` if moved; free, about 1 s
per frame), so schema-v3 reports such as the current `photos/cull-report.json` work
without paying for `judge` again. `decide` does the same.

**Reuse.**
- A set's stored order is reused while every currently rankable member appears in it.
- Members that dropped out (for example, a frame newly culled by a policy change) are
  simply removed; the relative order of the rest stays valid.
- A new or newly rankable member makes the set unranked until `cull rank`.

**Estimates.**
- `judge --estimate` adds an upper bound for ranking: every frame in 8-frame sets, at
  about 10k input and 1k output tokens per call, about $0.03 per call at Sonnet list
  price, half with `--batch`.
- `rank --estimate` is exact, because the sets are known.

## 3. Verdicts, report and commands

**Policy (`eval.Policy`).**
- `KeepBest int` (default 3; 0 = rank only, no demotion) and `Outranked Action`
  (ignore, review or cull; default review) replace `Duplicates`.
- In each set, a rankable frame whose rank is greater than KeepBest gets the Outranked
  action, with the reason "rank 5 of 7 in set 3 (keeping the best 3)".
- Ranking only demotes: keep can become review or cull; review and cull are untouched;
  nothing is promoted.

**Unranked sets** (`--no-rank`, a failed call, the budget, a scan-only report) are
ordered by the frames' own scores, using the existing `group.better`: sharpness, open
eyes, composition, exposure. The same KeepBest and Outranked apply, with the reason
"… by scores, not compared".

**Labels** still override outputs (sidecars, moves) as today. Ranking changes only the
report's decisions.

**Report schema v4.**
- The result's `dhash` is replaced by `look`.
- The result's `group` becomes:

  ```json
  {"id": 3, "size": 7, "rank": 2, "of": 6, "by": "model", "best": true,
   "strength": "…", "weakness": "…"}
  ```

  - `rank` is 0 when unranked.
  - `of` is the number of rankable frames.
  - `by` is `"model"` or `"scores"`.
  - `best` means rank ≥ 1 and rank ≤ KeepBest, in a set of at least 2 frames.
    A lone rankable survivor of a set is its best. Frames not in any set have no
    `group`.
- The report gains:
  - `keep_best`: the value used at the last decide or judge;
  - `sets`: one entry per set, `{id, members, order, summary, by, usage, cost_usd}`.
    `order` holds the model's ranking of the rankable members at rank time, as file
    names.
- The report's cost total includes the sets' `cost_usd`.
- Resume keeps refusing reports from other schema versions. `decide` and `rank` load
  v3 reports and save them as v4.

**Commands and flags.**
- `judge`: `--keep-best N`, `--outranked ACTION`, `--no-rank`, `--seq-gap DUR`,
  `--seq-look X`.
- `decide`: `--keep-best`, `--outranked`, `--seq-gap`, `--seq-look`. It regroups and
  reapplies, reusing stored orders under the reuse rule; it makes no model calls.
- `rank <dir>`: new (section 2).
- `--burst-gap`, `--burst-hash` and `--duplicates` are removed.

**Sidecars and Capture One.** Frames with `group.best` get the keyword `cull:best`,
both in sidecars (`labels.Sidecar`) and in `apply-c1`.

## 4. The review sheet

- **Card badge:** "set 3 · #2/7" for frames in a multi-frame set, with a distinct
  "best" marker for `group.best`. It must not look like the user's stars. Sets
  alternate a subtle edge tint. There are no header rows, which would break ↑/↓ row
  navigation.
- **Filters:** a third row, **Sets: All / Best / Outranked**, combined with the verdict
  and progress rows. Outranked means rank > KeepBest among rankable frames.
- **Detail view** for a frame in a set:
  - its rank with the model's strength and weakness;
  - the set's summary;
  - a filmstrip of the set's thumbnails, the current one highlighted. Clicking jumps
    to any frame visible under the current filters.
- No new keys. The existing Disagreements filter shows where the user's label differs
  from the model's decision, including ranking.

## 5. Testing and calibration

All tests use synthetic images. Ranking runs against a fake backend; there is no
network.

- **Look:**
  - the same scene shifted about 10%, one stop brighter, or zoomed 5% matches;
  - a different scene, or the same place with a different subject, doesn't.
- **Linking:**
  - the gap and look rules;
  - comparing with the previous frame, not the first;
  - the 40-frame cap;
  - file-name order when times are missing or identical;
  - failed frames stay in the set, unranked.
- **Rank call:**
  - frames are labelled 1…N in capture order, with no file names in the parts;
  - one full frame and one crop per frame;
  - a non-permutation is rejected and retried once;
  - chunk plans and merged orders for 9, 16 and 40 frames, with KeepBest 3 and 5.
- **Policy:**
  - top N stay keep and the rest get the Outranked action;
  - never promoting, never touching review or cull;
  - fallback by scores;
  - `KeepBest 0` means rank only.
- **Reuse:**
  - an unchanged set isn't re-ranked;
  - a dropped member keeps the stored order;
  - a new member makes the set unranked;
  - `--force` re-ranks.
- **Batch and cost:** the ranking batch round, estimates, and ranking stopping at the
  budget.
- **Schema:** v4 round-trips, and a v3 report is upgraded by `decide` and `rank` with
  looks computed.
- **Sidecars:** `cull:best` only for best frames.
- **Page** (jsdom harness outside the repo): set badges, the Sets filter, filmstrip
  jumps.

**Calibration (`calibrate`).** A new sets section, over multi-frame sets containing
labelled frames, reports:
- **your keeps ranked out:** frames labelled keep that aren't `best`. This is the
  number to drive down.
- **your culls and reviews among the best:** frames labelled cull or review that are
  `best`.
- **a `--keep-best` sweep from 1 to 5,** recomputed from stored ranks.

**Live check:** one small paid run on a real shoot with sequences, ideally files
straight off the card, and only with the user's approval.

## Open items

- M11-P capture-time spacing on files straight off the card (the sample's timestamps
  look rewritten). This decides whether `--seq-gap` does anything useful.
- The `--seq-look` default, set from measured distances (section 1).
- How good and how stable the model's ranking is. This is exactly what the calibrate
  sets section measures, and verdicts stay review until it's trusted.
