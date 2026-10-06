package offload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/ui"
)

// pipeCard writes n files of random bytes into a fresh card's DCIM folder, file i
// size(i) bytes long, and returns the card and each file's bytes by name.
func pipeCard(t *testing.T, n int, rng *rand.Rand, size func(i int) int) (string, map[string][]byte) {
	t.Helper()
	src := t.TempDir()
	dcim := filepath.Join(src, "DCIM")
	if err := os.MkdirAll(dcim, 0o755); err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("M%04d.DNG", i)
		b := make([]byte, size(i))
		rng.Read(b)
		p := filepath.Join(dcim, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, t0, t0)
		data[name] = b
	}
	return src, data
}

// pipePlan plans src into a fresh destination and backup: two copies of every file.
func pipePlan(t *testing.T, src string) *Plan {
	t.Helper()
	o := opts(t, src)
	o.Backup = t.TempDir()
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Dests) != 2 {
		t.Fatalf("dests %v", p.Dests)
	}
	return p
}

var tmpRE = regexp.MustCompile(`^\.(.+)\.cull-[0-9a-f]{8}\.tmp$`)

// watch installs hooks on a plan that check, every time any of them runs, what must
// hold at every moment of a run, not just at its end:
//   - a file has its final name only once its verify read began on that destination,
//     and then holds exactly the card's bytes (so a corrupted copy is never named);
//   - at most two files have temp files at once (invariant 10).
//
// Tests add their own behaviour (failures, delays, blocking) through its fields.
type watch struct {
	t    *testing.T
	p    *Plan
	data map[string][]byte

	mu        sync.Mutex
	verifying map[string]int // dest/name → verify reads begun
	checked   map[string]bool
	opens     map[string]int
	maxFiles  int // most files with temps at one moment

	open         func(name string, try int) error // try: 1 for the first open of name
	write        func(f *os.File) error
	afterWrite   func(tmp string)
	beforeVerify func(tmp string, try int) // try: 1 for the first verify of this name here
	delay        func()                    // before every hook, if set
}

func watchPlan(t *testing.T, p *Plan, data map[string][]byte) *watch {
	w := &watch{t: t, p: p, data: data, verifying: map[string]int{}, checked: map[string]bool{}, opens: map[string]int{}}
	p.h.open = func(s string) (io.ReadCloser, error) {
		w.pause()
		w.check()
		w.mu.Lock()
		w.opens[filepath.Base(s)]++
		try := w.opens[filepath.Base(s)]
		w.mu.Unlock()
		if w.open != nil {
			if err := w.open(filepath.Base(s), try); err != nil {
				return nil, err
			}
		}
		return openSource(s, hooks{})
	}
	p.h.write = func(f *os.File, b []byte) (int, error) {
		w.pause()
		w.check()
		if w.write != nil {
			if err := w.write(f); err != nil {
				return 0, err
			}
		}
		return f.Write(b)
	}
	p.h.afterWrite = func(tmp string) {
		w.pause()
		w.check()
		if w.afterWrite != nil {
			w.afterWrite(tmp)
		}
	}
	p.h.beforeVerify = func(tmp string) {
		w.pause()
		m := tmpRE.FindStringSubmatch(filepath.Base(tmp))
		if m == nil {
			t.Errorf("verify of %s, not a temp file", tmp)
			return
		}
		key := filepath.Join(filepath.Dir(tmp), m[1])
		w.mu.Lock()
		w.verifying[key]++
		try := w.verifying[key]
		w.mu.Unlock()
		w.check()
		if w.beforeVerify != nil {
			w.beforeVerify(tmp, try)
		}
	}
	return w
}

func (w *watch) pause() {
	if w.delay != nil {
		w.delay()
	}
}

