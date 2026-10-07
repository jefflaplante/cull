package redate_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/cli"
	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/redate"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/ui"
)

func TestMain(m *testing.M) {
	// The CLI test below must not pick up the user's ~/.cull.
	os.Setenv("CULL_CONFIG", filepath.Join(os.TempDir(), "cull-tests-no-dotfile"))
	os.Exit(m.Run())
}

// target is the corrected capture time every test sets.
var target = time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)

const (
	targetISO = "2026-10-04T12:00:00"
	stopped   = "2025:12:28 00:05:59" // a dead clock's time: every fixture carries it
)

var cardTime = time.Date(2026, 10, 2, 14, 0, 0, 0, time.Local)

var (
	previewOnce sync.Once
	previewJPEG []byte
)

// preview is a real-looking 1600×1067 gradient JPEG, so the pipeline judges the frame
// (a blank one would be junk and never reach the model).
func preview() []byte {
	previewOnce.Do(func() {
		img := image.NewGray(image.Rect(0, 0, 1600, 1067))
		for y := 0; y < 1067; y++ {
			for x := 0; x < 1600; x++ {
				img.Pix[y*1600+x] = uint8(40 + x*160/1600)
			}
		}
		var b bytes.Buffer
		jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
		previewJPEG = b.Bytes()
	})
	return previewJPEG
}

// dated is a frame with every kind of capture date and n random payload bytes.
func dated(seed int64, n int) dngtest.Fixture {
	payload := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(payload)
	return dngtest.Fixture{
		DateTime: stopped, DTO: stopped, DTD: stopped, SubSecOrig: "42",
		XMP:     `<x:xmpmeta><rdf:Description xmp:CreateDate="2025-12-28T00:05:59" xmp:ModifyDate="2025-12-28T00:05:59.00+01:00"/></x:xmpmeta>`,
		Payload: payload,
		Preview: preview(),
	}
}

// signed is a Content Credentials frame: never byte-patched.
func signed(seed int64) dngtest.Fixture {
	fx := dated(seed, 2000)
	fx.C2PA = []byte("jumbf c2pa manifest stand-in")
	return fx
}

