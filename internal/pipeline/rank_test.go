package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/group"
	"github.com/jefflaplante/gophotocull/internal/llm"
	"github.com/jefflaplante/gophotocull/internal/report"
)

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
	// Not in the brief: seqShoot's config has no price, and without one cost() is 0,
	// so the brief's `s.CostUSD == 0` check could never pass.
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
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
	if n, err := fillLooks(context.Background(), rep); n != 2 || err != nil || rep.Results[0].Look == "" {
		t.Fatalf("filled %d, %v", n, err)
	}
	_ = c
}

func jpegSize(t *testing.T, b []byte) (int, int) {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

// Each frame goes to the model as a 768 px full view plus, when judge had a focus
// box, a crop of at most 512 px at native resolution, read from where the frame
// lives now.
func TestRankImagesSizesAndMovedFrames(t *testing.T) {
	_, rep := seqShoot(t, 2)
	r := rep.Results[0]
	f, err := rankImages(r)
	if err != nil || f.Crop != nil {
		t.Fatalf("no focus box: crop %d bytes, err %v", len(f.Crop), err)
	}
	if w, h := jpegSize(t, f.Full); max(w, h) != 768 {
		t.Fatalf("full %dx%d, want a 768 px long edge", w, h)
	}

	r.FocusTarget = &report.FocusTarget{Source: "face", Box: &eval.NormBox{Left: 0.45, Top: 0.4, Right: 0.55, Bottom: 0.55}}
	moved := filepath.Join(filepath.Dir(r.File), CulledDir, filepath.Base(r.File))
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(r.File, moved); err != nil {
		t.Fatal(err)
	}
	r.MovedTo = moved
	if f, err = rankImages(r); err != nil {
		t.Fatalf("moved frame: %v", err)
	}
	if w, h := jpegSize(t, f.Crop); w != 512 || h != 512 { // the 768 px subject rect, capped
		t.Fatalf("crop %dx%d, want 512x512", w, h)
	}

	rep.Results[0] = r
	rep.Results[0].Look, rep.Results[1].Look = "", ""
	if n, err := fillLooks(context.Background(), rep); n != 2 || err != nil || rep.Results[0].Look == "" {
		t.Fatalf("filled %d (%v), the moved frame too", n, err)
	}
}

// Ctrl-C (a cancelled context) reaches the rank stage's image work: no frame is
// decoded once it is cancelled, and cull rank returns the cancellation.
func TestRankImageWorkHonoursCancel(t *testing.T) {
	c, rep := seqShoot(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	j := &setRank{set: &rep.Sets[0], files: rep.Sets[0].Members}
	byFile := map[string]int{}
	for i, r := range rep.Results {
		byFile[r.File] = i
	}
	if err := loadRankImages(ctx, rep, c, []*setRank{j}, byFile); !errors.Is(err, context.Canceled) {
		t.Fatalf("loadRankImages: %v", err)
	}
	for pos, f := range j.frames {
		if f.Full != nil {
			t.Fatalf("frame %d decoded after cancel", pos)
		}
	}

	for i := range rep.Results { // a v3 report: looks to compute
		rep.Results[i].Look = ""
	}
	if n, err := fillLooks(ctx, rep); n != 0 || !errors.Is(err, context.Canceled) || rep.Results[0].Look != "" {
		t.Fatalf("fillLooks: %d, %v", n, err)
	}
	if err := rep.Save(c.ReportPath); err != nil {
		t.Fatal(err)
	}
	b := &rankBackend{order: reverse}
	if _, err := Rank(ctx, c, b, false); !errors.Is(err, context.Canceled) || b.calls != 0 {
		t.Fatalf("rank: %v, %d calls", err, b.calls)
	}
	if _, err := report.Load(c.ReportPath); err != nil {
		t.Fatalf("report after a cancelled rank: %v", err)
	}
}

// A resumed judge keeps the stored rankings and what they cost: an unchanged set
// is not ranked again, and rank_cost_usd carries over.
func TestResumeKeepsRankingsAndRankCost(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 3; i++ {
		texturedDNG(t, filepath.Join(dir, fmt.Sprintf("L%07d.DNG", i)))
	}
	odd := image.NewGray(image.Rect(0, 0, 1600, 1067)) // upper half white: L4 is in no set
	for i := range odd.Pix[:len(odd.Pix)/2] {
		odd.Pix[i] = 255
	}
	dngWith(t, filepath.Join(dir, "L0000004.DNG"), odd)
	c := moveCfg(dir)
	c.MoveCulled, c.WriteXMP, c.Rank = false, false, true
	c.Seq = group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}
	c.Policy.KeepBest, c.Policy.Outranked = 1, eval.ActionReview
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	b := &judgeRankBackend{fakeBackend: fakeBackend{status: "sharp"}, rank: rankBackend{order: reverse}}
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	rep, err := report.Load(c.ReportPath)
	if err != nil || len(rep.Sets) != 1 || len(rep.Sets[0].Members) != 3 || rep.Sets[0].By != "model" || rep.RankCostUSD == 0 || b.rank.calls != 1 {
		t.Fatalf("setup: %v %+v, %d rank calls", err, rep.Sets, b.rank.calls)
	}
	rankCost := rep.RankCostUSD
	for i := range rep.Results { // L4 failed (say its preview was unreadable): resume retries it
		if r := &rep.Results[i]; filepath.Base(r.File) == "L0000004.DNG" {
			r.Error, r.Evaluation, r.Decision, r.CostUSD, r.Usage = "preview: simulated", nil, "", 0, eval.Usage{}
		}
	}
	if err := rep.Save(c.ReportPath); err != nil {
		t.Fatal(err)
	}
	before := rep.Cost()

	c.Resume = true
	rep2, _, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	if b.rank.calls != 1 {
		t.Fatalf("resume ranked the unchanged set again: %d rank calls", b.rank.calls)
	}
	if rep2.RankCostUSD != rankCost || len(rep2.Sets) != 1 || rep2.Sets[0].By != "model" || rep2.KeepBest != 1 {
		t.Fatalf("resumed report: rank cost %v (want %v), sets %+v, keep best %d", rep2.RankCostUSD, rankCost, rep2.Sets, rep2.KeepBest)
	}
	l4 := result(t, rep2, "L0000004.DNG")
	if l4.Error != "" || l4.CostUSD == 0 || math.Abs(rep2.Cost()-(before+l4.CostUSD)) > 1e-12 {
		t.Fatalf("cost %v, want %v + L4's %v", rep2.Cost(), before, l4.CostUSD)
	}
}

// Every ranking's cost stays in the report's total, even after a re-rank replaces
// the order or a regrouping drops the set.
func TestRankCostIsCumulative(t *testing.T) {
	c, rep := seqShoot(t, 3)
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	ex := syncExec{b: &rankBackend{order: reverse}, concurrency: 1}
	one := p.Cost(llm.Usage{InputTokens: 1000, OutputTokens: 100}, false)
	if err := RankSets(context.Background(), rep, c, ex, false); err != nil {
		t.Fatal(err)
	}
	if s := rep.Sets[0]; s.CostUSD != one || s.Usage.InputTokens != 1000 || rep.RankCostUSD != one {
		t.Fatalf("first ranking: set %+v, rank cost %v", s, rep.RankCostUSD)
	}
	decideAll(rep, c.Policy, c.Seq)
	if err := RankSets(context.Background(), rep, c, ex, true); err != nil {
		t.Fatal(err)
	}
	if s := rep.Sets[0]; s.CostUSD != 2*one || s.Usage.InputTokens != 2000 || rep.RankCostUSD != 2*one {
		t.Fatalf("a re-rank adds to the set's cost: set %+v, rank cost %v", s, rep.RankCostUSD)
	}
	decideAll(rep, c.Policy, group.Options{}) // grouping off: the set is gone
	if len(rep.Sets) != 0 || rep.Cost() != 2*one {
		t.Fatalf("the paid rankings left the total: sets %d, cost %v", len(rep.Sets), rep.Cost())
	}
}

// A call that never returns a clean order leaves the set unranked (by scores), but
// its spend is recorded.
func TestRankFailedCallLeavesSetUnranked(t *testing.T) {
	c, rep := seqShoot(t, 3)
	b := &rankBackend{order: func(int) []int { return []int{1, 1, 2} }} // not a permutation
	if err := RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 1}, false); err != nil {
		t.Fatalf("a failed set is not a stop: %v", err)
	}
	if s := rep.Sets[0]; s.By == "model" || len(s.Order) != 0 || s.Usage.InputTokens != 2000 || b.calls != 2 {
		t.Fatalf("set %+v after %d calls", s, b.calls)
	}
}

