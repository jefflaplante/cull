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

// Readers coexist; a writer is refused while any reader lives, naming it exactly, and a
// reader while a writer holds the gate, naming the writer. A reader's holder file goes
// when it lets go (or is refused); the gate stays.
func TestLock(t *testing.T) {
	dir := t.TempDir()
	r1, _, err := Lock(dir, false, "judge")
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := Lock(dir, false, "review")
	if err != nil {
		t.Fatalf("two readers: %v", err)
	}
	if n := len(holderFiles(t, dir)); n != 2 {
		t.Fatalf("%d holder files", n)
	}
	_, _, err = Lock(dir, true, "rename")
	if err == nil || !(strings.Contains(err.Error(), "in use by cull judge (pid ") || strings.Contains(err.Error(), "in use by cull review (pid ")) {
		t.Fatalf("writer while readers live: %v", err)
	}
	r1()
	_, _, err = Lock(dir, true, "rename")
	if err == nil || !strings.Contains(err.Error(), "in use by cull review (pid ") {
		t.Fatalf("writer while review lives: %v", err)
	}
	r2()
	if n := len(holderFiles(t, dir)); n != 0 {
		t.Fatalf("%d holder files left", n)
	}
	rx, _, err := Lock(dir, true, "rename")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Lock(dir, false, "judge")
	if err == nil || !strings.Contains(err.Error(), "in use by cull rename (pid ") {
		t.Fatalf("reader while writer holds: %v", err)
	}
	if n := len(holderFiles(t, dir)); n != 0 {
		t.Fatalf("a refused reader left its holder file (%d)", n)
	}
	if _, _, err := Lock(dir, true, "redate"); err == nil {
		t.Fatal("two writers")
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

func holderFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), HolderPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// A holder file nobody holds (its reader crashed) is cleared by the next writer.
func TestLockStaleHolderCleared(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, HolderPrefix+"99999-deadbeef")
	os.WriteFile(stale, []byte("cull judge (pid 99999, since 2026-10-06 10:00:00)\n"), 0o644)
	r, _, err := Lock(dir, true, "rename")
	if err != nil {
		t.Fatal(err)
	}
	r()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale holder file kept")
	}
	if !IsLockFile(LockName) || !IsLockFile(HolderPrefix+"1-ab") || IsLockFile("M1.DNG") {
		t.Fatal("IsLockFile")
	}
}

