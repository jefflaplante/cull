package rename_test

// Fix round 2 (re-review of 40aa5da): adapted from the reviewer's rr_test.go.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/rename"
)

// With --backup, each destination is satisfied by the copy under the name its own
// manifest records: renaming the primary, the backup, or both, then re-running the
// same offload copies nothing and is safe to format. With camera names and with
// --rename, for plain renumbering and for names shifted onto other frames' names.
func TestRRBackupRerun(t *testing.T) {
	for _, offloadPattern := range []string{"", "{orig}"} {
		for _, tc := range []struct {
			name, pattern string
			files         []string
		}{
			{"plain", "x_{n:3}", []string{"M1.DNG", "M2.DNG"}},
			{"names-of-others", "M{n}", []string{"A.DNG", "M1.DNG"}},
		} {
			for _, which := range []string{"primary", "backup", "both"} {
				t.Run(offloadPattern+"/"+tc.name+"/"+which, func(t *testing.T) {
					c, dest, bak := t.TempDir(), t.TempDir(), t.TempDir()
					fx := map[string]dngtest.Fixture{}
					for i, n := range tc.files {
						fx[n] = frame(int64(i+1), 4000+100*i)
					}
					card(t, c, fx)
					o := offload.Options{Sources: []string{c}, Dest: dest, Backup: bak, Name: "test", Date: "2026-10-02", Rename: offloadPattern}
					p, err := offload.MakePlan(o)
					if err != nil {
						t.Fatal(err)
					}
					if res, err := offload.Run(context.Background(), p, nil); err != nil || !res.Safe {
						t.Fatalf("%v %+v", err, res)
					}
					if which != "backup" {
						runRename(t, rename.Options{Dir: p.Dests[0], Pattern: tc.pattern})
					}
					if which != "primary" {
						runRename(t, rename.Options{Dir: p.Dests[1], Pattern: tc.pattern})
					}
					p2, err := offload.MakePlan(o)
					if err != nil {
						t.Fatalf("re-plan refused: %v", err)
					}
					for _, f := range p2.Files {
						if f.Skip == "" {
							t.Errorf("%s would be copied again as %s to %v", filepath.Base(f.Src), f.Name, f.To)
						}
					}
					res, err := offload.Run(context.Background(), p2, nil)
					if err != nil || !res.Safe || res.Copied != 0 {
						t.Fatalf("re-run: %v %+v", err, res)
					}
					for _, d := range p.Dests {
						if v, err := offload.Verify(context.Background(), d, nil); err != nil || v.OK != 2 || v.Bad != 0 {
							t.Fatalf("verify %s: %+v %v", d, v, err)
						}
					}
				})
			}
		}
	}
	// One copy missing from the backup: it is copied there again, under the name the
	// backup's own manifest records (its camera name), not the primary's new name.
	t.Run("missing-from-backup", func(t *testing.T) {
		c, dest, bak := t.TempDir(), t.TempDir(), t.TempDir()
		card(t, c, map[string]dngtest.Fixture{"M1.DNG": frame(1, 4000), "M2.DNG": frame(2, 4100)})
		o := offload.Options{Sources: []string{c}, Dest: dest, Backup: bak, Name: "test", Date: "2026-10-02"}
		p, _ := offload.MakePlan(o)
		offload.Run(context.Background(), p, nil)
		runRename(t, rename.Options{Dir: p.Dests[0], Pattern: "x_{n:3}"})
		os.Remove(filepath.Join(p.Dests[1], "M2.DNG"))
		p2, err := offload.MakePlan(o)
		if err != nil {
			t.Fatal(err)
		}
		res, err := offload.Run(context.Background(), p2, nil)
		if err != nil || !res.Safe || res.Copied != 1 {
			t.Fatalf("%v %+v", err, res)
		}
		if _, err := os.Stat(filepath.Join(p.Dests[1], "M2.DNG")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(p.Dests[0], "x_002.DNG")); err != nil {
			t.Fatal(err)
		}
	})
}

// Every dry run and --estimate takes no lock and leaves no .cull.lock.
func TestDryRunsTakeNoLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-04 trip")
	loose(t, dir, map[string]dngtest.Fixture{"A1.DNG": frame(1, 1000)})
	if _, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "x{n}", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"redate", "--dry-run", "--date", "2026-10-04", dir},
		{"judge", "--estimate", "--backend", "anthropic", "--model", "claude-sonnet-5-5", dir},
		{"decide", "--dry-run", dir},
	} {
		cullCmd(args...)
	}
	if _, err := os.Stat(filepath.Join(dir, journal.LockName)); !os.IsNotExist(err) {
		t.Fatalf("a dry run made the lock file: %v", err)
	}
}

// The refusal names exactly the command in the way: each reader names itself.
func TestRRLockHolderMessage(t *testing.T) {
	dir := t.TempDir()
	r1, _, _ := journal.Lock(dir, false, "judge")
	r2, _, _ := journal.Lock(dir, false, "review")
	r2()
	_, _, err := journal.Lock(dir, true, "rename")
	r1()
	if err == nil || !strings.Contains(err.Error(), "is in use by cull judge (pid ") {
		t.Fatalf("%v", err)
	}
}

// scan, tag, rank, import-labels and decide refuse while a rename holds the folder.
func TestRRUnlockedWriters(t *testing.T) {
	dir, _, _, _ := shoot(t)
	release, _, err := journal.Lock(dir, true, "rename")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	labels := filepath.Join(t.TempDir(), "labels.csv")
	os.WriteFile(labels, []byte("file,label\nM4.DNG,keep\n"), 0o644)
	for _, args := range [][]string{
		{"scan", dir},
		{"tag", dir, "--project", "p"},
		{"decide", dir},
		{"rank", "--backend", "openai", "--model", "m", dir},
		{"import-labels", labels, dir},
	} {
		out, err := cullCmd(args...)
		if err == nil || !strings.Contains(err.Error(), "is in use by cull rename (pid ") {
			t.Errorf("%v under a rename's lock: %v\n%.300s", args[0], err, out)
		}
	}
}

// judge -r holds every folder it reads frames from: a rename holding a subfolder stops it.
func TestJudgeRecursiveLocksSubfolders(t *testing.T) {
	parent := t.TempDir()
	sub := filepath.Join(parent, "day1")
	loose(t, sub, map[string]dngtest.Fixture{"A1.DNG": frame(1, 1000)})
	release, _, err := journal.Lock(sub, true, "rename")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if out, err := cullCmd("judge", "-r", "--backend", "openai", "--model", "m", parent); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("judge -r: %v\n%s", err, out)
	}
}

// offload holds a new shoot folder from the moment it creates it, and RunHeld keeps
// holding it (for the scan after the copy) until released.
func TestOffloadRunHeldKeepsLock(t *testing.T) {
	c, dest := t.TempDir(), t.TempDir()
	card(t, c, map[string]dngtest.Fixture{"M1.DNG": frame(1, 4000)})
	p, err := offload.MakePlan(offload.Options{Sources: []string{c}, Dest: dest, Name: "test", Date: "2026-10-02"})
	if err != nil {
		t.Fatal(err)
	}
	res, release, err := offload.RunHeld(context.Background(), p, nil)
	if err != nil || !res.Safe {
		t.Fatalf("%v %+v", err, res)
	}
	if _, _, err := journal.Lock(p.Dests[0], true, "rename"); err == nil || !strings.Contains(err.Error(), "cull offload") {
		t.Fatalf("a rename got in while offload held the folder: %v", err)
	}
	release()
	r, _, err := journal.Lock(p.Dests[0], true, "rename")
	if err != nil {
		t.Fatal(err)
	}
	r()
}
