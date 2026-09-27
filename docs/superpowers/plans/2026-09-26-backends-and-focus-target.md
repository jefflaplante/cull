# Model Backends and Focus-Target Localization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route every model call through one backend interface (Anthropic API, `claude -p`
on the user's subscription, OpenAI-compatible). Judge sharpness on a native-resolution
crop of the intended subject, found by pigo or by a model locate call.

**Architecture:** New `internal/llm` (the provider-neutral structured-call interface plus
three backends) and `internal/focus` (pigo face detection, subject-crop geometry, and the
contrast-normalized "where focus landed" tiles). `imageprep` is split into stages.
`pipeline` orchestrates decode → detect → locate → crops → evaluate → decide. `eval` keeps
the prompts, schemas and `Policy`.

**Tech Stack:** Go 1.22, stdlib, cobra, `github.com/esimov/pigo` v1.4.6 (`core` only).

**Spec:** `docs/superpowers/specs/2026-09-26-backends-and-focus-target-design.md`

**Execution note:** the user said "implement". This plan is executed natively in the same
session by the author, so steps name exact interfaces and test assertions and give code
only for non-obvious algorithms. No commits: the user has not asked for any. Work happens
on branch `focus-target`.

## Global Constraints

- Dependencies: stdlib + cobra + `github.com/esimov/pigo` v1.4.6 (MIT, only `core` imported). Nothing else.
- Never print, log, or read API key contents outside `internal/config`.
- Keep/review/cull decided only by `eval.Policy`; the model only assesses.
- Never modify DNGs; never overwrite `.xmp` unless `--overwrite-xmp`.
- Tests: synthetic fixtures, offline; never commit real images.
- Report `SchemaVersion` = 2.
- Subject crop side ∈ [768, 1536] native px (smaller only if the frame is smaller). Landed grid cell 384 px, landed crop 768 px, coarse gate = 75th percentile.
- pigo: 2000 px detection long edge; angles {0, 0.05, 0.95}; MinSize 24; MaxSize long/2; ShiftFactor 0.1; ScaleFactor 1.1; ClusterDetections(0.2); `--face-min-q` default 80.
- Defaults: `--backend anthropic`; model `claude-sonnet-5` / `sonnet` / required for openai; `--base-url http://127.0.0.1:8000/v1`; `-j` 4 / 2 / 4; `--quota-stop 0.9`; `--locate model`; `--tiles 1`; anthropic `max_tokens` 4096; openai non-stream max_tokens eval 1024 / locate 256.
- claude-code argv exactly as in the spec; env minus `ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL CLAUDECODE CLAUDE_CODE_ENTRYPOINT`.

## Review Focus

1. **Frame with no person** (landscape): locate says none. The frame must still be evaluated (subject nil, landed tile only), with `focus_target.source = none` and a reason. It must not error or auto-cull. → test in Task 8.
2. **Subject at the frame edge or frame smaller than 768 px**: the crop is clamped inside the frame and never has negative or out-of-bounds coordinates. → tests in Task 6.
3. **Malformed locate box** (right<left, >1, NaN, zero area): treated as none, no panic. → test in Task 8.
4. **Non-enforcing OpenAI server** wraps JSON in code fences or prose: the scanner still extracts the object; trailing garbage after the object is ignored. → tests in Task 1/3.
5. **Quota stop / abort mid-run**: finished results saved, unfinished files not marked done, `--resume` continues; and `--resume` against a report from a different backend/model/schema is refused. → tests in Task 5/8.

---

### Task 1: `internal/llm` core types, validator, portable schema, JSON scanner

**Files:**
- Create: `internal/llm/llm.go`, `internal/llm/validate.go`, `internal/llm/jsonscan.go`
- Test: `internal/llm/validate_test.go`, `internal/llm/jsonscan_test.go`

**Interfaces (Produces):**
```go
package llm
type Part struct{ Text string; JPEG []byte }
func Text(s string) Part
func JPEG(b []byte) Part
type Request struct {
    System, SchemaName string
    Parts     []Part
    Schema    map[string]any
    MaxTokens int
}
type Usage struct {
    InputTokens  int `json:"input_tokens"`
    OutputTokens int `json:"output_tokens"`
}
func (u *Usage) Add(o Usage)
type Quota struct{ Status string; FiveHour, SevenDay float64 }
type Response struct{ JSON json.RawMessage; Usage Usage; Quota *Quota }
type Backend interface {
    Name() string
    Call(ctx context.Context, req Request) (*Response, error)
}
var ErrQuotaStop = errors.New("subscription quota threshold reached")
var ErrAbortRun  = errors.New("aborting run")
func Validate(schema map[string]any, raw []byte) error
func Portable(schema map[string]any) map[string]any    // deep copy minus minimum/maximum/minLength/maxLength
func FirstJSONObject(s string) (obj string, ok bool)   // first complete top-level {...}
func validated(ctx context.Context, schema map[string]any, attempt func(context.Context) (*Response, error)) (*Response, error)
func backoff(attempt int, retryAfter time.Duration) time.Duration // moved from eval
```

