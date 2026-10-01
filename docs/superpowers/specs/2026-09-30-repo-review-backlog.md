# Repo review backlog (2026-09-30)

A full read-only review of the repo: pipeline and ranking, image/focus parsing, the
CLI and review server, and the LLM backends and prompts. Four parallel reviewers
read the code, and the highest-severity claims were then re-checked by hand.
**Verified** means re-read in the code during this review; **reported** means a
reviewer's reading, not yet re-checked. Line numbers are as of commit `d76f3b0`.

Build health at review time: `go vet` clean; 268 tests pass. The 3 failures need
loopback bind, which the Claude Code sandbox blocks (`sandbox.network.allowLocalBinding`).

**The main point.** The tool has grown faster than its evidence: 44 commits in five
days, but it has run on 17 real frames and has never been calibrated, and verdicts on
borderline frames change between runs. The order below is:
1. fix what can lose money or data;
2. make verdict quality measurable;
3. make labelling fast;
4. calibrate.

New features wait until `calibrate` shows an acceptable false-cull rate.

Plans: section 1 is `docs/superpowers/plans/2026-09-30-safety-and-spend-fixes.md`. The
other sections get their own plans when started.

---

## 1. Fix first: losing data or money

| # | Finding | Where | Status |
|---|---|---|---|
| 1.1 | Re-running `scan` or `judge` without `--resume` silently replaces a report holding paid assessments, rankings and `moved_to` records. After a `--move-culled`, `restore` can then no longer find those frames. | `pipeline.go:406` (`startRun`) | **done** `76929e9` |
| 1.2 | Frames are moved before the report recording the move is saved. A failed save or crash leaves DNGs in `culled/` that `restore` doesn't know about. | `pipeline.go:504-511`, `decide.go` → `redecide` | **done** `08911a4` |
| 1.3 | The "never overwrite" check in `relocate` is stat-then-rename, and macOS `rename(2)` silently replaces an existing destination. | `move.go:94-105` | **done** `08911a4` |
| 1.4 | `apply-c1` finds images by name across the whole Capture One document. Leica file numbers repeat, so verdicts, keywords, exposure and crop land on same-named images from other shoots. | `c1.go:106` (`matchImages`) | **done** `9a3e762` |
| 1.5 | AppleScript injection: the raw file name goes into a `--` comment line, so a name containing a newline becomes live script under `--run`. | `c1.go:61` | **done** `9a3e762` |
| 1.6 | `claude-sonnet-5-5` isn't in the price table, so it's treated as $0, and `--max-cost` never trips. The default model is still `claude-sonnet-5`. | `pricing.go:10`, `cli/backend.go:109` | **done** `a1af81e` |
| 1.7 | `validated` drops both attempts' usage when both fail schema or JSON checks. | `llm.go:113` | **done** `0bf41d6` |
| 1.8 | Resume drops errored results together with their `CostUSD`, so the report understates spend. Batch round 2 drops the round-1 locate usage when the frame can't be prepared again. | `pipeline.go:437`, `batch.go:156` | **done** `0bf41d6` |
| 1.9 | Batch ranking ignores `--max-cost`: the whole ranking goes in one wave, and the budget is only checked before it. | `rank.go:356-371` | **done** `5484ef7` |

Section 1 was closed on branch `fix/safety-and-spend` (2026-09-30), plus the final
review's fixes in `a471928`:
- a failed source removal no longer strands a hard link;
- `--fresh` counts unrecorded moves;
- adoption requires the same size and mtime;
- the resume-mismatch message names `--model`;
- the escalation model is price-checked.

