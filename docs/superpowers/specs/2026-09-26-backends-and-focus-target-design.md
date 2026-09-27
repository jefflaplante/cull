# Model backends and focus-target localization — design

Date: 2026-09-26 · Status: approved in conversation, awaiting written-spec review

## Why

Validating previews on 12 real M11-P frames (see CLAUDE.md, "Verified facts") changed
the picture:

- The embedded preview is full resolution (9504×6320), so no raw rendering is needed.
- The detail tiles sent to the model are wrong. Top-k raw Laplacian-variance tiles
  follow contrast, not focus, and landed on the face in 1/12 frames. The model
  would judge focus from out-of-focus foreground, which produces false culls: the
  most expensive error this tool can make.
- The user wants to run evaluations on (a) the Anthropic API, (b) their Claude
  subscription via `claude -p`, and (c) a local model served through an
  OpenAI-compatible API.

## Goals / success criteria

1. Every model call goes through one backend interface. `cull --backend
   anthropic|claude-code|openai` works end to end on the sample frames.
2. For frames with people, the sharpness judgement is made on a native-resolution
   crop of the subject's face or eyes. It is found by pigo when it can be, otherwise
   by a model locate call. On the 12 sample frames, `cull --save-inputs` (locate
   enabled) shows the subject crop on the person in all 12. `scan`, which has no
   locate call, shows it on the 6 frames with a confident face.
3. `scan` (no API) reports preview size stats and face-detection coverage, and can
   dump exactly what the model would see.
4. Invariants in CLAUDE.md hold: decisions stay in `eval.Policy`, keys are never
   logged, DNGs are never modified, tests stay synthetic and offline.

## Non-goals

