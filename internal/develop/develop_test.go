package develop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/develop/lctest"
	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/xmp"
)

// shoot is a synthetic shoot whose keeps are in keep/, developed by the fake LightCraft.
type shoot struct {
	dir, out, log string
	keeps         []Keep
	opts          Options
}

// newShoot writes one synthetic DNG per name into keep/: an LEICA M10-R unless the
// name contains M11.
func newShoot(t *testing.T, names ...string) *shoot {
	t.Helper()
	dir := t.TempDir()
	s := &shoot{dir: dir, out: filepath.Join(dir, "export"), log: filepath.Join(t.TempDir(), "lc.log")}
	for i, n := range names {
		model := "LEICA M10-R"
		if strings.Contains(n, "M11") {
			model = "LEICA M11-P"
		}
		p := filepath.Join(dir, "keep", n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, dngtest.Build(t, dngtest.Fixture{Model: model, Payload: []byte{byte(i)}}), 0o644); err != nil {
			t.Fatal(err)
		}
		s.keeps = append(s.keeps, Keep{Name: n, Path: p})
	}
	t.Setenv(lctest.Env, "1")
	t.Setenv("FAKE_LC_LOG", s.log)
	t.Setenv("FAKE_LC_CRASH_AFTER", "")
	t.Setenv("FAKE_LC_TAG", "v1")
	s.opts = Options{
		StatePath:  filepath.Join(dir, StateName),
		Work:       filepath.Join(dir, WorkName),
		Out:        s.out,
		LightCraft: os.Args[0],
		Recipe:     DefaultRecipe(),
		Jobs:       1,
		Chunk:      2,
	}
	return s
}

func (s *shoot) sidecar(t *testing.T, name string, ev float64) {
	t.Helper()
	p := xmp.Path(filepath.Join(s.dir, "keep", name))
	if err := xmp.Write(p, xmp.Sidecar{Label: "Green", ExposureEV: &ev}, true); err != nil {
		t.Fatal(err)
	}
}

func (s *shoot) develop(t *testing.T, ctx context.Context) (*Plan, Summary, error) {
	t.Helper()
	plan, err := Prepare(s.opts, s.keeps)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Run(ctx, s.opts, plan)
	return plan, sum, err
}

// runs is what the fake LightCraft was asked, one script per invocation.
func (s *shoot) runs(t *testing.T) [][]Command {
	t.Helper()
	f, err := os.Open(s.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]Command
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var cmds []Command
		json.Unmarshal(sc.Bytes(), &cmds)
		out = append(out, cmds)
	}
	return out
}

// exported lists the base names of the photos the fake exported, over every run.
func (s *shoot) exported(t *testing.T) []string {
	var names []string
	for _, run := range s.runs(t) {
		for _, c := range run {
			if c.Command == "app.export" {
				// .cull-develop-<hex>.<stem>.jpg
				_, rest, _ := strings.Cut(strings.TrimPrefix(filepath.Base(c.Params["path"].(string)), tempPrefix), ".")
				names = append(names, strings.TrimSuffix(rest, ".jpg"))
			}
		}
	}
	sort.Strings(names)
	return names
}

