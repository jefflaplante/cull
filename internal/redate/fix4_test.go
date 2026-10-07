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

// Tests from the re-review of 00943e6 (adapted from the reviewer's adv4 tests).

// interrupt runs redate once with a rename-over of name that deletes the original and
// then fails, as a filesystem without atomic rename can: the proven temp is the only
// copy. Other files are swapped as usual.
func interrupt(t *testing.T, dir, name string) {
	t.Helper()
	restore := offload.SetRenameHook(func(old, new string) error {
		if filepath.Base(new) == name {
			os.Remove(new)
			return syscall.EIO
		}
		return os.Rename(old, new)
	})
	res, n, _ := runNotes(t, dir)
	restore()
	if res.Interrupted != 1 {
		t.Fatalf("run 1: %+v\n%s", res, n.all())
	}
}

// A frame whose name starts with "_" (Pentax _IGP, Nikon/Sony _DSC, Canon _MG_): its
// temp once read as an AppleDouble companion ("._IGP0001.DNG.cull-….redate"), so an
// interrupted replacement was never restored, and the journal never finished.
func TestUnderscoreInterruptedRestored(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"_IGP0001.DNG": dated(1, 4000), "_IGP0002.DNG": dated(2, 4000)})
	p := filepath.Join(dir, "_IGP0001.DNG")
	interrupt(t, dir, "_IGP0001.DNG")
	temps := hiddenTemps(dir)
	if len(temps) != 1 || strings.HasPrefix(temps[0], "._") || !strings.HasSuffix(temps[0], "._IGP0001.DNG") {
		t.Fatalf("temps %v", temps)
	}
	res, n, err := runNotes(t, dir)
	if err != nil || len(res.Orphans) != 0 || res.Refused != 0 {
		t.Fatalf("run 2: %v %+v\n%s", err, res, n.all())
	}
	if !bytes.Equal(read(t, p), applyAll(t, data["_IGP0001.DNG"])) {
		t.Fatal("_IGP0001.DNG not restored")
	}
	if _, _, ok := journal.Incomplete(dir); ok {
		t.Fatal("journal incomplete")
	}
	noTemps(t, dir)
	if v, err := offload.Verify(context.Background(), dir, nil); err != nil || v.OK != 2 {
		t.Fatalf("verify %+v %v", v, err)
	}
}

// The AppleDouble companion exFAT gives the new temp ("._.cull-redate-…") is never
// taken for a temp, and never stops the restore.
func TestAppleDoubleOfRedateTempIgnored(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	interrupt(t, dir, "M1.DNG")
	temps := hiddenTemps(dir)
	if len(temps) != 1 {
		t.Fatalf("temps %v", temps)
	}
	ad := filepath.Join(dir, "._"+temps[0])
	os.WriteFile(ad, []byte("AppleDouble"), 0o644)
	res, n, err := runNotes(t, dir)
	if err != nil || len(res.Orphans) != 0 || res.Refused != 0 {
		t.Fatalf("run 2: %v %+v\n%s", err, res, n.all())
	}
	if !bytes.Equal(read(t, filepath.Join(dir, "M1.DNG")), applyAll(t, data["M1.DNG"])) {
		t.Fatal("M1.DNG not restored")
	}
	if got, err := os.ReadFile(ad); err != nil || string(got) != "AppleDouble" {
		t.Fatal("companion touched")
	}
}

// 3a: a dry run shows a frame a real run would hold (a temp beside a damaged journalled
// frame) as held, never as patched; it writes nothing.
func TestDryRunHeld(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	p := filepath.Join(dir, "M1.DNG")
	patched := applyAll(t, data["M1.DNG"])
	tmp := filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG")
	os.WriteFile(tmp, patched, 0o644)
	st, _ := os.Stat(p)
	j := &journal.Redate{Target: targetISO, Started: time.Now(), Files: map[string]journal.FileState{
		"M1.DNG": {Size: st.Size(), ModTime: st.ModTime(), Orig: sum(data["M1.DNG"]), Want: sum(patched)},
	}}
	j.Save(dir)
	b := read(t, p)
	b[len(b)-1] ^= 0xff
	os.WriteFile(p, b, 0o644)
	before := snapshot(t, dir)
	n := &noteList{}
	res, err := redate.Run(context.Background(), redate.Options{Dir: dir, Target: target, UI: n, DryRun: true})
	out := n.all()
	if err != nil || res.Patched != 0 || len(res.Orphans) != 1 || strings.Contains(out, "fields →") || !strings.Contains(out, "M1.DNG: would be left alone") {
		t.Fatalf("%v %+v\n%s", err, res, out)
	}
	if snapshot(t, dir) != before {
		t.Fatal("dry run wrote something")
	}
}