- A numeric sharpness gate in Go (belongs to calibration, roadmap #2).
- Group-shot logic beyond "highest-Q face" (count is recorded).
- Memory optimization of `imageprep` (measure only; see Risks).
- Message Batches API (roadmap #5).

---

## Part 1 — Model backends

### Package `internal/llm`

```go
type Part struct { // exactly one of Text / JPEG is set
    Text string
    JPEG []byte
}

type Request struct {
    System     string
    Parts      []Part
    SchemaName string         // e.g. "evaluation", "focus_target"
    Schema     map[string]any // JSON Schema; every object has additionalProperties:false
    MaxTokens  int
}

type Usage struct{ InputTokens, OutputTokens int } // moved from eval; eval aliases it

type Quota struct { // claude-code only
    Status             string  // "allowed", ...
    FiveHour, SevenDay float64 // utilization 0..1
}

type Response struct {
    JSON  json.RawMessage // validated against Request.Schema
    Usage Usage
    Quota *Quota
}

type Backend interface {
    Name() string
    Call(ctx context.Context, req Request) (*Response, error)
}

var ErrQuotaStop = errors.New("subscription quota threshold reached")
```

- `Validate(schema, raw) error` is a small in-house validator. It checks `type`,
  `required`, `properties`, `additionalProperties:false`, `enum` and `items`. Every
  backend validates, and on failure makes one fresh retry, then returns an error.
- `Portable(schema)` strips keywords that providers reject (`minimum`, `maximum`,
  `minLength`, `maxLength`). Range enforcement stays in `Policy.Sanitize`, which
  already clamps.
- The eval schema gains `additionalProperties: false` on every object.

### `anthropic` backend (refactor of today's `eval.Client`)

- Stays raw HTTP (dependency rule: stdlib plus cobra).
- Structured output switches from a forced tool call to
  `"output_config": {"format": {"type": "json_schema", "schema": <portable schema>}}`.
  The result is read from the first `text` content block. Reason: forced
  `tool_choice` returns 400 on newer models (Opus 5.5, Fable 5.1). Sonnet 5 runs
  adaptive thinking by default, so `max_tokens` rises from 1024 to 4096.
- `stop_reason` of `refusal` or `max_tokens` is an error for that frame (resumable).
- Retry policy unchanged: 429/529/5xx and connection errors, honouring `retry-after`.
- Key: `config.LoadAPIKey` as today.

### `claude-code` backend (Claude subscription)

Runs one `claude` process per call. Argv (verified on claude 2.1.283, CLAUDE.md):

```
claude -p --input-format stream-json --output-format stream-json --verbose
  --model <model> --tools "" --no-session-persistence --strict-mcp-config
  --setting-sources "" --system-prompt <system> --json-schema <portable schema>
```

- **Environment.** Inherit the environment minus `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `CLAUDECODE` and
  `CLAUDE_CODE_ENTRYPOINT`.
- **Working directory.** A fresh empty temp dir per process, removed afterwards.
- **Stdin.** One stream-json user message: text parts plus base64 `image` blocks.
- **Stdout parsing.**
  - `system/init`: abort the run (not just the frame) unless `apiKeySource == "none"`.
    This guarantees it never silently bills the API.
  - `rate_limit_event`: fills `Quota`.
  - `result`: `is_error`, `structured_output` (then validated) and `usage`. Input
    tokens are the sum of `input_tokens`, `cache_creation_input_tokens` and
    `cache_read_input_tokens`.
- **Quota stop.** `--quota-stop` (default 0.9). After a call, if
  `five_hour ≥ threshold` or `status != "allowed"`, the response is returned
  together with `ErrQuotaStop`. The pipeline records the result, stops dispatching,
  checkpoints, and exits non-zero with a "resume later with --resume" message.
- Per-call timeout of 5 min. `--claude-bin` (default `claude` on PATH). Default
  model `sonnet`, default concurrency 2.

### `openai` backend (any OpenAI-compatible server)

- **Request.** `POST <base-url>/chat/completions`, where `--base-url` defaults to
  `http://127.0.0.1:8000/v1` and `--model` is required.
  - Messages: `system` plus one `user` message of `text` and
    `image_url {url: "data:image/jpeg;base64,..."}` parts.
  - `response_format: {type: "json_schema", json_schema: {name, strict: true,
    schema: <portable>}}`. Verified enforced on a local server.
- **Early stop.** Some local servers keep generating to `max_tokens` after the JSON closes. The
  backend therefore sends `stream: true` and scans the accumulated `delta.content`
  with a string- and escape-aware brace counter. Once the top-level object closes,
  it cancels the request and validates.
  - Usage comes from the stream when present; zero otherwise (local tokens are free).
  - If early cancel turns out not to stop server-side generation, the fallback is
    non-streaming with a tight `max_tokens` (evaluation 1024, locate 256), chosen by
    `--openai-stream=false`.
- Ignores `reasoning_content`.
- **Retries.** 429/5xx and connection errors with backoff; other 4xx are fatal.
- **Key.** Optional: `--openai-key-file`, then `$OPENAI_API_KEY`, then none (via
  `internal/config`, never logged). Sent as `Authorization: Bearer`.
- Default concurrency 4.

### CLI, report, resume

- `cull` flags:
  - `--backend` (default `anthropic`) and `--model`. The model default depends on
    the backend (`claude-sonnet-5` / `sonnet`) and is required for `openai`.
  - `--base-url`, `--openai-key-file`, `--openai-stream`, `--claude-bin`,
    `--quota-stop`.
  - `-j` defaults per backend unless set explicitly.
- The startup line prints the backend, model and key source (never the key).
- The report gains a top-level `backend`. The summary labels tokens by basis:
  API-billed, subscription (not billed), or local.
- `--resume` reuses prior results only when the prior report's `schema_version`,
  `backend` and `model` match. Otherwise it errors: "report was produced by X/Y;
  drop --resume or use -o for a separate report". This keeps calibration
  comparisons from mixing backends.

---

## Part 2 — Focus-target localization

### Flow per frame (`cull`)

1. **Decode and orient** (existing): an oriented RGBA image plus a float32 luma plane.
2. **Detect faces** (`internal/focus`, pigo `core` v1.4.6):
   - Box-downscale luma to a 2000px long edge.
   - `RunCascade` at angles 0, 0.05 and 0.95 (0°, ±18°) with MinSize 24,
     MaxSize long/2, ShiftFactor 0.1 and ScaleFactor 1.1, then
     `ClusterDetections(0.2)`.
   - A face counts if Q ≥ `--face-min-q` (default 80).
   - The target is the highest-Q confident face.
   - The eyes are located with puploc using pigo's standard offsets. The target
     centre is the midpoint of both pupils if both are found, else the face centre
     moved up by 0.1×scale.
   - The cascade files `facefinder` (234 KB) and `puploc` (1.2 MB) are copied into
     `internal/focus/cascade/` with pigo's MIT `LICENSE` and compiled in with
     `go:embed`.
3. **Locate fallback.** Runs only when no confident face is found and `--locate
   model` (the default) is set.
   - `eval.Locate(ctx, backend, frame1024)`: one backend call with a 1024px frame.
   - Prompt: the intended focus target; for people the eye nearer the camera,
     otherwise the main subject's key detail.
   - Schema: `{confident: bool, kind: eye|face|other|none, subject: string,
     box: {left, top, right, bottom}}`. The box is normalized, in display
     orientation.
   - Go validates `0 ≤ left < right ≤ 1` and `0 ≤ top < bottom ≤ 1`.
   - If `confident` is false, `kind` is `none`, the box is invalid, or the call
     errors, there is no subject crop. The reason is recorded and the frame is still
     evaluated.
   - `--locate off` skips the call.
   - Locate tokens are added to the frame's usage. `ErrQuotaStop` propagates.
4. **Subject crop.** A square at native resolution centred on the target, clamped
   inside the frame.
   - For a face: side = clamp(face scale converted to native pixels, 768, 1536).
   - For a model box: side = clamp(1.2 × max box side, 768, 1536).
5. **"Where focus landed" tile(s).**
   - Grid of 384px cells at native resolution.
   - fine = variance of the 4-neighbour Laplacian. coarse = the same after a 4× box
     downsample. ratio = fine / coarse.
   - Only cells with coarse ≥ the 75th percentile are eligible, and cells
     overlapping the subject crop are excluded.
   - Take the top `--tiles` cells (default 1) and expand each to a 768px crop centred
     on the cell.
   - Record `landed_sharpness`, and `subject_sharpness` (the same ratio over the
     subject crop).
6. **Evaluate.**
   - Parts in order:
     - the full frame (1568px);
     - the subject crop, labelled with its source ("face detector" or "model:
       <subject>") and "native resolution";
     - the landed tile(s), labelled "most fine detail relative to local contrast:
       where focus most likely landed";
     - the stats text.
   - Prompt revision:
     - Judge sharpness on the subject crop.
     - If a landed tile is clearly sharper and lies in front of or behind the
       subject, that is `missed_focus`.
     - With no subject crop, judge from the frame and tiles and say so in
       `focus_target`.
     - Drop the raw Laplacian figures from the stats text.

`scan` runs steps 1, 2, 4 and 5 only (no backend, so no locate call).

### `scan` summary and `--save-inputs`

- The summary line reports:
  - preview long edge min/median/max and preview sources;
  - the orientation histogram;
  - faces found N/M.
- `--save-inputs <dir>` (persistent flag, so `scan` and `cull` both have it) writes
  per frame:
  - `<base>.full.jpg`, `<base>.subject.jpg` (if any) and `<base>.landed-<i>.jpg`;
  - `<base>.inputs.json`: labels in send order, boxes in normalized display coords,
    face Q, source, sharpness ratios and the stats text.
  - For `cull` it also holds the locate result.
- The directory is created if missing. The flag refuses the shoot directory itself,
  so output never lands next to the DNGs by accident.

### Report schema v2

```go
type FocusTarget struct {
    Source           string   `json:"source"`          // face | model | none
    Box              *NormBox `json:"box,omitempty"`   // normalized, display orientation
    FaceQ            float64  `json:"face_q,omitempty"`
    Faces            int      `json:"faces"`           // confident faces found
    Label            string   `json:"label,omitempty"` // model's subject description
    Reason           string   `json:"reason,omitempty"` // why none
    SubjectSharpness float64  `json:"subject_sharpness,omitempty"`
    LandedSharpness  float64  `json:"landed_sharpness"`
}
```

- `Result` gains `focus_target`. `Stats` loses `global_sharpness` and
  `peak_tile_sharpness`: both are contrast-driven and misleading.
- `SchemaVersion` becomes 2, and resume ignores v1 reports (see the Part 1 rule).
- The CSV gains source, face_q, subject_sharpness and landed_sharpness columns.

### Code layout

| Package | Change |
|---|---|
| `internal/llm` | new: types, `Validate`, `Portable`, `anthropic.go`, `claudecode.go`, `openai.go` (with the JSON-completion scanner) |
| `internal/focus` | new: `detect.go` (pigo wrapper, embedded cascades), `target.go` (Target, crop geometry), `landed.go` (ratio grid) |
| `internal/imageprep` | split `Prepare` into `Decode` (oriented RGBA + luma), `Measure` (stats), `Downscale`, `EncodeCrop`; delete `sharpestTiles` |
| `internal/eval` | `Evaluate(ctx, llm.Backend, Input)` and `Locate(...)`; prompts and schemas; `Policy` unchanged; `Client` moves to `llm` |
| `internal/pipeline` | orchestrates decode → detect → locate → crops → evaluate → decide; `SaveInputs`; handles `ErrQuotaStop` |
| `internal/report` | v2 fields, backend, CSV columns |
| `internal/config` | `LoadOpenAIKey` (optional key) |
| `internal/cli` | flags above; `scan` summary |

### Dependency

`github.com/esimov/pigo` v1.4.6 (MIT). Justification: face detection is the
difference between judging focus on the subject and judging it on background
(measured 6/12 confident faces; the fallback covers the rest). Only the `core`
package is imported, and it uses stdlib alone. The module's other requirements
(imaging, gg, x/term) appear in `go.sum` but are not compiled in.

---

## Error handling summary

| Failure | Effect |
|---|---|
| pigo panics or errors | treated as "no face"; reason recorded |
| locate call fails or is invalid | no subject crop; reason recorded; frame still evaluated |
| evaluate call fails | frame error, resumable (as today) |
| `ErrQuotaStop` | stop dispatch, keep results, checkpoint, exit non-zero with a resume hint |
| claude-code `apiKeySource != none` | abort the run; that call's output is discarded, earlier results kept |
| schema validation fails twice | frame error, resumable |
| `--resume` with a mismatched backend/model/schema | refuse to start |

## Testing (all offline, synthetic)

- `llm`:
  - `Validate` and `Portable` table tests.
  - JSON-completion scanner (strings containing braces, escapes, trailing
    whitespace).
  - `anthropic` and `openai` against `httptest` servers: happy path, retryable vs
    fatal errors, invalid JSON then retry, SSE streaming with early close.
  - `claude-code` against a fake `claude` shell script on PATH that emits canned
    stream-json: success, `apiKeySource != none`, quota over threshold, `is_error`.
- `focus`:
  - Crop geometry (clamping at edges, size bounds).
  - Landed ratio: a synthetic sharp texture beats the same texture blurred, and a
    high-contrast blurred edge loses to a low-contrast sharp texture. This is the
    exact failure seen on real frames.
  - pigo integration on pigo's own `testdata` image read from the module cache at
    test time (skipped if absent, never committed).
- `eval`: `Locate` and `Evaluate` with a fake backend; box validation.
- `pipeline`:
  - With a fake detector and a fake backend, locate is called only when no face is
    found.
  - `ErrQuotaStop` stops dispatch and checkpoints.
  - The resume mismatch is refused.
  - `--save-inputs` writes the expected files.
- Eval tests keep using `httptest` (loopback bind). CLAUDE.md documents the sandbox
  setting.

## Verification on real frames (after implementation)

1. `scan --save-inputs` on `./photos` (free): check the subject crops by eye; expect
   a face on 6/12.
2. `cull --backend openai --model <local-4b-vision-model>` on `./photos`
   (local, free): end-to-end run, and check the locate fallback on the other 6.
   Confirm early cancel really stops generation (watch the server's load / timing).
3. `cull --backend claude-code` on `./photos` (subscription quota): confirm the
   quota fields and results.
4. `cull --backend anthropic` only with explicit user approval (spends API credits).

## Risks and open questions

- The pigo Q threshold of 80 comes from 12 frames of one shoot. Calibrate it.
- Local VLM throughput for 1000 frames is unknown (could be hours). Measure it in
  verification step 2.
- Peak memory is roughly 850 MB per frame (estimated). Measure with
  `runtime/metrics` in a benchmark and optimize only if `-j` defaults become unsafe.
- Whether heavy scripted `claude -p` use is within the subscription usage policy is
  undocumented. The user accepts this. `--quota-stop` bounds the impact.
- EXIF orientations 3 and 6 are still unseen on real files. The existing unit tests
  cover them synthetically.