- [ ] **Step 1: Write failing tests.**
  - `Validate` tables:
    - valid eval-like object passes;
    - missing required → error naming the key;
    - wrong type (string for number) → error;
    - value not in enum → error;
    - extra key when `additionalProperties:false` → error;
    - array items validated;
    - `integer` rejects 1.5 and accepts 2;
    - nested object path appears in the error (`sharpness.status`).
  - `Portable`: strips the keys at every depth, leaves the original unmodified.
  - `FirstJSONObject` table:
    - `{"a":1}` → ok;
    - "```json\n{\"a\":{\"b\":\"}\"}}\n```" → `{"a":{"b":"}"}}`;
    - `prefix {"s":"\"{"} tail}` → `{"s":"\"{"}`;
    - `{"a":` → not ok;
    - `[]` → not ok;
    - `{"a":1}   \n\n` → ok.
  - `validated`: first attempt invalid, second valid → returns the second, Usage is the sum of both; two invalid → error mentions "schema".
- [ ] **Step 2:** `go test ./internal/llm/` → FAIL (undefined).
- [ ] **Step 3: Implement.** Scanner: skip to the first `{`, track depth; inside a string, handle `\` escapes and the closing `"`. Return at depth 0.
  Validator: recursive over `map[string]any`. `type` may be a string. Numbers are `float64` after `json.Unmarshal` into `any`.
- [ ] **Step 4:** `go test ./internal/llm/` → PASS.

### Task 2: `anthropic` backend; `eval.Evaluate` on `llm.Backend`

**Files:**
- Create: `internal/llm/anthropic.go`, `internal/llm/anthropic_test.go`, `internal/eval/evaluate.go`
- Modify: `internal/eval/prompt.go` (schema: `additionalProperties:false` everywhere), `internal/eval/client.go` (delete), `internal/eval/eval_test.go` (fake-backend tests replace httptest), `internal/eval/types.go` (`type Usage = llm.Usage`)
- Modify: `internal/pipeline/pipeline.go` (`Run(ctx, cfg, b llm.Backend)`; remove `Evaluator`), `internal/pipeline/pipeline_test.go`, `internal/cli/cull.go` (construct `llm.NewAnthropic`)

**Interfaces:**
- Consumes: Task 1.
- Produces:
```go
type Anthropic struct {
    APIKey, Model, Endpoint string
    MaxRetries int
    HTTP       *http.Client
}
func NewAnthropic(apiKey, model string) *Anthropic // Endpoint https://api.anthropic.com/v1/messages, 5 retries, 180 s timeout
// eval
type Labeled struct{ Label string; JPEG []byte }
type Input struct {
    Filename    string
    FullFrame   []byte
    Subject     *Labeled
    Landed      []Labeled
    StatsText   string
    MinCropArea float64
}
func Evaluate(ctx context.Context, b llm.Backend, in Input) (*Evaluation, llm.Usage, error)
func EvalRequest(in Input) llm.Request // exported for save-inputs labels
```

- [ ] **Step 1: Failing tests.**
  - The anthropic httptest server asserts:
    - headers `x-api-key` and `anthropic-version`;
    - body `output_config.format.type == "json_schema"`;
    - no `minimum` anywhere in the body schema;
    - no `tool_choice`, no `temperature`;
    - `max_tokens == 4096`;
    - image block `source.media_type == image/jpeg`.
  - First reply 529 + `retry-after: 0`, then 200 with a text block containing valid JSON → parsed; `Usage` matches.
  - `stop_reason: refusal` → error containing "refus"; `max_tokens` → error containing "max_tokens"; a 400 is not retried (1 call).
  - `eval.Evaluate` with a fake backend: parts order is frame, subject label+image, landed label+image, stats. The returned JSON decodes into `Evaluation`.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.**
  - Body shape:
    - `{"model","max_tokens","system","messages":[{"role":"user","content":[...]}],"output_config":{"format":{"type":"json_schema","schema":Portable(schema)}}}`.
    - Image block: `{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":...}}`.
  - Wrap the call in `validated`.
  - Pipeline: temporary wiring `eval.Evaluate(ctx, b, input)` with the Subject nil and the existing tiles as Landed, until Task 8.
