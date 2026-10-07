package redate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/redate"
	"github.com/jefflaplante/cull/internal/report"
)

// Tests from the integrity review of b59b11a (adapted from the reviewer's adversarial
// tests).

func fakeCard(t *testing.T, files map[string]dngtest.Fixture) string {
	t.Helper()
	c := t.TempDir()
	dcim := filepath.Join(c, "DCIM", "100LEICA")
	os.MkdirAll(dcim, 0o755)
	for name, fx := range files {
		p := filepath.Join(dcim, name)
		os.WriteFile(p, dngtest.Build(t, fx), 0o644)
		os.Chtimes(p, cardTime, cardTime)
	}
	return c
}

// Re-running offload on the same card after redate copies nothing and is still safe,
// with camera names, --checksum and --rename.
func TestOffloadRerunAfterRedate(t *testing.T) {
	files := map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000), "L3.DNG": signed(3)}
	for _, mode := range []string{"plain", "checksum", "rename"} {
		t.Run(mode, func(t *testing.T) {
			o := offload.Options{Sources: []string{fakeCard(t, files)}, Dest: t.TempDir(), Name: "test", Date: "2026-10-02"}
			if mode == "rename" {
				o.Rename = "{name}_{orig}"
			}
			p, err := offload.MakePlan(o)
			if err != nil {
				t.Fatal(err)
			}
			if res, err := offload.Run(context.Background(), p, nil); err != nil || !res.Safe {
				t.Fatalf("offload %v %+v", err, res)
			}
			run(t, p.Dests[0])
			o.Checksum = mode == "checksum"
			p2, err := offload.MakePlan(o)
			if err != nil {
				t.Fatal(err)
			}
			res, err := offload.Run(context.Background(), p2, nil)
			if err != nil || !res.Safe || res.Copied != 0 || len(res.Failed) != 0 {
				t.Fatalf("re-run: %v %+v", err, res)
			}
		})
	}
}

// Critical 1: a non-atomic rename-over (exFAT, SMB: delete, then rename) interrupted
// after the original's name went and before the temp took it leaves the proven temp as
// the only copy. The re-run proves it against the journal and puts it in place.
func TestOrphanTempIsOnlyCopy(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000)})
	judge(t, dir, false, &counting{})
	p := filepath.Join(dir, "M1.DNG")
	patched := applyAll(t, data["M1.DNG"])
	tmp := filepath.Join(dir, ".M1.DNG.cull-deadbeef.redate")
	os.WriteFile(tmp, patched, 0o644)
	os.Chtimes(tmp, target, target)
	st, _ := os.Stat(p)
	j := &journal.Redate{Target: targetISO, Started: time.Now(), Files: map[string]journal.FileState{
		"M1.DNG": {Size: st.Size(), ModTime: st.ModTime(), Orig: sum(data["M1.DNG"]), Want: sum(patched)},
	}}
	if err := j.Save(dir); err != nil {
		t.Fatal(err)
	}
	os.Remove(p)

	// Offload's own sweep of stale temps never takes a redate temp.
	offload.RemoveStaleTemps(dir)
	if _, err := os.Stat(tmp); err != nil {
		t.Fatal("offload's sweep removed a redate temp")
	}
	res := run(t, dir)
	if res.Refused != 0 || res.AlreadySet != 1 || res.Patched != 1 {
		t.Fatalf("result %+v", res)
	}
	if !bytes.Equal(read(t, p), patched) || !mtime(t, p).Equal(target) {
		t.Fatal("M1.DNG not restored from its proven temp")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("the temp is still there")
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("journal not completed")
	}
	if e := currentEntries(t, dir)["M1.DNG"]; e.FileSHA256 != sum(patched) || e.DatesSet != targetISO {
		t.Fatalf("manifest %+v", e)
	}
	b := &counting{}
	if _, n := judge(t, dir, true, b); n != 0 {
		t.Fatalf("judge made %d calls", n)
	}
}

// A temp whose target is missing but that doesn't prove (or has no journal record) is
// never deleted, and the journal stays.
func TestUnprovenOrphanTempKept(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	tmp := filepath.Join(dir, ".M1.DNG.cull-deadbeef.redate")
	bad := applyAll(t, data["M1.DNG"])
	bad[len(bad)-5] ^= 0xFF
	os.WriteFile(tmp, bad, 0o644)
	st, _ := os.Stat(filepath.Join(dir, "M1.DNG"))
	j := &journal.Redate{Target: targetISO, Started: time.Now(), Files: map[string]journal.FileState{
		"M1.DNG": {Size: st.Size(), ModTime: st.ModTime(), Want: sum(applyAll(t, data["M1.DNG"]))},
	}}
	j.Save(dir)
	os.Remove(filepath.Join(dir, "M1.DNG"))
	redate.Run(context.Background(), redate.Options{Dir: dir, Target: target})
	if got, err := os.ReadFile(tmp); err != nil || !bytes.Equal(got, bad) {
		t.Fatal("an unproven orphan temp was touched")
	}
	if _, _, ok := journal.Incomplete(dir); !ok {
		t.Fatal("journal completed with a frame missing")
	}
}