// quotaRank answers rank calls and reports the subscription quota crossed.
type quotaRank struct{ rankBackend }

func (q *quotaRank) Call(ctx context.Context, r llm.Request) (*llm.Response, error) {
	resp, _ := q.rankBackend.Call(ctx, r)
	return resp, llm.ErrQuotaStop
}

// twoSets: seqShoot(4) regrouped as {L1, L2} and {L3, L4}.
func twoSets(t *testing.T) (Config, *report.Report) {
	t.Helper()
	c, rep := seqShoot(t, 4)
	bright := make([]uint8, 192) // upper half bright: far from the textured look
	for i := range bright[:96] {
		bright[i] = 255
	}
	rep.Results[2].Look, rep.Results[3].Look = report.EncodeLook(bright), report.EncodeLook(bright)
	decideAll(rep, c.Policy, c.Seq)
	if len(rep.Sets) != 2 {
		t.Fatalf("setup: %d sets", len(rep.Sets))
	}
	return c, rep
}

// Once a set's cost reaches --max-cost, no further set starts; they stay by scores.
func TestRankBudgetLeavesLaterSetsByScores(t *testing.T) {
	c, rep := twoSets(t)
	c.Concurrency, c.MaxCost = 1, 1e-9
	p := llm.Price{In: 2, Out: 10}
	c.Price = &p
	b := &rankBackend{order: reverse}
	err := RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 1}, false)
	if !errors.Is(err, llm.ErrBudget) || b.calls != 1 || rep.Sets[0].By != "model" || rep.Sets[1].By == "model" {
		t.Fatalf("err %v, calls %d, sets %+v", err, b.calls, rep.Sets)
	}
}

