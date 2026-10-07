package rename_test

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
	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/labels"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/rename"
	"github.com/jefflaplante/cull/internal/report"
	"github.com/jefflaplante/cull/internal/review"
	"github.com/jefflaplante/cull/internal/ui"
)

func TestMain(m *testing.M) {
	// The CLI tests must not pick up the user's ~/.cull.
	os.Setenv("CULL_CONFIG", filepath.Join(os.TempDir(), "cull-tests-no-dotfile"))
	os.Exit(m.Run())
}

var cardTime = time.Date(2026, 10, 2, 14, 0, 0, 0, time.Local)

const capture = "2025:12:28 00:05:59"

var (
	previewOnce sync.Once
	previewJPEG []byte
)

// preview is a real-looking gradient JPEG, so the pipeline judges the frame.
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

// frame is a synthetic DNG with a capture date, a preview and n random payload bytes.
func frame(seed int64, n int) dngtest.Fixture {
	payload := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(payload)
	return dngtest.Fixture{DateTime: capture, DTO: capture, DTD: capture, Payload: payload, Preview: preview()}
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

// card writes frames into a synthetic card's DCIM folder (adding to it when it exists).
func card(t *testing.T, c string, files map[string]dngtest.Fixture) map[string][]byte {
	t.Helper()
	dcim := filepath.Join(c, "DCIM", "100LEICA")
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
	return data
}

// offloadCard runs cull offload of c into dest's "2026-10-02 test" and returns the folder.
func offloadCard(t *testing.T, c, dest string) string {
	t.Helper()
	p, err := offload.MakePlan(offload.Options{Sources: []string{c}, Dest: dest, Name: "test", Date: "2026-10-02"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := offload.Run(context.Background(), p, nil)
	if err != nil || !res.Safe {
		t.Fatalf("offload: %v %+v", err, res)
	}
	return p.Dests[0]
}

// counting answers every call with a frame whose sharpness status is set by file name.
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

// judge runs the pipeline, continuing the report, and returns how many model calls it made.
func judge(t *testing.T, dir string, b *counting, edit func(*pipeline.Config)) int {
	t.Helper()
	c := judgeCfg(dir)
	c.Resume = true
	if edit != nil {
		edit(&c)
	}
	before := atomic.LoadInt32(&b.calls)
	if _, _, err := pipeline.Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	return int(atomic.LoadInt32(&b.calls) - before)
}

func label(t *testing.T, dir, name, l string, stars int) {
	t.Helper()
	if err := labels.Append(filepath.Join(dir, labels.FileName), labels.Entry{File: name, Label: l, Stars: stars, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func readLabels(t *testing.T, dir string) map[string]labels.Entry {
	t.Helper()
	m, err := labels.Read(filepath.Join(dir, labels.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// shoot is a judged shoot folder: M1–M3 offloaded and judged with --sort (M1 into cull/,
// M2 into keep/, M3 into review/), then M4 and M5 offloaded from the same card and
// judged without sorting (home). Every frame has cull's sidecar; the review sheet's
// images are cached; M1, M2 and M4 are labelled. It returns the folder, each frame's
// card bytes by camera name, and where each frame is (relative).
func shoot(t *testing.T) (dir string, data map[string][]byte, where map[string]string, b *counting) {
	t.Helper()
	c, dest := t.TempDir(), t.TempDir()
	data = card(t, c, map[string]dngtest.Fixture{"M1.DNG": frame(1, 4000), "M2.DNG": frame(2, 4100), "M3.DNG": frame(3, 4200)})
	dir = offloadCard(t, c, dest)
	b = &counting{status: map[string]string{"M1.DNG": "missed_focus", "M3.DNG": "soft"}}
	if n := judge(t, dir, b, func(c *pipeline.Config) { c.Sort = true }); n == 0 {
		t.Fatal("judge made no calls")
	}
	for k, v := range card(t, c, map[string]dngtest.Fixture{"M4.DNG": frame(4, 4300), "M5.DNG": frame(5, 4400)}) {
		data[k] = v
	}
	if offloadCard(t, c, dest) != dir {
		t.Fatal("second offload went to another folder")
	}
	judge(t, dir, b, nil)
	where = map[string]string{"M1.DNG": "cull/M1.DNG", "M2.DNG": "keep/M2.DNG", "M3.DNG": "review/M3.DNG", "M4.DNG": "M4.DNG", "M5.DNG": "M5.DNG"}
	for name, rel := range where {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Fatalf("%s not at %s: %v", name, rel, err)
		}
		if _, err := os.Stat(filepath.Join(dir, sidecarOf(rel))); err != nil {
			t.Fatalf("%s has no sidecar: %v", name, err)
		}
	}
	label(t, dir, "M1.DNG", "keep", 3)
	label(t, dir, "M2.DNG", "cull", 0)
	label(t, dir, "M4.DNG", "review", 2)
	buildSheet(t, dir)
	return dir, data, where, b
}

func buildSheet(t *testing.T, dir string) {
	t.Helper()
	rp := filepath.Join(dir, "cull-report.json")
	rep, err := report.Load(rp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := review.Build(rep, rp, review.Options{Out: filepath.Join(dir, "cull-review")}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func sidecarOf(rel string) string { return strings.TrimSuffix(rel, filepath.Ext(rel)) + ".xmp" }

// renamed is where shoot's frames are after "{date}_{name}_{n:3}".
var renamed = map[string]string{
	"M1.DNG": "cull/20251228_test_001.DNG", "M2.DNG": "keep/20251228_test_002.DNG", "M3.DNG": "review/20251228_test_003.DNG",
	"M4.DNG": "20251228_test_004.DNG", "M5.DNG": "20251228_test_005.DNG",
}

const pattern = "{date}_{name}_{n:3}"

func runRename(t *testing.T, o rename.Options) rename.Result {
	t.Helper()
	res, err := rename.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("rename: %v (%+v)", err, res)
	}
	return res
}

// follows checks that every frame is at to[camera name] (relative), with its card
// bytes and its sidecar, and nothing is left at from; that the report, labels log,
// manifest and review cache follow; that Verify passes; and that judge makes no calls.
// wantLabels is each camera name's expected label entry (nil: none).
func follows(t *testing.T, dir string, data map[string][]byte, from, to map[string]string, wantLabels map[string]*labels.Entry, b *counting) {
	t.Helper()
	rp := filepath.Join(dir, "cull-report.json")
	raw := string(read(t, rp))
	rep, err := report.Load(rp)
	if err != nil {
		t.Fatal(err)
	}
	lab := readLabels(t, dir)
	man := map[string]offload.Entry{}
	es, err := offload.CurrentManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		man[e.Orig] = e
	}
	newBases := map[string]bool{}
	legit := map[string]bool{} // paths the report rightly holds now: frames', homes', sidecars'
	for _, rel := range to {
		newBases[filepath.Base(rel)] = true
		for _, r := range []string{rel, filepath.Base(rel)} {
			legit[filepath.Join(dir, r)], legit[filepath.Join(dir, sidecarOf(r))] = true, true
		}
	}
	for name, rel := range to {
		p := filepath.Join(dir, rel)
		if got := read(t, p); sum(got) != sum(data[name]) {
			t.Errorf("%s: %s holds another frame's bytes", name, rel)
		}
		if _, err := os.Stat(filepath.Join(dir, sidecarOf(rel))); err != nil {
			t.Errorf("%s: sidecar not beside it: %v", name, err)
		}
		old := from[name]
		if old != rel && !contains(to, old) {
			for _, gone := range []string{old, sidecarOf(old)} {
				if gone != sidecarOf(rel) && exactly(filepath.Join(dir, gone)) {
					t.Errorf("%s: %s still there", name, gone)
				}
			}
		}
		// The report keys follow: File is the home path, MovedTo the sort-folder path.
		home := filepath.Join(dir, filepath.Base(rel))
		var x *report.Result
		for i := range rep.Results {
			if rep.Results[i].File == home {
				x = &rep.Results[i]
			}
		}
		if x == nil {
			t.Errorf("%s: no report entry at %s", name, home)
		} else {
			wantMoved := ""
			if filepath.Dir(rel) != "." {
				wantMoved = p
			}
			if x.MovedTo != wantMoved || x.Size != int64(len(data[name])) || x.XMP != filepath.Join(dir, sidecarOf(rel)) {
				t.Errorf("%s: report %s moved %q xmp %q size %d", name, x.File, x.MovedTo, x.XMP, x.Size)
			}
			for _, img := range review.CachedImages(rep.Dir, *x) {
				if strings.Contains(img, ".native.") {
					continue
				}
				if _, err := os.Stat(filepath.Join(dir, "cull-review", review.AssetsDir, img)); err != nil {
					t.Errorf("%s: review image %s not carried to the new name", name, img)
				}
			}
		}
		// No old path anywhere in the report.
		if old != rel && !contains(to, old) {
			for _, gone := range []string{filepath.Join(dir, old), filepath.Join(dir, filepath.Base(old)), filepath.Join(dir, sidecarOf(old)), filepath.Join(dir, sidecarOf(filepath.Base(old)))} {
				if !legit[gone] && strings.Contains(raw, `"`+gone+`"`) {
					t.Errorf("%s: the report still holds %s", name, gone)
				}
			}
		}
		// The label follows; one left under the old name is cleared.
		got, ok := lab[filepath.Base(rel)]
		want := wantLabels[name]
		switch {
		case want == nil && ok:
			t.Errorf("%s: a label %+v under %s", name, got, filepath.Base(rel))
		case want != nil && (!ok || got.Label != want.Label || got.Stars != want.Stars):
			t.Errorf("%s: label %+v (%v), want %+v", name, got, ok, *want)
		}
		if ob := filepath.Base(old); !newBases[ob] {
			if e, ok := lab[ob]; ok {
				t.Errorf("%s: a label is left under its old name: %+v", name, e)
			}
		}
		if e, ok := man[name]; ok && e.Name != filepath.Base(rel) {
			t.Errorf("%s: manifest names it %s", name, e.Name)
		}
	}
	if v, err := offload.Verify(context.Background(), dir, nil); err != nil || v.Bad != 0 || v.OK != len(man) || len(v.Unrecorded) != len(to)-len(man) {
		t.Errorf("verify %+v %v", v, err)
	}
	noTemps(t, dir)
	if n := judge(t, dir, b, nil); n != 0 {
		t.Errorf("judge after the rename made %d model calls", n)
	}
}

// exactly reports whether a file is listed under exactly that name (on a
// case-insensitive volume, Lstat finds "m1.dng" under "m1.DNG").
func exactly(p string) bool {
	ents, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range ents {
		if e.Name() == filepath.Base(p) {
			return true
		}
	}
	return false
}

func contains(m map[string]string, v string) bool {
	for _, x := range m {
		if x == v {
			return true
		}
	}
	return false
}

func noTemps(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".") && (strings.Contains(d.Name(), ".cull-") || strings.HasSuffix(d.Name(), ".tmp")) {
			t.Errorf("temp left behind: %s", p)
		}
		return nil
	})
}

// labelsOf is the shoot's labels by camera name, as they were labelled.
func labelsOf() map[string]*labels.Entry {
	return map[string]*labels.Entry{
		"M1.DNG": {Label: "keep", Stars: 3}, "M2.DNG": {Label: "cull"}, "M4.DNG": {Label: "review", Stars: 2},
	}
}

// snapshot is every file under dir with its size, time and checksum.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.Name() == ".cull.lock" { // the folder lock: no frame, temp or record
			return nil
		}
		if d.IsDir() {
			lines = append(lines, rel+"/")
			return nil
		}
		st, _ := d.Info()
		b, _ := os.ReadFile(p)
		lines = append(lines, fmt.Sprintf("%s %d %d %s", rel, st.Size(), st.ModTime().UnixNano(), sum(b)))
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

type sinkFunc func(ui.Event)

func (f sinkFunc) Emit(e ui.Event) { f(e) }
func (f sinkFunc) Close() error    { return nil }

// notes collects the run's notes.
func notes(out *[]string) ui.Sink {
	return sinkFunc(func(e ui.Event) {
		if e.Note != nil {
			*out = append(*out, e.Note.Text)
		}
	})
}

// cullCmd runs the cull command line.
func cullCmd(args ...string) (string, error) {
	cmd := cli.NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}
