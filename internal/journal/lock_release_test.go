package journal

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// lockRoots runs f on a temp folder, and also on a folder made in CULL_LOCK_DIR when
// that is set (a real volume, such as an SMB share); each folder made is removed.
func lockRoots(t *testing.T, f func(t *testing.T, dir string, onVolume bool)) {
	roots := []struct {
		name, root string
	}{{"tempdir", ""}}
	if v := os.Getenv("CULL_LOCK_DIR"); v != "" {
		roots = append(roots, struct{ name, root string }{"volume", v})
	}
	for _, r := range roots {
		t.Run(r.name, func(t *testing.T) {
			root := r.root
			if root == "" {
				root = t.TempDir()
			}
			dir, err := os.MkdirTemp(root, "cull-lock-probe")
			if err != nil {
				t.Fatal(err)
			}
			defer removeProbe(t, dir)
			f(t, dir, r.root != "")
		})
	}
}

// removeProbe removes a probe folder. An SMB share can refuse the rmdir for a moment
// after the last file in it went (its delete is still pending): retry briefly.
func removeProbe(t *testing.T, dir string) {
	var err error
	for i := 0; i < 50; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("probe folder left: %v", err)
}

// The volume behaviour that makes every release close-only. Another process takes the
// gate after this one has let go: closing this side's file must leave the other's lock
// alone. With unlockFile (close, nothing first) it must. With LOCK_UN and the close
// after the other took it, an SMB share drops the other's lock: that variant only logs
// (it is the volume's behaviour, not cull's), and runs only with
// CULL_LOCK_SHOW_UNLOCK=1, because on the NAS it once left a lock nobody owns on the
// probe's gate (unlink: resource busy) until the share let go of it.
func TestLockCloseAfterUnlockOnVolume(t *testing.T) {
	variants := []string{"close only"}
	if os.Getenv("CULL_LOCK_SHOW_UNLOCK") != "" {
		variants = append(variants, "LOCK_UN, then close after the other took it")
	}
	lockRoots(t, func(t *testing.T, dir string, _ bool) {
		gatePath := filepath.Join(dir, LockName)
		heldElsewhere := func() bool {
			f, err := os.Open(gatePath)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			return flockNB(f) != nil
		}
		for _, variant := range variants {
			f, err := os.OpenFile(gatePath, os.O_RDWR|os.O_CREATE, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if err := flockNB(f); err != nil {
				t.Fatal(err)
			}
			closeOnly := variant == "close only"
			if closeOnly {
				unlockFile(f)
			} else {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			}
			hold := exec.Command(os.Args[0], "-test.run", "^TestLockHelper$")
			hold.Env = append(os.Environ(), "CULL_LOCK_HOLD="+dir, "CULL_LOCK_MODE=writer")
			in, _ := hold.StdinPipe()
			outp, _ := hold.StdoutPipe()
			if err := hold.Start(); err != nil {
				t.Fatal(err)
			}
			line, _ := bufio.NewReader(outp).ReadString('\n')
			if !strings.Contains(line, "err=<nil>") {
				t.Fatalf("%s: the helper didn't take the gate: %s", variant, line)
			}
			if !closeOnly {
				f.Close()
			}
			kept := heldElsewhere()
			in.Close()
			hold.Wait()
			switch {
			case closeOnly && !kept:
				t.Errorf("%s: the other process's gate lock was lost", variant)
			case !closeOnly && !kept:
				t.Logf("%s: this volume drops the other process's lock (why every release is close-only)", variant)
			default:
				t.Logf("%s: the other process's lock was kept", variant)
			}
		}
	})
}

// A writer lets go while another process spins on the gate: once the other has it, it
// keeps it. Every round races the other's take against this side's release, so a release
// that unlocks before it closes loses the other's lock on an SMB share.
func TestLockReleaseKeepsNextHolder(t *testing.T) {
	lockRoots(t, func(t *testing.T, dir string, onVolume bool) {
		rounds := 20
		if onVolume {
			rounds = 60
		}
		spin := exec.Command(os.Args[0], "-test.run", "^TestLockSpinHelper$")
		spin.Env = append(os.Environ(), "CULL_LOCK_SPIN="+dir)
		in, _ := spin.StdinPipe()
		outp, _ := spin.StdoutPipe()
		if err := spin.Start(); err != nil {
			t.Fatal(err)
		}
		defer spin.Wait()
		defer in.Close()
		out := bufio.NewReader(outp)
		lost := 0
		for i := 0; i < rounds; i++ {
			release, note, err := Lock(dir, true, "rename")
			if err != nil || note != "" {
				t.Fatalf("round %d: %v %q", i, err, note)
			}
			fmt.Fprintln(in, "take")
			time.Sleep(time.Duration(rand.Intn(3000)) * time.Microsecond) // the other is spinning
			release()
			if l, _ := out.ReadString('\n'); !strings.HasPrefix(l, "got") {
				t.Fatalf("round %d: helper said %q", i, l)
			}
			if f, err := os.Open(filepath.Join(dir, LockName)); err != nil {
				t.Fatal(err)
			} else {
				if flockNB(f) == nil {
					lost++
				}
				f.Close()
			}
			fmt.Fprintln(in, "drop")
			if l, _ := out.ReadString('\n'); !strings.HasPrefix(l, "dropped") {
				t.Fatalf("round %d: helper said %q", i, l)
			}
		}
		if lost != 0 {
			t.Fatalf("the next holder's gate lock was lost in %d of %d rounds", lost, rounds)
		}
	})
}

// TestLockSpinHelper takes the gate for TestLockReleaseKeepsNextHolder: on "take" it
// spins until it has it, on "drop" it lets go.
func TestLockSpinHelper(t *testing.T) {
	dir := os.Getenv("CULL_LOCK_SPIN")
	if dir == "" {
		t.Skip("helper process only")
	}
	in := bufio.NewScanner(os.Stdin)
	var g *os.File
	for in.Scan() {
		switch in.Text() {
		case "take":
			f, err := os.OpenFile(filepath.Join(dir, LockName), os.O_RDWR|os.O_CREATE, 0o644)
			if err != nil {
				fmt.Println("error", err)
				return
			}
			for flockNB(f) != nil {
			}
			g = f
			fmt.Println("got")
		case "drop":
			unlockFile(g)
			fmt.Println("dropped")
		}
	}
}

// Separate processes, 4 readers and 2 writers, hammer one folder with random delays at
// every step; marker files in a local folder witness who holds it. Never two writers,
// never a writer with a reader. On a volume (CULL_LOCK_DIR) it runs longer.
func TestLockStressProcesses(t *testing.T) {
	lockRoots(t, func(t *testing.T, dir string, onVolume bool) {
		dur := 3 * time.Second
		if onVolume {
			dur = 12 * time.Second
		}
		wit := t.TempDir()
		var cmds []*exec.Cmd
		for i := 0; i < 6; i++ {
			mode := "reader"
			if i >= 4 {
				mode = "writer"
			}
			c := exec.Command(os.Args[0], "-test.run", "^TestLockStressHelper$", "-test.v")
			c.Env = append(os.Environ(), "CULL_STRESS_DIR="+dir, "CULL_STRESS_WIT="+wit, "CULL_STRESS_MODE="+mode,
				"CULL_STRESS_DUR="+dur.String(), "CULL_STRESS_SEED="+strconv.Itoa(i))
			cmds = append(cmds, c)
		}
		outs := make([][]byte, len(cmds))
		var wg sync.WaitGroup
		for i, c := range cmds {
			wg.Add(1)
			go func(i int, c *exec.Cmd) { defer wg.Done(); outs[i], _ = c.CombinedOutput() }(i, c)
		}
		wg.Wait()
		got := map[string]int{}
		for i, o := range outs {
			for _, l := range strings.Split(string(o), "\n") {
				l = strings.TrimSpace(l)
				if !strings.HasPrefix(l, "STRESS") {
					continue
				}
				t.Logf("%d: %s", i, l)
				if strings.Contains(l, "VIOLATION") || strings.Contains(l, "note") {
					t.Errorf("%d: %s", i, l)
				}
				var me string
				var ok, ref, viol int
				if n, _ := fmt.Sscanf(l, "STRESS %s ok %d refused %d violations %d", &me, &ok, &ref, &viol); n == 4 {
					got[me[:1]] += ok
					got["v"] += viol
				}
			}
		}
		if got["v"] != 0 {
			t.Errorf("%d violations", got["v"])
		}
		if got["r"] == 0 || got["w"] == 0 {
			t.Errorf("no progress on one side: readers %d, writers %d", got["r"], got["w"])
		}
		if n := holderFiles(t, dir); len(n) != 0 {
			t.Errorf("holder files left: %v", n)
		}
	})
}

// TestLockStressHelper is one process of TestLockStressProcesses.
func TestLockStressHelper(t *testing.T) {
	dir := os.Getenv("CULL_STRESS_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	wit, mode := os.Getenv("CULL_STRESS_WIT"), os.Getenv("CULL_STRESS_MODE")
	d, _ := time.ParseDuration(os.Getenv("CULL_STRESS_DUR"))
	seed, _ := strconv.Atoi(os.Getenv("CULL_STRESS_SEED"))
	rng := rand.New(rand.NewSource(int64(seed) + time.Now().UnixNano()))
	lockHook = func(string) { time.Sleep(time.Duration(rng.Intn(3000)) * time.Microsecond) }
	end := time.Now().Add(d)
	ok, ref, viol := 0, 0, 0
	me := fmt.Sprintf("%s-%d", mode[:1], os.Getpid())
	for time.Now().Before(end) {
		rel, note, err := Lock(dir, mode == "writer", mode)
		if note != "" {
			fmt.Println("STRESS note", note)
		}
		if err != nil {
			ref++
			time.Sleep(time.Duration(rng.Intn(30)) * time.Millisecond)
			continue
		}
		ok++
		m := filepath.Join(wit, me)
		os.WriteFile(m, nil, 0o644)
		check := func() {
			ents, _ := os.ReadDir(wit)
			for _, e := range ents {
				n := e.Name()
				if n != me && (mode == "writer" || strings.HasPrefix(n, "w-")) {
					viol++
					fmt.Println("STRESS VIOLATION", me, "sees", n)
				}
			}
		}
		check()
		time.Sleep(time.Duration(rng.Intn(20)) * time.Millisecond)
		check()
		os.Remove(m)
		rel()
		if mode == "reader" {
			time.Sleep(time.Duration(rng.Intn(150)) * time.Millisecond)
		}
	}
	fmt.Printf("STRESS %s ok %d refused %d violations %d\n", me, ok, ref, viol)
}

// The same within one process (flock is per open file here, as across processes):
// readers and writers on goroutines, random delays at every step, RetryFor short and
// long. Never a writer with a reader, never two writers, nothing left.
func TestLockStressGoroutines(t *testing.T) {
	for _, retry := range []time.Duration{0, 3 * time.Millisecond, 50 * time.Millisecond} {
		t.Run(retry.String(), func(t *testing.T) {
			old := RetryFor
			RetryFor = retry
			defer func() { RetryFor = old; lockHook = nil }()
			var rngMu sync.Mutex
			rng := rand.New(rand.NewSource(int64(retry) + 7))
			jitter := func(max int) time.Duration {
				rngMu.Lock()
				defer rngMu.Unlock()
				return time.Duration(rng.Intn(max)) * time.Microsecond
			}
			lockHook = func(string) { time.Sleep(jitter(400)) }
			dir := t.TempDir()
			var readers, writers, viol, rOK, wOK int32
			deadline := time.Now().Add(3 * time.Second)
			var wg sync.WaitGroup
			for i := 0; i < 7; i++ {
				exclusive := i >= 4
				wg.Add(1)
				go func() {
					defer wg.Done()
					for time.Now().Before(deadline) {
						rel, note, err := Lock(dir, exclusive, "stress")
						if note != "" {
							t.Errorf("note %s", note)
						}
						if err != nil {
							time.Sleep(jitter(200))
							continue
						}
						if exclusive {
							atomic.AddInt32(&wOK, 1)
							if atomic.AddInt32(&writers, 1) != 1 || atomic.LoadInt32(&readers) != 0 {
								atomic.AddInt32(&viol, 1)
							}
						} else {
							atomic.AddInt32(&rOK, 1)
							atomic.AddInt32(&readers, 1)
							if atomic.LoadInt32(&writers) != 0 {
								atomic.AddInt32(&viol, 1)
							}
						}
						time.Sleep(jitter(300))
						if exclusive {
							if atomic.LoadInt32(&readers) != 0 {
								atomic.AddInt32(&viol, 1)
							}
							atomic.AddInt32(&writers, -1)
						} else {
							if atomic.LoadInt32(&writers) != 0 {
								atomic.AddInt32(&viol, 1)
							}
							atomic.AddInt32(&readers, -1)
						}
						rel()
						if exclusive {
							time.Sleep(jitter(200))
						} else {
							time.Sleep(jitter(10000))
						}
					}
				}()
			}
			wg.Wait()
			t.Logf("readers ok %d; writers ok %d; violations %d", rOK, wOK, viol)
			if viol != 0 {
				t.Fatalf("%d mutual-exclusion violations", viol)
			}
			// With a long RetryFor a reader probing a writer's gate waits that long with
			// its holder file locked, so writers seldom find the folder free: progress is
			// required of them only without waits.
			if rOK == 0 || (wOK == 0 && retry == 0) {
				t.Fatal("no progress on one side")
			}
			if n := holderFiles(t, dir); len(n) != 0 {
				t.Fatalf("holder files left: %v", n)
			}
		})
	}
}

// A holder file the writer can't open read-write (another user's, or read-only) is still
// checked: a live reader's refuses, naming it. One it can't open at all, or can't lock
// for any reason but "held", may be a live reader's: the writer refuses, naming the file
// and the error, and lets the gate go.
func TestLockHolderUnchecked(t *testing.T) {
	t.Run("read-only, live", func(t *testing.T) {
		dir := t.TempDir()
		rel, _, err := Lock(dir, false, "judge")
		if err != nil {
			t.Fatal(err)
		}
		defer rel()
		for _, n := range holderFiles(t, dir) {
			os.Chmod(filepath.Join(dir, n), 0o444)
		}
		w, _, err := Lock(dir, true, "rename")
		if err == nil {
			w()
			t.Fatal("a writer proceeded while a reader lives")
		}
		if !strings.Contains(err.Error(), "in use by cull judge (pid ") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("unopenable", func(t *testing.T) {
		dir := t.TempDir()
		name := HolderPrefix + "99999-deadbeef"
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("cull judge (pid 99999)\n"), 0o644)
		os.Chmod(p, 0)
		defer os.Chmod(p, 0o644)
		w, note, err := Lock(dir, true, "rename")
		if err == nil {
			w()
			t.Fatalf("a writer proceeded past a holder file it can't open (note %q)", note)
		}
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("%v", err)
		}
		gateFree(t, dir)
	})
	t.Run("flock error", func(t *testing.T) {
		dir := t.TempDir()
		name := HolderPrefix + "99999-deadbeef"
		os.WriteFile(filepath.Join(dir, name), []byte("cull judge (pid 99999)\n"), 0o644)
		restore := flockFn
		defer func() { flockFn = restore }()
		var calls int
		flockFn = func(fd, how int) error {
			if calls++; calls == 1 { // the gate
				return syscall.Flock(fd, how)
			}
			return syscall.EIO
		}
		w, note, err := Lock(dir, true, "rename")
		flockFn = restore
		if err == nil {
			w()
			t.Fatalf("a writer proceeded past a holder file it can't lock (note %q)", note)
		}
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), syscall.EIO.Error()) {
			t.Fatalf("%v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("the holder file was removed")
		}
		gateFree(t, dir)
	})
}

// gateFree fails unless dir's gate can be taken (a refused writer let it go).
func gateFree(t *testing.T, dir string) {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, LockName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := flockNB(f); err != nil {
		t.Fatalf("the gate is still held: %v", err)
	}
}

// A gate that refuses with EACCES (what an SMB share returns for a stale lock nobody
// owns) counts as held: retried for RetryFor, then reader and writer alike are refused,
// told how to clear a stale lock, and leave no holder file. Other gate errors still
// proceed with a note (TestLockOtherErrorsProceed).
func TestLockGateEACCESRefuses(t *testing.T) {
	old := RetryFor
	RetryFor = 100 * time.Millisecond
	defer func() { RetryFor = old }()
	for _, exclusive := range []bool{true, false} {
		dir := t.TempDir()
		restore := flockFn
		var calls int
		flockFn = func(fd, how int) error {
			calls++
			if !exclusive && calls == 1 { // the reader's own holder file
				return syscall.Flock(fd, how)
			}
			return syscall.EACCES
		}
		release, note, err := Lock(dir, exclusive, "rename")
		flockFn = restore
		if err == nil {
			release()
			t.Fatalf("exclusive %v: proceeded past a gate refusing with EACCES (note %q)", exclusive, note)
		}
		want := "if no cull command is running, the share holds a stale lock: remount it, or remove " + filepath.Join(dir, LockName)
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "is in use by") {
			t.Fatalf("exclusive %v: %v", exclusive, err)
		}
		if calls < 3 {
			t.Fatalf("exclusive %v: the gate was tried %d times, not retried", exclusive, calls)
		}
		if n := holderFiles(t, dir); len(n) != 0 {
			t.Fatalf("exclusive %v: holder files left %v", exclusive, n)
		}
		t.Logf("exclusive %v: %v", exclusive, err)
	}
}