func applyAll(t *testing.T, b []byte) []byte {
	t.Helper()
	ps, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 {
		t.Fatal("fixture has nothing to patch")
	}
	return dngtest.Apply(b, ps)
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func read(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mtime(t *testing.T, p string) time.Time {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime()
}

// offloaded copies fixture frames off a synthetic card with cull offload (no
// --set-date) and returns the shoot folder and each frame's card bytes.
func offloaded(t *testing.T, files map[string]dngtest.Fixture) (string, map[string][]byte) {
	t.Helper()
	card := t.TempDir()
	dcim := filepath.Join(card, "DCIM", "100LEICA")
	if err := os.MkdirAll(dcim, 0o755); err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{}
	for name, fx := range files {
		b := dngtest.Build(t, fx)
		p := filepath.Join(dcim, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, cardTime, cardTime)
		data[name] = b
	}
	p, err := offload.MakePlan(offload.Options{Sources: []string{card}, Dest: t.TempDir(), Name: "test", Date: "2026-10-02"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := offload.Run(context.Background(), p, nil)
	if err != nil || !res.Safe || res.Copied != len(files) {
		t.Fatalf("offload: %v %+v", err, res)
	}
	return p.Dests[0], data
}

// loose writes fixture frames straight into a new folder: copied with Finder, no
// manifest, no report.
func loose(t *testing.T, files map[string]dngtest.Fixture) (string, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	data := map[string][]byte{}
	for name, fx := range files {
		b := dngtest.Build(t, fx)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, cardTime, cardTime)
		data[name] = b
	}
	return dir, data
}

// counting answers every call with a sharp, well-exposed frame; status by file name.
type counting struct {
	status map[string]string
	calls  int32
}

func (c *counting) Name() string { return "fake" }
func (c *counting) Call(_ context.Context, req llm.Request) (*llm.Response, error) {
	atomic.AddInt32(&c.calls, 1)
	status := "sharp"
	for name, s := range c.status {
		if strings.Contains(req.Parts[0].Text, name) {
			status = s
		}
	}
	js := fmt.Sprintf(`{"sharpness":{"score":8,"status":%q,"focus_target":""},
 "exposure":{"score":7,"status":"good","ev_adjust":0,"clipping":"none","reason":""},
 "composition":{"score":6,"status":"good","issues":[],"crop":{"apply":false,"left":0,"top":0,"right":1,"bottom":1}},
 "notes":"","keywords":["portrait"]}`, status)
	return &llm.Response{JSON: json.RawMessage(js), Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}, nil
}

func judgeCfg(dir string) pipeline.Config {
	return pipeline.Config{Dir: dir, ReportPath: filepath.Join(dir, "cull-report.json"), Concurrency: 2, WriteXMP: true,
		Prep: imageprep.Options{MaxEdge: 800}, Policy: eval.Policy{MinCropArea: 0.6}, CheckpointN: 25, Log: io.Discard}
}

// judge runs the pipeline (continuing the report when resume) and returns how many
// model calls it made.
func judge(t *testing.T, dir string, resume bool, b *counting) (*report.Report, int) {
	t.Helper()
	c := judgeCfg(dir)
	c.Resume = resume
	before := atomic.LoadInt32(&b.calls)
	rep, _, err := pipeline.Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	return rep, int(atomic.LoadInt32(&b.calls) - before)
}

func run(t *testing.T, dir string) redate.Result {
	t.Helper()
	res, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// currentEntries is the manifest's current entry per name.
func currentEntries(t *testing.T, dir string) map[string]offload.Entry {
	t.Helper()
	es, err := offload.CurrentManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]offload.Entry{}
	for _, e := range es {
		out[e.Name] = e
	}
	return out
}

func noTemps(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		// Offload's ".tmp" temps, redate's ".redate" ones, the journal's and the report's.
		if err == nil && strings.HasPrefix(d.Name(), ".") && (strings.Contains(d.Name(), ".cull-") || strings.HasSuffix(d.Name(), ".tmp")) {
			t.Errorf("temp left behind: %s", p)
		}
		return nil
	})
}

func resultFor(t *testing.T, rep *report.Report, name string) report.Result {
	t.Helper()
	for _, r := range rep.Results {
		if filepath.Base(r.File) == name {
			return r
		}
	}
	t.Fatalf("%s not in the report", name)
	return report.Result{}
}

// An offloaded folder (no --set-date), judged, with sidecars: redate patches every
// frame to exactly its card bytes with the date patches, proves it, sets its times,
// and the manifest, report and sidecars follow. A Content Credentials frame keeps its
// bytes; its times, manifest, report and sidecar still get the date.
func TestRedatePatchesAndProves(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{
		"M1.DNG": dated(1, 9<<20), // several 4 MiB chunks
		"M2.DNG": dated(2, 3000),
		"L3.DNG": signed(3),
	})
	judge(t, dir, false, &counting{})

	res := run(t, dir)
	if res.Patched != 2 || res.TimesOnly != 1 || res.AlreadySet != 0 || res.Refused != 0 {
		t.Fatalf("result %+v", res)
	}
	if len(res.Signed) != 1 || res.Signed[0] != "L3.DNG" {
		t.Fatalf("signed %v", res.Signed)
	}
	man := currentEntries(t, dir)
	rep, err := report.Load(filepath.Join(dir, "cull-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, card := range data {
		p := filepath.Join(dir, name)
		got := read(t, p)
		want := card
		if name != "L3.DNG" {
			want = applyAll(t, card)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s isn't its card bytes with exactly the date patches", name)
		}
		if m := mtime(t, p); !m.Equal(target) {
			t.Errorf("%s mtime %v", name, m)
		}
		e := man[name]
		if e.SHA256 != sum(card) || e.DatesSet != targetISO {
			t.Errorf("%s manifest %+v", name, e)
		}
		if name == "L3.DNG" {
			if e.FileSHA256 != "" {
				t.Errorf("%s: a Content Credentials frame records no patched checksum: %+v", name, e)
			}
		} else if e.FileSHA256 != sum(got) || e.PatchedAt.IsZero() {
			t.Errorf("%s manifest %+v", name, e)
		}
		r := resultFor(t, rep, name)
		if !r.ModTime.Equal(target) || r.DatesSet != targetISO || r.Evaluation == nil {
			t.Errorf("%s report %+v", name, r)
		}
		// The report keeps the capture time as the camera recorded it and as it was
		// judged (its sets came from it); the date set is DatesSet.
		if r.Exif == nil || r.Exif.DateTimeOriginal != stopped || r.Exif.SubSec != "42" {
			t.Errorf("%s exif %+v", name, r.Exif)
		}
		x := read(t, filepath.Join(dir, strings.TrimSuffix(name, ".DNG")+".xmp"))
		if !bytes.Contains(x, []byte(`exif:DateTimeOriginal="`+targetISO+`"`)) {
			t.Errorf("%s sidecar lacks the date:\n%s", name, x)
		}
	}
	v, err := offload.Verify(context.Background(), dir, nil)
	if err != nil || v.OK != 3 || v.Bad != 0 {
		t.Fatalf("verify %+v %v", v, err)
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("journal left behind")
	}
	if _, err := os.Stat(filepath.Join(dir, journal.RedateName)); !os.IsNotExist(err) {
		t.Fatal("journal file left behind")
	}
	noTemps(t, dir)
}

// Review Focus 2: after redate, judge continues the report with no model call; and
// again after a second redate.
func TestRedateThenJudgeMakesNoCalls(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{
		"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000), "L3.DNG": signed(3),
	})
	b := &counting{}
	if _, n := judge(t, dir, false, b); n == 0 {
		t.Fatal("the first judge made no calls")
	}
	run(t, dir)
	rep, n := judge(t, dir, true, b)
	if n != 0 {
		t.Fatalf("judge after redate made %d calls", n)
	}
	for _, r := range rep.Results {
		if r.DatesSet != targetISO {
			t.Errorf("%s DatesSet %q", r.File, r.DatesSet)
		}
	}
	if res := run(t, dir); res.Patched != 0 || res.AlreadySet != 3 {
		t.Fatalf("second redate %+v", res)
	}
	if _, n := judge(t, dir, true, b); n != 0 {
		t.Fatalf("judge after a second redate made %d calls", n)
	}
}

