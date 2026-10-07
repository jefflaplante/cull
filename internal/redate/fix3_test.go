package redate_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/redate"
)

// Tests from the re-review of 34ee2ab (adapted from the reviewer's adv3 tests).

func hiddenTemps(dir string) []string {
	var out []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".cull-redate-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// 1: a proven temp beside a damaged journalled frame is kept on every run (file()
// mustn't drop the record that vouches for it), and the frame is left alone, until the
// user acts.
func TestHardeningTempSurvivesRuns(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
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
	b := read(t, p)
	b[len(b)-1] ^= 0xff // damaged: same size, dates still parse
	os.WriteFile(p, b, 0o644)
	os.Chtimes(p, st.ModTime(), st.ModTime())
	for i := 1; i <= 3; i++ {
		res, n, err := runNotes(t, dir)
		if err != nil || len(res.Orphans) != 1 || res.Patched != 0 {
			t.Fatalf("run %d: %v %+v\n%s", i, err, res, n.all())
		}
		if got, err := os.ReadFile(tmp); err != nil || !bytes.Equal(got, patched) {
			t.Fatalf("run %d: the proven temp beside a damaged frame was touched", i)
		}
		if !bytes.Equal(read(t, p), b) {
			t.Fatalf("run %d: the damaged frame was changed", i)
		}
		jj, _ := journal.LoadRedate(dir)
		if jj == nil || jj.Files["M1.DNG"].Want != sum(patched) {
			t.Fatalf("run %d: journal record lost: %+v", i, jj)
		}
	}
}

// A temp beside its frame that no journal records: a partial one (shorter) is removed;
// a full-size one is removed only if the frame proves against its manifest.
func TestUnjournalledTempBesideFrame(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
		tmp := filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG")
		os.WriteFile(tmp, []byte("half"), 0o644)
		if res := run(t, dir); len(res.Orphans) != 0 {
			t.Fatalf("%+v", res)
		}
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Fatal("partial temp kept")
		}
	})
	t.Run("full beside a damaged frame", func(t *testing.T) {
		dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
		p := filepath.Join(dir, "M1.DNG")
		tmp := filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG")
		os.WriteFile(tmp, applyAll(t, data["M1.DNG"]), 0o644)
		b := read(t, p)
		b[len(b)-1] ^= 0xff
		os.WriteFile(p, b, 0o644)
		res, _, _ := runNotes(t, dir)
		if len(res.Orphans) != 1 {
			t.Fatalf("%+v", res)
		}
		if _, err := os.Stat(tmp); err != nil {
			t.Fatal("full temp beside an unproven frame removed")
		}
	})
	t.Run("full beside an intact frame", func(t *testing.T) {
		dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
		tmp := filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG")
		os.WriteFile(tmp, applyAll(t, data["M1.DNG"]), 0o644)
		if res := run(t, dir); len(res.Orphans) != 0 || res.Patched != 1 {
			t.Fatalf("%+v", res)
		}
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Fatal("temp kept beside an intact frame")
		}
	})
}

// A rename that fails without touching the original: untouched, no temp, no journal.
func TestRenameFailsCleanly(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	p := filepath.Join(dir, "M1.DNG")
	restore := offload.SetRenameHook(func(old, new string) error { return syscall.EIO })
	res, n, _ := runNotes(t, dir)
	restore()
	if !bytes.Equal(read(t, p), data["M1.DNG"]) || res.Refused != 1 || len(hiddenTemps(dir)) != 0 {
		t.Fatalf("%+v temps %v\n%s", res, hiddenTemps(dir), n.all())
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("journal kept for an untouched file")
	}
	if r := run(t, dir); r.Patched != 1 {
		t.Fatalf("%+v", r)
	}
}

// After an interrupted replacement, the user moves the temp back by hand: the next run
// recognises it and finishes.
func TestInterruptedThenManualMv(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000), "M2.DNG": dated(2, 4000)})
	b := &counting{}
	judge(t, dir, false, b)
	p := filepath.Join(dir, "M1.DNG")
	restore := offload.SetRenameHook(func(old, new string) error {
		if filepath.Base(new) == "M1.DNG" {
			os.Remove(new)
			return syscall.EIO
		}
		return os.Rename(old, new)
	})
	runNotes(t, dir)
	restore()
	ts := hiddenTemps(dir)
	if len(ts) != 1 {
		t.Fatalf("temps %v", ts)
	}
	os.Rename(filepath.Join(dir, ts[0]), p)
	if res, n, err := runNotes(t, dir); err != nil || res.Refused != 0 || res.Interrupted != 0 {
		t.Fatalf("%v %+v\n%s", err, res, n.all())
	}
	if !bytes.Equal(read(t, p), applyAll(t, data["M1.DNG"])) {
		t.Fatal("bytes")
	}
	if v, err := offload.Verify(context.Background(), dir, nil); err != nil || v.OK != 2 {
		t.Fatalf("verify %+v %v", v, err)
	}
	if _, k := judge(t, dir, true, b); k != 0 {
		t.Fatalf("judge %d calls", k)
	}
}

