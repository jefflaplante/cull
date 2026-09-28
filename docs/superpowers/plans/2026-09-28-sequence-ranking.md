# Sequences and Best-of-Set Ranking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Group similar frames into sequences (mostly slow sequences, sometimes
bursts). Rank each sequence with one side-by-side model call. Keep the best N and send
the rest to review with their rank.

**Architecture:**
- `internal/group` grouping:
  - gains an 8×8 colour "look" fingerprint (computed in `imageprep`);
  - links frames to the previous frame by time gap and look;
  - replaces the dHash burst code.
- `internal/eval` gains the rank call (prompt, schema, permutation check).
- `internal/pipeline`:
  - plans chunked ranking;
  - runs it through an executor (a synchronous backend pool, or Message Batches);
  - stores per-set orders in the report (schema v4);
  - applies `KeepBest`/`Outranked` in `decideAll`.
- The CLI gains `cull rank` and new flags.
- `labels.Sidecar` and `apply-c1` add `cull:best`.
- `calibrate` gets a sets section, and the review page gets set badges, a Sets filter
  and a filmstrip.

**Tech Stack:** Go 1.22 stdlib, cobra, pigo core. No new dependencies. The page is
plain JS, checked by the out-of-repo jsdom harness.

**Spec:** `docs/superpowers/specs/2026-09-28-sequence-ranking-design.md`

**Execution note:**
- Branch `sequence-ranking`. No commits until the user picks an integration option;
  the "Checkpoint" steps record the test run in the ledger.
- Tests that bind loopback (`internal/cli`, `internal/llm`) run unsandboxed.
  Everything else runs sandboxed with `GOCACHE=$SCRATCH/gocache`.
- Keep each unsandboxed command minimal and separate. Git steps that touch
  `.claude/settings.json` need an unsandboxed git command (user memory).
- **Never overwrite `photos/cull-report.json`.** It is the user's paid judge run.
  Measurements on the real frames write to `-o $SCRATCH/…`.

## Global Constraints

- Dependencies: stdlib + cobra + pigo core only.
- Never modify or delete DNGs. Never overwrite a sidecar cull didn't write unless
  `--overwrite-xmp`.
- Keep/review/cull is decided in Go (`eval.Policy`). The model only ranks. Ranking
  only demotes (keep → Outranked action); it never promotes, and never touches review
  or cull.
- Report schema **v4**. `decide` and `rank` load v3 reports and save them as v4.
  Resume still refuses a different schema.
- Rank call: at most **8** frames per call, labelled "Frame 1…N" in capture order,
  with no file names in any part. Each frame gets a 768 px full frame plus its
  subject crop (native resolution, ≤512 px). No judge scores are included.
- Defaults: `--keep-best 3`, `--outranked review`, `--seq-gap 60s`, `MaxSequence 40`.
  `--seq-look` is measured in Task 3.
- No paid API calls without the user's approval. Tests use fakes only.

## Review Focus

1. **Wrong-set grouping on real shoots:** a slow walk, or two different subjects at
   one location, must not chain into one set. The look threshold plus the
   40-frame cap must hold.
   - Tests: Task 3, `TestSequencesSplitDifferentScenes` and `TestSequencesCap`.
2. **A paid ranking thrown away or repeated:**
   - a policy change that only drops members must keep the stored order;
   - an interrupted `--batch` ranking must re-attach, not resubmit.
   - Tests: Task 5, `TestDecideReusesOrderWhenMembersDrop`; Task 9,
     `TestRankBatchReattaches`.
3. **Ranking answers that aren't a clean order:** missing, repeated or out-of-range
   frames must be rejected, never half-applied.
   - Test: Task 6, `TestRankRejectsNonPermutation`.
4. **Leaking file names or scores into the rank call,** which would bias the model.
   - Test: Task 6, `TestRankRequestHasNoNamesOrScores`.
5. **Your keeps demoted silently:** every demotion carries a reason naming the rank
   and the set, and your labels still win for sidecars and moves.
   - Tests: Task 5, `TestOutrankedReasonAndNeverPromotes`; Task 10,
     `TestSidecarBestKeyword`.

---

### Task 1: Look fingerprint

**Files:**
- Modify: `internal/imageprep/prep.go`, `internal/group/group.go`
- Test: `internal/imageprep/prep_test.go`, `internal/group/group_test.go`

**Interfaces:**
- Produces:
  - `(*imageprep.Frame).Grid(n int) []uint8` (n·n·3 bytes, mean RGB, display
    orientation, row-major);
  - `group.LookSize = 8`;
  - `group.LookDistance(a, b []uint8) float64` (in [0,1]).

- [ ] **Step 1: Write the failing tests**

`internal/imageprep/prep_test.go`:

```go
func TestGridFollowsDisplayOrientation(t *testing.T) {
	// Stored image: left half red, right half blue.
	img := image.NewRGBA(image.Rect(0, 0, 160, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 160; x++ {
			c := color.RGBA{220, 20, 20, 255}
			if x >= 80 {
				c = color.RGBA{20, 20, 220, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 95})
	cell := func(g []uint8, n, cx, cy int) (r, bl uint8) { i := (cy*n + cx) * 3; return g[i], g[i+2] }

	f, _ := Decode(b.Bytes(), 1)
	g := f.Grid(8)
	if r, bl := cell(g, 8, 0, 0); r < 150 || bl > 80 {
		t.Fatalf("orientation 1, top-left should be red: r=%d b=%d", r, bl)
	}
	if r, bl := cell(g, 8, 7, 0); bl < 150 || r > 80 {
		t.Fatalf("orientation 1, top-right should be blue: r=%d b=%d", r, bl)
	}
	f6, _ := Decode(b.Bytes(), 6) // displayed rotated 90° CW: stored left half becomes the top
	g6 := f6.Grid(8)
	if r, _ := cell(g6, 8, 0, 0); r < 150 {
		t.Fatalf("orientation 6, top should be red: r=%d", r)
	}
	if _, bl := cell(g6, 8, 0, 7); bl < 150 {
		t.Fatalf("orientation 6, bottom should be blue: b=%d", bl)
	}
	if len(g) != 8*8*3 {
		t.Fatalf("len %d", len(g))
	}
}
```

`internal/group/group_test.go` (new helpers plus tests):

```go
// scene renders a synthetic 320×240 "photo": a background gradient with a subject
// block. dx/dy shift the whole view, gain scales brightness, zoom scales about
// the centre.
func scene(bg, subj [3]float64, sx, sy float64, dx, dy int, gain, zoom float64) []uint8 {
	const w, h = 320, 240
	grid := make([]uint8, LookSize*LookSize*3)
	for cy := 0; cy < LookSize; cy++ {
		for cx := 0; cx < LookSize; cx++ {
			var acc [3]float64
			n := 0
			for y := cy * h / LookSize; y < (cy+1)*h/LookSize; y += 4 {
				for x := cx * w / LookSize; x < (cx+1)*w/LookSize; x += 4 {
					u := (float64(x-w/2)/zoom + float64(w/2) + float64(dx)) / w
					v := (float64(y-h/2)/zoom + float64(h/2) + float64(dy)) / h
					c := bg
					for k := range c {
						c[k] = bg[k] * (0.6 + 0.4*v)
					}
					if math.Abs(u-sx) < 0.12 && math.Abs(v-sy) < 0.2 {
						c = subj
					}
					for k := range acc {
						acc[k] += math.Min(255, c[k]*gain)
					}
					n++
				}
			}
			for k := range acc {
				grid[(cy*LookSize+cx)*3+k] = uint8(acc[k] / float64(n))
			}
		}
	}
	return grid
}

var (
	park  = [3]float64{70, 140, 60}
	coat  = [3]float64{200, 60, 40}
	wall  = [3]float64{180, 170, 150}
	shirt = [3]float64{40, 60, 160}
)

func TestLookDistanceToleratesReframingAndExposure(t *testing.T) {
	base := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	for name, other := range map[string][]uint8{
		"shift 10%":      scene(park, coat, 0.5, 0.5, 32, 0, 1, 1),
		"one stop up":    scene(park, coat, 0.5, 0.5, 0, 0, 1.6, 1),
		"zoom 5%":        scene(park, coat, 0.5, 0.5, 0, 0, 1, 1.05),
		"subject moved":  scene(park, coat, 0.56, 0.5, 0, 0, 1, 1),
	} {
		if d := LookDistance(base, other); d > 0.08 {
			t.Errorf("%s: distance %.3f, want ≤ 0.08", name, d)
		}
	}
}

func TestLookDistanceSeparatesScenes(t *testing.T) {
	base := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	for name, other := range map[string][]uint8{
		"different scene":              scene(wall, shirt, 0.3, 0.6, 0, 0, 1, 1),
		"same place, different subject": scene(park, shirt, 0.3, 0.5, 0, 0, 1, 1),
	} {
		if d := LookDistance(base, other); d < 0.12 {
			t.Errorf("%s: distance %.3f, want ≥ 0.12", name, d)
		}
	}
	if d := LookDistance(base, base); d != 0 {
		t.Errorf("identical: %.3f", d)
	}
}
```

