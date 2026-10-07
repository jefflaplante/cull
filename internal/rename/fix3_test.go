package rename_test

// Fix round 3: the folder lock's files are never frames, temps or records.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/journal"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/rename"
)

// A crashed reader's holder file and the gate, in the shoot folder and a sort folder:
// status counts no extra frame and lists no temp, Verify finds nothing unrecorded, and
// rename goes ahead (clearing the stale holder file) with every record following.
func TestLockFilesIgnored(t *testing.T) {
	dir, data, where, b := shoot(t)
	for _, d := range []string{dir, filepath.Join(dir, "keep")} {
		os.WriteFile(filepath.Join(d, journal.HolderPrefix+"99999-deadbeef"), []byte("cull judge (pid 99999)\n"), 0o644)
		os.WriteFile(filepath.Join(d, journal.LockName), nil, 0o644)
	}
	out, err := cullCmd("status", dir)
	if err != nil || !strings.Contains(out, ": 5 DNGs") || strings.Contains(out, "cull-holder") || strings.Contains(out, "next: mv") {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if v, err := offload.Verify(context.Background(), dir, nil); err != nil || len(v.Unrecorded) != 0 || v.OK != 5 {
		t.Fatalf("verify %+v %v", v, err)
	}
	runRename(t, rename.Options{Dir: dir, Pattern: pattern})
	follows(t, dir, data, where, renamed, labelsOf(), b)
	if _, err := os.Stat(filepath.Join(dir, journal.HolderPrefix+"99999-deadbeef")); !os.IsNotExist(err) {
		t.Fatal("the stale holder file in the shoot folder was kept")
	}
}