func (s *shoot) jpeg(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.out, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func names(xs []Failure) []string {
	var out []string
	for _, x := range xs {
		out = append(out, x.Name)
	}
	sort.Strings(out)
	return out
}

func hiddenIn(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestRunDevelopsKeeps(t *testing.T) {
	s := newShoot(t, "A.DNG", "B.DNG", "C.DNG")
	s.sidecar(t, "A.DNG", 0.5)
	plan, sum, err := s.develop(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Frames) != 3 || plan.UpToDate != 0 {
		t.Fatalf("plan: %d to develop, %d up to date", len(plan.Frames), plan.UpToDate)
	}
	if sum.Developed != 3 || len(sum.Failures) != 0 || len(sum.Exports) != 3 {
		t.Fatalf("summary %+v", sum)
	}
	for _, n := range []string{"A", "B", "C"} {
		if got := s.jpeg(t, n+".jpg"); !strings.Contains(got, n+".DNG") {
			t.Errorf("%s.jpg holds %q", n, got)
		}
	}
	if h := hiddenIn(t, s.out); len(h) != 0 {
		t.Errorf("temps left in the export folder: %v", h)
	}
	if _, err := os.Stat(s.opts.Work); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("work folder left after a clean run: %v", err)
	}
	// Two chunks of at most 2: each is an import run, then a develop run.
	runs := s.runs(t)
	if len(runs) != 4 {
		t.Fatalf("%d LightCraft runs, want 4", len(runs))
	}
	if runs[0][0].Command != "library.xmpPreferences" || runs[0][0].Params["autoWrite"] != false {
		t.Errorf("first command %+v: LightCraft must never write sidecars beside the DNGs", runs[0][0])
	}
	dev := runs[1]
	if dev[0].Command != "preset.import" {
		t.Errorf("develop run starts with %s, want preset.import", dev[0].Command)
	}
	var setEV []any
	for _, c := range dev {
		if c.Command == "develop.set" {
			setEV = append(setEV, c.Params["value"])
		}
	}
	if len(setEV) != 1 || setEV[0] != 0.8 {
		t.Errorf("exposure set to %v, want [0.8] (preset +0.3, A's sidecar +0.5)", setEV)
	}
	st, err := loadState(s.opts.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	a := st.Frames["A.DNG"]
	if a == nil || a.Status != statusDone || a.JPEG != filepath.Join(s.out, "A.jpg") || a.SHA256 == "" || a.Preset != "leica-m10r-std" || a.EV == nil || *a.EV != 0.5 {
		t.Errorf("A's record: %+v", a)
	}
	if st.Recipe != DefaultRecipe() {
		t.Errorf("recipe recorded %+v", st.Recipe)
	}
}

func TestRunIsUpToDate(t *testing.T) {
	s := newShoot(t, "A.DNG", "B.DNG")
	if _, _, err := s.develop(t, context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(s.runs(t))
	plan, sum, err := s.develop(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Frames) != 0 || plan.UpToDate != 2 || sum.Developed != 0 || sum.UpToDate != 2 {
		t.Errorf("re-run: plan %d/%d, summary %+v", len(plan.Frames), plan.UpToDate, sum)
	}
	if len(s.runs(t)) != before {
		t.Error("an up-to-date re-run started LightCraft")
	}
	// A deleted export is developed again.
	os.Remove(filepath.Join(s.out, "B.jpg"))
	plan, _, _ = s.develop(t, context.Background())
	if len(plan.Frames) != 1 || plan.Frames[0].Name != "B.DNG" {
		t.Errorf("after deleting B.jpg: %+v", plan.Frames)
	}
	// --force develops everything.
	s.opts.Force = true
	if plan, _, _ = s.develop(t, context.Background()); len(plan.Frames) != 2 {
		t.Errorf("force: %d frames", len(plan.Frames))
	}
}

// LightCraft dying mid-chunk (a crash, the machine sleeping, kill -9) loses only the
// frames not yet exported: the re-run develops just those.
func TestRunResumesAfterCrash(t *testing.T) {
	s := newShoot(t, "A.DNG", "B.DNG", "C.DNG")
	s.opts.Chunk = 10
	t.Setenv("FAKE_LC_CRASH_AFTER", "1")
	_, sum, err := s.develop(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Developed != 1 || len(sum.Failures) != 2 {
		t.Fatalf("after the crash: %+v", sum)
	}
	if !strings.Contains(sum.Failures[0].Err, "stopped before exporting") {
		t.Errorf("failure text %q", sum.Failures[0].Err)
	}
	if h := hiddenIn(t, s.out); len(h) != 0 {
		t.Errorf("temps left: %v", h)
	}
	t.Setenv("FAKE_LC_CRASH_AFTER", "")
	os.Remove(s.log)
	plan, sum, err := s.develop(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if plan.UpToDate != 1 || sum.Developed != 2 || len(sum.Failures) != 0 {
		t.Errorf("re-run: up to date %d, %+v", plan.UpToDate, sum)
	}
	if got := s.exported(t); strings.Join(got, ",") != "B,C" {
		t.Errorf("re-run exported %v, want B,C", got)
	}
}

func TestRunReportsFailures(t *testing.T) {
	s := newShoot(t, "A_BADIMPORT.DNG", "B_BADEXPORT.DNG", "C.DNG")
	s.opts.Chunk = 10
	_, sum, err := s.develop(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Developed != 1 || strings.Join(names(sum.Failures), ",") != "A_BADIMPORT.DNG,B_BADEXPORT.DNG" {
		t.Fatalf("summary %+v", sum)
	}
	for _, f := range sum.Failures {
		want := map[string]string{"A_BADIMPORT.DNG": "unrecognized file format", "B_BADEXPORT.DNG": "render failed"}[f.Name]
		if !strings.Contains(f.Err, want) {
			t.Errorf("%s: %q, want it to say %q", f.Name, f.Err, want)
		}
	}
	if len(sum.Logs) != 1 {
		t.Errorf("logs kept: %v, want the failed chunk's", sum.Logs)
	}
	// Failures are retried; the export that worked isn't redone.
	plan, err := Prepare(s.opts, s.keeps)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Frames) != 2 || plan.UpToDate != 1 {
		t.Errorf("re-run plan: %d to develop, %d up to date", len(plan.Frames), plan.UpToDate)
	}
}

// Changing a keep's sidecar (a new EV from review, say) develops it again, replacing
// the JPEG cull exported.
func TestRunRedevelopsChangedSidecar(t *testing.T) {
	s := newShoot(t, "A.DNG", "B.DNG")
	if _, _, err := s.develop(t, context.Background()); err != nil {
		t.Fatal(err)
	}
	s.sidecar(t, "A.DNG", -0.7)
	t.Setenv("FAKE_LC_TAG", "v2")
	plan, sum, err := s.develop(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Frames) != 1 || plan.Frames[0].Name != "A.DNG" || sum.Developed != 1 || len(sum.Failures) != 0 {
		t.Fatalf("plan %+v summary %+v", plan.Frames, sum)
	}
	if got := s.jpeg(t, "A.jpg"); !strings.Contains(got, "v2") {
		t.Errorf("A.jpg not replaced: %q", got)
	}
	if got := s.jpeg(t, "B.jpg"); !strings.Contains(got, "v1") {
		t.Errorf("B.jpg changed: %q", got)
	}
}

// A JPEG cull didn't export is never replaced.
func TestRunLeavesForeignJPEG(t *testing.T) {
	s := newShoot(t, "A.DNG", "B.DNG")
	os.MkdirAll(s.out, 0o755)
	os.WriteFile(filepath.Join(s.out, "A.jpg"), []byte("the retoucher's"), 0o644)
	_, sum, err := s.develop(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Developed != 1 || len(sum.Failures) != 1 || sum.Failures[0].Name != "A.DNG" || !strings.Contains(sum.Failures[0].Err, "exists") {
		t.Fatalf("summary %+v", sum)
	}
	if got := s.jpeg(t, "A.jpg"); got != "the retoucher's" {
		t.Errorf("foreign A.jpg replaced: %q", got)
	}
	if h := hiddenIn(t, s.out); len(h) != 0 {
		t.Errorf("temps left: %v", h)
	}
}

func TestRunRemovesStaleTemps(t *testing.T) {
	s := newShoot(t, "A.DNG")
	os.MkdirAll(s.out, 0o755)
	stale := filepath.Join(s.out, ".cull-develop-0123abcd.A.jpg")
	other := filepath.Join(s.out, ".DS_Store")
	os.WriteFile(stale, []byte{0xFF, 0xD8}, 0o644)
	os.WriteFile(other, nil, 0o644)
	if _, _, err := s.develop(t, context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale temp kept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("someone else's hidden file removed")
	}
}

// Ctrl-C stops LightCraft and records nothing for the frame it was on.
func TestRunCancel(t *testing.T) {
	s := newShoot(t, "A.DNG", "B_SLOW.DNG")
	s.opts.Chunk = 10
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 200; i++ { // until A's export lands
			if _, err := os.Stat(filepath.Join(s.out, "A.jpg")); err == nil {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	_, sum, err := s.develop(t, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("cancel took %v", d)
	}
	if sum.Developed != 1 || len(sum.Failures) != 0 {
		t.Errorf("summary %+v", sum)
	}
	st, _ := loadState(s.opts.StatePath)
	if st.Frames["B_SLOW.DNG"] != nil {
		t.Errorf("the interrupted frame was recorded: %+v", st.Frames["B_SLOW.DNG"])
	}
	if h := hiddenIn(t, s.out); len(h) != 0 {
		t.Errorf("temps left: %v", h)
	}
}

func TestPrepareNoPreset(t *testing.T) {
	s := newShoot(t, "A.DNG", "B_M11.DNG")
	plan, err := Prepare(s.opts, s.keeps)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.NoPreset) != 1 || plan.NoPreset["LEICA M11-P"] != 1 {
		t.Errorf("NoPreset %v", plan.NoPreset)
	}
	if _, err := Run(context.Background(), s.opts, plan); err != nil {
		t.Fatal(err)
	}
	var sawAuto, sawPresetFor bool
	for _, run := range s.runs(t) {
		for _, c := range run {
			sawAuto = sawAuto || c.Command == "develop.auto"
			sawPresetFor = sawPresetFor || c.Command == "preset.apply"
		}
	}
	if !sawAuto || !sawPresetFor {
		t.Errorf("auto %v preset %v: want auto for the M11-P, the preset for the M10-R", sawAuto, sawPresetFor)
	}
}

func TestEstimate(t *testing.T) {
	s := newShoot(t, "A.DNG", "B.DNG", "C.DNG")
	s.opts.Jobs = 2
	plan, err := Prepare(s.opts, s.keeps)
	if err != nil {
		t.Fatal(err)
	}
	e := plan.Estimate
	if e.Measured || e.PerFrame != DefaultPerFrame || e.Total != 2*DefaultPerFrame || e.PeakBytes != 2*PeakPerProcess {
		t.Errorf("first run estimate %+v", e)
	}
	if _, err := Run(context.Background(), s.opts, plan); err != nil {
		t.Fatal(err)
	}
	s.opts.Force, s.opts.Jobs = true, 1
	plan, _ = Prepare(s.opts, s.keeps)
	// The fake reports 5 ms per command: the next estimate is measured from them.
	if e := plan.Estimate; !e.Measured || e.PerFrame <= 0 || e.PerFrame > time.Second || e.Total != 3*e.PerFrame {
		t.Errorf("measured estimate %+v", e)
	}
}

func TestRunParallel(t *testing.T) {
	s := newShoot(t, "A.DNG", "B.DNG", "C.DNG", "D.DNG", "E.DNG")
	s.opts.Jobs, s.opts.Chunk = 3, 1
	_, sum, err := s.develop(t, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Developed != 5 || len(sum.Failures) != 0 {
		t.Fatalf("summary %+v", sum)
	}
	st, _ := loadState(s.opts.StatePath)
	if len(st.Frames) != 5 {
		t.Errorf("%d frames recorded", len(st.Frames))
	}
}

// Two develops of one shoot at once would race on its state: the second refuses.
func TestRunRefusesConcurrent(t *testing.T) {
	s := newShoot(t, "A.DNG")
	release, err := lockWork(s.opts.Work)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	plan, err := Prepare(s.opts, s.keeps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), s.opts, plan); err == nil || !strings.Contains(err.Error(), "another cull develop") {
		t.Fatalf("err %v", err)
	}
}

func TestPrepareRefusesClashingNames(t *testing.T) {
	s := newShoot(t, "A.DNG")
	s.keeps = append(s.keeps, Keep{Name: "A.dng", Path: s.keeps[0].Path})
	if _, err := Prepare(s.opts, s.keeps); err == nil || !strings.Contains(err.Error(), "A.jpg") {
		t.Fatalf("err %v", err)
	}
}

func TestStateVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), StateName)
	os.WriteFile(p, []byte(`{"version": 99, "frames": {}}`), 0o644)
	if _, err := loadState(p); err == nil {
		t.Error("a newer state version loaded")
	}
	if st, err := loadState(filepath.Join(t.TempDir(), "none.json")); err != nil || st.Frames == nil {
		t.Errorf("missing state: %v %+v", err, st)
	}
}