The 0.08 and 0.12 bounds belong to these synthetic scenes. Task 3 sets the real
`--seq-look` default from real frames.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/imageprep ./internal/group -run 'Grid|Look'`
Expected: build failed (`f.Grid undefined`, `undefined: LookSize`, `LookDistance`).

- [ ] **Step 3: Implement**

`internal/imageprep/prep.go`:

```go
// Grid returns an n×n grid of mean RGB (row-major, 3 bytes per cell) of the frame
// as displayed: a small colour-and-layout fingerprint for grouping similar frames.
func (f *Frame) Grid(n int) []uint8 {
	out := make([]uint8, 0, n*n*3)
	for cy := 0; cy < n; cy++ {
		for cx := 0; cx < n; cx++ {
			r := f.storedRect(image.Rect(cx*f.W/n, cy*f.H/n, (cx+1)*f.W/n, (cy+1)*f.H/n))
			var sy, sb, sr, cnt int
			for y := r.Min.Y; y < r.Max.Y; y += 4 {
				for x := r.Min.X; x < r.Max.X; x += 4 {
					yi, ci := f.src.YOffset(x, y), f.src.COffset(x, y)
					sy += int(f.src.Y[yi])
					sb += int(f.src.Cb[ci])
					sr += int(f.src.Cr[ci])
					cnt++
				}
			}
			if cnt == 0 {
				out = append(out, 0, 0, 0)
				continue
			}
			R, G, B := color.YCbCrToRGB(uint8(sy/cnt), uint8(sb/cnt), uint8(sr/cnt))
			out = append(out, R, G, B)
		}
	}
	return out
}
```

Add the `image/color` import.

`internal/group/group.go`:

```go
// LookSize is the look fingerprint's grid: LookSize×LookSize cells of mean RGB.
const LookSize = 8

// LookDistance compares two look fingerprints, in [0,1]. Each grid is divided by
// its own mean brightness (about ±1 stop of exposure drops out), then the mean
// absolute difference over the overlapping cells is taken for shifts of up to
// one cell (about 12% reframing); the smallest wins.
func LookDistance(a, b []uint8) float64 {
	na, nb := levelled(a), levelled(b)
	best := 1.0
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			sum, n := 0.0, 0
			for y := 0; y < LookSize; y++ {
				for x := 0; x < LookSize; x++ {
					x2, y2 := x+dx, y+dy
					if x2 < 0 || y2 < 0 || x2 >= LookSize || y2 >= LookSize {
						continue
					}
					for c := 0; c < 3; c++ {
						sum += math.Abs(na[(y*LookSize+x)*3+c] - nb[(y2*LookSize+x2)*3+c])
						n++
					}
				}
			}
			if d := sum / float64(n); d < best {
				best = d
			}
		}
	}
	return best
}

// levelled divides a grid by its mean and scales it so typical differences fall
// in [0,1].
func levelled(g []uint8) []float64 {
	mean := 0.0
	for _, v := range g {
		mean += float64(v)
	}
	mean = math.Max(1, mean/float64(len(g)))
	out := make([]float64, len(g))
	for i, v := range g {
		out[i] = math.Min(1, float64(v)/mean/4)
	}
	return out
}
```

Add the `math` import.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test -race ./internal/imageprep ./internal/group`
Expected: PASS.
- If a synthetic bound fails, tune the **scale factor in `levelled`** (not the test
  bounds) and ledger it as a ruling with the measured distances.
- If no scale separates the two tests, rule on the smallest change (a colour-only or
  luma-only term) and ledger it.

- [ ] **Step 5: Checkpoint.** Record in the ledger.

---

### Task 2: Report schema v4, and the look replacing dHash

**Files:**
- Modify:
  - `internal/report/report.go`;
  - `internal/pipeline/stages.go` (compute `Look`, drop `DHash`);
  - `internal/pipeline/groups.go` (compile only; rewritten in Task 5);
  - `internal/group/group.go` (delete `DHash`, `Hamming`);
  - `internal/review/review.go` (card fields compile).
- Test: create `internal/report/report_test.go`; `internal/pipeline/pipeline_test.go`.

**Interfaces:**
- Produces:
  - `report.SchemaVersion = 4`;
  - `Result.Look string` (base64 of the Grid bytes; json `look`);
  - `Result.Group *Group`, where:

    ```go
    Group{ID, Size int; Rank, Of int; By string; Best bool; Strength, Weakness string}
    ```

    with json `id,size,rank,of,by,best,strength,weakness`;
  - `Report.KeepBest int` (json `keep_best`);
  - `Report.Sets []Set`, where:

    ```go
    Set{ID int; Members []string; Order []string; Notes []RankNote; Summary, By string;
        Usage eval.Usage; CostUSD float64}
    RankNote{File, Strength, Weakness string}
    ```

    `Members` and `Order` hold `Result.File` paths.
  - `(Result) LookBytes() ([]uint8, bool)`;
  - `(*Report) Cost() float64`: results plus sets.

- [ ] **Step 1: Write the failing tests**

`internal/report/report_test.go`:

```go
package report

import (
	"path/filepath"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
)

func TestSchemaV4RoundTrip(t *testing.T) {
	look := make([]uint8, 192)
	look[5] = 200
	r := &Report{SchemaVersion: SchemaVersion, KeepBest: 3,
		Results: []Result{{File: "/s/L1.DNG", Look: EncodeLook(look), CostUSD: 0.02,
			Group: &Group{ID: 1, Size: 2, Rank: 1, Of: 2, By: "model", Best: true, Strength: "eyes", Weakness: "tilt"}}},
		Sets: []Set{{ID: 1, Members: []string{"/s/L1.DNG", "/s/L2.DNG"}, Order: []string{"/s/L1.DNG", "/s/L2.DNG"},
			Notes: []RankNote{{File: "/s/L1.DNG", Strength: "eyes", Weakness: "tilt"}}, Summary: "sharper eyes", By: "model",
			Usage: eval.Usage{InputTokens: 9000, OutputTokens: 800}, CostUSD: 0.03}}}
	p := filepath.Join(t.TempDir(), "r.json")
	if err := r.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || SchemaVersion != 4 || got.KeepBest != 3 || len(got.Sets) != 1 || got.Sets[0].Summary != "sharper eyes" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if b, ok := got.Results[0].LookBytes(); !ok || len(b) != 192 || b[5] != 200 {
		t.Fatalf("look: %v %v", b, ok)
	}
	if !got.Results[0].Group.Best || got.Results[0].Group.By != "model" {
		t.Fatalf("group: %+v", got.Results[0].Group)
	}
	if c := got.Cost(); c < 0.0499 || c > 0.0501 {
		t.Fatalf("cost includes sets: %v", c)
	}
}
```

In `internal/pipeline/pipeline_test.go`, extend the existing end-to-end `Run` test, or
add a small one, so that every measured result has a decodable 192-byte look:

```go
func TestRunRecordsLook(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled, c.WriteXMP = false, false
	rep, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		if g, ok := r.LookBytes(); !ok || len(g) != 192 {
			t.Fatalf("%s: look %v %v", r.File, g, ok)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/report ./internal/pipeline -run 'SchemaV4|RecordsLook'`
Expected: build failed (`KeepBest`, `Sets`, `EncodeLook`, `LookBytes` undefined).

- [ ] **Step 3: Implement**

`report.go`:
- set `SchemaVersion = 4`;
- replace `DHash` with `Look string` (json `look,omitempty`);
- replace `Group` with the struct above;
- add `KeepBest` and `Sets` to `Report`;
- add `Set` and `RankNote`;
- add:

```go
// EncodeLook stores a look fingerprint compactly.
func EncodeLook(g []uint8) string { return base64.StdEncoding.EncodeToString(g) }

// LookBytes decodes the look fingerprint.
func (r Result) LookBytes() ([]uint8, bool) {
	if r.Look == "" {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(r.Look)
	return b, err == nil && len(b) > 0
}

// Cost is everything the report's model calls cost at list price.
func (r *Report) Cost() float64 {
	c := 0.0
	for _, x := range r.Results {
		c += x.CostUSD
	}
	for _, s := range r.Sets {
		c += s.CostUSD
	}
	return c
}
```

`stages.go`: replace the `res.DHash = …` line with