// Every interleaving of a reader's and a writer's steps: they never both hold the
// folder, a reader that proceeds is registered (a later writer sees it), and nothing is
// left behind once both let go. A writer either holds on, or (quick) finishes and lets
// go at once.
func TestLockInterleavings(t *testing.T) {
	restore := RetryFor
	RetryFor = 0 // no waiting: each step is a seam
	defer func() { RetryFor = restore; lockHook = nil }()
	const steps = 10
	for _, quick := range []bool{false, true} {
		for sched := 0; sched < 1<<steps; sched++ {
			dir := t.TempDir()
			type side struct {
				at      chan string
				goOn    chan struct{}
				done    chan error
				rel     chan func()
				waiting bool
				over    bool
				err     error
				release func()
			}
			mk := func() *side {
				return &side{at: make(chan string), goOn: make(chan struct{}), done: make(chan error, 1), rel: make(chan func(), 1)}
			}
			reader, writer := mk(), mk()
			lockHook = func(step string) {
				s := writer
				if strings.HasPrefix(step, "reader") {
					s = reader
				}
				s.at <- step
				<-s.goOn
			}
			run := func(s *side, exclusive bool, who string) {
				r, _, err := Lock(dir, exclusive, who)
				if exclusive && quick {
					r() // done already
					r = func() {}
				}
				s.rel <- r
				s.done <- err
			}
			wait := func(s *side) {
				select {
				case <-s.at:
					s.waiting = true
				case err := <-s.done:
					s.over, s.err, s.release = true, err, <-s.rel
				}
			}
			go run(reader, false, "judge")
			go run(writer, true, "rename")
			wait(reader)
			wait(writer)
			var trace []string
			for i := 0; i < steps; i++ {
				s, name := reader, "R"
				if sched&(1<<i) != 0 {
					s, name = writer, "W"
				}
				if s.over {
					if s == reader {
						s, name = writer, "W"
					} else {
						s, name = reader, "R"
					}
				}
				if s.over {
					break
				}
				trace = append(trace, name)
				s.goOn <- struct{}{}
				wait(s)
			}
			for _, s := range []*side{reader, writer} {
				for !s.over {
					s.goOn <- struct{}{}
					wait(s)
				}
			}
			if reader.err == nil && writer.err == nil && !quick {
				t.Fatalf("schedule %s: a reader and a writer both proceeded", strings.Join(trace, ""))
			}
			// A reader that proceeded is still registered: a later writer sees it.
			if reader.err == nil {
				writer.release()
				writer.release = func() {}
				lockHook = nil
				if r, _, err := Lock(dir, true, "redate"); err == nil {
					r()
					t.Fatalf("schedule %s: the reader proceeded, but a later writer doesn't see it", strings.Join(trace, ""))
				}
			}
			reader.release()
			writer.release()
			if n := holderFiles(t, dir); len(n) != 0 {
				t.Fatalf("schedule %s: holder files left %v", strings.Join(trace, ""), n)
			}
		}
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

// Across processes, as separate cull commands run: two readers coexist; a writer is
// refused while a reader lives; a reader while a writer holds; a crashed reader's holder
// file is cleared. In a temp folder; on a real volume with CULL_LOCK_DIR=<a folder
// there> (the folder made is removed).
func TestLockAcrossProcesses(t *testing.T) {
	root := os.Getenv("CULL_LOCK_DIR")
	if root == "" {
		root = t.TempDir()
	}
	dir, err := os.MkdirTemp(root, "cull-lock-probe")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	for _, c := range []struct {
		held        string
		exclusive   bool
		wantRefused bool
	}{
		{"reader", false, false},
		{"reader", true, true},
		{"writer", false, true},
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
		t.Logf("%s held by another process (%s); asking exclusive=%v: note %q err %v", c.held, strings.TrimSpace(line), c.exclusive, note, err)
		release()
		in.Close()
		hold.Wait()
		if note != "" {
			t.Errorf("no lock here: %s", note)
		} else if refused := err != nil; refused != c.wantRefused {
			t.Errorf("%s held, asking exclusive=%v: refused %v, want %v", c.held, c.exclusive, refused, c.wantRefused)
		}
	}
	// A reader that crashed: its process is gone, its holder file stays.
	crash := exec.Command(os.Args[0], "-test.run", "^TestLockHelper$")
	crash.Env = append(os.Environ(), "CULL_LOCK_HOLD="+dir, "CULL_LOCK_MODE=reader", "CULL_LOCK_CRASH=1")
	if out, err := crash.CombinedOutput(); err == nil {
		t.Fatalf("helper didn't crash: %s", out)
	}
	if n := len(holderFiles(t, dir)); n != 1 {
		t.Fatalf("%d holder files after the crash", n)
	}
	r, note, err := Lock(dir, true, "rename")
	if err != nil || note != "" {
		t.Fatalf("writer after a crashed reader: %v %q", err, note)
	}
	r()
	if n := len(holderFiles(t, dir)); n != 0 {
		t.Fatalf("stale holder file kept (%d)", n)
	}
}

// TestLockHelper holds a lock for TestLockOnVolume until its stdin closes.
func TestLockHelper(t *testing.T) {
	dir := os.Getenv("CULL_LOCK_HOLD")
	if dir == "" {
		t.Skip("helper process only")
	}
	release, note, err := Lock(dir, os.Getenv("CULL_LOCK_MODE") == "writer", "helper")
	fmt.Printf("held note=%q err=%v\n", note, err)
	if os.Getenv("CULL_LOCK_CRASH") != "" {
		os.Exit(3) // no release: the holder file stays, its lock goes with the process
	}
	io.Copy(io.Discard, os.Stdin)
	release()
}
