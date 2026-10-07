package journal

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Shared holders share; an exclusive holder excludes everyone, and a refusal names the
// holder. The lock file stays (removing it would split later holders).
func TestLock(t *testing.T) {
	dir := t.TempDir()
	r1, _, err := Lock(dir, false, "judge")
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := Lock(dir, false, "review")
	if err != nil {
		t.Fatalf("two shared holders: %v", err)
	}
	if _, _, err := Lock(dir, true, "rename"); err == nil || !strings.Contains(err.Error(), "cull ") {
		t.Fatalf("exclusive while shared: %v", err)
	}
	r1()
	r2()
	rx, _, err := Lock(dir, true, "rename")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Lock(dir, false, "judge")
	if err == nil || !strings.Contains(err.Error(), "cull rename") || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("shared while exclusive: %v", err)
	}
	rx()
	if _, err := os.Stat(filepath.Join(dir, LockName)); err != nil {
		t.Fatal(err)
	}
	if r, _, err := Lock(dir, true, "redate"); err != nil {
		t.Fatal(err)
	} else {
		r()
	}
}

// An unfinished redate and an unfinished rename in one folder, or below it: both are
// reported.
func TestIncompleteBelowEveryKind(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "day1")
	os.Mkdir(sub, 0o755)
	(&Redate{Target: "2026-10-04T12:00:00"}).Save(dir)
	(&Rename{Pattern: "{n}", Phase: 1}).Save(dir)
	(&Rename{Pattern: "{n:2}", Phase: 1}).Save(sub)
	got := IncompleteBelow(dir)
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	kinds := map[string]bool{}
	for _, p := range got {
		kinds[p.Folder+" "+p.Which] = true
	}
	if !kinds[dir+" redate"] || !kinds[dir+" rename"] || !kinds[sub+" rename"] {
		t.Fatalf("%+v", got)
	}
}

// Any flock failure but "held elsewhere" (ENOTSUP on a share, ENOLCK, EIO…) proceeds
// unlocked with a one-line note, shared and exclusive alike.
func TestLockOtherErrorsProceed(t *testing.T) {
	dir := t.TempDir()
	for _, e := range []error{syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOLCK, syscall.EIO} {
		restore := flockFn
		flockFn = func(int, int) error { return e }
		for _, ex := range []bool{false, true} {
			release, note, err := Lock(dir, ex, "rename")
			if err != nil || note == "" || !strings.Contains(note, "lock unavailable") {
				t.Errorf("%v exclusive %v: note %q err %v", e, ex, note, err)
			}
			release()
		}
		flockFn = restore
	}
}

// On a real volume (CULL_LOCK_DIR=<a folder there>; skipped otherwise), in a temp
// folder made and removed: a lock held by another process (as another cull command
// would hold it) excludes as it should, or the volume has no locks and every call
// proceeds with a note. Same-process probes say nothing on SMB, whose locks are
// per process.
func TestLockOnVolume(t *testing.T) {
	root := os.Getenv("CULL_LOCK_DIR")
	if root == "" {
		t.Skip("set CULL_LOCK_DIR to a folder on the volume to probe")
	}
	dir, err := os.MkdirTemp(root, "cull-lock-probe")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	for _, c := range []struct {
		held, want   string
		exclusive    bool
		shouldRefuse bool
	}{
		{"shared", "exclusive", true, true},
		{"shared", "shared", false, false},
		{"exclusive", "shared", false, true},
	} {
		hold := exec.Command(os.Args[0], "-test.run", "^TestLockHelper$")
		hold.Env = append(os.Environ(), "CULL_LOCK_HOLD="+dir, "CULL_LOCK_MODE="+c.held)
		in, _ := hold.StdinPipe()
		outp, _ := hold.StdoutPipe()
		if err := hold.Start(); err != nil {
			t.Fatal(err)
		}
		line, _ := bufio.NewReader(outp).ReadString('\n')
		release, note, err := Lock(dir, c.exclusive, "probe")
		t.Logf("held %s by another process (%s); asking %s: note %q err %v", c.held, strings.TrimSpace(line), c.want, note, err)
		release()
		in.Close()
		hold.Wait()
		refused := err != nil
		switch {
		case note != "" || refused == c.shouldRefuse:
		case refused: // safe side: SMB (smbfs) treats shared locks as exclusive
			t.Logf("this volume refuses a %s lock while another process holds a %s one: two such cull commands can't run on one folder here", c.want, c.held)
		default:
			t.Errorf("%s granted while %s held by another process", c.want, c.held)
		}
	}
}

// TestLockHelper holds a lock for TestLockOnVolume until its stdin closes.
func TestLockHelper(t *testing.T) {
	dir := os.Getenv("CULL_LOCK_HOLD")
	if dir == "" {
		t.Skip("helper process only")
	}
	release, note, err := Lock(dir, os.Getenv("CULL_LOCK_MODE") == "exclusive", "helper")
	fmt.Printf("held note=%q err=%v\n", note, err)
	io.Copy(io.Discard, os.Stdin)
	release()
}