```go
res.Look = report.EncodeLook(p.frame.Grid(group.LookSize))
```

`group.go`: delete `DHash` and `Hamming`, and their tests in `group_test.go`.

`pipeline/groups.go`: build `group.Frame.Look` from `r.LookBytes()` in place of the
hash, so the package compiles. The old `Groups`/`Best` stay until Task 3/5; keep
`Hash` fields compiling by removing them in Task 3.

`review/review.go`: the card's `Group` is the new struct; `Best` is now a bool.

The CLI's printed summary (`cull.go`: "cost in report") uses `rep.Cost()`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test -race ./internal/report ./internal/pipeline ./internal/review ./internal/group`
Expected: PASS. Existing burst tests that relied on dHash may need a note; Task 3
replaces them. Mark any failing burst test `t.Skip("replaced in Task 3")` only if it
tests removed code, and ledger it.

- [ ] **Step 5: Checkpoint.**

---

### Task 3: Sequences, and measuring the `--seq-look` default

**Files:**
- Modify: `internal/group/group.go` (replace `Groups`/`Best`/`MaxBurst`/`Options`)
- Test: `internal/group/group_test.go`

**Interfaces:**
- Produces:
  - `group.Frame{Key string; Time time.Time; HasTime bool; Look []uint8; Score Score}`;
  - `group.Options{Gap time.Duration; MaxLook float64}`;
  - `group.MaxSequence = 40`;
  - `group.DefaultLook` (the measured default);
  - `group.Sequences(frames []Frame, o Options) [][]int` (sets of ≥2, each in
    capture order);
  - `group.ScoreOrder(frames []Frame, set []int) []int` (best first, by the existing
    `better`).

- [ ] **Step 1: Write the failing tests**

```go
func seqFrame(key string, sec int, look []uint8) Frame {
	return Frame{Key: key, Time: time.Unix(1_800_000_000+int64(sec), 0), HasTime: true, Look: look}
}

func TestSequencesLinkByGapAndLookToPrevious(t *testing.T) {
	a := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	drift := func(i int) []uint8 { return scene(park, coat, 0.5, 0.5, 6*i, 0, 1, 1) } // walks away slowly
	frames := []Frame{seqFrame("L1", 0, a)}
	for i := 1; i < 10; i++ {
		frames = append(frames, seqFrame(fmt.Sprintf("L%d", i+1), 20*i, drift(i)))
	}
	frames = append(frames, seqFrame("L11", 400, a)) // same look, but 220 s later: new set
	sets := Sequences(frames, Options{Gap: 60 * time.Second, MaxLook: 0.08})
	if len(sets) != 1 || len(sets[0]) != 10 {
		t.Fatalf("sets %v: the 10 drifting frames link via their neighbours; the late one stands alone", sets)
	}
}

func TestSequencesSplitDifferentScenes(t *testing.T) {
	frames := []Frame{
		seqFrame("L1", 0, scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)),
		seqFrame("L2", 5, scene(park, coat, 0.52, 0.5, 0, 0, 1, 1)),
		seqFrame("L3", 10, scene(wall, shirt, 0.3, 0.6, 0, 0, 1, 1)),
		seqFrame("L4", 15, scene(wall, shirt, 0.31, 0.6, 0, 0, 1, 1)),
		seqFrame("L5", 20, scene(park, shirt, 0.3, 0.5, 0, 0, 1, 1)), // same place, different subject
	}
	sets := Sequences(frames, Options{Gap: 60 * time.Second, MaxLook: 0.08})
	if len(sets) != 2 || len(sets[0]) != 2 || len(sets[1]) != 2 {
		t.Fatalf("sets %v", sets)
	}
}

func TestSequencesCapAndOrdering(t *testing.T) {
	look := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	var frames []Frame
	for i := 0; i < 45; i++ {
		frames = append(frames, Frame{Key: fmt.Sprintf("L%03d", 45-i), Look: look}) // no times, reverse input order
	}
	sets := Sequences(frames, Options{Gap: 60 * time.Second, MaxLook: 0.08})
	if len(sets) != 2 || len(sets[0]) != MaxSequence || len(sets[1]) != 5 {
		t.Fatalf("cap: %d sets, sizes %d/%d", len(sets), len(sets[0]), len(sets[len(sets)-1]))
	}
	if frames[sets[0][0]].Key != "L001" {
		t.Fatalf("untimed frames go in file-name order, got %s first", frames[sets[0][0]].Key)
	}
	if Sequences(frames, Options{Gap: 0, MaxLook: 0.08}) != nil {
		t.Fatal("Gap 0 disables grouping")
	}
}

func TestScoreOrder(t *testing.T) {
	frames := []Frame{
		{Key: "a", Score: Score{Evaluated: true, Sharp: 7}},
		{Key: "b", Score: Score{Evaluated: true, Sharp: 9}},
		{Key: "c", Score: Score{Evaluated: true, Sharp: 9, EyesOpen: true}},
	}
	if got := ScoreOrder(frames, []int{0, 1, 2}); !reflect.DeepEqual(got, []int{2, 1, 0}) {
		t.Fatalf("%v", got)
	}
}
```

Replace the old `Groups`, `Best` and burst tests.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/group`
Expected: build failed (`undefined: Sequences`, `MaxSequence`, `ScoreOrder`;
`unknown field Look`).

- [ ] **Step 3: Implement**

```go
// MaxSequence caps a set, so a slow walk down a street can't chain into one.
const MaxSequence = 40

// Options: Gap 0 disables grouping.
type Options struct {
	Gap     time.Duration // max capture-time gap to the previous frame (ignored when either lacks a time)
	MaxLook float64       // max LookDistance to the previous frame
}

// Sequences returns sets of two or more similar frames as indices into frames,
// each in capture order. Frames are ordered by capture time then key when every
// frame has a time, otherwise by key (Leica numbers are sequential). A frame joins
// the current set when it is within Gap of the previous frame and looks like the
// previous frame (not the first: sequences drift), up to MaxSequence frames.
func Sequences(frames []Frame, o Options) [][]int {
	if o.Gap <= 0 || len(frames) < 2 {
		return nil
	}
	allTimed := true
	for _, f := range frames {
		allTimed = allTimed && f.HasTime
	}
	order := make([]int, len(frames))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		fa, fb := frames[order[a]], frames[order[b]]
		if allTimed && !fa.Time.Equal(fb.Time) {
			return fa.Time.Before(fb.Time)
		}
		return fa.Key < fb.Key
	})
	var sets [][]int
	cur := []int{order[0]}
	flush := func() {
		if len(cur) > 1 {
			sets = append(sets, cur)
		}
	}
	for k := 1; k < len(order); k++ {
		prev, next := frames[order[k-1]], frames[order[k]]
		if linked(prev, next, o) && len(cur) < MaxSequence {
			cur = append(cur, order[k])
			continue
		}
		flush()
		cur = []int{order[k]}
	}
	flush()
	return sets
}

func linked(a, b Frame, o Options) bool {
	if len(a.Look) == 0 || len(b.Look) == 0 || LookDistance(a.Look, b.Look) > o.MaxLook {
		return false
	}
	if a.HasTime && b.HasTime {
		d := b.Time.Sub(a.Time)
		return d >= 0 && d <= o.Gap
	}
	return true
}

// ScoreOrder orders a set best first by the frames' own scores: evaluated, then
// sharpness, open eyes, composition, exposure; capture order on a tie.
func ScoreOrder(frames []Frame, set []int) []int {
	out := append([]int(nil), set...)
	sort.SliceStable(out, func(a, b int) bool { return better(frames[out[a]].Score, frames[out[b]].Score) })
	return out
}
```

Delete `Groups`, `Best`, `MaxBurst`, `Hash` and `HasHash`. Update the package doc to
"sequences of similar frames".

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test -race ./internal/group`
Expected: PASS. `pipeline/groups.go` won't compile until Task 5 wires it; to keep
`go build ./...` green, switch `decideAll` to `Sequences`/`ScoreOrder` now with the
old single-best behaviour, and ledger it. Task 5 rewrites it.

- [ ] **Step 5: Measure the default on the real frames.**
  1. Run `./bin/cull scan -o $SCRATCH/looks.json photos` (make build first). This is
     free, and writes only to the scratchpad; `photos/cull-report.json` is never
     touched.
  2. With a throwaway Go program in `$SCRATCH/lookdist/`, which imports
     `internal/report` and `internal/group`, print `LookDistance` for every
     consecutive pair in file-name order.
  3. Set `DefaultLook` to a value that links M1104114→M1104115 and
     M1104116→M1104117, with margin, and separates the most pairs of different
     set-ups.
  4. Record the distances and the chosen value in CLAUDE.md "Verified facts", and
     ledger the ruling.