// 3b: a temp beside a damaged journalled frame. A partial one (shorter than the frame,
// and not the journal's patched file) is removed; a full one that proves against the
// journal stays, even when the frame grew past it.
func TestTempBesideDamagedJournalledFrame(t *testing.T) {
	setup := func(t *testing.T) (dir, p, tmp string, data map[string][]byte) {
		dir, data = offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
		p = filepath.Join(dir, "M1.DNG")
		st, _ := os.Stat(p)
		j := &journal.Redate{Target: targetISO, Started: time.Now(), Files: map[string]journal.FileState{
			"M1.DNG": {Size: st.Size(), ModTime: st.ModTime(), Orig: sum(data["M1.DNG"]), Want: sum(applyAll(t, data["M1.DNG"]))},
		}}
		j.Save(dir)
		return dir, p, filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG"), data
	}
	t.Run("partial", func(t *testing.T) {
		dir, p, tmp, _ := setup(t)
		os.WriteFile(tmp, []byte("half"), 0o644)
		b := read(t, p)
		b[len(b)-1] ^= 0xff
		os.WriteFile(p, b, 0o644)
		res, n, _ := runNotes(t, dir)
		if len(res.Orphans) != 0 {
			t.Fatalf("%+v\n%s", res, n.all())
		}
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Fatal("partial temp kept")
		}
		if !bytes.Equal(read(t, p), b) {
			t.Fatal("damaged frame changed")
		}
	})
	t.Run("full, the frame grown past it", func(t *testing.T) {
		dir, p, tmp, data := setup(t)
		patched := applyAll(t, data["M1.DNG"])
		os.WriteFile(tmp, patched, 0o644)
		b := append(read(t, p), make([]byte, 1000)...)
		os.WriteFile(p, b, 0o644)
		for i := 1; i <= 2; i++ {
			res, n, _ := runNotes(t, dir)
			if len(res.Orphans) != 1 || res.Patched != 0 {
				t.Fatalf("run %d: %+v\n%s", i, res, n.all())
			}
			if got, err := os.ReadFile(tmp); err != nil || !bytes.Equal(got, patched) {
				t.Fatalf("run %d: the proven temp was removed", i)
			}
			if !bytes.Equal(read(t, p), b) {
				t.Fatalf("run %d: the frame changed", i)
			}
		}
	})
}

// 3c: with no journal record, a full temp is measured against the manifest's size, not
// against whatever sits at the frame's name: a larger foreign file there never gets the
// only copy of the patched frame removed.
func TestShorterRuleLargerTarget(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	p := filepath.Join(dir, "M1.DNG")
	interrupt(t, dir, "M1.DNG")
	os.Rename(filepath.Join(dir, journal.RedateName), filepath.Join(dir, "aside.json"))
	big := append(append([]byte{}, data["M1.DNG"]...), make([]byte, 1000)...)
	os.WriteFile(p, big, 0o644)
	for i := 1; i <= 2; i++ {
		res, n, _ := runNotes(t, dir)
		temps := hiddenTemps(dir)
		if len(res.Orphans) != 1 || len(temps) != 1 {
			t.Fatalf("run %d: %+v temps %v\n%s", i, res, temps, n.all())
		}
		if got := read(t, filepath.Join(dir, temps[0])); !bytes.Equal(got, applyAll(t, data["M1.DNG"])) {
			t.Fatalf("run %d: temp changed", i)
		}
		if !bytes.Equal(read(t, p), big) {
			t.Fatalf("run %d: the file at M1.DNG changed", i)
		}
	}
}

// A temp beside a damaged journalled frame in keep/: held there on every run.
func TestHeldInSortFolder(t *testing.T) {
	dir, data := offloaded(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	keep := filepath.Join(dir, "keep")
	os.Mkdir(keep, 0o755)
	p := filepath.Join(keep, "M1.DNG")
	os.Rename(filepath.Join(dir, "M1.DNG"), p)
	patched := applyAll(t, data["M1.DNG"])
	tmp := filepath.Join(keep, ".cull-redate-deadbeef.M1.DNG")
	os.WriteFile(tmp, patched, 0o644)
	st, _ := os.Stat(p)
	j := &journal.Redate{Target: targetISO, Started: time.Now(), Files: map[string]journal.FileState{
		"keep/M1.DNG": {Size: st.Size(), ModTime: st.ModTime(), Orig: sum(data["M1.DNG"]), Want: sum(patched)},
	}}
	j.Save(dir)
	b := read(t, p)
	b[len(b)-1] ^= 0xff
	os.WriteFile(p, b, 0o644)
	for i := 1; i <= 2; i++ {
		res, n, err := runNotes(t, dir)
		if _, e := os.Stat(tmp); e != nil || !bytes.Equal(read(t, p), b) || len(res.Orphans) != 1 {
			t.Fatalf("run %d: %v %+v\n%s", i, err, res, n.all())
		}
		if jj, _ := journal.LoadRedate(dir); jj == nil || jj.Files["keep/M1.DNG"].Want != sum(patched) {
			t.Fatalf("run %d: record lost", i)
		}
	}
}

// A full temp beside a frame in a folder no manifest records: kept on every run, and
// the journal stays.
func TestUnrecordedFullTempHeld(t *testing.T) {
	dir, data := loose(t, map[string]dngtest.Fixture{"M1.DNG": dated(1, 4000)})
	tmp := filepath.Join(dir, ".cull-redate-deadbeef.M1.DNG")
	os.WriteFile(tmp, applyAll(t, data["M1.DNG"]), 0o644)
	for i := 1; i <= 2; i++ {
		res, n, err := runNotes(t, dir)
		if _, e := os.Stat(tmp); e != nil || len(res.Orphans) != 1 {
			t.Fatalf("run %d: %v %+v\n%s", i, err, res, n.all())
		}
		if _, _, ok := journal.Incomplete(dir); !ok {
			t.Fatalf("run %d: journal finished with a temp held", i)
		}
	}
}
