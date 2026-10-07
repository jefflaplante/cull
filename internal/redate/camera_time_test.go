package redate_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/redate"
	"github.com/jefflaplante/cull/internal/report"
)

// Final review I1 (adapted from the reviewer's chain tests): grouping reads the
// camera's own recorded time. After offload --set-date, or redate before the first
// judge, patched M… frames carry the target date while Content Credentials L… frames
// keep the camera's, so a capture-time order put every L before every M. The manifest
// records the camera's time when a frame is first patched (camera_time), and grouping
// uses it.

// mixedNames share one counter (the M11-P): camera order M1, M2, L3, L4, M5, M6.
var mixedNames = []string{"M1000001.DNG", "M1000002.DNG", "L1000003.DNG", "L1000004.DNG", "M1000005.DNG", "M1000006.DNG"}

// mixedCard writes the six frames: the first four at one power-on of a dead clock, the
// last two an hour later (a 60 s sequence gap splits them).
func mixedCard(t *testing.T) string {
	t.Helper()
	card := t.TempDir()
	dcim := filepath.Join(card, "DCIM", "100LEICA")
	if err := os.MkdirAll(dcim, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, n := range mixedNames {
		fx := dated(int64(i+1), 3000+i*17)
		if strings.HasPrefix(n, "L") {
			fx.C2PA = []byte("jumbf c2pa manifest stand-in")
		}
		if i >= 4 {
			fx.DateTime, fx.DTO, fx.DTD = "2025:12:28 01:05:59", "2025:12:28 01:05:59", "2025:12:28 01:05:59"
		}
		p := filepath.Join(dcim, n)
		if err := os.WriteFile(p, dngtest.Build(t, fx), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, cardTime, cardTime)
	}
	return card
}

func offloadMixed(t *testing.T, card string, setDate bool) string {
	t.Helper()
	o := offload.Options{Sources: []string{card}, Dest: t.TempDir(), Name: "test"}
	if setDate {
		o.SetDate, o.SetDateSet = target, true
	} else {
		o.Date = "2026-10-02"
	}
	p, err := offload.MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := offload.Run(context.Background(), p, nil)
	if err != nil || !res.Safe || res.Copied != len(mixedNames) {
		t.Fatalf("offload: %v %+v", err, res)
	}
	return p.Dests[0]
}

// judgeMixed judges dir (ranking on, 60 s sequence gap) and returns the calls made, the
// frames in grouping order and the sets.
func judgeMixed(t *testing.T, dir string, resume bool, b *ranking) (int, string, string) {
	t.Helper()
	c := judgeCfg(dir)
	c.Resume = resume
	c.Seq = group.Options{Gap: 60 * time.Second, MaxLook: group.DefaultLook}
	c.Rank = true
	before := atomic.LoadInt32(&b.calls)
	rep, _, err := pipeline.Run(context.Background(), c, b)
	if err != nil {
		t.Fatal(err)
	}
	var fr []group.Frame
	for _, x := range rep.Results {
		fr = append(fr, x.GroupFrame())
	}
	var order []string
	for _, i := range group.Order(fr) {
		order = append(order, filepath.Base(rep.Results[i].File))
	}
	var sets []string
	for _, s := range rep.Sets {
		var m []string
		for _, f := range s.Members {
			m = append(m, filepath.Base(f))
		}
		sets = append(sets, strings.Join(m, "+"))
	}
	return int(atomic.LoadInt32(&b.calls) - before), strings.Join(order, " "), strings.Join(sets, " ")
}

const (
	cameraOrder = "M1000001.DNG M1000002.DNG L1000003.DNG L1000004.DNG M1000005.DNG M1000006.DNG"
	cameraSets  = "M1000001.DNG+M1000002.DNG+L1000003.DNG+L1000004.DNG M1000005.DNG+M1000006.DNG"
)

// checkCameraTimes: each M… entry records the card's DateTimeOriginal (+SubSec) as
// camera_time, the L… entries (never patched) none, and the report carries the same.
func checkCameraTimes(t *testing.T, step, dir string) {
	t.Helper()
	es := currentEntries(t, dir)
	rep, err := report.Load(filepath.Join(dir, "cull-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range mixedNames {
		want := ""
		if strings.HasPrefix(n, "M") {
			want = "2025-12-28T00:05:59.42"
			if i >= 4 {
				want = "2025-12-28T01:05:59.42"
			}
		}
		if got := es[n].CameraTime; got != want {
			t.Errorf("%s: manifest camera_time of %s = %q, want %q", step, n, got, want)
		}
		if got := resultFor(t, rep, n).CameraTime; got != want {
			t.Errorf("%s: report camera_time of %s = %q, want %q", step, n, got, want)
		}
	}
}

func TestCameraOrderAfterSetDateAndRedate(t *testing.T) {
	card := mixedCard(t)

	// The camera's own order and sets: a plain offload, judged.
	plain := offloadMixed(t, card, false)
	if _, order, sets := judgeMixed(t, plain, false, &ranking{}); order != cameraOrder || sets != cameraSets {
		t.Fatalf("setup: order %q sets %q", order, sets)
	}

	// offload --set-date, then the first judge.
	setDir := offloadMixed(t, card, true)
	if _, order, sets := judgeMixed(t, setDir, false, &ranking{}); order != cameraOrder || sets != cameraSets {
		t.Errorf("after offload --set-date: order %q sets %q, want %q %q", order, sets, cameraOrder, cameraSets)
	}
	checkCameraTimes(t, "offload --set-date", setDir)

	// A plain offload, redated before the first judge.
	redDir := offloadMixed(t, card, false)
	if res := run(t, redDir); res.Patched != 4 || res.TimesOnly != 2 {
		t.Fatalf("redate: %+v", res)
	}
	if _, order, sets := judgeMixed(t, redDir, false, &ranking{}); order != cameraOrder || sets != cameraSets {
		t.Errorf("after redate: order %q sets %q, want %q %q", order, sets, cameraOrder, cameraSets)
	}
	checkCameraTimes(t, "redate", redDir)

	// A second redate (another date) never overwrites the camera's time.
	if res, err := redate.Run(context.Background(), redate.Options{Dir: redDir, Target: target.Add(48 * time.Hour)}); err != nil || res.Patched != 4 {
		t.Fatalf("second redate: %v %+v", err, res)
	}
	checkCameraTimes(t, "second redate", redDir)
	os.Remove(filepath.Join(redDir, "cull-report.json")) // a fresh scan reads the files and the manifest
	if _, order, sets := judgeMixed(t, redDir, false, &ranking{}); order != cameraOrder || sets != cameraSets {
		t.Errorf("after a second redate: order %q sets %q", order, sets)
	}
}

// A report judged (and ranked) before redate groups identically after it: no call.
func TestJudgedBeforeRedateGroupsTheSame(t *testing.T) {
	dir := offloadMixed(t, mixedCard(t), false)
	b := &ranking{}
	if _, order, sets := judgeMixed(t, dir, false, b); order != cameraOrder || sets != cameraSets || b.ranks == 0 {
		t.Fatalf("setup: order %q sets %q ranks %d", order, sets, b.ranks)
	}
	run(t, dir)
	n, order, sets := judgeMixed(t, dir, true, b)
	if n != 0 || order != cameraOrder || sets != cameraSets {
		t.Fatalf("judge after redate: %d calls, order %q sets %q", n, order, sets)
	}
	checkCameraTimes(t, "redate after judge", dir)
}

// A redate interrupted right after a swap (before its manifest line) still records the
// camera's time when the next run finishes it: the journal carries it.
func TestInterruptedRedateKeepsCameraTime(t *testing.T) {
	dir := offloadMixed(t, mixedCard(t), false)
	restore := redate.SetCrashAfterSwap(func(p string) bool { return filepath.Base(p) == "M1000002.DNG" })
	_, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target})
	restore()
	if err == nil {
		t.Fatal("the crash seam didn't stop the run")
	}
	j, err := journal.LoadRedate(dir)
	if err != nil || j == nil || j.Files["M1000002.DNG"].CameraTime != "2025-12-28T00:05:59.42" {
		t.Fatalf("journal: %v %+v", err, j)
	}
	run(t, dir)
	if got := currentEntries(t, dir)["M1000002.DNG"].CameraTime; got != "2025-12-28T00:05:59.42" {
		t.Fatalf("camera_time after finishing: %q", got)
	}
	if _, order, sets := judgeMixed(t, dir, false, &ranking{}); order != cameraOrder || sets != cameraSets {
		t.Errorf("order %q sets %q", order, sets)
	}
}