- [ ] **Step 4:** `go test ./...` (unsandboxed for httptest) → PASS; `go vet ./...` clean.

### Task 3: `openai` backend

**Files:**
- Create: `internal/llm/openai.go`, `internal/llm/openai_test.go`

**Interfaces (Produces):**
```go
type OpenAI struct {
    BaseURL, APIKey, Model string
    Stream     bool
    MaxRetries int
    HTTP       *http.Client
}
func NewOpenAI(baseURL, apiKey, model string) *OpenAI // Stream true, 5 retries, 600 s timeout
```

- [ ] **Step 1: Failing tests (httptest).**
  - Request checks: path `/chat/completions`; `Authorization: Bearer k` only when a key is set; `response_format.type == json_schema` and `.json_schema.strict == true`; system message first; user content has an `image_url` with a `data:image/jpeg;base64,` prefix; `temperature == 0`.
  - Stream mode:
    - SSE chunks split the JSON across 3 deltas, followed by 50 more whitespace deltas before `[DONE]`. The client returns after the object closes, and the server handler observes `r.Context().Done()` (the connection closed early).
    - Usage chunk honoured when present.
  - Non-stream mode: `choices[0].message.content` = "```json\n{...}\n```" → extracted.
  - 429 then 200 → retried; 401 → not retried.
  - Stream ends without a complete object → error "incomplete JSON".
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.**
  - Stream: `bufio.Scanner` over the body with a large buffer. Lines `data: ` → `[DONE]` or a chunk; append `choices[0].delta.content`. After each append, `FirstJSONObject(acc)` → ok → `cancel()`, close the body, return.
  - Set `stream_options: {"include_usage": true}`.
  - `max_tokens`: stream → `req.MaxTokens`; non-stream → `req.MaxTokens` (the caller passes the tight values).
- [ ] **Step 4:** run → PASS.

### Task 4: `claude-code` backend

**Files:**
- Create: `internal/llm/claudecode.go`, `internal/llm/claudecode_test.go`

**Interfaces (Produces):**
```go
type ClaudeCode struct {
    Bin, Model string
    QuotaStop  float64
    Timeout    time.Duration
}
func NewClaudeCode(bin, model string, quotaStop float64) *ClaudeCode // Timeout 5 min
```

- [ ] **Step 1: Failing tests.** A fake `claude` shell script in `t.TempDir()` records `$@` → args.txt, `env` → env.txt, stdin → stdin.json, `pwd` → pwd.txt, then `cat $FAKE_OUT`.
  - Cases:
    - (a) success: init `apiKeySource:"none"`, rate_limit_event (five_hour 0.2), result with `structured_output` → Response.JSON is valid, Quota.FiveHour == 0.2, usage input = sum of 3 fields.
    - (b) args contain exactly the spec flags; `--tools` followed by an empty arg; `--json-schema` value has no `minimum`.
    - (c) env.txt lacks `ANTHROPIC_API_KEY` and `CLAUDECODE` even when the test sets them.
    - (d) pwd is not the test's cwd and is removed afterwards.
    - (e) `apiKeySource:"ANTHROPIC_API_KEY"` → `errors.Is(err, ErrAbortRun)`.
    - (f) five_hour 0.95 with QuotaStop 0.9 → Response non-nil AND `errors.Is(err, ErrQuotaStop)`.
    - (g) `is_error:true` → error containing the result text.
    - (h) stdin.json is one line with `type:user` and an image block.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.**
  - `exec.CommandContext` with a timeout ctx; `cmd.Dir = os.MkdirTemp`, `defer os.RemoveAll`; `cmd.Env` filtered from `os.Environ()`.
  - Stdout decoded line by line into `map[string]any` by `type`.
  - Wrap in `validated`, except that `ErrQuotaStop` is returned alongside a successful Response, after validation.
- [ ] **Step 4:** run → PASS.

### Task 5: Config, CLI backend selection, report backend, resume guard