// Review Focus 2: redate twice changes nothing the second time; interrupted right
// after a swap (before its manifest line and any report save), judge refuses, a
// different date is refused, and the same command finishes it: every file proven,
// the keys current, and judge makes no call.
func TestRedateIdempotentAndResumes(t *testing.T) {
	files := map[string]dngtest.Fixture{
		"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000), "M3.DNG": dated(3, 4000), "M4.DNG": dated(4, 4000),
	}

	t.Run("twice", func(t *testing.T) {
		dir, _ := offloaded(t, files)
		run(t, dir)
		before := snapshot(t, dir)
		manBefore := read(t, filepath.Join(dir, offload.ManifestName))
		res := run(t, dir)
		if res.Patched != 0 || res.AlreadySet != 4 || res.Refused != 0 {
			t.Fatalf("second run %+v", res)
		}
		if after := snapshot(t, dir); after != before {
			t.Fatalf("second run changed the folder:\n%s\n%s", before, after)
		}
		if !bytes.Equal(read(t, filepath.Join(dir, offload.ManifestName)), manBefore) {
			t.Fatal("second run appended to the manifest")
		}
	})

	t.Run("interrupted", func(t *testing.T) {
		dir, data := offloaded(t, files)
		b := &counting{}
		judge(t, dir, false, b)
		swaps := 0
		restore := redate.SetCrashAfterSwap(func(string) bool { swaps++; return swaps == 2 })
		_, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target})
		restore()
		if err == nil {
			t.Fatal("the injected crash returned no error")
		}
		which, finish, ok := journal.Incomplete(dir)
		if !ok || which != "redate" || !strings.Contains(finish, "cull redate") || !strings.Contains(finish, "--date 2026-10-04") {
			t.Fatalf("journal: %q %q %v", which, finish, ok)
		}
		// judge (the command) refuses while the journal is incomplete.
		cmd := cli.NewRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"judge", "--backend", "openai", "--model", "m", dir})
		if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "cull redate") {
			t.Fatalf("judge didn't refuse: %v\n%s", err, out.String())
		}
		// Another date is refused while this one is unfinished.
		other := target.AddDate(0, 0, 1)
		if _, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: other}); err == nil || !strings.Contains(err.Error(), "2026-10-04") {
			t.Fatalf("another date: %v", err)
		}
		res := run(t, dir)
		if res.Patched != 2 || res.AlreadySet != 2 || res.Refused != 0 {
			t.Fatalf("resumed %+v", res)
		}
		if _, _, ok := journal.Incomplete(dir); ok {
			t.Fatal("journal still incomplete")
		}
		man := currentEntries(t, dir)
		for name, card := range data {
			got := read(t, filepath.Join(dir, name))
			if !bytes.Equal(got, applyAll(t, card)) {
				t.Errorf("%s not patched exactly", name)
			}
			if e := man[name]; e.FileSHA256 != sum(got) || e.DatesSet != targetISO {
				t.Errorf("%s manifest %+v", name, e)
			}
		}
		v, err := offload.Verify(context.Background(), dir, nil)
		if err != nil || v.OK != 4 || v.Bad != 0 {
			t.Fatalf("verify %+v %v", v, err)
		}
		if _, n := judge(t, dir, true, b); n != 0 {
			t.Fatalf("judge after the finished redate made %d calls", n)
		}
		noTemps(t, dir)
	})
}