// Final review M1: a folder with no manifest and no report has nowhere to record a
// Content Credentials frame's corrected date: redate says so once, with the count, and
// suggests scanning first. With a report, it's recorded there: no warning.
func TestLooseRedateWarnsSignedUnrecorded(t *testing.T) {
	dir, _ := loose(t, map[string]dngtest.Fixture{"M1000001.DNG": dated(1, 3000), "L1000002.DNG": signed(2), "L1000003.DNG": signed(3)})
	_, notes, err := runNotes(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	all := notes.all()
	if strings.Count(all, "nowhere to record") != 1 || !strings.Contains(all, "2 Content Credentials frame(s)") ||
		!strings.Contains(all, "cull scan "+dir) {
		t.Fatalf("notes:\n%s", all)
	}

	dir2, _ := loose(t, map[string]dngtest.Fixture{"M1000001.DNG": dated(1, 3000), "L1000002.DNG": signed(2)})
	judge(t, dir2, false, &counting{})
	_, notes, err = runNotes(t, dir2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(notes.all(), "nowhere to record") {
		t.Fatalf("warned with a report:\n%s", notes.all())
	}
	rep, _ := report.Load(filepath.Join(dir2, "cull-report.json"))
	if r := resultFor(t, rep, "L1000002.DNG"); r.DatesSet != targetISO {
		t.Fatalf("signed frame's date not in the report: %q", r.DatesSet)
	}
}