func (w *watch) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	inFlight := map[string]bool{}
	for _, d := range w.p.Dests {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			n := e.Name()
			if m := tmpRE.FindStringSubmatch(n); m != nil {
				inFlight[m[1]] = true
				continue
			}
			if !strings.EqualFold(filepath.Ext(n), ".dng") {
				continue
			}
			p := filepath.Join(d, n)
			if w.verifying[p] == 0 {
				w.t.Errorf("%s has its final name before any verify read of it", p)
			}
			if w.checked[p] {
				continue
			}
			w.checked[p] = true
			if b, err := os.ReadFile(p); err != nil || !bytes.Equal(b, w.data[n]) {
				w.t.Errorf("%s has its final name but isn't the card's bytes (err %v)", p, err)
			}
		}
	}
	if len(inFlight) > w.maxFiles {
		w.maxFiles = len(inFlight)
	}
}

func (w *watch) opened(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.opens[name]
}

// corrupt flips one byte of a temp file in place.
func corrupt(t *testing.T, p string) {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Errorf("corrupt %s: %v", p, err)
		return
	}
	defer f.Close()
	b := []byte{0}
	f.ReadAt(b, 0)
	b[0] ^= 0xFF
	f.WriteAt(b, 0)
}

// planNames is p's files to copy, in plan order.
func planNames(p *Plan) []string {
	var out []string
	for _, f := range p.Files {
		if f.Skip == "" {
			out = append(out, f.Name)
		}
	}
	return out
}

func without(names []string, drop ...string) []string {
	var out []string
	for _, n := range names {
		keep := true
		for _, d := range drop {
			keep = keep && n != d
		}
		if keep {
			out = append(out, n)
		}
	}
	return out
}