// snapshot is every file's name, size, mtime and hash under dir.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		st, _ := d.Info()
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		lines = append(lines, fmt.Sprintf("%s %d %d %s", rel, st.Size(), st.ModTime().UnixNano(), sum(b)))
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// A frame whose bytes no longer match its manifest checksum is refused and left
// exactly as it is; the others are fixed.
func TestRedateRefusesChangedFile(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000)})
	p := filepath.Join(dir, "M1.DNG")
	damaged := read(t, p)
	damaged[len(damaged)-100] ^= 0xFF // a bit flip in the raw data, dates intact
	if err := os.WriteFile(p, damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, cardTime, cardTime)
	res := run(t, dir)
	if res.Refused != 1 || res.Patched != 1 || len(res.Refusals) != 1 ||
		!strings.Contains(res.Refusals[0], "M1.DNG") || !strings.Contains(res.Refusals[0], "no longer matches its offload checksum") {
		t.Fatalf("result %+v", res)
	}
	if !bytes.Equal(read(t, p), damaged) || !mtime(t, p).Equal(cardTime) {
		t.Fatal("the refused file changed")
	}
	if e := currentEntries(t, dir)["M1.DNG"]; e.DatesSet != "" || e.FileSHA256 != "" {
		t.Fatalf("refused file recorded %+v", e)
	}
	if !bytes.Equal(read(t, filepath.Join(dir, "M2.DNG")), applyAll(t, data["M2.DNG"])) {
		t.Fatal("M2 not patched")
	}
	noTemps(t, dir)
}