**Deferred minors from that review** (not yet done):
- `apply-c1` lists ambiguous names under "not found" too, and says "not applied" although the unambiguous frames were applied.
- `apply-c1` tries only the frame's current path, not also its pre-move path (an import made before `--move-culled`).
- Resume, plus an unrecorded move, plus `--write-xmp`, can write an orphan sidecar before `moveCulled` reconciles. Fix: reconcile at the top of `finishRun`.
- The overwrite guard ignores spend on a report where every frame errored (`Cost() > 0`, no assessments).
- The re-attach exemption for batch ranking also skips the estimate for newly needed calls.
- A `culled/` DNG deleted by hand leaves `moved_to` set, so `--fresh` stays refused (`restore` can't clear it).
- Test gaps:
  - no test of `guardOverwrite` on a corrupt report;
  - the no-hardlink (exFAT) fallback in `renameNoReplace` is untested.

## 2. Correctness

| # | Finding | Where | Status |
|---|---|---|---|
| 2.1 | `--seq-gap` and `--seq-look` aren't stored with the policy. A later `decide` at the defaults regroups the sets, which can change which frames are best and drop paid rankings. Fix: store them in `Policy` and resolve them like the other settings. | `decide.go:60`, `cli/policy.go` | **done** `a4cf159` |
| 2.2 | Resume keys on the absolute path, and nothing compares `prev.Dir` with `cfg.Dir`. After a folder rename, every frame is re-judged (and re-billed) and appears twice in the report, which switches off labels (`Duplicates`). | `pipeline.go:436-452` | **done** `8ed79ee, 7acee55` |
| 2.3 | Sidecars are written before the checkpoint. After a crash, `r.XMP` is empty on resume and the sidecar is treated as foreign from then on. Batch mode has the same gap. Fix: write in finishRun, or recognise our own sidecars by content. | `stages.go:184-199`, `batch.go:362,441` | **done** `58b1da2` |
| 2.4 | A torn line in the labels log becomes permanent: `Append` doesn't add a missing `\n` first, so the torn line ends up mid-file and `Read` rejects the whole log. | `labels.go:63-67,92` | **done** `484973c` |
| 2.5 | The calibrate sweep ignores set demotion (`DecideFacts` only), so it doesn't match the confusion matrix. Fix: run it through `decideAll` on a copy. | `calib.go:101-104` | **done** `b183cbf` |
| 2.6 | calibrate passes `rep.KeepBest` instead of the resolved `p.KeepBest`, and it skips the `labels.Duplicates` check. | `cli/calibrate.go:61` | **done** `b183cbf` |
| 2.7 | The claude-code backend ignores quota on error results and never checks the 7-day window. | `claudecode.go:96-104,136` | **done** `030c26a` |
| 2.8 | A report from a newer schema is loaded and saved back as v4, dropping unknown fields. Fix: refuse `SchemaVersion > current`. | `report.go:184`, `decide.go:103` | **done** `2fa162e` |
| 2.9 | Batch finish: a crash between `rep.Save` and removing the state file duplicates results on resume. | `batch.go:200-204` | **done** `7ebc9ba` |
| 2.10 | `moveCulled` uses `append`, not `addFixup`, so fixups pile up on each re-run. | `move.go:37` | **done** `08911a4` |
| 2.11 | `apply-c1 --exposure/--crop` overwrite edits made in Capture One. Fix: apply only when the current value is the default. | `c1.go:91,95` | **done** `103cb97` |
| 2.12 | Label changes in review drop the `crs:` develop settings from sidecars (`WriteSidecar(..., false, ...)`). | `serve.go:173` | **done** `b8a63c8` |
| 2.13 | xmp write: the no-clobber check is stat-then-rename, the `.tmp` name is fixed, there's no fsync, and the temp file is left behind if the rename fails. | `xmp.go:44-53` | **done** `58b1da2, 7acee55` |
| 2.14 | The landed-tile noise floor (`fines[len/10]`) collapses to ~0 on blown skies and black backdrops, undoing the noise subtraction. Fix: exclude clipped cells, or estimate noise per luma bin. | `focus/landed.go:66` | **done** `8ef88ff` |

Section 2 was closed on branch `fix/correctness` (2026-09-30), plus the final review's
fixes in `7acee55`:
- judge and rank batch states refuse a renamed folder instead of re-billing;
- a report is rebased only when its folder really moved, not onto another existing folder;
- sidecar `chmod` is best-effort.

**Deferred minors from that review** (not yet done):
- `healTail` races when two processes append to a log with a torn tail. Fix with `flock` if two review servers ever share a folder.
- `xmp.Ours` lets any report rewrite any cull-written sidecar: a second `-o` report, or a cull sidecar edited by hand. Document this in the README.
- `decide --write-xmp` without `--xmp-develop` strips the develop settings a previous decide wrote.
- `apply-c1`'s `isFullFrame` uses a 1-px tolerance in an unverified pixel space. Use a relative tolerance, add the assumption to CLAUDE.md's unverified list, and have `--probe` print one crop/dimensions pair.
- `calibrate` reports a typed bad `--seq-look` as "the report's stored grouping".
- The 7-day stop shares `--quota-stop`. The message and README could say which window tripped.
- The landed noise cutoff (mean ≥ 16) excludes dark-but-noisy backdrops. Consider ~8 once there's a clipped-backdrop sample.
- `xmp.Write` leaves a unique hidden temp file per crash, never cleaned up.

## 3. Verdict quality

**done** `8e07d95`: 1. **Test–retest measure.** `calibrate --compare a.json b.json` reports verdict flips between runs and backends.
**done** `abb37eb`, `ed40dee`, `29501d6`: 2. **Cull only when two signals agree.** Give each sharpness status a score band in the prompt, and cull only when the status and a low score agree; send contradictions to review (`types.go:174`). Escalation should send disagreement to review rather than overwrite (`pipeline.go:266-273`).
**done** `c833551`: 3. **Evidence before score.** Schemas are marshalled from maps, so keys go out alphabetically and sharpness comes last. Emit ordered JSON with sharpness first and the evidence fields before score and status (`anthropic.go:75`, `claudecode.go:44`).
**done** `99a4d08` (opt-in `--second-opinion`): 4. **Second opinion on culls and soft frames only.** Sample the same model again on those (~10–15% of frames) and send disagreement to review.
**done** `b343d2e` (opt-in `--rank-twice`): 5. **Rank position bias.** For sets of 8 or fewer, make a second call in reversed order and trust the top-k only where the two agree (`eval/rank.go:69`).
**done** `2496c53`, `8dcc05f` (advisory; eye vs whole face): 6. **Local eye-level focus check** (advisory). Compare fine/coarse detail at the pupils with the ear, hairline and cheek of the same face. Also:
   - Puploc always runs at 0° (`detect.go:95`), so tilted faces get no pupils.
   - Use a single pupil when only one is found.
**done** `2496c53`, `29501d6`: 7. **Subject face choice.** `faces[0]` is the most confident face, which can be a frontal passer-by; weight by area (`stages.go:82`).

Section 3 was implemented on branch `feat/verdict-quality` (2026-09-30). It is verified
with synthetic fixtures and free `scan` runs only. No live `judge` run has been made, so
the evidence-first ordering, the score bands, `--second-opinion` and `--rank-twice` are
unmeasured until two runs are compared with `calibrate --compare` and labels.

**Deferred minors from that review:**
- Re-deciding an old escalated report now sends disagreeing frames to review. That is
  intended, but it is the one exception to "old reports decide as before": document it.
- With escalation on, the second opinion comes from the primary model re-judging the
  escalated verdict. Either ask the escalation backend or document it.
- A demoted cull keeps "sharpness: missed_focus" as a reason. Add "(not culled: disputed)".
- The `--rank-twice` help should say that sets already ranked need `--force`.
- The "reversed ranking failed" log line is misleading when re-attaching a state that was
  recorded without `--rank-twice`.
- No test covers a disputed place on a frame that is already review.
- `marshalOrdered` descends `[]any` but not `[]map[string]any`. No current schema uses
  the latter.
- The pigo-sample pupil assertion may flake (puploc uses `math/rand`). If it does, loosen
  it to `>= 1`.
- New finding: pigo's puploc is nondeterministic, so `subject_sharpness` varies by about
  ±5% between runs.

## 4. Cost (A/B each with `calibrate` before adopting)

- `output_config.effort: "low"` for locate; A/B low or medium effort for evaluate. Output is ~40% of per-frame cost.
- `--max-edge 1024`: the full frame is ~30% of input tokens and is "for context only".
- Drop `straighten_degrees`: requested, never applied or clamped (`types.go:138`).
- Prompt caching: only ~6% saved. If added, parse the cache token fields and price them.
- `claude-code` backend: record the resolved model from the init event, and scrub `ANTHROPIC_MODEL` and `ANTHROPIC_DEFAULT_SONNET_MODEL`.

## 5. Workflow and UX

- **Review page:**
  - a native-resolution loupe (Z) that opens on the subject and pans;
  - a side-by-side set compare (S) at equal scale, with 1–9 to pick the best;
  - "next unlabelled" and "next set" keys;
  - a rejected save should revert the badge.
- **`cull status <dir>`:** counts, labelling progress, spend, unranked sets, pending batches, and the next step.
- **Ask before spending:** on a terminal, confirm when the estimate is over ~$1; `--yes` skips it.
- **`calibrate` takes a directory:** today, given a directory it looks for labels in the parent folder, with a misleading error.
- **Persistent flags:** only `-o` and `-r` should be global. The prep and seq flags move to scan, judge, rank and decide; today `calibrate`, `restore` and `review` accept them and silently ignore them.
- **Flag consistency:**
  - `decide --overwrite-xmp` without `--write-xmp` is silently accepted.
  - `--labels` is missing on judge and review.
  - `apply-c1 --probe` requires a `<dir>` it ignores.
  - Help text typo in `cli/review.go:38`.
- **Docs:** README and WORKFLOW describe badge positions the wrong way round compared with `page.html:44-45`. K/R/C only advance in the detail view.
- **Static-mode labels:** there's no way to get labels from `--static` mode into the log.

## 6. Hardening

- **Fuzz tests:** for `FindCandidates`, `ReadRaw`, `ReadExif`, `decodeLJPEG` (without the recover), and the XMP render.
  - Latent panic: an SOS segment of length 2 at `ljpeg.go:82`.
  - Property test: `xmp.FromDisplay` against `Frame.storedRect` for orientations 1, 3, 6 and 8.
- **Raw strip size:** reject `offset + count > file size`, and measure raw clip before the full-frame decode to lower peak RSS (`rawclip/raw.go:50`, `dng/raw.go:96`).
- **exiftool:** give it a context and timeout, and use one call instead of four (`dng/preview.go:327`).
- **`claude -p`:** set `cmd.WaitDelay`, and kill the whole process group on cancel.
- **Review server:** `X-Frame-Options: DENY`, `nosniff`, and a SameSite token cookie for `/assets/`.
- **Anthropic retries:** retry 408/409, parse HTTP-date `retry-after`, and cap it.
- **Orientations 2, 4, 5 and 7** are treated as 1. Implement them or record a fixup.
- **Memory:**
  - Alias `Luma` to the Y plane at orientation 1.
  - `pick` should `DecodeConfig` candidates rather than read them all.
  - Run the ±18° pigo passes only when 0° finds nothing confident.