func TestRankQuotaStopKeepsAnswerAndStartsNoMoreSets(t *testing.T) {
	c, rep := twoSets(t)
	b := &quotaRank{rankBackend{order: reverse}}
	err := RankSets(context.Background(), rep, c, syncExec{b: b, concurrency: 1}, false)
	if !errors.Is(err, llm.ErrQuotaStop) || b.calls != 1 || rep.Sets[0].By != "model" || rep.Sets[1].By == "model" {
		t.Fatalf("err %v, calls %d, sets %+v", err, b.calls, rep.Sets)
	}
}

// judgeRankBackend answers evaluations like fakeBackend and rank calls like rankBackend.
type judgeRankBackend struct {
	fakeBackend
	rank rankBackend
}

func (b *judgeRankBackend) Call(ctx context.Context, r llm.Request) (*llm.Response, error) {
	if r.SchemaName == "ranking" {
		return b.rank.Call(ctx, r)
	}
	return b.fakeBackend.Call(ctx, r)
}

// judge ranks the sets at the end of the run with its own backend, and sidecars
// follow the decisions after ranking.
func TestJudgeRanksSetsAtTheEnd(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 4; i++ {
		texturedDNG(t, filepath.Join(dir, fmt.Sprintf("L%07d.DNG", i)))
	}
	c := moveCfg(dir)
	c.MoveCulled, c.Rank = false, true // sidecars on
	c.Seq = group.Options{Gap: time.Minute, MaxLook: group.DefaultLook}
	c.Policy.KeepBest, c.Policy.Outranked = 1, eval.ActionReview
	b := &judgeRankBackend{fakeBackend: fakeBackend{status: "sharp"}, rank: rankBackend{order: reverse}}
	rep, usage, err := Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	if s := rep.Sets[0]; s.By != "model" || filepath.Base(s.Order[0]) != "L0000004.DNG" || b.rank.calls != 1 {
		t.Fatalf("set %+v after %d rank calls", s, b.rank.calls)
	}
	if usage.InputTokens != 4*100+1000 {
		t.Fatalf("this run's usage includes the ranking: %+v", usage)
	}
	// By scores L1 was best and L4 outranked; the model reverses that.
	l1, l4 := result(t, rep, "L0000001.DNG"), result(t, rep, "L0000004.DNG")
	if l1.Decision != eval.Review || l4.Decision != eval.Keep {
		t.Fatalf("L1 %s, L4 %s", l1.Decision, l4.Decision)
	}
	if x, _ := os.ReadFile(l4.XMP); !strings.Contains(string(x), `xmp:Label="Green"`) {
		t.Fatalf("L4 sidecar:\n%s", x)
	}
	if x, _ := os.ReadFile(l1.XMP); !strings.Contains(string(x), `xmp:Label="Yellow"`) {
		t.Fatalf("L1 sidecar:\n%s", x)
	}
	// L2 was outranked by scores (the first decide) and stays outranked by the model
	// (no change in the second): its sidecar must still leave keep.
	if l2 := result(t, rep, "L0000002.DNG"); l2.Decision != eval.Review {
		t.Fatalf("L2 %s", l2.Decision)
	} else if x, _ := os.ReadFile(l2.XMP); !strings.Contains(string(x), `xmp:Label="Yellow"`) {
		t.Fatalf("L2 sidecar:\n%s", x)
	}
	if saved, err := report.Load(c.ReportPath); err != nil || saved.Sets[0].By != "model" {
		t.Fatalf("saved: %v", err)
	}
}