// Review Focus 5: DNGs no manifest records, in a folder with no report, are patched
// and proven; nothing else is created.
func TestRedateUnrecordedFolder(t *testing.T) {
	dir, data := loose(t, map[string]dngtest.Fixture{"A.DNG": dated(1, 4000), "B.dng": dated(2, 5000), "C.DNG": signed(3)})
	res := run(t, dir)
	if res.Patched != 2 || res.TimesOnly != 1 || res.Refused != 0 {
		t.Fatalf("result %+v", res)
	}
	for name, b := range data {
		want := b
		if name != "C.DNG" {
			want = applyAll(t, b)
		}
		if !bytes.Equal(read(t, filepath.Join(dir, name)), want) || !mtime(t, filepath.Join(dir, name)).Equal(target) {
			t.Errorf("%s not fixed", name)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 3 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("folder holds %v", names)
	}
}

// Frames sorted into keep/, review/ and cull/ are fixed where they are; the report
// follows them by MovedTo, so restoring them and judging again calls nothing.
func TestRedateSortFolders(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{
		"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000), "M3.DNG": dated(3, 4000),
	})
	b := &counting{status: map[string]string{"M1.DNG": "missed_focus", "M3.DNG": "soft"}}
	c := judgeCfg(dir)
	c.Sort = true
	if _, _, err := pipeline.Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	where := map[string]string{}
	for name := range data {
		for _, sub := range []string{"keep", "review", "cull"} {
			if _, err := os.Stat(filepath.Join(dir, sub, name)); err == nil {
				where[name] = filepath.Join(dir, sub, name)
			}
		}
	}
	if len(where) != 3 {
		t.Fatalf("not sorted: %v", where)
	}
	res := run(t, dir)
	if res.Patched != 3 {
		t.Fatalf("result %+v", res)
	}
	rep, err := report.Load(c.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range where {
		if !bytes.Equal(read(t, p), applyAll(t, data[name])) {
			t.Errorf("%s not patched in place", name)
		}
		r := resultFor(t, rep, name)
		if r.MovedTo != p || !r.ModTime.Equal(mtime(t, p)) || r.DatesSet != targetISO {
			t.Errorf("%s report %+v", name, r)
		}
		x := read(t, strings.TrimSuffix(p, ".DNG")+".xmp")
		if !bytes.Contains(x, []byte(targetISO)) {
			t.Errorf("%s sidecar lacks the date", name)
		}
	}
	if v, err := offload.Verify(context.Background(), dir, nil); err != nil || v.OK != 3 {
		t.Fatalf("verify %+v %v", v, err)
	}
	if _, err := pipeline.Restore(c.ReportPath, dir, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if _, n := judge(t, dir, true, b); n != 0 {
		t.Fatalf("judge after redate and restore made %d calls", n)
	}
}

// --dry-run says what it would do and writes nothing.
func TestRedateDryRunWritesNothing(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "L2.DNG": signed(2)})
	judge(t, dir, false, &counting{})
	before := snapshot(t, dir)
	var lines []string
	res, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target, DryRun: true, UI: sinkFunc(func(e ui.Event) {
		if e.Note != nil {
			lines = append(lines, e.Note.Text)
		}
	})})
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); after != before {
		t.Fatalf("dry run wrote:\n%s\n%s", before, after)
	}
	if res.Patched != 1 || res.TimesOnly != 1 {
		t.Fatalf("dry run counts %+v", res)
	}
	b := read(t, filepath.Join(dir, "M1.DNG"))
	ps, _, _ := dng.PatchDates(bytes.NewReader(b), int64(len(b)), target)
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, fmt.Sprintf("M1.DNG: %d fields → 2026-10-04 12:00:00", len(ps))) || !strings.Contains(all, "L2.DNG: Content Credentials") {
		t.Fatalf("dry run said:\n%s", all)
	}
}

type sinkFunc func(ui.Event)

func (f sinkFunc) Emit(e ui.Event) { f(e) }
func (f sinkFunc) Close() error    { return nil }

// Many Content Credentials frames: one note names five, the verbose one all; each
// keeps its bytes and gets its times.
func TestRedateContentCredentialsListed(t *testing.T) {
	fx := map[string]dngtest.Fixture{}
	for i := 1; i <= 7; i++ {
		fx[fmt.Sprintf("L%d.DNG", i)] = signed(int64(i))
	}
	dir, data := loose(t, fx)
	var normal, verbose []string
	res, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target, UI: sinkFunc(func(e ui.Event) {
		if n := e.Note; n != nil && strings.Contains(n.Text, "Content Credentials") {
			if n.Level == ui.Verbose {
				verbose = append(verbose, n.Text)
			} else {
				normal = append(normal, n.Text)
			}
		}
	})})
	if err != nil || res.TimesOnly != 7 || len(res.Signed) != 7 {
		t.Fatalf("%v %+v", err, res)
	}
	if len(normal) != 1 || !strings.Contains(normal[0], "L5.DNG and 2 more") || len(verbose) != 1 || !strings.Contains(verbose[0], "L7.DNG") {
		t.Fatalf("normal %q verbose %q", normal, verbose)
	}
	for name, b := range data {
		p := filepath.Join(dir, name)
		if !bytes.Equal(read(t, p), b) || !mtime(t, p).Equal(target) {
			t.Errorf("%s", name)
		}
	}
}