**Files:**
- Modify: `internal/config/apikey.go` (+`LoadOpenAIKey`; `readKeyFile(p, anthropic bool)`), `internal/report/report.go` (`Backend string json:"backend"`; `SchemaVersion = 2`), `internal/pipeline/pipeline.go` (`Config.Backend`; resume guard; stop-dispatch on `ErrQuotaStop`/`ErrAbortRun`), `internal/cli/cull.go`, `internal/cli/root.go`
- Test: `internal/config/apikey_test.go`, `internal/cli/cli_test.go`, `internal/pipeline/pipeline_test.go`

**Interfaces (Produces):**
```go
func LoadOpenAIKey(explicitFile string) (key, source string, warnings []string, err error) // "", "none", nil, nil when absent
// pipeline
type Config struct { /* existing */ Backend string; LandedTiles int; FaceMinQ float64; Locate bool; SaveInputs string }
func Run(ctx context.Context, cfg Config, b llm.Backend) (*report.Report, llm.Usage, error)
```

- [ ] **Step 1: Failing tests.**
  - config: none / env / file; no sk-ant warning for openai.
  - cli:
    - `--backend bogus` → error;
    - `--backend openai` without `--model` → error "requires --model";
    - `--quota-stop 0` → error;
    - `--locate maybe` → error;
    - `--backend claude-code --claude-bin /nonexistent` → error "not found";
    - `--backend openai --model m` with no key → no key error (proceeds; empty dir → "0 DNGs found").
  - pipeline:
    - Resume against a report whose backend differs → error containing "drop --resume".
    - A fake backend returns `ErrQuotaStop` on call 2 of 4 with `-j 1` → returned err `Is ErrQuotaStop`; the report on disk has 2 successful results; a second `Run` with Resume processes the remaining 2.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.**
  - Stop dispatch via a `stop` channel closed once (`sync.Once`). In-flight work finishes and is recorded.
  - For `ErrQuotaStop` the frame result is kept. For `ErrAbortRun` the frame is recorded as an error.
  - `-j` default 0 = per-backend auto.
  - Summary basis labels: anthropic "API-billed", claude-code "subscription (not billed; list-price equivalent shown)", openai "local/OpenAI-compatible".
- [ ] **Step 4:** run → PASS.

### Task 6: `imageprep` stages; `focus` landed tiles and subject geometry

**Files:**
- Modify: `internal/imageprep/prep.go` (replace `Prepare`/`Options.Tiles`/`sharpestTiles`/sharpness stats), `internal/imageprep/prep_test.go`
- Create: `internal/focus/landed.go`, `internal/focus/target.go`, `internal/focus/focus_test.go`

**Interfaces (Produces):**
```go
// imageprep
type Frame struct {
    RGBA *image.RGBA
    Luma []float32
    W, H int
} // oriented for display
func Decode(jpegData []byte, orientation int) (*Frame, error)
func Measure(f *Frame) Stats // MeanLuma, LumaP1/P50/P99, HighlightClipPct, ShadowClipPct
func (f *Frame) Downscaled(maxEdge, quality int) ([]byte, error)
func (f *Frame) Crop(r image.Rectangle, quality int) ([]byte, error)
func DownLuma(luma []float32, w, h, maxEdge int) (g []uint8, dw, dh int, scale float64) // box, scale = dw/w
// focus
const CellSize, LandedSize, MinSubject, MaxSubject = 384, 768, 768, 1536
type Target struct {
    Center image.Point
    Size   int
    Source string
} // display px; Size = extent to cover
func SubjectRect(t Target, w, h int) image.Rectangle
func Ratio(luma []float32, stride int, r image.Rectangle) float64 // fine/coarse
type Cell struct {
    Crop  image.Rectangle
    Ratio float64
}
func Landed(luma []float32, w, h int, exclude image.Rectangle, k int) []Cell
```