// The original vanishes after the journal record and before the swap: the temp is
// proven, so the run says the replacement was interrupted and the next run restores it.
func TestOriginalDeletedMidSwap(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	p := filepath.Join(dir, "M1.DNG")
	saves := 0
	restore := redate.SetTrace(func(ev string) {
		if ev == "journal save" {
			if saves++; saves == 2 { // the record, before the swap
				os.Remove(p)
			}
		}
	})
	res, n, _ := runNotes(t, dir)
	restore()
	if res.Interrupted != 1 || !strings.Contains(n.all(), "proven temp") {
		t.Fatalf("run 1: %+v\n%s", res, n.all())
	}
	if res, n, err := runNotes(t, dir); err != nil || len(res.Orphans) != 0 {
		t.Fatalf("run 2: %v %+v\n%s", err, res, n.all())
	}
	if !bytes.Equal(read(t, p), applyAll(t, data["M1.DNG"])) {
		t.Fatal("not restored")
	}
}

// The original vanishes while it is being read, before any journal record (here it
// also no longer matches its manifest, so nothing is journalled): the temp is an
// unproven copy, reported as an orphan, never called proven.
func TestOriginalDeletedWhileRead(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	p := filepath.Join(dir, "M1.DNG")
	b := read(t, p)
	b[len(b)-1] ^= 0xff
	os.WriteFile(p, b, 0o644)
	restore := offload.SetAfterStreamHook(func(path string) { os.Remove(path) })
	res, n, _ := runNotes(t, dir)
	restore()
	if len(res.Orphans) != 1 || res.Interrupted != 0 || strings.Contains(n.all(), "proven temp") || !strings.Contains(n.all(), "unproven") {
		t.Fatalf("%+v\n%s", res, n.all())
	}
	if len(hiddenTemps(dir)) != 1 {
		t.Fatal("temp not kept")
	}
	if _, _, ok := journal.Incomplete(dir); !ok {
		t.Fatal("journal removed with a temp left alone")
	}
}

// A dry run after an interrupted replacement says a re-run would restore the frame,
// and has no orphans to report.
func TestDryRunAfterInterrupted(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	restore := offload.SetRenameHook(func(old, new string) error { os.Remove(new); return syscall.EIO })
	runNotes(t, dir)
	restore()
	n := &noteList{}
	res, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target, UI: n, DryRun: true})
	if err != nil || len(res.Orphans) != 0 || !strings.Contains(n.all(), "M1.DNG: a re-run would restore it") || strings.Contains(n.all(), "mv ") {
		t.Fatalf("%v %+v\n%s", err, res, n.all())
	}
	if len(hiddenTemps(dir)) != 1 {
		t.Fatal("dry run touched the temp")
	}
}

// An interrupted replacement in keep/ is restored there.
func TestInterruptedInSortFolder(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	keep := filepath.Join(dir, "keep")
	os.Mkdir(keep, 0o755)
	p := filepath.Join(keep, "M1.DNG")
	os.Rename(filepath.Join(dir, "M1.DNG"), p)
	restore := offload.SetRenameHook(func(old, new string) error { os.Remove(new); return syscall.EIO })
	runNotes(t, dir)
	restore()
	if res, n, err := runNotes(t, dir); err != nil || res.Refused != 0 || len(res.Orphans) != 0 {
		t.Fatalf("%v %+v\n%s", err, res, n.all())
	}
	if !bytes.Equal(read(t, p), applyAll(t, data["M1.DNG"])) {
		t.Fatal("not restored")
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("journal incomplete")
	}
}

// FAT tolerance: ±2 s is set; beyond it, the times are set again.
func TestFATBounds(t *testing.T) {
	dir, _ := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	run(t, dir)
	p := filepath.Join(dir, "M1.DNG")
	for d, already := range map[time.Duration]bool{2 * time.Second: true, -2 * time.Second: true, 2*time.Second + time.Millisecond: false} {
		off := target.Add(d)
		os.Chtimes(p, off, off)
		if res := run(t, dir); (res.AlreadySet == 1) != already {
			t.Fatalf("off %v: %+v", d, res)
		}
	}
}
