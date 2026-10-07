package redate_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/redate"
	"github.com/jefflaplante/cull/internal/ui"
)

// Tests from the re-review of f42260d (adapted from the reviewer's rv tests).

type noteList struct {
	mu sync.Mutex
	l  []string
}

func (n *noteList) Emit(e ui.Event) {
	if e.Note != nil {
		n.mu.Lock()
		n.l = append(n.l, e.Note.Text)
		n.mu.Unlock()
	}
}
func (n *noteList) Close() error { return nil }
func (n *noteList) all() string  { return strings.Join(n.l, "\n") }

func runNotes(t *testing.T, dir string) (redate.Result, *noteList, error) {
	t.Helper()
	n := &noteList{}
	res, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target, UI: n})
	return res, n, err
}

// NEW-1: rename(2) over the original fails after removing it (a non-atomic
// rename-over on exFAT/FAT/SMB failing mid-way). The run says so, keeps its journal
// record, and the next run restores the proven temp.
func TestRenameErrorAfterTargetGone(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000)})
	b := &counting{}
	judge(t, dir, false, b)
	p := filepath.Join(dir, "M1.DNG")
	restore := offload.SetRenameHook(func(old, new string) error {
		if filepath.Base(new) == "M1.DNG" {
			os.Remove(new)
			return &os.LinkError{Op: "rename", Old: old, New: new, Err: syscall.EIO}
		}
		return os.Rename(old, new)
	})
	res, n, _ := runNotes(t, dir)
	restore()
	if res.Interrupted != 1 || res.Refused != 0 || !strings.Contains(n.all(), "M1.DNG: replacement interrupted") || strings.Contains(n.all(), "M1.DNG: not changed") {
		t.Fatalf("run 1: %+v\n%s", res, n.all())
	}
	if _, _, ok := journal.Incomplete(dir); !ok {
		t.Fatal("journal removed while M1.DNG is missing")
	}
	res, n, err := runNotes(t, dir)
	if err != nil || res.Refused != 0 || len(res.Orphans) != 0 {
		t.Fatalf("run 2: %v %+v\n%s", err, res, n.all())
	}
	if !bytes.Equal(read(t, p), applyAll(t, data["M1.DNG"])) {
		t.Fatal("M1.DNG not restored")
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("journal still incomplete")
	}
	if v, err := offload.Verify(context.Background(), dir, nil); err != nil || v.OK != 2 || v.Bad != 0 {
		t.Fatalf("verify %+v %v", v, err)
	}
	if _, n := judge(t, dir, true, b); n != 0 {
		t.Fatalf("judge made %d calls", n)
	}
	noTemps(t, dir)
}

// NEW-2: a folder whose name holds glob characters: temps are still found.
func TestOrphanTempBracketFolder(t *testing.T) {
	d0, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000)})
	dir := filepath.Join(filepath.Dir(d0), "2026-10-02 trip [day 1]")
	if err := os.Rename(d0, dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "M1.DNG")
	patched := applyAll(t, data["M1.DNG"])
	tmp := filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG")
	os.WriteFile(tmp, patched, 0o644)
	os.Chtimes(tmp, target, target)
	st, _ := os.Stat(p)
	j := &journal.Redate{Target: targetISO, Started: time.Now(), Files: map[string]journal.FileState{
		"M1.DNG": {Size: st.Size(), ModTime: st.ModTime(), Orig: sum(data["M1.DNG"]), Want: sum(patched)},
	}}
	j.Save(dir)
	os.Remove(p)
	leftover := filepath.Join(dir, ".cull-redate-cafebabe.M2.DNG")
	os.WriteFile(leftover, []byte("x"), 0o644)
	if res, n, err := runNotes(t, dir); err != nil || res.Refused != 0 {
		t.Fatalf("%v %+v\n%s", err, res, n.all())
	}
	if !bytes.Equal(read(t, p), patched) {
		t.Fatal("M1.DNG not restored")
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatal("leftover temp not removed")
	}
	noTemps(t, dir)
}