- [ ] **Step 1: Failing tests.**
  - imageprep: orientation 6 dims swap and pixel mapping (a marked pixel at stored (0,0) lands at display (h-1,0)); `Downscaled(1024)` dims; `Crop` dims; `Measure` percentiles on a synthetic gradient.
  - focus:
    - `SubjectRect`:
      - centre (0,0), size 900, frame 4000×3000 → Min (0,0), side 900;
      - size 300 → side 768;
      - size 5000 → 1536;
      - frame 600×400 → side 400, within bounds;
      - centre beyond the frame edge → clamped.
    - `Ratio`: the same random texture sharp > blurred (3×3 box, applied twice).
    - `Landed` on a 3072×1536 synthetic: left half is a high-contrast **blurred** checker (0/255, blurred), right half a low-contrast **sharp** checker (100/140). The top cell is in the right half. This is the real-frame failure mode.
    - `exclude` covering the right half → the top cell comes from outside it; k=0 → empty; a frame smaller than 384 → empty.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.**
  - `Ratio`: fine = var(4-neighbour Laplacian on luma over r); coarse = the same on a 4× box-downsampled copy of r; return fine/(coarse+1e-6).
  - `Landed`:
    - grid cells over the full frame; compute fine and coarse per cell;
    - gate at the 75th percentile of coarse;
    - drop cells overlapping `exclude`;
    - sort by ratio;
    - take k, each expanded to a 768 crop centred on the cell, clamped (via a `SubjectRect`-style clamp).
  - Update the pipeline call sites: `Decode` → `Measure`, `Downscaled`; Landed tiles replace the old tiles; statsText drops the Laplacian figures; `summarize` drops peak-sharpness.
- [ ] **Step 4:** `go test ./...` → PASS.

### Task 7: pigo face detection

**Files:**
- Create: `internal/focus/detect.go`, `internal/focus/detect_test.go`, `internal/focus/cascade/{facefinder,puploc,LICENSE}` (copied from the pigo v1.4.6 module)
- Modify: `go.mod`, `go.sum`

**Interfaces (Produces):**
```go
type Face struct {
    Rect image.Rectangle // display px, native res
    Q    float64
    Eyes *image.Point    // midpoint of both pupils, when both found
}
type Detector struct{ /* unpacked cascades */ }
func NewDetector() (*Detector, error)
func (d *Detector) Detect(luma []float32, w, h int) []Face // all clustered detections, Q desc
func Confident(faces []Face, minQ float64) []Face
func FaceTarget(f Face) Target // centre = eyes, else face centre - 0.1*side; Size = side
func BoxTarget(r image.Rectangle) Target // Size = 1.2*max(dx,dy)
```

- [ ] **Step 1: Failing tests.**
  - `NewDetector` ok.
  - A flat grey 2000×1333 image → no confident faces.
  - pigo integration: locate a sample image under the pigo module dir (`go env GOMODCACHE` + `/github.com/esimov/pigo@v1.4.6/testdata/`), `t.Skip` if absent. Assert ≥1 face with Q ≥ 5 and a Rect inside the frame.
  - `FaceTarget` with and without eyes; `BoxTarget` size.
  - `-race`: Detect called from 4 goroutines on one Detector.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.** `Detect` recovers from a pigo panic and returns nil (the spec treats it as "no face"). `DownLuma` to 2000 → `pigo.CascadeParams` → `RunCascade` for each angle → `ClusterDetections(0.2)` → scale back by 1/scale. Puploc for Q≥5 faces with Perturbs 63 and the offsets (−0.085 row, ±0.185 col, scale 0.4). Both eyes found (Row>0 && Col>0) → midpoint.
- [ ] **Step 4:** run with `-race` → PASS.

### Task 8: `eval.Locate`, prompt revision, pipeline orchestration, report v2

**Files:**
- Create: `internal/eval/locate.go`
- Modify: `internal/eval/prompt.go` (evaluation sharpness section, locate prompt/schema), `internal/pipeline/pipeline.go`, `internal/report/report.go` (`FocusTarget`, CSV columns), tests in `internal/eval`, `internal/pipeline`

**Interfaces (Produces):**
```go
type NormBox struct {
    Left   float64 `json:"left"`
    Top    float64 `json:"top"`
    Right  float64 `json:"right"`
    Bottom float64 `json:"bottom"`
}
func (b NormBox) Valid() bool // finite, 0<=l<r<=1, 0<=t<b<=1
type LocateResult struct {
    Confident bool    `json:"confident"`
    Kind      string  `json:"kind"`
    Subject   string  `json:"subject"`
    Box       NormBox `json:"box"`
}
func Locate(ctx context.Context, b llm.Backend, frame []byte, maxTokens int) (*LocateResult, llm.Usage, error)
// report
type FocusTarget struct {
    Source           string        `json:"source"`
    Box              *eval.NormBox `json:"box,omitempty"`
    FaceQ            float64       `json:"face_q,omitempty"`
    Faces            int           `json:"faces"`
    Label            string        `json:"label,omitempty"`
    Reason           string        `json:"reason,omitempty"`
    SubjectSharpness float64       `json:"subject_sharpness,omitempty"`
    LandedSharpness  float64       `json:"landed_sharpness"`
}
```