// recorded checks every destination after a run: its manifest lists exactly want, in
// that order, each with the card's checksum; exactly those files have final names,
// each the card's bytes; and no temp file is left (invariants 4 and 5).
func recorded(t *testing.T, p *Plan, data map[string][]byte, want []string) {
	t.Helper()
	for _, d := range p.Dests {
		man, err := readManifest(d)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range man {
			got = append(got, e.Name)
			if sum := sha256.Sum256(data[e.Name]); e.SHA256 != hexOf(sum[:]) || e.Size != int64(len(data[e.Name])) {
				t.Errorf("%s: manifest line for %s has the wrong checksum or size", d, e.Name)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s manifest:\n got %v\nwant %v", d, got, want)
		}
		ents, _ := os.ReadDir(d)
		named := map[string]bool{}
		for _, e := range ents {
			n := e.Name()
			if tmpRE.MatchString(n) {
				t.Errorf("%s: temp file %s left", d, n)
			}
			if strings.EqualFold(filepath.Ext(n), ".dng") && !strings.HasPrefix(n, ".") {
				named[n] = true
				if b, _ := os.ReadFile(filepath.Join(d, n)); !bytes.Equal(b, data[n]) {
					t.Errorf("%s: %s isn't the card's bytes", d, n)
				}
			}
		}
		for _, n := range want {
			if !named[n] {
				t.Errorf("%s: %s recorded but not there", d, n)
			}
			delete(named, n)
		}
		for n := range named {
			t.Errorf("%s: %s has its final name but no manifest line", d, n)
		}
	}
}

func sizes(data map[string][]byte, names []string) (n int64) {
	for _, s := range names {
		n += int64(len(data[s]))
	}
	return n
}

// noLeak fails if the goroutines started during a run haven't exited (invariant 11).
func noLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		buf := make([]byte, 1<<20)
		t.Fatalf("%d goroutines before the run, %d after:\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
}

// The pipeline does exactly what the one-file-at-a-time engine did: the same copies,
// the same manifests in the same order, the same counts.
func TestPipelineMatchesSerial(t *testing.T) {
	src, data := pipeCard(t, 20, rand.New(rand.NewSource(1)), func(i int) int {
		if i%5 == 0 {
			return 5<<20 + 77*i // more than one card read buffer
		}
		return 1000 + 997*i
	})
	serial, pipe := pipePlan(t, src), pipePlan(t, src)
	serial.h.serial = true
	w := watchPlan(t, pipe, data)
	rs, err := Run(context.Background(), serial, nil)
	if err != nil || !rs.Safe {
		t.Fatalf("serial: %v %+v", err, rs)
	}
	rp, err := Run(context.Background(), pipe, nil)
	if err != nil || !rp.Safe {
		t.Fatalf("pipelined: %v %+v", err, rp)
	}
	if rs.Copied != rp.Copied || rs.Skipped != rp.Skipped || rs.Unverified != rp.Unverified ||
		rs.Bytes != rp.Bytes || len(rs.Failed) != len(rp.Failed) || rs.Copied != 20 {
		t.Fatalf("serial %+v\npipelined %+v", rs, rp)
	}
	all := planNames(pipe)
	recorded(t, serial, data, all)
	recorded(t, pipe, data, all)
	for i := range pipe.Dests {
		ms, _ := readManifest(serial.Dests[i])
		mp, _ := readManifest(pipe.Dests[i])
		for j := range ms {
			ms[j].At, mp[j].At = time.Time{}, time.Time{}
			if ms[j] != mp[j] {
				t.Fatalf("manifest line %d differs:\n serial %+v\npipelined %+v", j, ms[j], mp[j])
			}
		}
	}
	if w.maxFiles > 2 {
		t.Fatalf("%d files had temps at once", w.maxFiles)
	}
}

// While file 1 is in stage B (fsync, evict, verify, link), file 2's card read runs;
// file 3's doesn't start until file 1 is done (depth 1 per stage).
func TestPipelineOverlapsCardReadWithVerify(t *testing.T) {
	src, data := pipeCard(t, 3, rand.New(rand.NewSource(2)), func(int) int { return 64 << 10 })
	p := pipePlan(t, src)
	w := watchPlan(t, p, data)
	names := planNames(p)
	reading2 := make(chan struct{})
	var once sync.Once
	w.write = func(f *os.File) error {
		if strings.Contains(f.Name(), names[1]) {
			once.Do(func() { close(reading2) })
		}
		return nil
	}
	overlapped := false
	w.beforeVerify = func(tmp string, try int) {
		if !strings.Contains(tmp, names[0]) || filepath.Dir(tmp) != p.Dests[0] {
			return
		}
		select {
		case <-reading2:
			overlapped = true
		case <-time.After(5 * time.Second):
			t.Errorf("file 2's card read didn't run while file 1 was in stage B")
			return
		}
		time.Sleep(50 * time.Millisecond)
		if n := w.opened(names[2]); n != 0 {
			t.Errorf("file 3 opened while file 1 was still in stage B: more than two files in flight")
		}
	}
	res, err := Run(context.Background(), p, nil)
	if err != nil || !res.Safe || res.Copied != 3 || !overlapped {
		t.Fatalf("err %v result %+v overlapped %v", err, res, overlapped)
	}
	recorded(t, p, data, names)
}

// A copy that doesn't match the card at verify (once) is copied again from the card;
// nothing is named before its own verify passed, and the manifest lists every file
// once, in order.
func TestPipelineRetriesVerifyMismatch(t *testing.T) {
	src, data := pipeCard(t, 8, rand.New(rand.NewSource(3)), func(i int) int { return 20000 + i })
	p := pipePlan(t, src)
	w := watchPlan(t, p, data)
	names := planNames(p)
	k := names[3]
	w.beforeVerify = func(tmp string, try int) {
		if strings.Contains(tmp, k) && filepath.Dir(tmp) == p.Dests[0] && try == 1 {
			corrupt(t, tmp)
		}
	}
	res, err := Run(context.Background(), p, nil)
	if err != nil || !res.Safe || res.Copied != 8 || len(res.Failed) != 0 {
		t.Fatalf("err %v result %+v", err, res)
	}
	if n := w.opened(k); n != 2 {
		t.Fatalf("%s read off the card %d times, want 2 (the retry re-reads it)", k, n)
	}
	recorded(t, p, data, names)
	if w.maxFiles > 2 {
		t.Fatalf("%d files had temps at once", w.maxFiles)
	}
}

// A file that fails every try, in stage A or in stage B, is listed as failed and
// leaves nothing behind; every other file is copied and recorded, in order.
func TestPipelinePersistentFailure(t *testing.T) {
	for _, stage := range []string{"card read", "verify"} {
		t.Run(stage, func(t *testing.T) {
			src, data := pipeCard(t, 8, rand.New(rand.NewSource(4)), func(i int) int { return 30000 + i })
			p := pipePlan(t, src)
			w := watchPlan(t, p, data)
			names := planNames(p)
			k := names[5]
			if stage == "card read" {
				w.open = func(name string, try int) error {
					if name == k {
						return errors.New("input/output error")
					}
					return nil
				}
			} else {
				w.beforeVerify = func(tmp string, try int) {
					if strings.Contains(tmp, k) {
						corrupt(t, tmp)
					}
				}
			}
			before := runtime.NumGoroutine()
			res, err := Run(context.Background(), p, nil)
			noLeak(t, before)
			if err != nil || res.Safe || res.Copied != 7 || len(res.Failed) != 1 || !strings.HasPrefix(res.Failed[0], k+": ") {
				t.Fatalf("err %v result %+v", err, res)
			}
			if n := w.opened(k); n != 1+len(retryDelays) {
				t.Fatalf("%s opened %d times, want %d", k, n, 1+len(retryDelays))
			}
			if res.Bytes != sizes(data, without(names, k)) {
				t.Fatalf("bytes %d", res.Bytes)
			}
			recorded(t, p, data, without(names, k))
		})
	}
}

// Ctrl-C while a file is in stage B (or while the next file's card read runs beside
// it): Run returns ctx.Err(); every file with a final name was verified and recorded,
// and no temp file is left.
func TestPipelineCancel(t *testing.T) {
	for _, when := range []string{"in stage B", "in the next file's stage A"} {
		t.Run(when, func(t *testing.T) {
			src, data := pipeCard(t, 10, rand.New(rand.NewSource(5)), func(i int) int { return 40000 + i })
			p := pipePlan(t, src)
			w := watchPlan(t, p, data)
			names := planNames(p)
			k, next := names[4], names[5]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reading := make(chan struct{})
			var once sync.Once
			if when == "in stage B" {
				w.beforeVerify = func(tmp string, try int) {
					if strings.Contains(tmp, k) {
						cancel()
					}
				}
			} else {
				w.write = func(f *os.File) error {
					if strings.Contains(f.Name(), next) {
						once.Do(func() { cancel(); close(reading) })
					}
					return nil
				}
				w.beforeVerify = func(tmp string, try int) {
					if strings.Contains(tmp, k) {
						select {
						case <-reading:
						case <-time.After(5 * time.Second):
							t.Errorf("%s's card read didn't run while %s verified", next, k)
						}
					}
				}
			}
			before := runtime.NumGoroutine()
			res, err := Run(ctx, p, nil)
			noLeak(t, before)
			if !errors.Is(err, context.Canceled) || res.Safe {
				t.Fatalf("err %v result %+v", err, res)
			}
			man, _ := readManifest(p.Dests[0])
			if res.Copied != len(man) || res.Copied < 4 || res.Copied > 5 {
				t.Fatalf("copied %d, manifest %d lines", res.Copied, len(man))
			}
			recorded(t, p, data, names[:res.Copied])
		})
	}
}

// A full destination in stage A stops the run: the file in stage B finishes and is
// recorded, the full file and every later one are listed, and no later file is read.
func TestPipelineDestinationFull(t *testing.T) {
	src, data := pipeCard(t, 8, rand.New(rand.NewSource(6)), func(i int) int { return 50000 + i })
	p := pipePlan(t, src)
	w := watchPlan(t, p, data)
	names := planNames(p)
	k := 4
	w.write = func(f *os.File) error {
		if strings.Contains(f.Name(), names[k]) {
			return syscall.ENOSPC
		}
		return nil
	}
	before := runtime.NumGoroutine()
	res, err := Run(context.Background(), p, nil)
	noLeak(t, before)
	if err != nil || res.Safe || res.Copied != k || len(res.Failed) != len(names)-k {
		t.Fatalf("err %v result %+v", err, res)
	}
	if !strings.HasPrefix(res.Failed[0], names[k]+": destination full") {
		t.Fatalf("first failure %q", res.Failed[0])
	}
	for i, f := range res.Failed[1:] {
		if f != names[k+1+i]+": not attempted: destination full" {
			t.Fatalf("failure %q", f)
		}
	}
	if w.opened(names[k]) != 1 {
		t.Fatalf("%s opened %d times: a full disk isn't retried", names[k], w.opened(names[k]))
	}
	for _, n := range names[k+1:] {
		if w.opened(n) != 0 {
			t.Fatalf("%s read off the card after the destination filled", n)
		}
	}
	recorded(t, p, data, names[:k])
}

// Many files, random small delays in every hook, a few injected failures: at every
// moment nothing is named unverified and at most two files are in flight; at the end
// every good file is recorded once, in order, nothing is left behind, and every
// goroutine has exited. Run it under -race.
func TestPipelineStress(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewSource(seed))
	n := 200
	src, data := pipeCard(t, n, rng, func(int) int { return 1 + rng.Intn(64<<10) })
	p := pipePlan(t, src)
	w := watchPlan(t, p, data)
	names := planNames(p)
	perm := rng.Perm(n)
	mismatch, broken, flaky := names[perm[0]], names[perm[1]], names[perm[2]]
	t.Logf("verify mismatch once: %s; unreadable: %s; one read error: %s", mismatch, broken, flaky)
	var mu sync.Mutex
	w.delay = func() {
		mu.Lock()
		d := time.Duration(rng.Intn(300)) * time.Microsecond
		mu.Unlock()
		time.Sleep(d)
	}
	w.open = func(name string, try int) error {
		if name == broken || (name == flaky && try == 1) {
			return errors.New("input/output error")
		}
		return nil
	}
	w.beforeVerify = func(tmp string, try int) {
		if strings.Contains(tmp, mismatch) && filepath.Dir(tmp) == p.Dests[1] && try == 1 {
			corrupt(t, tmp)
		}
	}
	before := runtime.NumGoroutine()
	res, err := Run(context.Background(), p, nil)
	noLeak(t, before)
	if err != nil || res.Safe || res.Copied != n-1 || len(res.Failed) != 1 || !strings.HasPrefix(res.Failed[0], broken+": ") {
		t.Fatalf("err %v result %+v", err, res)
	}
	if res.Bytes != sizes(data, without(names, broken)) {
		t.Fatalf("bytes %d", res.Bytes)
	}
	if w.opened(mismatch) != 2 || w.opened(flaky) != 2 || w.opened(broken) != 1+len(retryDelays) {
		t.Fatalf("opens: mismatch %d, flaky %d, broken %d", w.opened(mismatch), w.opened(flaky), w.opened(broken))
	}
	w.check()
	recorded(t, p, data, without(names, broken))
	if w.maxFiles > 2 {
		t.Fatalf("%d files had temps at once", w.maxFiles)
	}
}