// NEW-3: a read-only original's Finder tag survives.
func TestXattrReadOnly(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	p := filepath.Join(dir, "M1.DNG")
	const tag = "com.apple.metadata:_kMDItemUserTags"
	if err := unix.Setxattr(p, tag, []byte("bplist-red-tag"), 0); err != nil {
		t.Skip(err)
	}
	os.Chmod(p, 0o444)
	if res, n, err := runNotes(t, dir); err != nil || res.Patched != 1 {
		t.Fatalf("%v %+v\n%s", err, res, n.all())
	}
	buf := make([]byte, 256)
	if n, err := unix.Getxattr(p, tag, buf); err != nil || string(buf[:n]) != "bplist-red-tag" {
		t.Fatalf("Finder tag lost: %v", err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o444 {
		t.Fatalf("mode %v", st.Mode())
	}
}

// Regression (rv2): finishing with the journal's -o, judge on that report calls nothing.
func TestResumeWithOtherReport(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000)})
	b := &counting{}
	other := filepath.Join(t.TempDir(), "r.json")
	c := judgeCfg(dir)
	c.ReportPath = other
	pipelineRun(t, c, b)
	restore := redate.SetCrashAfterSwap(func(string) bool { return true })
	redate.Run(context.Background(), redate.Options{Dir: dir, Target: target, ReportPath: other})
	restore()
	if _, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target, ReportPath: other}); err != nil {
		t.Fatal(err)
	}
	c.Resume = true
	if n := pipelineRun(t, c, b); n != 0 {
		t.Fatalf("judge made %d calls", n)
	}
}

// Regression (rv2): an orphan temp in keep/ is restored there.
func TestOrphanInSortFolder(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000)})
	keep := filepath.Join(dir, "keep")
	os.Mkdir(keep, 0o755)
	p := filepath.Join(keep, "M1.DNG")
	os.Rename(filepath.Join(dir, "M1.DNG"), p)
	patched := applyAll(t, data["M1.DNG"])
	tmp := filepath.Join(keep, ".cull-redate-0badf00d.M1.DNG")
	os.WriteFile(tmp, patched, 0o644)
	os.Chtimes(tmp, target, target)
	st, _ := os.Stat(p)
	j := &journal.Redate{Target: targetISO, Started: time.Now(), Files: map[string]journal.FileState{
		"keep/M1.DNG": {Size: st.Size(), ModTime: st.ModTime(), Want: sum(patched)},
	}}
	j.Save(dir)
	os.Remove(p)
	if res, n, err := runNotes(t, dir); err != nil || res.Refused != 0 {
		t.Fatalf("%v %+v\n%s", err, res, n.all())
	}
	if !bytes.Equal(read(t, p), patched) {
		t.Fatal("keep/M1.DNG not restored")
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("journal still incomplete")
	}
}

// Hardening: a temp beside a journalled file is removed only when the file proves to
// be what the journal recorded (before or after); otherwise both stay, reported.
func TestTempBesideUnprovenFileKept(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	p := filepath.Join(dir, "M1.DNG")
	patched := applyAll(t, data["M1.DNG"])
	tmp := filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG")
	os.WriteFile(tmp, patched, 0o644)
	st, _ := os.Stat(p)
	j := &journal.Redate{Target: targetISO, Started: time.Now(), Files: map[string]journal.FileState{
		"M1.DNG": {Size: st.Size(), ModTime: st.ModTime(), Orig: sum([]byte("something else")), Want: sum(patched)},
	}}
	j.Save(dir)
	res, n, err := runNotes(t, dir)
	if err != nil || len(res.Orphans) != 1 || !strings.Contains(n.all(), tmp) {
		t.Fatalf("%v %+v\n%s", err, res, n.all())
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatal("temp removed beside a file that isn't the journal's")
	}
	// With the journal's Orig matching the file, the temp is a swap that never happened.
	j.Files["M1.DNG"] = journal.FileState{Size: st.Size(), ModTime: st.ModTime(), Orig: sum(data["M1.DNG"]), Want: sum(patched)}
	j.Save(dir)
	if res, n, err := runNotes(t, dir); err != nil || len(res.Orphans) != 0 {
		t.Fatalf("%v %+v\n%s", err, res, n.all())
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("temp kept beside the journal's original")
	}
}