- [ ] **Step 6: Checkpoint.**

---

### Task 4: Policy: `KeepBest`, `Outranked`

**Files:**
- Modify: `internal/eval/types.go`
- Test: `internal/eval/eval_test.go`

**Interfaces:**
- Produces:
  - `Policy.KeepBest int` (the default 3 is applied by the CLI; 0 = rank only);
  - `Policy.Outranked Action`, replacing `Duplicates`;
  - `(Policy).ApplyOutranked(d Decision, reasons []string, rank, of, set int, byScores bool) (Decision, []string)`,
    replacing `ApplyDuplicate`.

- [ ] **Step 1: Write the failing test**

```go
func TestApplyOutranked(t *testing.T) {
	p := Policy{KeepBest: 3, Outranked: ActionReview}
	d, why := p.ApplyOutranked(Keep, nil, 5, 7, 3, false)
	if d != Review || len(why) != 1 || why[0] != "rank 5 of 7 in set 3 (keeping the best 3)" {
		t.Fatalf("%s %v", d, why)
	}
	if _, why := p.ApplyOutranked(Keep, nil, 4, 6, 2, true); why[0] != "rank 4 of 6 in set 2 by scores, not compared (keeping the best 3)" {
		t.Fatalf("%v", why)
	}
	if d, _ := (Policy{KeepBest: 3, Outranked: ActionCull}).ApplyOutranked(Review, nil, 5, 7, 3, false); d != Cull {
		t.Fatalf("cull action: %s", d)
	}
	if d, _ := (Policy{KeepBest: 3, Outranked: ActionIgnore}).ApplyOutranked(Keep, nil, 5, 7, 3, false); d != Keep {
		t.Fatalf("ignore: %s", d)
	}
}
```

(Callers only call it for rank > KeepBest; "never touch review/cull" lives in Task 5.)

- [ ] **Step 2: Run it to verify it fails**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/eval -run ApplyOutranked`
Expected: build failed (`ApplyOutranked` undefined; `unknown field KeepBest`).

- [ ] **Step 3: Implement**

```go
	KeepBest             int     // per set, keep this many best-ranked frames; 0 = rank only
	Outranked            Action  // frames ranked below KeepBest in their set
```

```go
// ApplyOutranked raises a frame ranked below KeepBest in its set by the Outranked
// action (review by default) and always leaves the reason.
func (p Policy) ApplyOutranked(d Decision, reasons []string, rank, of, set int, byScores bool) (Decision, []string) {
	if to, act := p.Outranked.decision(); act && rank(to) > rank(d) {
		d = to
	}
	how := ""
	if byScores {
		how = " by scores, not compared"
	}
	return d, append(reasons, fmt.Sprintf("rank %d of %d in set %d%s (keeping the best %d)", rank, of, set, how, p.KeepBest))
}
```

The parameter `rank` shadows the package func `rank`. Name it `pos` in the real code.
Delete `Duplicates` and `ApplyDuplicate`.

- [ ] **Step 4: Run** `go test -race ./internal/eval`. Expected: PASS.
- [ ] **Step 5: Checkpoint.**

---

### Task 5: `decideAll` with sets, stored orders, reuse and `best`

**Files:**
- Modify: `internal/pipeline/groups.go`, `pipeline.go`, `decide.go`
  (`DecideOptions.GroupGap`/`GroupHamming` → `Seq group.Options`); `Config` likewise.
- Test: `internal/pipeline/decide_test.go` (replace the burst tests).

**Interfaces:**
- Consumes: `group.Sequences`, `group.ScoreOrder`, `Policy.ApplyOutranked`,
  `report.Set`.
- Produces:
  - `decideAll(rep *report.Report, p eval.Policy, o group.Options) []int`. It
    rebuilds `rep.Sets` for the current grouping, reusing stored `Order`, `Notes`,
    `Summary`, `Usage` and `CostUSD` under the reuse rule, and sets `rep.KeepBest`.
  - `rankable(r report.Result, d eval.Decision) bool`: evaluated and d != cull.
  - `needsRanking(rep *report.Report) []int`: indices into `rep.Sets` of sets with ≥2
    rankable frames and no reusable model order.

The **reuse rule**: a stored set's `Order` is reused for a current set when every
currently rankable member appears in that `Order`. Members that dropped out are
removed; the relative order of the rest is kept.

- [ ] **Step 1: Write the failing tests**

Use a helper that builds a report directly, with no DNGs needed:

```go
// setReport: n frames in one visual sequence 10 s apart, all evaluated keep with
// sharpness scores from sharp[], looks identical.
func setReport(t *testing.T, sharp ...float64) *report.Report {
	t.Helper()
	look := report.EncodeLook(make([]uint8, 192))
	rep := &report.Report{}
	for i, s := range sharp {
		e := &eval.Evaluation{Sharpness: eval.Sharpness{Status: "sharp", Score: s}}
		rep.Results = append(rep.Results, report.Result{File: fmt.Sprintf("/s/L%03d.DNG", i+1), Look: look,
			Preview: &report.PreviewInfo{Width: 100, Height: 100, Orientation: 1}, Evaluation: e, Decision: eval.Keep})
	}
	return rep
}

var seq = group.Options{Gap: time.Minute, MaxLook: 0.1}

func TestKeepBestByScoresWithoutRanking(t *testing.T) {
	rep := setReport(t, 7, 9, 8, 6, 9.5)
	decideAll(rep, eval.Policy{KeepBest: 3, Outranked: eval.ActionReview}, seq)
	want := map[string]eval.Decision{"L001": eval.Review, "L002": eval.Keep, "L003": eval.Keep, "L004": eval.Review, "L005": eval.Keep}
	for _, r := range rep.Results {
		if d := want[strings.TrimSuffix(filepath.Base(r.File), ".DNG")]; r.Decision != d || r.Group == nil || r.Group.By != "scores" {
			t.Errorf("%s: %s group=%+v", r.File, r.Decision, r.Group)
		}
	}
	if len(rep.Sets) != 1 || rep.KeepBest != 3 || len(needsRanking(rep)) != 1 {
		t.Fatalf("sets %+v keepBest %d needs %v", rep.Sets, rep.KeepBest, needsRanking(rep))
	}
}

func TestStoredModelOrderWinsAndIsReused(t *testing.T) {
	rep := setReport(t, 9, 9, 9, 9)
	p := eval.Policy{KeepBest: 2, Outranked: eval.ActionReview}
	decideAll(rep, p, seq)
	rep.Sets[0].Order = []string{"/s/L003.DNG", "/s/L001.DNG", "/s/L004.DNG", "/s/L002.DNG"}
	rep.Sets[0].By = "model"
	decideAll(rep, p, seq)
	got := map[string]int{}
	for _, r := range rep.Results {
		got[filepath.Base(r.File)] = r.Group.Rank
	}
	if got["L003.DNG"] != 1 || got["L002.DNG"] != 4 || len(needsRanking(rep)) != 0 {
		t.Fatalf("ranks %v needs %v", got, needsRanking(rep))
	}
}

func TestDecideReusesOrderWhenMembersDrop(t *testing.T) {
	rep := setReport(t, 9, 9, 9, 9)
	p := eval.Policy{KeepBest: 2, Outranked: eval.ActionReview}
	decideAll(rep, p, seq)
	rep.Sets[0].Order = []string{"/s/L003.DNG", "/s/L001.DNG", "/s/L004.DNG", "/s/L002.DNG"}
	rep.Sets[0].By = "model"
	rep.Results[0].Evaluation.Sharpness.Status = "missed_focus" // L001 becomes a technical cull
	decideAll(rep, p, seq)
	if len(needsRanking(rep)) != 0 {
		t.Fatal("a dropped member must not throw away the paid order")
	}
	for _, r := range rep.Results {
		if filepath.Base(r.File) == "L004.DNG" && (r.Group.Rank != 2 || r.Decision != eval.Keep) {
			t.Fatalf("L004 moves up to rank 2 and stays keep: %+v %s", r.Group, r.Decision)
		}
	}
	rep.Results = append(rep.Results, report.Result{File: "/s/L005.DNG", Look: rep.Results[1].Look,
		Preview: rep.Results[1].Preview, Evaluation: &eval.Evaluation{Sharpness: eval.Sharpness{Status: "sharp", Score: 9}}})
	decideAll(rep, p, seq)
	if len(needsRanking(rep)) != 1 {
		t.Fatal("a new member makes the set unranked")
	}
}