// decide and rank load a schema-v3 report (no looks), compute the looks from the
// DNGs and save v4; rank also ranks the sets.
func TestRankAndDecideUpgradeV3Reports(t *testing.T) {
	c, rep := seqShoot(t, 3)
	downgrade := func() {
		t.Helper()
		rep.SchemaVersion, rep.Sets = 3, nil
		for i := range rep.Results {
			rep.Results[i].Look, rep.Results[i].Group = "", nil
		}
		if err := rep.Save(c.ReportPath); err != nil {
			t.Fatal(err)
		}
	}
	downgrade()
	if _, err := Decide(c.ReportPath, DecideOptions{Policy: c.Policy, Seq: c.Seq}, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := report.Load(c.ReportPath)
	if err != nil || got.SchemaVersion != report.SchemaVersion || got.Results[0].Look == "" || len(got.Sets) != 1 {
		t.Fatalf("decide: %v %+v", err, got)
	}

	downgrade()
	b := &rankBackend{order: reverse}
	ranked, err := Rank(context.Background(), c, b, false)
	if err != nil || ranked.Sets[0].By != "model" || b.calls != 1 {
		t.Fatalf("rank: %v, %d calls", err, b.calls)
	}
	got, err = report.Load(c.ReportPath)
	if err != nil || got.SchemaVersion != report.SchemaVersion || got.Results[0].Look == "" || got.Sets[0].By != "model" {
		t.Fatalf("saved: %v %+v", err, got)
	}
	if g := result(t, got, "L0000001.DNG").Group; g.Rank != 3 || g.By != "model" {
		t.Fatalf("L1 %+v", g)
	}
}