- [ ] **Step 1: Failing tests.**
  - `NormBox.Valid` table, including NaN and zero area.
  - `Locate` with a fake backend: the request carries 1 image, the schema name is `focus_target`, and the response is parsed.
  - Pipeline, with a fake detector injected via `Config.detect func(*imageprep.Frame) []focus.Face` (unexported field, set only in tests) and a fake backend recording calls by `SchemaName`:
    - (a) face found → no `focus_target` call; the evaluate request includes the "face detector" label; report `source=face`.
    - (b) no face + locate model → a locate call, then evaluate with the "model: <subject>" label; `source=model`, box set.
    - (c) no face + locate returns confident=false → evaluate without a subject part; `source=none`, reason "model: no clear subject"; Decision from Policy; no error.
    - (d) no face + locate returns an invalid box → as (c) with reason "invalid box".
    - (e) `--locate off` → no locate call; reason "no face; locate off".
    - (f) DryRun → no backend calls at all; FocusTarget filled from the detector only.
    - The report JSON has `schema_version: 2`, `focus_target`, and `backend`.
  - CSV header includes `focus_source,face_q,subject_sharpness,landed_sharpness`.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.**
  - processOne:
    1. decode → measure → detect → confident;
    2. if a face → FaceTarget;
    3. else if !DryRun && Locate → Locate on `Downscaled(1024,85)`; if valid and confident → BoxTarget(denorm box);
    4. SubjectRect → Ratio → Crop(90);
    5. Landed(exclude subject) → crops;
    6. Downscaled(MaxEdge, 85);
    7. unless DryRun → Evaluate;
    8. save-inputs (Task 9).
  - Locate `maxTokens`: 256 for openai non-stream, otherwise 1024.
  - Prompt: rewrite item 1 (SHARPNESS) per the spec, and state what each labelled image is.
- [ ] **Step 4:** `go test ./...` → PASS.

### Task 9: `scan` summary, `--save-inputs`, docs

**Files:**
- Create: `internal/pipeline/saveinputs.go`, `internal/cli/summary.go`
- Modify: `internal/cli/root.go` (persistent `--face-min-q`, `--save-inputs`, `--tiles` default 1), `internal/cli/scan.go`, `README.md`, `CLAUDE.md` (layout, commands)
- Test: `internal/pipeline/pipeline_test.go`, `internal/cli/cli_test.go`

**Interfaces (Produces):**
```go
func saveInputs(dir, base string, req llm.Request, labels []string, meta map[string]any) error
// writes <base>.full.jpg, <base>.subject.jpg, <base>.landed-<i>.jpg, <base>.inputs.json
func ScanSummary(rep *report.Report) string // "previews: long edge min/median/max = a/b/c px; sources: tiff-ifd=N; orientation: 1=a 8=b; faces: N/M"
```

- [ ] **Step 1: Failing tests.**
  - The pipeline with SaveInputs writes `L1000001.full.jpg`, `L1000001.landed-1.jpg` and `L1000001.inputs.json`, whose `parts` list labels in send order.
  - cli: `--save-inputs <shoot dir>` → error "refusing to write inputs into the shoot directory"; `ScanSummary` string for a synthetic report.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.** Base name = the path relative to Dir with separators → `__`, extension stripped. Update README flags/commands and CLAUDE.md layout.
- [ ] **Step 4:** `go test ./... && go vet ./...` → PASS.

### Task 10: Real-frame verification and memory measurement

- [ ] `make build && ./bin/gophotocull scan --save-inputs $SCRATCH/scan-inputs photos`: faces 6/12 expected; eyeball the subject crops.
- [ ] Benchmark `BenchmarkDecodePrepare60MP` (synthetic 9504×6320) reporting peak heap via `runtime/metrics`; record the number in CLAUDE.md, replacing the estimate.
- [ ] Local: `cull --backend openai --model <local-4b-vision-model> -o $SCRATCH/local.json --save-inputs $SCRATCH/local-inputs photos` (unsandboxed for localhost). Check the locate crops on the 6 no-face frames, per-frame time, and whether early cancel stops generation (compare stream vs `--openai-stream=false` timing on 2 frames).
- [ ] Subscription: `cull --backend claude-code -o $SCRATCH/cc.json photos`. Check quota fields and results. (Uses subscription quota; approved as "probe first" scope. Only 12 frames.)
- [ ] No `--backend anthropic` run without explicit user approval.
- [ ] Update CLAUDE.md verified facts and roadmap with the results.