func TestOutrankedReasonAndNeverPromotes(t *testing.T) {
	rep := setReport(t, 9, 8, 7, 6)
	rep.Results[3].Evaluation.Sharpness.Status = "soft" // policy says review on its own
	p := eval.Policy{KeepBest: 1, Outranked: eval.ActionReview}
	decideAll(rep, p, seq)
	for _, r := range rep.Results[1:3] {
		if r.Decision != eval.Review || !strings.Contains(strings.Join(r.Reasons, ";"), "in set 1 by scores, not compared (keeping the best 1)") {
			t.Fatalf("%s: %s %v", r.File, r.Decision, r.Reasons)
		}
	}
	if r := rep.Results[0]; r.Decision != eval.Keep || !r.Group.Best {
		t.Fatalf("best: %s %+v", r.Decision, r.Group)
	}
	decideAll(rep, eval.Policy{KeepBest: 0, Outranked: eval.ActionReview}, seq)
	for _, r := range rep.Results[:3] {
		if r.Decision != eval.Keep {
			t.Fatalf("KeepBest 0 ranks only: %s %s", r.File, r.Decision)
		}
	}
}

func TestLoneSurvivorIsBest(t *testing.T) {
	rep := setReport(t, 9, 3, 2)
	rep.Results[1].Evaluation.Sharpness.Status = "missed_focus"
	rep.Results[2].Evaluation.Sharpness.Status = "motion_blur"
	decideAll(rep, eval.Policy{KeepBest: 3, Outranked: eval.ActionReview}, seq)
	if g := rep.Results[0].Group; g == nil || g.Rank != 1 || !g.Best || g.Of != 1 || len(needsRanking(rep)) != 0 {
		t.Fatalf("lone survivor: %+v", g)
	}
	if g := rep.Results[1].Group; g == nil || g.Rank != 0 || g.Best {
		t.Fatalf("culled member stays in the set, unranked: %+v", g)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/pipeline -run 'KeepBest|StoredModel|ReusesOrder|Outranked|LoneSurvivor'`
Expected: FAIL. `needsRanking` is undefined; Rank, By and Sets are unset.

- [ ] **Step 3: Implement `decideAll`**

1. Build `group.Frame`s from results that have a preview and no error: Key = File;
   Time from EXIF; Look from `LookBytes`; Score from the evaluation.
2. Compute each evaluated frame's policy decision (`DecideFacts`), as today.
3. Index the stored sets: file → stored set.
4. For each `group.Sequences(frames, o)` set, with id = position + 1:
   - rankable = members that are evaluated with policy decision ≠ cull, kept in
     capture order.
   - Reuse: find the stored set whose `Order` contains the first rankable member. If
     every rankable member is in that `Order`, then:
     - `order` = that `Order` filtered to the rankable members;
     - `by` = "model";
     - carry over Notes, Summary, Usage and CostUSD.

     Otherwise, `order` = the file names from `group.ScoreOrder(rankable frames)` and
     `by` = "scores".
   - A single rankable frame gets `order` = [it] and `by` = "scores". Rank 1 needs no
     call.
   - For each member: `Group{ID, Size, Of: len(rankable), By, Rank: position in
     order+1 (0 if not rankable), Best: rank in 1..max(1, KeepBest)}`, plus the
     strength and weakness from Notes when `by` = model.
     - With `KeepBest` 0, `Best` = rank 1 (rank only).
     - If rank > KeepBest > 0 and the policy decision is keep, apply
       `p.ApplyOutranked(...)`. Never apply it to review or cull; that is the
       "never touches" rule.
   - Append `report.Set{ID, Members, Order (by==model ? order : nil), Notes, Summary,
     By, Usage, CostUSD}` to the new `rep.Sets`.
5. Replace `rep.Sets`, and set `rep.KeepBest = p.KeepBest`.
6. Compare decisions to find the changed indices, as today.

`needsRanking`: sets with `Of ≥ 2` and `By != "model"`. Compute Of from the members'
groups, or store it on `Set` (add `Of int` to `Set`; ledger it if added).

Config/DecideOptions: replace `GroupGap`/`GroupHamming` with `Seq group.Options` and
update the call sites.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=$SCRATCH/gocache go test -race ./internal/pipeline`
Expected: PASS. The old burst tests are replaced;
`TestBurstDuplicatesGoToReviewAndDecideCanCullThem` becomes a sequence test using
`KeepBest 1`/`ActionCull`.

- [ ] **Step 5: Checkpoint.**

---

### Task 6: The rank call (`internal/eval/rank.go`)

**Files:**
- Create: `internal/eval/rank.go`
- Test: `internal/eval/rank_test.go`

**Interfaces:**
- Produces:

  ```go
  type RankFrame struct{ Full, Crop []byte } // Crop may be nil
  type RankEntry struct{ Frame int `json:"frame"`; Strength, Weakness string }
  type Ranking struct{ Ranking []RankEntry `json:"ranking"`; Summary string `json:"summary"` }
  func RankRequest(frames []RankFrame, maxTokens int) llm.Request   // SchemaName "ranking"
  func RankSchema() map[string]any
  func DecodeRank(raw []byte, n int) (*Ranking, error)               // permutation of 1..n
  func Rank(ctx context.Context, b llm.Backend, frames []RankFrame, maxTokens int) (*Ranking, Usage, error)
  ```

  `Rank` retries once on a non-permutation. The schema retry already lives in
  `llm.validated`.

- [ ] **Step 1: Write the failing tests**

```go
type rankFake struct {
	answers []string
	reqs    []llm.Request
}

func (f *rankFake) Name() string { return "fake" }
func (f *rankFake) Call(_ context.Context, r llm.Request) (*llm.Response, error) {
	f.reqs = append(f.reqs, r)
	a := f.answers[0]
	f.answers = f.answers[1:]
	return &llm.Response{JSON: json.RawMessage(a), Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}, nil
}

func frames3() []RankFrame {
	return []RankFrame{{Full: []byte{1}, Crop: []byte{2}}, {Full: []byte{3}}, {Full: []byte{4}, Crop: []byte{5}}}
}

const good3 = `{"ranking":[{"frame":2,"strength":"eyes","weakness":"tilt"},{"frame":1,"strength":"s","weakness":"w"},{"frame":3,"strength":"s","weakness":"w"}],"summary":"frame 2 has the moment"}`

func TestRankRequestHasNoNamesOrScores(t *testing.T) {
	req := RankRequest(frames3(), 2000)
	var jpegs int
	for _, p := range req.Parts {
		if p.JPEG != nil {
			jpegs++
		}
		for _, bad := range []string{".DNG", "score", "sharp:", "keep", "cull"} {
			if strings.Contains(p.Text, bad) {
				t.Errorf("part text leaks %q: %q", bad, p.Text)
			}
		}
	}
	if jpegs != 5 || !strings.Contains(req.Parts[0].Text+req.Parts[1].Text, "Frame 1") {
		t.Fatalf("5 images (3 full + 2 crops), labelled Frame 1..3; got %d", jpegs)
	}
	for _, want := range []string{"sharp", "expression", "moment", "composition", "fixed"} {
		if !strings.Contains(strings.ToLower(req.System), want) {
			t.Errorf("rubric lacks %q", want)
		}
	}
}

func TestRankDecodes(t *testing.T) {
	f := &rankFake{answers: []string{good3}}
	r, u, err := Rank(context.Background(), f, frames3(), 2000)
	if err != nil || r.Ranking[0].Frame != 2 || r.Summary == "" || u.InputTokens != 100 {
		t.Fatalf("%+v %+v %v", r, u, err)
	}
}

func TestRankRejectsNonPermutation(t *testing.T) {
	for name, bad := range map[string]string{
		"missing":  `{"ranking":[{"frame":1,"strength":"","weakness":""},{"frame":2,"strength":"","weakness":""}],"summary":""}`,
		"repeated": `{"ranking":[{"frame":1,"strength":"","weakness":""},{"frame":1,"strength":"","weakness":""},{"frame":3,"strength":"","weakness":""}],"summary":""}`,
		"range":    `{"ranking":[{"frame":1,"strength":"","weakness":""},{"frame":2,"strength":"","weakness":""},{"frame":4,"strength":"","weakness":""}],"summary":""}`,
	} {
		f := &rankFake{answers: []string{bad, good3}}
		if r, _, err := Rank(context.Background(), f, frames3(), 2000); err != nil || r.Ranking[0].Frame != 2 || len(f.reqs) != 2 {
			t.Errorf("%s: one retry then success: %+v %v calls=%d", name, r, err, len(f.reqs))
		}
		f = &rankFake{answers: []string{bad, bad}}
		if _, _, err := Rank(context.Background(), f, frames3(), 2000); err == nil {
			t.Errorf("%s: twice bad must fail, never half-apply", name)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOCACHE=$SCRATCH/gocache go test ./internal/eval -run Rank`
Expected: build failed (`undefined: RankRequest`, …).

- [ ] **Step 3: Implement `rank.go`**

**Prompt.** A system prompt with the rubric, in priority order:
1. subject sharpness where it matters (the eyes), judged on the crops;
2. eyes and expression (open, engaged, natural; not mid-blink or mid-word);
3. gesture and moment;
4. composition and background (framing, horizon, distractions at the edges, cropped
   limbs);
5. exposure only if it cannot be fixed; fixable exposure never counts against a frame.

It also says:
- "The frames are near-duplicates of one scene; compare them to each other."
- "Return every frame exactly once, best first."

**Parts.** For each frame i:
- `llm.Text(fmt.Sprintf("Frame %d: full frame", i))`, then `llm.JPEG(Full)`;
- if there is a crop: `llm.Text(fmt.Sprintf("Frame %d: subject at native resolution", i))`,
  then `llm.JPEG(Crop)`;
- then a closing text: "Rank these N frames."

**Schema.** An object with `ranking` (an array of `{frame integer ≥1, strength
string, weakness string}`) and `summary` (string); `additionalProperties: false`
everywhere.

**`DecodeRank`.** Unmarshal, then check `len == n`, each frame in 1..n, and none
seen twice. Otherwise return `errNotPermutation`.

**`Rank`.** Call, then decode. On `errNotPermutation`, call once more and add the
usages. A `llm.ErrQuotaStop` delivered with a valid answer returns the answer with
the error, as `Locate` does.

- [ ] **Step 4: Run** `go test -race ./internal/eval`. Expected: PASS.
- [ ] **Step 5: Checkpoint.**

---

### Task 7: Chunk plan and merged order

**Files:**
- Create: `internal/pipeline/rankplan.go`
- Test: `internal/pipeline/rankplan_test.go`

**Interfaces:**
- Produces:
  - `const maxRankFrames = 8`;
  - `chunks(n int) [][]int`: split positions 0..n-1 in order into ⌈n/8⌉ nearly equal
    chunks;
  - `finalists(nChunks, keepBest int) int`, which is
    `max(8/nChunks, ceilDiv(keepBest, nChunks))`, with a minimum of 1;
  - `merge(chunkOrders [][]int, finalOrder []int) []int`. It returns the final order,
    then the non-finalists by their chunk rank, ties by position.

- [ ] **Step 1: Write the failing tests**

```go
func TestChunksAndFinalists(t *testing.T) {
	for _, c := range []struct{ n, keep, chunks, per int }{
		{8, 3, 1, 8}, {9, 3, 2, 4}, {16, 3, 2, 4}, {40, 3, 5, 1}, {40, 5, 5, 1}, {40, 8, 5, 2},
	} {
		ch := chunks(c.n)
		total := 0
		for _, x := range ch {
			total += len(x)
			if len(x) > maxRankFrames {
				t.Errorf("n=%d: chunk of %d", c.n, len(x))
			}
		}
		if len(ch) != c.chunks || total != c.n {
			t.Errorf("n=%d: %d chunks covering %d", c.n, len(ch), total)
		}
		if f := finalists(len(ch), c.keep); c.chunks > 1 && f != c.per {
			t.Errorf("n=%d keep=%d: finalists %d, want %d", c.n, c.keep, f, c.per)
		}
		if f := finalists(len(ch), c.keep); c.chunks > 1 && f*len(ch) < c.keep {
			t.Errorf("n=%d keep=%d: final round smaller than KeepBest", c.n, c.keep)
		}
	}
}

func TestMergeOrder(t *testing.T) {
	// Two chunks: positions 0-3 ranked [2,0,3,1], 4-7 ranked [5,7,4,6]; 2 finalists each
	// → finals among {2,0,5,7} ranked [5,2,7,0].
	got := merge([][]int{{2, 0, 3, 1}, {5, 7, 4, 6}}, []int{5, 2, 7, 0})
	want := []int{5, 2, 7, 0, 3, 4, 1, 6} // finals, then chunk rank 3s (3 before 4), then rank 4s
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/pipeline -run 'Chunks|MergeOrder'`. Expected: build failed.
- [ ] **Step 3: Implement.**
  - `chunks`: `k := ceilDiv(n, 8)`; chunk i gets positions `[i*n/k, (i+1)*n/k)`.
  - `merge`: a finalist set from `finalOrder`; then iterate r = finalists…7 over each
    chunk's order at index r, appending non-finalists in chunk order. Chunks are in
    position order, so ties break by position.
- [ ] **Step 4: Run** `go test -race ./internal/pipeline -run 'Chunks|MergeOrder'`. Expected: PASS.
- [ ] **Step 5: Checkpoint.**

---

### Task 8: The rank stage (synchronous), in `judge` and `cull rank`

**Files:**
- Create: `internal/pipeline/rank.go`
- Modify: `pipeline.go` (`finishRun`: decideAll → rank → decideAll → sidecars → moves
  → save); `Config` gains `Rank bool` and `RankTokens int`.
- Test: `internal/pipeline/rank_test.go`

**Interfaces:**
- Consumes: `eval.Rank`, `chunks`, `finalists`, `merge`, `needsRanking`, the budget
  (`spend`), `cost()`.
- Produces:
  - `rankImages(r report.Result) (eval.RankFrame, error)`: re-extract the preview from
    where the frame lives (`MovedTo` if set). Full = `Downscaled(768, 82)`. Crop = the
    focus box via `focus.BoxTarget` and `SubjectRect`, capped at 512 around its
    centre; nil if there is no box.
  - `type rankExec interface { Run(ctx context.Context, reqs []rankCall) []rankOut }`,
    where:

    ```go
    rankCall{ID string; Frames []eval.RankFrame}
    rankOut{ID string; R *eval.Ranking; U eval.Usage; Err error}
    ```

    `syncExec{b llm.Backend; concurrency int; maxTokens int}` implements it.
  - `RankSets(ctx context.Context, rep *report.Report, cfg Config, ex rankExec, force bool) error`:
    - for each set in `needsRanking` (all sets with Of ≥ 2 when `force`), build the
      chunk calls (round 1), run them, then the final calls (round 2), run them;
    - write `Order` (file names), `Notes` (by file), `Summary` (from the final or
      single call), `By: "model"`, `Usage` and `CostUSD` into `rep.Sets[i]`;
    - stop starting new sets once `--max-cost` is reached; the rest stay unranked;
    - print one line per set: "ranked set 3 (7 frames): L…23 wins — <summary>".
  - `Rank(ctx context.Context, cfg Config, b llm.Backend, force bool) (*report.Report, error)`:
    for `cull rank`. It loads the report, computes missing looks (below), runs
    `decideAll` → `RankSets` → `decideAll`, writes sidecars and moves as `decide`
    would (labels, as in `DecideOptions`), and saves.
  - `fillLooks(rep *report.Report) int`: for results without a `Look`, decode the
    preview from where the frame lives and set it. It returns the count. `Decide`
    calls it too, so v3 reports upgrade.

- [ ] **Step 1: Write the failing tests** (fake backend, synthetic DNGs from the existing `shoot`/`texturedDNG` helpers)

```go
type rankBackend struct {
	mu    sync.Mutex
	calls int
	order func(n int) []int // 1-based ranking to return for n frames
}

func (b *rankBackend) Name() string { return "fake" }
func (b *rankBackend) Call(_ context.Context, r llm.Request) (*llm.Response, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	n := 0
	for _, p := range r.Parts {
		if strings.HasSuffix(p.Text, ": full frame") {
			n++
		}
	}
	var entries []string
	for _, f := range b.order(n) {
		entries = append(entries, fmt.Sprintf(`{"frame":%d,"strength":"s%d","weakness":"w%d"}`, f, f, f))
	}
	return &llm.Response{JSON: json.RawMessage(`{"ranking":[` + strings.Join(entries, ",") + `],"summary":"best moment"}`),
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 100}}, nil
}

func reverse(n int) []int {
	o := make([]int, n)
	for i := range o {
		o[i] = n - i
	}
	return o
}

// seqShoot: n identical textured frames, all judged sharp 9 by the fake judge backend.
func seqShoot(t *testing.T, n int) (Config, *report.Report) {
	t.Helper()
	dir := t.TempDir()
	for i := 1; i <= n; i++ {
		texturedDNG(t, filepath.Join(dir, fmt.Sprintf("L%07d.DNG", i)))
	}
	c := moveCfg(dir)
	c.MoveCulled, c.WriteXMP, c.Rank = false, false, false
	c.Seq = group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}
	c.Policy.KeepBest, c.Policy.Outranked = 3, eval.ActionReview
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil {
		t.Fatal(err)
	}
	return c, rep
}

func TestRankSetsAppliesModelOrder(t *testing.T) {
	c, rep := seqShoot(t, 5)
	b := &rankBackend{order: reverse}
	if err := RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 2}, false); err != nil {
		t.Fatal(err)
	}
	decideAll(rep, c.Policy, c.Seq)
	s := rep.Sets[0]
	if s.By != "model" || s.Summary != "best moment" || filepath.Base(s.Order[0]) != "L0000005.DNG" || s.CostUSD == 0 {
		t.Fatalf("set %+v", s)
	}
	last := rep.Results[0] // L0000001 ranked last of 5
	if last.Group.Rank != 5 || last.Decision != eval.Review || last.Group.Strength != "s1" {
		t.Fatalf("L1 %+v %s", last.Group, last.Decision)
	}
	calls := b.calls
	RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 2}, false)
	if b.calls != calls {
		t.Fatal("an unchanged set was ranked again")
	}
	RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 2}, true)
	if b.calls == calls {
		t.Fatal("force must re-rank")
	}
}

func TestRankSetsChunksLongSets(t *testing.T) {
	c, rep := seqShoot(t, 9)
	b := &rankBackend{order: reverse}
	RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 2}, false)
	if b.calls != 3 { // 2 chunks (5+4) + final
		t.Fatalf("calls %d", b.calls)
	}
	if o := rep.Sets[0].Order; len(o) != 9 {
		t.Fatalf("merged order covers all 9: %v", o)
	}
}

func TestRankStopsAtBudget(t *testing.T) {
	c, rep := seqShoot(t, 4)
	c.MaxCost = 1e-9
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	if err := RankSets(context.Background(), rep, c, syncExec{b: &rankBackend{order: reverse}, concurrency: 1}, false); !errors.Is(err, llm.ErrBudget) {
		t.Fatalf("budget: %v", err)
	}
}

func TestRankReadsFramesWhereTheyLiveAndFillsLooks(t *testing.T) {
	c, rep := seqShoot(t, 2)
	for i := range rep.Results {
		rep.Results[i].Look = ""
	}
	if n := fillLooks(rep); n != 2 || rep.Results[0].Look == "" {
		t.Fatalf("filled %d", n)
	}
	_ = c
}
```

- [ ] **Step 2: Run** `go test ./internal/pipeline -run 'RankSets|RankStops|RankReads'`. Expected: build failed.
- [ ] **Step 3: Implement** `rank.go` as described in the Interfaces:
  - `syncExec` runs the calls through a worker pool (`concurrency`), calling
    `eval.Rank` for each.
  - `RankSets` builds, per set:
    - for rankable frames (in the stored `Members` capture order, filtered to
      `Group.Rank > 0`), their `RankFrame`s via `rankImages`;
    - for n ≤ 8, one call with ID `"S<id>"`;
    - otherwise chunk calls `"S<id>-C<k>"`, then, from each chunk's top
      `finalists(...)`, a final call `"S<id>-F"`, and `merge`.
  - Cost per set = `cost(cfg, usage, llm.Usage{})`; check the budget before each set
    via `spend`.
  - In `finishRun`, when `cfg.Rank` and a backend is set (sync mode), call `RankSets`
    between two `decideAll`s. Wire the backend in through `Run` by keeping it on a
    private `Config` field (`rankWith llm.Backend`, set in `Run`), and ledger it.
- [ ] **Step 4: Run** `go test -race ./internal/pipeline`. Expected: PASS.
- [ ] **Step 5: Checkpoint.**

---

### Task 9: Ranking with `--batch` (re-attachable)

**Files:**
- Modify: `internal/pipeline/rank.go` (add `batchExec`), `batch.go` (after the judge
  rounds, when `cfg.Rank`: `RankSets` with a `batchExec`)
- Test: `internal/pipeline/batch_test.go`

**Interfaces:**
- Produces: `batchExec{client BatchClient; cfg Config; statePath string}`.
  - Its `Run(ctx, calls)` submits all calls of one round as one Message Batch, with
    custom IDs = `rankCall.ID`.
  - It records `{round-key, batchID, status}` in `<report>.rank-batch.json` before
    polling. The round key is a hash of the sorted call IDs plus their frame files.
  - It polls with `cfg.BatchPoll`, and collects with the schema `eval.RankSchema()`,
    decoding via `eval.DecodeRank`.
  - A non-permutation from a batch is an error for that set: the set stays unranked
    and falls back to scores. There is no resubmission inside a batch.
  - On re-run, a recorded submitted batch for the same round key is re-attached, not
    resubmitted. On success the record is marked collected, and the file is deleted
    when ranking finishes.

- [ ] **Step 1: Write the failing tests** with the existing `fakeBatch`

First, extend `fakeBatch.BatchResults` with a ranking case, as the first case of its
`switch`:

```go
		case r.Req.SchemaName == "ranking":
			n := 0
			for _, p := range r.Req.Parts {
				if strings.HasSuffix(p.Text, ": full frame") {
					n++
				}
			}
			var entries []string
			for k := n; k >= 1; k-- { // reverse order
				entries = append(entries, fmt.Sprintf(`{"frame":%d,"strength":"s","weakness":"w"}`, k))
			}
			res.Response = &llm.Response{JSON: json.RawMessage(`{"ranking":[` + strings.Join(entries, ",") + `],"summary":"batch best"}`),
				Usage: llm.Usage{InputTokens: 1000, OutputTokens: 100}}
```

Then the tests:

```go
func TestRankBatchRoundsAndCost(t *testing.T) {
	c, rep := seqShoot(t, 9)
	p := llm.Price{In: 2, Out: 10}
	c.Price, c.Batch = &p, true
	fb := &fakeBatch{}
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json"}
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 2 || len(fb.submitted[0]) != 2 || len(fb.submitted[1]) != 1 {
		t.Fatalf("want round 1 = 2 chunk requests, round 2 = 1 final; got %d batches", len(fb.submitted))
	}
	s := rep.Sets[0]
	if s.By != "model" || len(s.Order) != 9 || s.Summary != "batch best" {
		t.Fatalf("set %+v", s)
	}
	if want := p.Cost(s.Usage, true); s.CostUSD != want {
		t.Fatalf("batch price: %v, want %v", s.CostUSD, want)
	}
	if _, err := os.Stat(ex.statePath); err == nil {
		t.Fatal("state file left behind after ranking finished")
	}
}

func TestRankBatchReattaches(t *testing.T) {
	c, rep := seqShoot(t, 3)
	c.Batch = true
	fb := &fakeBatch{statusErr: errors.New("network down")}
	ex := batchExec{client: fb, cfg: c, statePath: c.ReportPath + ".rank-batch.json"}
	if err := RankSets(context.Background(), rep, c, ex, false); err == nil {
		t.Fatal("a status failure must surface")
	}
	if _, err := os.Stat(ex.statePath); err != nil {
		t.Fatal("the submitted batch must be recorded before polling")
	}
	fb.statusErr = nil
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if len(fb.submitted) != 1 {
		t.Fatalf("the re-run resubmitted: %d batches", len(fb.submitted))
	}
	if rep.Sets[0].By != "model" {
		t.Fatal("not ranked after re-attaching")
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/pipeline -run RankBatch`. Expected: FAIL (`batchExec` undefined).
- [ ] **Step 3: Implement** `batchExec` as specified. Reuse the polling pattern of
  `collect`. Save the state atomically, as `saveBatchState` does.
- [ ] **Step 4: Run** `go test -race -count=5 ./internal/pipeline -run 'Batch|Rank'`. Expected: PASS.
- [ ] **Step 5: Checkpoint.**

---

### Task 10: CLI: flags, `cull rank`, estimates; sidecar and apply-c1 `cull:best`

**Files:**
- Modify:
  - `internal/cli/root.go`: replace `--burst-gap`/`--burst-hash` with `--seq-gap`
    (60s) and `--seq-look` (`group.DefaultLook`); `sharedOpts.base` fills
    `cfg.Seq`.
  - `internal/cli/policy.go`: replace `--duplicates` with `--outranked` (review) and
    add `--keep-best` (3).
  - `internal/cli/cull.go` (`judge`): `--no-rank`; the estimate adds a ranking upper
    bound.
  - `internal/cli/decide.go`: fills looks via `pipeline.Decide`.
  - create `internal/cli/rank.go`;
  - `internal/labels/sidecar.go`: `cull:best`;
  - `internal/c1/c1.go`: `cull:best`.
- Test: `internal/cli/cli_test.go`, `internal/labels/sidecar_test.go`, `internal/c1/c1_test.go`

**Interfaces:**
- Produces:
  - `cull rank <dir> [--estimate] [--max-cost USD] [--batch] [--force]`, plus the
    backend flags shared with judge (`--backend`, `--model`, `--claude-bin`,
    `--base-url`, …). Factor the backend flags into a helper used by both commands.
  - `llm.EstimateRank(calls int, p Price, batch bool) (usd float64, in, out int)`, at
    10k input and 1k output tokens per call.

- [ ] **Step 1: Write the failing tests**

```go
func TestSidecarBestKeyword(t *testing.T) { // labels/sidecar_test.go
	r := report.Result{Decision: eval.Keep, Group: &report.Group{ID: 1, Size: 4, Rank: 2, Of: 4, Best: true}}
	if sc := Sidecar(r, Entry{}, 1, false); !reflect.DeepEqual(sc.Keywords, []string{"cull:keep", "cull:best"}) {
		t.Fatalf("%v", sc.Keywords)
	}
	r.Group.Best = false
	if sc := Sidecar(r, Entry{}, 1, false); len(sc.Keywords) != 1 {
		t.Fatalf("%v", sc.Keywords)
	}
}
```

- `c1_test.go`: a result with `Group.Best` gets `"cull:best"` in its block; the
  others don't.
- `cli_test.go`:
  - `--burst-gap`, `--burst-hash` and `--duplicates` are unknown flags;
  - `--keep-best -1`, `--outranked delete` and `--seq-look 2` are rejected;
  - `judge --estimate` on 2 tiny DNGs prints "ranking ≤ $…";
  - `rank --estimate` on a report with one 2-frame set prints "1 set, 1 call";
  - `rank` on a v3 report (a fixture JSON written by the test with
    `schema_version: 3` and no `look`) with a fake claude backend, which answers the
    rank schema, saves v4 with looks and a ranked set.

  Extend the `fakeClaudeCull` script, or add a `fakeClaudeRank` script that returns a
  valid ranking for requests whose schema has `ranking`.

- [ ] **Step 2: Run the tests to verify they fail** (unsandboxed for `internal/cli`).
- [ ] **Step 3: Implement.**
  - The `cull rank` command: load the report, `Rank`/`RankSets` through
    `pipeline.Rank` with a `syncExec` or `batchExec`, then print the summary and the
    report's cost.
  - Estimates:
    - `rank --estimate` counts the chunk and final calls from `needsRanking` sets;
    - `judge --estimate` assumes every frame is in 8-frame sets: ⌈n/8⌉ calls,
      labelled "≤".
  - `Sidecar`: when `r.Group != nil && r.Group.Best`, append `"cull:best"`.
  - `c1.Script`: likewise.
- [ ] **Step 4: Run** the CLI suite (unsandboxed), plus `labels`, `c1` and `pipeline`. Expected: PASS.
- [ ] **Step 5: Checkpoint.**

---

### Task 11: `calibrate` sets section

**Files:**
- Modify: `internal/calib/calib.go`, `internal/cli/calibrate.go`
- Test: `internal/calib/calib_test.go`

**Interfaces:**
- Produces:
  - `calib.Sets(rep *report.Report, labels map[string]string, keepBest int) SetStats`,
    where `SetStats{Kept, KeptRankedOut, Culled, CulledInBest, Sets int}`. It counts
    over multi-frame sets containing labelled frames.
  - `calib.SweepKeepBest(rep, labels, 1..5)`, recomputed from `Group.Rank`
    (`Best = 1 ≤ Rank ≤ k`).
  - `calib.FormatSets(w, stats, sweep)`.

- [ ] **Step 1: Write the failing test**

```go
func TestSetsAgreement(t *testing.T) {
	g := func(rank int, best bool) *report.Group { return &report.Group{ID: 1, Size: 4, Rank: rank, Of: 4, Best: best} }
	rep := &report.Report{KeepBest: 2, Results: []report.Result{
		{File: "/s/A.DNG", Group: g(1, true)}, {File: "/s/B.DNG", Group: g(2, true)},
		{File: "/s/C.DNG", Group: g(3, false)}, {File: "/s/D.DNG", Group: g(4, false)},
		{File: "/s/E.DNG"}, // not in a set: ignored
	}}
	labels := map[string]string{"A.DNG": "keep", "B.DNG": "cull", "C.DNG": "keep", "D.DNG": "review", "E.DNG": "keep"}
	s := Sets(rep, labels, 2)
	if s.Kept != 2 || s.KeptRankedOut != 1 || s.Culled != 2 || s.CulledInBest != 1 || s.Sets != 1 {
		t.Fatalf("%+v", s)
	}
	sw := SweepKeepBest(rep, labels, []int{1, 3})
	if sw[0].KeptRankedOut != 1 || sw[1].KeptRankedOut != 0 || sw[1].CulledInBest != 1 {
		t.Fatalf("%+v", sw)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/calib`. Expected: build failed.
- [ ] **Step 3: Implement.** `calibrate` prints the sets section after the matrix when
  the report has sets. The labels are the `Verdicts` map already loaded.
- [ ] **Step 4: Run** `go test -race ./internal/calib` and the CLI calibrate test. Expected: PASS.
- [ ] **Step 5: Checkpoint.**

---

### Task 12: The review sheet: badges, Sets filter, filmstrip

**Files:**
- Modify: `internal/review/review.go` (card carries `Group` as is, plus the set
  summary per card), `internal/review/page.html`
- Test: `internal/review/review_test.go`; `$SCRATCH/jsdom/check.mjs` (add checks)

**Interfaces:**
- Consumes: `report.Group`, `report.Set` (summary by set ID).
- Produces: card JSON `group` (as in the report), `set_summary`, and page data
  `keep_best`.

- [ ] **Step 1: Write the failing tests**
  - Go: a page built from a report with a 3-frame set contains `"rank":2` and
    `"set_summary"`.
  - jsdom: add checks 16–19 to `check.mjs` against a sheet built from a synthetic
    ranked report:
    - (16) cards show "set 1 · #2/3", and the best cards carry a `.best` badge;
    - (17) the Sets row filters Best and Outranked;
    - (18) the detail view shows the strength, weakness and summary, and a filmstrip
      with 3 thumbnails, the current one marked;
    - (19) a filmstrip click moves to that frame.

    Build the synthetic ranked report with a throwaway Go program in the scratchpad
    that calls `review.Build`, or by editing the report JSON of `$SCRATCH/sheet-test`
    to add groups and sets.
- [ ] **Step 2: Run them to verify they fail.**
- [ ] **Step 3: Implement.**
  - Badge text: `` `set ${g.id} · #${g.rank}/${g.of}` ``.
  - A `.best` pill: a small uppercase "best", outlined, never star-shaped.
  - `.card.set-odd` / `.set-even` edge tint.
  - A third filter group `sets`: All / Best / Outranked (`g && g.rank > data.keep_best
    && data.keep_best > 0`), combined in `matches`.
  - Detail: `row(info, "set", …)`, strength and weakness rows, and the summary; a
    filmstrip `div.strip` of `img` thumbs for the cards with the same `group.id`,
    where clicking sets `sel` to that card's index in `visible()` if present.
- [ ] **Step 4: Rebuild; run the harness (all checks PASS) and the Go review tests.**
- [ ] **Step 5: Checkpoint.**

---

### Task 13: Docs, verification, final review

- [ ] **Step 1: README.**
  - A section "Sequences and best of set": the grouping rule, `--seq-gap` and
    `--seq-look`, ranking and its cost, `--keep-best` and `--outranked`, `cull rank`,
    the reuse rule, and `cull:best`.
  - Replace the burst mentions and the `--burst-*`/`--duplicates` flags.
- [ ] **Step 2: CLAUDE.md.**
  - Layout: group = look plus sequences; `eval/rank.go`; `pipeline/rank.go` and
    `rankplan.go`.
  - The `--seq-look` measurement (from Task 3).
  - Unverified: ranking quality and stability, and card timestamps.
  - Roadmap: burst grouping → sequences.
- [ ] **Step 3: Full verification.**
  - Run unsandboxed: `go test -race -count=1 ./...`.
  - Then `go vet ./...` and `gofmt -l .`.
  - Then the jsdom harness.
  - Expected: all pass and clean.
- [ ] **Step 4: Live check, only with the user's approval (it costs money).**
  - `./bin/cull rank photos` on the user's judged report. It upgrades v3 → v4, fills
    looks, and ranks the sets found, for a few cents.
  - Then `./bin/cull calibrate photos/cull-report.json` for the sets section, and
    `./bin/cull review photos`.
  - Before running it, tell the user that `rank` rewrites `photos/cull-report.json`,
    upgrading it in place.
- [ ] **Step 5: Final whole-branch review.** Use the fresh reviewer on the most capable
  model, with this Review Focus. Run one fix pass, then
  superpowers:finishing-a-development-branch.