// stubSyncTemp swaps stage B's per-temp fsync for the test.
func stubSyncTemp(t *testing.T, fn func(f *os.File) error) {
	t.Helper()
	old := syncTemp
	t.Cleanup(func() { syncTemp = old })
	syncTemp = fn
}

// isTemp reports whether f is a temp copy of name.
func isTemp(f *os.File, name string) bool {
	m := tmpRE.FindStringSubmatch(filepath.Base(f.Name()))
	return m != nil && m[1] == name
}

// An SMB share reports a full disk at fsync, in stage B, while the next file has
// already been read: the run stops as for a full disk in stage A. The file is listed as
// full, every later one as not attempted, the file already read is removed, nothing
// further is read off the card.
func TestPipelineDestinationFullInStageB(t *testing.T) {
	src, data := pipeCard(t, 8, rand.New(rand.NewSource(11)), func(i int) int { return 50000 + i })
	p := pipePlan(t, src)
	w := watchPlan(t, p, data)
	names := planNames(p)
	k := 4
	readNext := make(chan struct{})
	var once sync.Once
	w.write = func(f *os.File) error {
		if strings.Contains(f.Name(), names[k+1]) {
			once.Do(func() { close(readNext) })
		}
		return nil
	}
	stubSyncTemp(t, func(f *os.File) error {
		if isTemp(f, names[k]) {
			select { // k+1's card read is under way: it will be held, read, when k fails
			case <-readNext:
			case <-time.After(5 * time.Second):
				t.Errorf("%s not read while %s was in stage B", names[k+1], names[k])
			}
			return syscall.ENOSPC
		}
		return plainSync(f)
	})
	before := runtime.NumGoroutine()
	res, err := Run(context.Background(), p, nil)
	noLeak(t, before)
	if err != nil || res.Safe || res.Copied != k || len(res.Failed) != len(names)-k {
		t.Fatalf("err %v result %+v", err, res)
	}
	if !strings.HasPrefix(res.Failed[0], names[k]+": destination full") {
		t.Fatalf("first failure %q", res.Failed[0])
	}
	for i, f := range res.Failed[1:] {
		if f != names[k+1+i]+": not attempted: destination full" {
			t.Fatalf("failure %q", f)
		}
	}
	if w.opened(names[k]) != 1 || w.opened(names[k+1]) != 1 || w.opened(names[k+2]) != 0 {
		t.Fatalf("opens: k %d, k+1 %d, k+2 %d", w.opened(names[k]), w.opened(names[k+1]), w.opened(names[k+2]))
	}
	recorded(t, p, data, names[:k]) // also: no temps of k or k+1 left, neither named
}