// Orphan UX: a temp that can't be proven is listed with its full path and the exact
// command to put it back; the run doesn't claim a re-run will finish it.
func TestOrphanAdvice(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	os.Remove(filepath.Join(dir, "M1.DNG"))
	tmp := filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG")
	os.WriteFile(tmp, []byte("unknown"), 0o644)
	res, n, _ := runNotes(t, dir)
	if len(res.Orphans) != 1 || res.Orphans[0] != tmp {
		t.Fatalf("%+v", res)
	}
	all := n.all()
	if !strings.Contains(all, "mv "+journal.ShellQuote(tmp)+" "+journal.ShellQuote(filepath.Join(dir, "M1.DNG"))) || strings.Contains(all, "run the same redate again") {
		t.Fatalf("notes:\n%s", all)
	}
}

// Minors: the times-only proof refuses an unreadable manifest checksum, and proves a
// journalled (already patched) frame against the journal, not the manifest.
func TestTimesOnlyProofMinors(t *testing.T) {
	t.Run("bad checksum", func(t *testing.T) {
		dir, _ := offloaded(t, map[string]dngtest.Fixture{"L3.DNG": signed(3)})
		man := filepath.Join(dir, offload.ManifestName)
		es, _ := offload.CurrentManifest(dir)
		e := es[0]
		e.SHA256 = "not hex"
		offload.AppendManifest(dir, e)
		res, _, _ := runNotes(t, dir)
		if res.Refused != 1 || !strings.Contains(strings.Join(res.Refusals, ""), "checksum can't be read") {
			t.Fatalf("%+v", res)
		}
		_ = man
	})
	t.Run("journalled", func(t *testing.T) {
		dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
		p := filepath.Join(dir, "M1.DNG")
		st, _ := os.Stat(p)
		patched := applyAll(t, data["M1.DNG"])
		// A swap happened, but the filesystem kept another mtime (as if it rounded).
		os.WriteFile(p, patched, 0o644)
		odd := target.Add(-time.Hour)
		os.Chtimes(p, odd, odd)
		j := &journal.Redate{Target: targetISO, Started: time.Now(), Files: map[string]journal.FileState{
			"M1.DNG": {Size: st.Size(), ModTime: st.ModTime(), Orig: sum(data["M1.DNG"]), Want: sum(patched)},
		}}
		j.Save(dir)
		res, n, err := runNotes(t, dir)
		if err != nil || res.Refused != 0 || res.TimesOnly != 1 {
			t.Fatalf("%v %+v\n%s", err, res, n.all())
		}
		if e := currentEntries(t, dir)["M1.DNG"]; e.FileSHA256 != sum(patched) {
			t.Fatalf("manifest %+v", e)
		}
	})
}

// FAT keeps 2 s mtimes: a frame within 2 s of the target counts as already set.
func TestFATTimeResolution(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	run(t, dir)
	p := filepath.Join(dir, "M1.DNG")
	off := target.Add(time.Second) // what FAT32 would store for an odd second
	os.Chtimes(p, off, off)
	if res := run(t, dir); res.AlreadySet != 1 || res.TimesOnly != 0 {
		t.Fatalf("%+v", res)
	}
}

// pipelineRun judges with c and returns how many model calls it made.
func pipelineRun(t *testing.T, c pipeline.Config, b *counting) int {
	t.Helper()
	before := atomic.LoadInt32(&b.calls)
	if _, _, err := pipeline.Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	return int(atomic.LoadInt32(&b.calls) - before)
}