// A redate temp beside an existing file is a leftover from a swap that didn't
// happen: removed.
func TestLeftoverTempRemoved(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	tmp := filepath.Join(dir, ".M1.DNG.cull-deadbeef.redate")
	os.WriteFile(tmp, []byte("half written"), 0o644)
	run(t, dir)
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("leftover temp kept")
	}
}

// Important 4: a recorded frame that only gets its times (Content Credentials) is
// proven against its manifest first: a damaged one is refused and untouched.
func TestDamagedSignedFrameRefused(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"L3.DNG": signed(3), "M1.DNG": dated(1, 4000)})
	judge(t, dir, false, &counting{})
	p := filepath.Join(dir, "L3.DNG")
	b := read(t, p)
	st, _ := os.Stat(p)
	b[len(b)-10] ^= 0xff
	os.WriteFile(p, b, 0o644)
	os.Chtimes(p, st.ModTime(), st.ModTime())
	res := run(t, dir)
	if res.Refused != 1 || !strings.Contains(strings.Join(res.Refusals, ""), "L3.DNG: no longer matches its offload checksum") {
		t.Fatalf("result %+v", res)
	}
	if !mtime(t, p).Equal(st.ModTime()) || !bytes.Equal(read(t, p), b) {
		t.Fatal("damaged frame changed")
	}
	if e := currentEntries(t, dir)["L3.DNG"]; e.DatesSet != "" {
		t.Fatalf("damaged frame recorded %+v", e)
	}
	rep, _ := report.Load(filepath.Join(dir, "cull-report.json"))
	if r := resultFor(t, rep, "L3.DNG"); r.DatesSet != "" || !r.ModTime.Equal(st.ModTime()) {
		t.Fatalf("report %+v", r)
	}
}

// Important 5: extended attributes (a Finder tag) and the mode survive the swap.
func TestXattrKept(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	p := filepath.Join(dir, "M1.DNG")
	const tag = "com.apple.metadata:_kMDItemUserTags"
	if err := unix.Setxattr(p, tag, []byte("bplist-red-tag"), 0); err != nil {
		t.Skip("no xattrs here:", err)
	}
	os.Chmod(p, 0o640)
	if res := run(t, dir); res.Patched != 1 {
		t.Fatalf("%+v", res)
	}
	buf := make([]byte, 256)
	n, err := unix.Getxattr(p, tag, buf)
	if err != nil || string(buf[:n]) != "bplist-red-tag" {
		t.Fatalf("Finder tag lost: %q %v", buf[:max(n, 0)], err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", st.Mode())
	}
}

// Important 3: -r on a folder holding shoot folders of their own is refused before
// anything changes: their reports and manifests aren't this run's.
func TestRecursiveRefusesChildShootFolder(t *testing.T) {
	shoot, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	parent := filepath.Dir(shoot)
	_, err := redate.Run(context.Background(), redate.Options{Dir: parent, Target: target, Recursive: true})
	if err == nil || !strings.Contains(err.Error(), filepath.Base(shoot)) || !strings.Contains(err.Error(), "each shoot folder") {
		t.Fatalf("err %v", err)
	}
	if !bytes.Equal(read(t, filepath.Join(shoot, "M1.DNG")), data["M1.DNG"]) {
		t.Fatal("changed")
	}
	if _, err := os.Stat(filepath.Join(parent, journal.RedateName)); !os.IsNotExist(err) {
		t.Fatal("journal written")
	}
}

// Minor b: a sort folder isn't a shoot folder.
func TestSortFolderAsDirRefused(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	keep := filepath.Join(dir, "keep")
	os.Mkdir(keep, 0o755)
	os.Rename(filepath.Join(dir, "M1.DNG"), filepath.Join(keep, "M1.DNG"))
	_, err := redate.Run(context.Background(), redate.Options{Dir: keep, Target: target})
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("err %v", err)
	}
}