// An I/O error from stage B's fsync is retried like a verify mismatch: the file is
// read off the card again, and the run ends safe.
func TestPipelineStageBSyncErrorRetries(t *testing.T) {
	src, data := pipeCard(t, 6, rand.New(rand.NewSource(15)), func(i int) int { return 20000 + i })
	p := pipePlan(t, src)
	w := watchPlan(t, p, data)
	names := planNames(p)
	k := 2
	var mu sync.Mutex
	failed := false
	stubSyncTemp(t, func(f *os.File) error {
		mu.Lock()
		defer mu.Unlock()
		if isTemp(f, names[k]) && filepath.Dir(f.Name()) == p.Dests[1] && !failed {
			failed = true
			return syscall.EIO
		}
		return plainSync(f)
	})
	res, err := Run(context.Background(), p, nil)
	if err != nil || !res.Safe || res.Copied != 6 || len(res.Failed) != 0 || w.opened(names[k]) != 2 {
		t.Fatalf("err %v result %+v opens %d", err, res, w.opened(names[k]))
	}
	recorded(t, p, data, names)
}

// Ctrl-C just as a file's card read completes, with no file in stage B: no new work
// starts. The file read is removed, not synced, verified or named.
func TestPipelineCancelAfterReadStartsNoStageB(t *testing.T) {
	src, data := pipeCard(t, 4, rand.New(rand.NewSource(13)), func(i int) int { return 30000 + i })
	p := pipePlan(t, src)
	w := watchPlan(t, p, data)
	names := planNames(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writes := 0 // stage A runs on Run's goroutine only
	w.write = func(f *os.File) error {
		// A one-chunk file is written to the 2 destinations; cancelling on the last write
		// lets stream finish (it checks ctx before each chunk), so stage A succeeds.
		if strings.Contains(f.Name(), names[0]) {
			if writes++; writes == 2 {
				cancel()
			}
		}
		return nil
	}
	syncs := 0
	stubSyncTemp(t, func(f *os.File) error { syncs++; return plainSync(f) })
	before := runtime.NumGoroutine()
	res, err := Run(ctx, p, nil)
	noLeak(t, before)
	if !errors.Is(err, context.Canceled) || res.Safe || res.Copied != 0 || syncs != 0 {
		t.Fatalf("err %v result %+v, stage-B fsyncs %d", err, res, syncs)
	}
	if w.opened(names[1]) != 0 {
		t.Fatal("next file read after Ctrl-C")
	}
	recorded(t, p, data, nil)
}

type noteSink struct{ notes chan string }

func (noteSink) Close() error { return nil }

func (s noteSink) Emit(e ui.Event) {
	if e.Note != nil {
		s.notes <- e.Note.Text
	}
}

// Ctrl-C while a file is in stage B: the run says, once, that it is finishing that file,
// so the wait for its fsync and verify doesn't look hung.
func TestPipelineCancelSaysItIsFinishing(t *testing.T) {
	src, data := pipeCard(t, 6, rand.New(rand.NewSource(19)), func(i int) int { return 30000 + i })
	p := pipePlan(t, src)
	w := watchPlan(t, p, data)
	names := planNames(p)
	k := names[2]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := noteSink{notes: make(chan string, 100)}
	said := make(chan struct{})
	var notes []string
	var mu sync.Mutex
	go func() {
		for n := range sink.notes {
			mu.Lock()
			notes = append(notes, n)
			mu.Unlock()
			if strings.Contains(n, "finishing "+k) {
				close(said)
			}
		}
	}()
	w.beforeVerify = func(tmp string, try int) {
		if strings.Contains(tmp, k) && filepath.Dir(tmp) == p.Dests[0] {
			cancel()
			select { // hold stage B until the run has said why it waits
			case <-said:
			case <-time.After(5 * time.Second):
				t.Errorf("no note that %s is being finished", k)
			}
		}
	}
	res, err := Run(ctx, p, sink)
	close(sink.notes)
	if !errors.Is(err, context.Canceled) || res.Copied != 3 {
		t.Fatalf("err %v result %+v", err, res)
	}
	recorded(t, p, data, names[:3])
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, s := range notes {
		if strings.Contains(s, "finishing") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d finishing notes: %q", n, notes)
	}
}

// The destinations are flushed only once the last file's stage B is settled and
// recorded (invariant 6).
func TestPipelineFlushesAfterDrain(t *testing.T) {
	src, data := pipeCard(t, 6, rand.New(rand.NewSource(18)), func(i int) int { return 20000 + i })
	p := pipePlan(t, src)
	w := watchPlan(t, p, data)
	names := planNames(p)
	w.beforeVerify = func(tmp string, try int) {
		if strings.Contains(tmp, names[len(names)-1]) {
			time.Sleep(100 * time.Millisecond) // a slow last verify
		}
	}
	flushes := 0
	old := fullSyncFn
	t.Cleanup(func() { fullSyncFn = old })
	fullSyncFn = func(f *os.File) error {
		flushes++
		if man, _ := readManifest(f.Name()); len(man) != len(names) {
			t.Errorf("%s flushed with %d of %d files recorded", f.Name(), len(man), len(names))
		}
		return old(f)
	}
	res, err := Run(context.Background(), p, nil)
	if err != nil || !res.Safe || flushes != len(p.Dests) {
		t.Fatalf("err %v result %+v flushes %d", err, res, flushes)
	}
	recorded(t, p, data, names)
}