// Minor a: finishing an interrupted run with another report or recursion is refused,
// naming what the journal recorded.
func TestResumeDifferentOptionsRefused(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000)})
	other := filepath.Join(t.TempDir(), "r.json")
	c := judgeCfg(dir)
	c.ReportPath = other
	if _, _, err := pipeline.Run(context.Background(), c, &counting{}); err != nil {
		t.Fatal(err)
	}
	restore := redate.SetCrashAfterSwap(func(string) bool { return true })
	redate.Run(context.Background(), redate.Options{Dir: dir, Target: target, ReportPath: other})
	restore()
	for _, o := range []redate.Options{
		{Dir: dir, Target: target},
		{Dir: dir, Target: target, ReportPath: other, Recursive: true},
	} {
		if _, err := redate.Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "-o "+other) {
			t.Fatalf("%+v: err %v", o, err)
		}
	}
	if res, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target, ReportPath: other}); err != nil || res.AlreadySet != 1 || res.Patched != 1 {
		t.Fatalf("finish: %v %+v", err, res)
	}
}

// Design ruling: redate leaves the report's EXIF as the camera recorded and as judged.
// Frames that a dead clock's times split into two sets stay in those sets, and judge
// after redate makes no call, ranking included.
type ranking struct {
	counting
	ranks int32
}

func (r *ranking) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if req.SchemaName != "ranking" {
		return r.counting.Call(ctx, req)
	}
	atomic.AddInt32(&r.calls, 1)
	atomic.AddInt32(&r.ranks, 1)
	n := 0
	for _, p := range req.Parts {
		if strings.HasSuffix(p.Text, ": full frame") {
			n++
		}
	}
	var items []string
	for i := 1; i <= n; i++ {
		items = append(items, fmt.Sprintf(`{"frame":%d,"strength":"s","weakness":"w"}`, i))
	}
	js := `{"summary":"x","ranking":[` + strings.Join(items, ",") + `]}`
	return &llm.Response{JSON: json.RawMessage(js), Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}, nil
}

func TestRedateKeepsSetsAndRanks(t *testing.T) {
	at := func(seed int64, dto string) dngtest.Fixture {
		fx := dated(seed, 4000)
		fx.DateTime, fx.DTO, fx.DTD = dto, dto, dto
		return fx
	}
	dir, _ := offloaded(t, map[string]dngtest.Fixture{
		"M1.DNG": at(1, "2025:12:28 00:05:59"), "M2.DNG": at(2, "2025:12:28 00:06:00"),
		"M3.DNG": at(3, "2025:12:28 01:05:59"), "M4.DNG": at(4, "2025:12:28 01:06:00"),
	})
	b := &ranking{}
	judgeSets := func(resume bool) (int, string) {
		c := judgeCfg(dir)
		c.Resume = resume
		c.Seq = group.Options{Gap: 60 * time.Second, MaxLook: group.DefaultLook}
		c.Rank = true
		before := atomic.LoadInt32(&b.calls)
		rep, _, err := pipeline.Run(context.Background(), c, b)
		if err != nil {
			t.Fatal(err)
		}
		var sets []string
		for _, s := range rep.Sets {
			var m []string
			for _, f := range s.Members {
				m = append(m, filepath.Base(f))
			}
			sets = append(sets, strings.Join(m, "+"))
		}
		return int(atomic.LoadInt32(&b.calls) - before), strings.Join(sets, " ")
	}
	_, before := judgeSets(false)
	if b.ranks == 0 || before != "M1.DNG+M2.DNG M3.DNG+M4.DNG" {
		t.Fatalf("setup: ranks %d sets %q", b.ranks, before)
	}
	run(t, dir)
	rep, _ := report.Load(filepath.Join(dir, "cull-report.json"))
	if r := resultFor(t, rep, "M3.DNG"); r.Exif == nil || r.Exif.DateTimeOriginal != "2025:12:28 01:05:59" || r.DatesSet != targetISO {
		t.Fatalf("report after redate %+v %+v", r.Exif, r.DatesSet)
	}
	n, after := judgeSets(true)
	if n != 0 || after != before {
		t.Fatalf("judge after redate: %d calls, sets %q (were %q)", n, after, before)
	}
}

// Important 2: the report is flushed to the media (file and folder), after the
// manifest lines it goes with, before the journal that vouches for it is cleared.
func TestCheckpointFlushesBeforeJournal(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	judge(t, dir, false, &counting{})
	var events []string
	restore := redate.SetTrace(func(ev string) { events = append(events, ev) })
	run(t, dir)
	restore()
	rp := filepath.Join(dir, "cull-report.json")
	idx := func(ev string) int {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i] == ev {
				return i
			}
		}
		return -1
	}
	last := idx("journal remove")
	for _, ev := range []string{"flush " + filepath.Join(dir, offload.ManifestName), "flush " + rp, "flush " + dir} {
		if i := idx(ev); i < 0 || i > last {
			t.Fatalf("%q not before the journal removal: %q", ev, events)
		}
	}
}
