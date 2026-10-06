//go:build cardbench

package offload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The stage-A profile: where offload's copy time goes, card → destination. Measurement
// only; it reads the card and writes only into fresh folders under each destination root,
// removed afterwards.
//
//	CULL_PROF_DST=/tmp/x,/Volumes/Grey/x go test -tags cardbench -run Profile -v -timeout 60m ./internal/offload
//
// Settings, from the environment:
//   - CULL_PROF_SRC: the card's DNG folder (default /Volumes/LEICA M/DCIM/100LEICA)
//   - CULL_PROF_N: frames per run, from the first in file-name order (default 10)
//   - CULL_PROF_DST: comma-separated destination roots (default t.TempDir())
//   - CULL_PROF_REPS: clean runs per measurement (default 3)
//   - CULL_PROF_ONLY: run only the measurements whose label contains this
//
// The tests, each logging MB/s as median [min–max], CPU (user+sys) as a share of wall
// time, and a breakdown in seconds per run (one run = the N frames):
//   - TestProfileCardRead: the card alone. F_NOCACHE (the engine until 2026-10-06), plain
//     (cached, kernel read-ahead: the engine since), F_NOCACHE + F_RDAHEAD, and F_NOCACHE with two reads in
//     flight; 1, 4, 8 and 16 MiB reads.
//   - TestProfileHash: SHA-256 in memory; the card read with the hash inline on the
//     reading goroutine (the engine's reader) vs on another goroutine.
//   - TestProfileWrite: memory → each destination, F_NOCACHE vs normal, 1–16 MiB writes,
//     each file's fsync timed apart; then the same writes while the previous file goes
//     through stage B on another goroutine (fsync, or evict + uncached re-read + hash, or
//     both), as the pipeline overlaps them.
//   - TestProfileStageA: writeStage itself, its card reads and writes timed through the
//     hooks; then profStream, a copy of stream with the buffer size, the buffer count and
//     the hashing goroutine as parameters and every wait timed.
//   - TestProfileRun: Run, pipelined and serial, with stage A, stage B's fsync and evict,
//     and the main goroutine's wait for stage B split out; the card opened F_NOCACHE (the
//     engine until 2026-10-06) and plainly (the engine since; both through the open hook), and the card pages left cached.
//   - TestProfilePipeline: profPipeline, Run's two stages with stage A's parameters (card
//     open mode, ring size, hashing goroutine) and how far stage A may run ahead of B.
//   - TestProfileEvict, TestProfileReadUnderWrite, TestProfileEvictDuringRead: what an
//     F_NOCACHE card read leaves cached, and whether destination writes or evictions
//     (of a destination file or a card frame) slow the card's reads.
//
// Every run that reads the card first evicts its frames' pages (dropCache: msync
// MS_INVALIDATE on a read-only mapping, nothing written; checked with mincore). A run
// whose frames still have any page cached at its start is logged, discarded and tried
// again.
//
// First run (2026-10-06, M3 Pro, the LEICA M card's 10 frames, 602 MB): the card reads at
// 262 MB/s plainly but ~210 with F_NOCACHE (no read-ahead), and an F_NOCACHE read still
// leaves 98% of the frame's pages cached on exFAT. Opening the card plainly took the
// pipelined Run from ~200 to ~225-235 MB/s to the Mac SSD and from ~140 to ~165-180 to
// a USB SSD (exFAT), which is bound by its own mixed write + verify-read throughput.
// SHA-256 runs at 2.6 GB/s; ring size and count made no measurable difference.

const mib = 1 << 20

// openNoCache opens a card file F_NOCACHE, as openSource did until 2026-10-06 (it now
// opens plainly, for the kernel's read-ahead): the profile's "nocache" mode.
func openNoCache(src string) (*os.File, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	noCache(f)
	return f, nil
}

type profEnv struct {
	frames []string
	sizes  []int64
	bytes  int64
	reps   int
	dsts   []string // fresh folders, one per root, removed afterwards
}

func profSetup(t *testing.T) *profEnv {
	src := env("CULL_PROF_SRC", "/Volumes/LEICA M/DCIM/100LEICA")
	n := envInt(t, "CULL_PROF_N", 10)
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Skip(err)
	}
	e := &profEnv{reps: envInt(t, "CULL_PROF_REPS", 3)}
	for _, d := range ents {
		if !d.IsDir() && strings.EqualFold(filepath.Ext(d.Name()), ".dng") && !strings.HasPrefix(d.Name(), ".") {
			e.frames = append(e.frames, filepath.Join(src, d.Name()))
		}
	}
	sort.Strings(e.frames)
	if len(e.frames) > n {
		e.frames = e.frames[:n]
	}
	for _, f := range e.frames {
		s := sizeOf(t, f)
		e.sizes = append(e.sizes, s)
		e.bytes += s
	}
	roots := strings.Split(os.Getenv("CULL_PROF_DST"), ",")
	if os.Getenv("CULL_PROF_DST") == "" {
		roots = []string{t.TempDir()}
	}
	for _, r := range roots {
		d, err := os.MkdirTemp(r, "cullprof-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		e.dsts = append(e.dsts, d)
	}
	t.Logf("%d frames, %d MB per run, %d clean runs per measurement; destinations %v", len(e.frames), e.bytes/1e6, e.reps, e.dsts)
	return e
}

// sample is one clean run: its wall and CPU time, and named parts of it.
type sample struct {
	wall, cpu time.Duration
	parts     map[string]time.Duration
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// coldCard evicts the frames' pages and returns how many are still cached. A pass can
// leave pages behind: after an F_NOCACHE read of the card (exFAT) most of a file's pages
// are cached anyway, and some come back just after an eviction (seen as one frame's
// worth, 3664 pages, at the start of the next run). So it evicts until a pass finds
// none, up to five passes, before reporting what's left.
func coldCard(t *testing.T, frames []string) (resident, pages int) {
	for pass := 0; pass < 5; pass++ {
		if pass > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		for _, f := range frames {
			dropCache(f) // a failure shows in the count below
		}
		resident, pages = 0, 0
		for _, f := range frames {
			r, p, err := residentPages(f)
			if err != nil {
				t.Fatal(err)
			}
			resident, pages = resident+r, pages+p
		}
		if resident == 0 {
			break
		}
	}
	return resident, pages
}

// measure runs fn until it has e.reps clean runs (a run reading the card must start with
// none of its pages cached), and logs the result under label. fn fills parts.
func (e *profEnv) measure(t *testing.T, label string, card bool, bytes int64, fn func(parts map[string]time.Duration) error) []sample {
	t.Helper()
	if only := os.Getenv("CULL_PROF_ONLY"); only != "" && !strings.Contains(label, only) {
		return nil
	}
	var out []sample
	for try := 0; len(out) < e.reps; try++ {
		if try >= e.reps*3 {
			t.Errorf("%s: only %d clean runs in %d tries", label, len(out), try)
			break
		}
		if card {
			if r, p := coldCard(t, e.frames); r > 0 {
				t.Logf("  %s: discarded, %d of %d card pages cached at start", label, r, p)
				time.Sleep(500 * time.Millisecond)
				continue
			}
		}
		parts := map[string]time.Duration{}
		c0, w0 := cpuTime(), time.Now()
		if err := fn(parts); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		out = append(out, sample{wall: time.Since(w0), cpu: cpuTime() - c0, parts: parts})
	}
	logSamples(t, label, bytes, out)
	return out
}

func median(v []float64) (med, lo, hi float64) {
	if len(v) == 0 {
		return 0, 0, 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return m, s[0], s[len(s)-1]
}

func logSamples(t *testing.T, label string, bytes int64, ss []sample) {
	t.Helper()
	var rate, cpu, wall []float64
	keys := map[string]bool{}
	for _, s := range ss {
		rate = append(rate, mbps(bytes, s.wall))
		cpu = append(cpu, 100*s.cpu.Seconds()/s.wall.Seconds())
		wall = append(wall, s.wall.Seconds())
		for k := range s.parts {
			keys[k] = true
		}
	}
	m, lo, hi := median(rate)
	wm, _, _ := median(wall)
	cm, _, _ := median(cpu)
	line := fmt.Sprintf("%-46s %4.0f MB/s [%3.0f–%3.0f]  wall %5.2fs  CPU %3.0f%%", label, m, lo, hi, wm, cm)
	if bytes == 0 { // a measurement of steps, not of a stream
		line = fmt.Sprintf("%-46s wall %5.2fs  CPU %3.0f%%", label, wm, cm)
	}
	var ks []string
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		var v []float64
		for _, s := range ss {
			v = append(v, s.parts[k].Seconds())
		}
		pm, plo, phi := median(v)
		line += fmt.Sprintf("  %s %.2f [%.2f–%.2f]", k, pm, plo, phi)
	}
	t.Log(line)
}

// readMode opens f for reading as named.
func readMode(f *os.File, mode string) error {
	if strings.HasPrefix(mode, "nocache") {
		if err := noCache(f); err != nil {
			return err
		}
	}
	if strings.Contains(mode, "rdahead") {
		if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_RDAHEAD, 1); e != 0 {
			return e
		}
	}
	return nil
}

// readFile reads p to the end in chunk-sized reads, handing each to use (on this
// goroutine); qd 2 keeps two reads in flight (pread on alternating chunks).
func readFile(p, mode string, chunk int, use func([]byte)) (int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := readMode(f, mode); err != nil {
		return 0, err
	}
	if strings.HasSuffix(mode, "qd2") {
		st, _ := f.Stat()
		size := st.Size()
		var wg sync.WaitGroup
		var mu sync.Mutex
		var total int64
		var firstErr error
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				buf := make([]byte, chunk)
				for off := int64(g * chunk); off < size; off += int64(2 * chunk) {
					n, err := f.ReadAt(buf, off)
					if err != nil && err != io.EOF {
						mu.Lock()
						firstErr = err
						mu.Unlock()
						return
					}
					mu.Lock()
					total += int64(n)
					mu.Unlock()
				}
			}(g)
		}
		wg.Wait()
		return total, firstErr
	}
	buf := make([]byte, chunk)
	var total int64
	for {
		n, err := io.ReadFull(f, buf)
		total += int64(n)
		if use != nil {
			use(buf[:n])
		}
		if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// 1. The card alone.
func TestProfileCardRead(t *testing.T) {
	e := profSetup(t)
	for _, chunk := range []int{1, 4, 8, 16} {
		for _, mode := range []string{"nocache", "plain", "nocache+rdahead", "nocache qd2"} {
			e.measure(t, fmt.Sprintf("read %-16s %2d MiB", mode, chunk), true, e.bytes, func(map[string]time.Duration) error {
				for _, f := range e.frames {
					if _, err := readFile(f, mode, chunk*mib, nil); err != nil {
						return err
					}
				}
				return nil
			})
		}
	}
}

// 2. SHA-256: in memory, inline with the card read, and on another goroutine.
func TestProfileHash(t *testing.T) {
	e := profSetup(t)
	buf := make([]byte, 64*mib)
	rand.Read(buf)
	e.measure(t, "sha256 in memory, 64 MiB × 10", false, 10*int64(len(buf)), func(map[string]time.Duration) error {
		for i := 0; i < 10; i++ {
			h := sha256.New()
			h.Write(buf)
			h.Sum(nil)
		}
		return nil
	})
	for _, chunk := range []int{4, 16} {
		e.measure(t, fmt.Sprintf("read+hash inline, nocache %2d MiB", chunk), true, e.bytes, func(parts map[string]time.Duration) error {
			for _, f := range e.frames {
				h := sha256.New()
				if _, err := readFile(f, "nocache", chunk*mib, func(b []byte) {
					t0 := time.Now()
					h.Write(b)
					parts["hash"] += time.Since(t0)
				}); err != nil {
					return err
				}
			}
			return nil
		})
		e.measure(t, fmt.Sprintf("read, hash on another goroutine, %2d MiB ×4", chunk), true, e.bytes, func(parts map[string]time.Duration) error {
			for _, f := range e.frames {
				in, err := openNoCache(f)
				if err != nil {
					return err
				}
				var st streamStats
				_, err = profStream(context.Background(), in, sha256.New(), nil, chunk*mib, 4, hashInWriter, &st)
				in.Close()
				if err != nil {
					return err
				}
				st.add(parts)
			}
			return nil
		})
	}
}

// 3. Writes from memory to each destination.
func TestProfileWrite(t *testing.T) {
	e := profSetup(t)
	data := make([]byte, e.sizes[0])
	for _, s := range e.sizes {
		if s > int64(len(data)) {
			data = make([]byte, s)
		}
	}
	rand.Read(data)
	for _, dst := range e.dsts {
		for _, nocache := range []bool{true, false} {
			for _, chunk := range []int{1, 4, 8, 16} {
				mode := map[bool]string{true: "nocache", false: "normal"}[nocache]
				label := fmt.Sprintf("write %s %-7s %2d MiB +fsync", shortDst(dst), mode, chunk)
				e.measure(t, label, false, e.bytes, func(parts map[string]time.Duration) error {
					dir, err := os.MkdirTemp(dst, "w-")
					if err != nil {
						return err
					}
					defer os.RemoveAll(dir)
					for i, size := range e.sizes {
						f, err := profCreate(dir, i, nocache)
						if err != nil {
							return err
						}
						t0 := time.Now()
						if err := writeChunks(f, data[:size], chunk*mib); err != nil {
							return err
						}
						t1 := time.Now()
						if err := plainSync(f); err != nil {
							return err
						}
						parts["write"] += t1.Sub(t0)
						parts["fsync"] += time.Since(t1)
						f.Close()
					}
					return nil
				})
			}
		}
		// Stage A's write of file k while file k-1 goes through some of stage B on another
		// goroutine, as the pipeline runs them. "write" is stage A's side alone.
		// Stage B's and settle's small steps on this destination, per 10 files: what's left
		// of stage B besides fsync, evict and the verify read, and the manifest line that
		// the main goroutine appends (fsynced) before the next card read starts.
		e.measure(t, fmt.Sprintf("metadata %s ×%d (B naming, settle manifest)", shortDst(dst), len(e.sizes)), false, 0, func(parts map[string]time.Duration) error {
			dir, err := os.MkdirTemp(dst, "m-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			for i := range e.sizes {
				f, err := createTemp(dir, fmt.Sprintf("f%02d", i))
				if err != nil {
					return err
				}
				f.Write([]byte("x"))
				plainSync(f)
				f.Close()
				t0 := time.Now()
				os.Chtimes(f.Name(), time.Now(), time.Now())
				os.Chmod(f.Name(), 0o644)
				t1 := time.Now()
				if err := linkNoReplace(f.Name(), filepath.Join(dir, fmt.Sprintf("f%02d", i))); err != nil {
					return err
				}
				os.Remove(f.Name())
				t2 := time.Now()
				syncDir(dir)
				t3 := time.Now()
				if err := appendManifest(dir, Entry{Name: fmt.Sprintf("f%02d", i), Size: 1, At: time.Now()}); err != nil {
					return err
				}
				parts["chtimes+chmod"] += t1.Sub(t0)
				parts["link"] += t2.Sub(t1)
				parts["syncDir"] += t3.Sub(t2)
				parts["manifest"] += time.Since(t3)
			}
			return nil
		})
		// The verify read alone: the copies fsynced, evicted and hashed back from the device.
		vdir, err := os.MkdirTemp(dst, "v-")
		if err != nil {
			t.Fatal(err)
		}
		var copies []string
		for i, size := range e.sizes {
			f, err := profCreate(vdir, i, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeChunks(f, data[:size], 4*mib); err != nil {
				t.Fatal(err)
			}
			plainSync(f)
			f.Close()
			copies = append(copies, f.Name())
		}
		for _, chunk := range []int{1, 4, 16} {
			e.measure(t, fmt.Sprintf("verify %s alone, %2d MiB reads", shortDst(dst), chunk), false, e.bytes, func(parts map[string]time.Duration) error {
				for _, c := range copies {
					t0 := time.Now()
					if err := dropCache(c); err != nil {
						return err
					}
					parts["evict"] += time.Since(t0)
					if _, err := hashUncachedSize(c, chunk*mib); err != nil {
						return err
					}
				}
				return nil
			})
		}
		os.RemoveAll(vdir)
		for _, cb := range []struct {
			chunk int
			b     string
		}{{4, "none"}, {4, "fsync"}, {4, "verify"}, {4, "fsync+verify"}, {16, "fsync+verify"}, {1, "fsync+verify"}} {
			chunk, b := cb.chunk, cb.b
			label := fmt.Sprintf("write %s nocache %2d MiB ∥ prev %s", shortDst(dst), chunk, b)
			e.measure(t, label, false, e.bytes, func(parts map[string]time.Duration) error {
				dir, err := os.MkdirTemp(dst, "wb-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(dir)
				var mu sync.Mutex
				stageB := func(f *os.File) {
					t0 := time.Now()
					if strings.Contains(b, "fsync") {
						plainSync(f)
					}
					f.Close()
					t1 := time.Now()
					if strings.Contains(b, "verify") {
						dropCache(f.Name())
						hashUncachedSize(f.Name(), chunk*mib)
					}
					mu.Lock()
					parts["B fsync"] += t1.Sub(t0)
					parts["B verify"] += time.Since(t1)
					mu.Unlock()
				}
				done := make(chan struct{})
				close(done)
				for i, size := range e.sizes {
					f, err := profCreate(dir, i, true)
					if err != nil {
						return err
					}
					t0 := time.Now()
					if err := writeChunks(f, data[:size], chunk*mib); err != nil {
						return err
					}
					wrote := time.Since(t0)
					t1 := time.Now()
					<-done
					mu.Lock() // stage B's goroutine writes parts too
					parts["write"] += wrote
					parts["wait B"] += time.Since(t1)
					mu.Unlock()
					if !strings.Contains(b, "fsync") {
						plainSync(f) // fsynced here, not overlapped: on the wall clock, not in "write"
					}
					done = make(chan struct{})
					go func(f *os.File, done chan struct{}) { defer close(done); stageB(f) }(f, done)
				}
				<-done
				return nil
			})
		}
	}
}

// hashUncachedSize is hashUncached (copy.go) with the read size as a parameter.
func hashUncachedSize(p string, chunk int) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(p)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	if err := noCache(f); err != nil {
		return sum, err
	}
	hs := sha256.New()
	buf := make([]byte, chunk)
	for {
		n, err := f.Read(buf)
		hs.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return sum, err
		}
	}
	copy(sum[:], hs.Sum(nil))
	return sum, nil
}

func profCreate(dir string, i int, nocache bool) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("f%02d", i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if nocache {
		if err := noCache(f); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func writeChunks(f *os.File, b []byte, chunk int) error {
	for len(b) > 0 {
		n := min(chunk, len(b))
		if _, err := f.Write(b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func shortDst(d string) string {
	if strings.HasPrefix(d, "/Volumes/") {
		return strings.SplitN(strings.TrimPrefix(d, "/Volumes/"), "/", 2)[0]
	}
	return "MacSSD"
}

// timedReader times the card reads stage A makes.
type timedReader struct {
	f *os.File
	d *time.Duration
}

func (r timedReader) Read(p []byte) (int, error) {
	t0 := time.Now()
	n, err := r.f.Read(p)
	*r.d += time.Since(t0)
	return n, err
}
func (r timedReader) Close() error { return r.f.Close() }

// 4. Stage A itself: writeStage, timed through its hooks; then the stream variants.
func TestProfileStageA(t *testing.T) {
	e := profSetup(t)
	ctx := context.Background()
	for _, dst := range e.dsts {
		label := fmt.Sprintf("writeStage %s (4 MiB ×4, reader hashes)", shortDst(dst))
		e.measure(t, label, true, e.bytes, func(parts map[string]time.Duration) error {
			var rd, wr time.Duration
			h := hooks{
				open: func(s string) (io.ReadCloser, error) {
					f, err := os.Open(s)
					if err != nil {
						return nil, err
					}
					noCache(f)
					return timedReader{f, &rd}, nil
				},
				write: func(f *os.File, p []byte) (int, error) {
					t0 := time.Now()
					n, err := f.Write(p)
					wr += time.Since(t0)
					return n, err
				},
			}
			for i, f := range e.frames {
				w, err := writeStage(ctx, f, filepath.Base(f), []string{dst}, e.sizes[i], time.Now(), h)
				if err != nil {
					return err
				}
				w.discard()
			}
			parts["card read"], parts["write"] = rd, wr
			return nil
		})
		for _, c := range []struct {
			size, n int
			where   hashWhere
			src     string
		}{
			{4, 4, hashInReader, "nocache"}, {4, 8, hashInReader, "nocache"}, {8, 4, hashInReader, "nocache"}, {16, 4, hashInReader, "nocache"}, {1, 4, hashInReader, "nocache"},
			{4, 4, hashInWriter, "nocache"}, {8, 4, hashInWriter, "nocache"}, {1, 4, hashInWriter, "nocache"}, {4, 8, hashInWriter, "nocache"},
			{4, 4, hashOwn, "nocache"},
			{4, 4, hashInReader, "plain+evict"}, {4, 4, hashInWriter, "plain+evict"},
			{4, 4, hashInReader, "plain"}, {4, 8, hashInReader, "plain"}, {8, 4, hashInReader, "plain"}, {16, 4, hashInReader, "plain"}, {1, 8, hashInReader, "plain"},
			{4, 4, hashInWriter, "plain"}, {4, 4, hashOwn, "plain"},
		} {
			label := fmt.Sprintf("profStream %s src %-11s %2d MiB ×%d hash in %s", shortDst(dst), c.src, c.size, c.n, c.where)
			e.measure(t, label, true, e.bytes, func(parts map[string]time.Duration) error {
				for i, f := range e.frames {
					sum, err := profWriteStage(ctx, f, filepath.Base(f), dst, e.sizes[i], c.size*mib, c.n, c.where, c.src, parts)
					if err != nil {
						return err
					}
					_ = sum
				}
				return nil
			})
		}
	}
	// The variants hash the same card bytes as the engine: check one frame against writeStage.
	w, err := writeStage(ctx, e.frames[0], "x", []string{e.dsts[0]}, e.sizes[0], time.Now(), hooks{})
	if err != nil {
		t.Fatal(err)
	}
	w.discard()
	for _, where := range []hashWhere{hashInReader, hashInWriter, hashOwn} {
		sum, err := profWriteStage(ctx, e.frames[0], "x", e.dsts[0], e.sizes[0], 8*mib, 4, where, map[hashWhere]string{hashInReader: "nocache", hashInWriter: "plain", hashOwn: "plain+evict"}[where], map[string]time.Duration{})
		if err != nil {
			t.Fatal(err)
		}
		if sum != w.sum {
			t.Errorf("hash in %s: %x, writeStage %x", where, sum[:4], w.sum[:4])
		}
	}
}

// profWriteStage is writeStage with profStream's parameters; it discards its temp.
// profWriteStage is writeStage with profStream's parameters; it discards its temp. src
// is how the card is opened: "nocache" (F_NOCACHE: the engine until 2026-10-06), "plain" (kernel
// read-ahead on, pages left cached), or "plain+evict" (each frame's card pages evicted
// once it's read: msync MS_INVALIDATE, read only; timed as "src evict").
func profWriteStage(ctx context.Context, src, name, dir string, size int64, bufSize, n int, where hashWhere, srcMode string, parts map[string]time.Duration) (sum [32]byte, err error) {
	w, err := profStageA(ctx, src, name, dir, size, bufSize, n, where, srcMode, parts)
	if err != nil {
		return sum, err
	}
	w.discard()
	return w.sum, nil
}

// profStageA is writeStage (copy.go) with profWriteStage's parameters: it leaves the temp
// copy open and unsynced in the returned written, for finishStage.
func profStageA(ctx context.Context, src, name, dir string, size int64, bufSize, n int, where hashWhere, srcMode string, parts map[string]time.Duration) (w *written, err error) {
	var in io.ReadCloser
	if srcMode == "nocache" {
		in, err = openNoCache(src)
	} else {
		in, err = os.Open(src)
	}
	if err != nil {
		return nil, err
	}
	defer in.Close()
	if srcMode == "plain+evict" {
		defer func() {
			t0 := time.Now()
			evict(src)
			parts["src evict"] += time.Since(t0)
		}()
	}
	f, err := createTemp(dir, name)
	if err != nil {
		return nil, err
	}
	if profCachedWrites { // the temp written through the page cache; finishStage still fsyncs, evicts (checked) and verifies
		syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_NOCACHE, 0)
	}
	w = &written{name: name, dirs: []string{dir}, temps: []*os.File{f}, size: size, mtime: time.Now()}
	hs := sha256.New()
	var st streamStats
	got, err := profStream(ctx, in, hs, w.temps, bufSize, n, where, &st)
	if err == nil && got != size {
		err = fmt.Errorf("short read of %s: %d of %d", name, got, size)
	}
	if err != nil {
		w.discard()
		return nil, err
	}
	st.add(parts)
	copy(w.sum[:], hs.Sum(nil))
	return w, nil
}

// profCachedWrites makes profStageA write its temp through the page cache (F_NOCACHE off).
var profCachedWrites bool

// profPipeline is Run's pipeline (run.go) reduced to its two stages, with profStageA's
// parameters and a depth: how many files stage A may finish ahead of stage B. Stage B
// (finishStage, unchanged: fsync, evict, uncached re-read and compare, link) runs on one
// goroutine, one file at a time in plan order. Depth 1 is the engine: file N+2's card
// read waits until file N's stage B is done. The destination is flushed at the end.
func profPipeline(ctx context.Context, e *profEnv, dir string, bufSize, n int, where hashWhere, srcMode string, depth int, parts map[string]time.Duration) error {
	queue := make(chan *written, depth-1)
	var bErr error
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		for w := range queue {
			t0 := time.Now()
			if err := finishStage(ctx, w, hooks{}); err != nil && bErr == nil {
				bErr = err
			}
			parts["B busy"] += time.Since(t0) // read by the main goroutine only after bDone
		}
	}()
	var aBusy, handoff time.Duration
	for i, f := range e.frames {
		t0 := time.Now()
		w, err := profStageA(ctx, f, filepath.Base(f), dir, e.sizes[i], bufSize, n, where, srcMode, map[string]time.Duration{})
		if err != nil {
			close(queue)
			<-bDone
			return err
		}
		t1 := time.Now()
		queue <- w
		aBusy += t1.Sub(t0)
		handoff += time.Since(t1)
	}
	close(queue)
	t0 := time.Now()
	<-bDone
	parts["A busy"], parts["A waits for B"], parts["last B"] = aBusy, handoff, time.Since(t0)
	if bErr != nil {
		return bErr
	}
	t1 := time.Now()
	_, err := flushDrive(dir)
	parts["flush"] = time.Since(t1)
	return err
}

type hashWhere string

const (
	hashInReader hashWhere = "reader" // the engine: the reading goroutine hashes before handing over
	hashInWriter hashWhere = "writer" // the writing goroutine hashes each chunk, then writes it
	hashOwn      hashWhere = "own"    // a third goroutine hashes while the writer writes the same chunk
)

// streamStats is where each goroutine of profStream spent its time.
type streamStats struct {
	rFree, rRead, rHash, rSend time.Duration // reader: wait for a free buffer, card read, hash, wait to hand over
	wWait, wHash, wWrite       time.Duration // writer: wait for a chunk, hash, write
	hWait, hHash, wJoin        time.Duration // own hasher: wait, hash; writer's wait for it
}

func (s *streamStats) add(p map[string]time.Duration) {
	for k, v := range map[string]time.Duration{
		"r.free": s.rFree, "r.read": s.rRead, "r.hash": s.rHash, "r.send": s.rSend,
		"w.wait": s.wWait, "w.hash": s.wHash, "w.write": s.wWrite, "h.hash": s.hHash, "w.join": s.wJoin,
	} {
		if v > 0 {
			p[k] += v
		}
	}
}

// profStream is stream (copy.go) with the buffer size, the buffer count and the hashing
// goroutine as parameters, and every wait timed. With hashInReader, 4 MiB and 4 buffers
// it is the engine's stream. outs may be empty (read and hash only).
func profStream(ctx context.Context, in io.Reader, hs hash.Hash, outs []*os.File, bufSize, nbufs int, where hashWhere, st *streamStats) (total int64, err error) {
	free := make(chan []byte, nbufs)
	for i := 0; i < nbufs; i++ {
		free <- make([]byte, bufSize)
	}
	chunks := make(chan chunk, nbufs)
	stop := make(chan struct{})
	var rs streamStats // the reader's, merged at the end (it has exited by then)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(chunks)
		for {
			var buf []byte
			t0 := time.Now()
			select {
			case buf = <-free:
			case <-stop:
				return
			}
			t1 := time.Now()
			n, err := io.ReadFull(in, buf)
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			t2 := time.Now()
			if where == hashInReader {
				hs.Write(buf[:n])
			}
			t3 := time.Now()
			select {
			case chunks <- chunk{buf, n, err}:
			case <-stop:
				return
			}
			rs.rFree += t1.Sub(t0)
			rs.rRead += t2.Sub(t1)
			rs.rHash += t3.Sub(t2)
			rs.rSend += time.Since(t3)
			if err != nil {
				return
			}
		}
	}()
	defer func() { // stop the reader, wait for it, then take its times
		close(stop)
		<-readerDone
		st.rFree, st.rRead, st.rHash, st.rSend = rs.rFree, rs.rRead, rs.rHash, rs.rSend
	}()
	for {
		t0 := time.Now()
		c, ok := <-chunks
		st.wWait += time.Since(t0)
		if !ok {
			return total, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var hdone chan struct{}
		switch where {
		case hashInWriter:
			t := time.Now()
			hs.Write(c.buf[:c.n])
			st.wHash += time.Since(t)
		case hashOwn:
			hdone = make(chan struct{})
			go func(b []byte) {
				t := time.Now()
				hs.Write(b)
				st.hHash += time.Since(t)
				close(hdone)
			}(c.buf[:c.n])
		}
		t1 := time.Now()
		for _, f := range outs {
			if _, err := f.Write(c.buf[:c.n]); err != nil {
				return total, fmt.Errorf("write %s: %w", f.Name(), err)
			}
		}
		st.wWrite += time.Since(t1)
		if hdone != nil {
			t := time.Now()
			<-hdone
			st.wJoin += time.Since(t)
		}
		total += int64(c.n)
		free <- c.buf
		switch {
		case c.err == io.EOF:
			return total, nil
		case c.err != nil:
			return total, fmt.Errorf("read card: %w", c.err)
		}
	}
}

// 5. Run, pipelined and serial, split into stages.
func TestProfileRun(t *testing.T) {
	e := profSetup(t)
	for _, dst := range e.dsts {
		for _, c := range []struct {
			serial bool
			src    string
		}{{false, "nocache"}, {true, "nocache"}, {false, "plain"}, {true, "plain"}} {
			serial := c.serial
			label := fmt.Sprintf("Run %s %-9s card %s", shortDst(dst), map[bool]string{false: "pipelined", true: "serial"}[serial], c.src)
			e.measure(t, label, true, e.bytes, func(parts map[string]time.Duration) error {
				dir, err := os.MkdirTemp(dst, "run-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(dir)
				p := &Plan{Dests: []string{dir}}
				for i, f := range e.frames {
					p.Files = append(p.Files, File{Src: f, Size: e.sizes[i], ModTime: time.Now(), Name: filepath.Base(f), To: []string{dir}})
					p.Bytes += e.sizes[i]
				}
				p.h.serial = serial
				var mu sync.Mutex
				var opens, lastWrite []time.Time
				synced := map[string]time.Time{}
				var rd time.Duration
				p.h.open = func(s string) (io.ReadCloser, error) {
					f, err := os.Open(s)
					if err != nil {
						return nil, err
					}
					if c.src == "nocache" { // as openSource did until 2026-10-06; "plain" (now the engine) leaves the kernel's read-ahead on
						noCache(f)
					}
					mu.Lock()
					opens = append(opens, time.Now())
					lastWrite = append(lastWrite, time.Time{})
					mu.Unlock()
					return timedReader{f, &rd}, nil
				}
				p.h.write = func(f *os.File, b []byte) (int, error) {
					t0 := time.Now()
					n, err := f.Write(b)
					mu.Lock()
					parts["A write"] += time.Since(t0)
					lastWrite[len(lastWrite)-1] = time.Now()
					mu.Unlock()
					return n, err
				}
				p.h.afterWrite = func(tmp string) { mu.Lock(); synced[tmp] = time.Now(); mu.Unlock() }
				p.h.beforeVerify = func(tmp string) {
					mu.Lock()
					parts["B evict"] += time.Since(synced[tmp])
					mu.Unlock()
				}
				old := syncTemp
				syncTemp = func(f *os.File) error {
					t0 := time.Now()
					err := plainSync(f)
					mu.Lock()
					parts["B fsync"] += time.Since(t0)
					mu.Unlock()
					return err
				}
				defer func() { syncTemp = old }()
				start := time.Now()
				res, err := Run(context.Background(), p, nil)
				end := time.Now()
				if err != nil {
					return err
				}
				if !res.Safe || res.Copied != len(e.frames) {
					return fmt.Errorf("not safe: %+v", res.Failed)
				}
				mu.Lock()
				defer mu.Unlock()
				for i := range opens {
					parts["A total"] += lastWrite[i].Sub(opens[i])
					if i+1 < len(opens) {
						parts["between A"] += opens[i+1].Sub(lastWrite[i]) // pipelined: waiting for stage B, settling
					}
				}
				parts["A card read"] = rd
				parts["before first A"] = opens[0].Sub(start)
				parts["after last A"] = end.Sub(lastWrite[len(lastWrite)-1]) // last stage B + flush
				var cached, pages int
				for _, f := range e.frames {
					r, p, _ := residentPages(f)
					cached, pages = cached+r, pages+p
				}
				t.Logf("  %s: card pages cached after the run %d/%d", label, cached, pages)
				return nil
			})
		}
	}
}

// TestProfileEvict shows, per frame, what dropCache leaves cached on the card side,
// after reading the frames plainly (every page cached) and then without the cache.
func TestProfileEvict(t *testing.T) {
	e := profSetup(t)
	for _, mode := range []string{"plain", "nocache"} {
		for _, f := range e.frames {
			if _, err := readFile(f, mode, 4*mib, nil); err != nil {
				t.Fatal(err)
			}
		}
		for _, f := range e.frames {
			r0, p, _ := residentPages(f)
			err := dropCache(f)
			r1, _, _ := residentPages(f)
			t.Logf("after %-7s read: %s %5d/%d cached, after dropCache %5d (err %v)", mode, filepath.Base(f), r0, p, r1, err)
		}
	}
}

// TestProfileReadUnderWrite: does writing a destination slow the card's reads? The card
// read plainly (read-ahead on) alone, with the hash inline, through profStream with no
// destination, and while another goroutine writes the same number of bytes from memory
// to each destination (F_NOCACHE or normal), with no handoff between the two.
func TestProfileReadUnderWrite(t *testing.T) {
	e := profSetup(t)
	ctx := context.Background()
	data := make([]byte, 64*mib)
	rand.Read(data)
	readAll := func(mode string, hashIt bool) error {
		for _, f := range e.frames {
			h := sha256.New()
			if _, err := readFile(f, mode, 4*mib, func(b []byte) {
				if hashIt {
					h.Write(b)
				}
			}); err != nil {
				return err
			}
		}
		return nil
	}
	for _, mode := range []string{"plain", "nocache"} {
		e.measure(t, "read "+mode+" alone", true, e.bytes, func(map[string]time.Duration) error { return readAll(mode, false) })
		e.measure(t, "read "+mode+" + hash inline", true, e.bytes, func(map[string]time.Duration) error { return readAll(mode, true) })
		e.measure(t, "read "+mode+" via profStream, no destination", true, e.bytes, func(parts map[string]time.Duration) error {
			for _, f := range e.frames {
				fh, err := os.Open(f)
				if err != nil {
					return err
				}
				readMode(fh, mode)
				var st streamStats
				_, err = profStream(ctx, fh, sha256.New(), nil, 4*mib, 4, hashInReader, &st)
				fh.Close()
				if err != nil {
					return err
				}
				st.add(parts)
			}
			return nil
		})
		for _, dst := range e.dsts {
			for _, nocache := range []bool{true, false} {
				wm := map[bool]string{true: "nocache", false: "normal"}[nocache]
				label := fmt.Sprintf("read %s ∥ write %s %s", mode, shortDst(dst), wm)
				e.measure(t, label, true, e.bytes, func(parts map[string]time.Duration) error {
					dir, err := os.MkdirTemp(dst, "rw-")
					if err != nil {
						return err
					}
					defer os.RemoveAll(dir)
					errc := make(chan error, 1)
					var writer time.Duration
					go func() {
						t0 := time.Now()
						var left = e.bytes
						for i := 0; left > 0; i++ {
							f, err := profCreate(dir, i, nocache)
							if err != nil {
								errc <- err
								return
							}
							n := min(left, int64(len(data)))
							err = writeChunks(f, data[:n], 4*mib)
							f.Close()
							if err != nil {
								errc <- err
								return
							}
							left -= n
						}
						writer = time.Since(t0) // read by the main goroutine after errc
						errc <- nil
					}()
					t0 := time.Now()
					if err := readAll(mode, false); err != nil {
						return err
					}
					parts["reader"] = time.Since(t0)
					err = <-errc
					parts["writer"] = writer
					return err
				})
			}
		}
	}
}

// TestProfileEvictDuringRead: does dropCache (mmap + msync MS_INVALIDATE + mincore +
// munmap) on some other file slow the card's reads? The card read alone, and while
// another goroutine evicts, once per frame-time (every 230 ms), a 64 MB file on each
// destination (as stage B does to the previous file's copy) or a card frame already
// read (as an eviction of the source after its stage A would).
func TestProfileEvictDuringRead(t *testing.T) {
	e := profSetup(t)
	data := make([]byte, 64*mib)
	rand.Read(data)
	var targets []string
	for _, d := range e.dsts {
		p := filepath.Join(d, "evict-target")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, p)
	}
	targets = append(targets, "card")
	readAll := func(mode string) error {
		for _, f := range e.frames {
			if _, err := readFile(f, mode, 4*mib, nil); err != nil {
				return err
			}
		}
		return nil
	}
	for _, mode := range []string{"plain", "nocache"} {
		e.measure(t, "read "+mode+" alone", true, e.bytes, func(map[string]time.Duration) error { return readAll(mode) })
		for _, target := range targets {
			name := "card frame"
			if target != "card" {
				name = shortDst(target)
			}
			e.measure(t, fmt.Sprintf("read %s ∥ dropCache %s every 230 ms", mode, name), true, e.bytes, func(parts map[string]time.Duration) error {
				stop := make(chan struct{})
				done := make(chan struct{})
				go func() {
					defer close(done)
					tick := time.NewTicker(230 * time.Millisecond)
					defer tick.Stop()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						case <-tick.C:
						}
						p := target
						if target == "card" {
							p = e.frames[len(e.frames)-1-i%len(e.frames)] // read last, or long ago
						} else {
							os.ReadFile(p) // something to evict, as a copy written under load has
						}
						t0 := time.Now()
						dropCache(p)
						parts["evict"] += time.Since(t0)
					}
				}()
				err := readAll(mode)
				close(stop)
				<-done
				return err
			})
		}
	}
}

// TestProfilePipeline: the pipeline with stage A's parameters varied, under stage B's
// real contention on the destination: the card opened plainly (the engine) or F_NOCACHE, the
// ring's size, and how many files stage A may run ahead of stage B (depth). Integrity
// steps are finishStage's own, unchanged.
func TestProfilePipeline(t *testing.T) {
	e := profSetup(t)
	ctx := context.Background()
	for _, dst := range e.dsts {
		for _, c := range []struct {
			size, n, depth int
			where          hashWhere
			src            string
		}{
			{4, 4, 1, hashInReader, "nocache"}, // the engine until 2026-10-06
			{4, 4, 1, hashInReader, "plain"},   // the engine since
			{4, 16, 1, hashInReader, "plain"},
			{4, 4, 2, hashInReader, "plain"},
			{4, 16, 2, hashInReader, "plain"},
			{4, 4, 1, hashOwn, "plain"},
			{4, 16, 2, hashOwn, "plain"},
			{4, 4, 1, hashInReader, "plain cached-write"},
		} {
			profCachedWrites = strings.HasSuffix(c.src, "cached-write")
			c.src = strings.TrimSuffix(c.src, " cached-write")
			label := fmt.Sprintf("pipeline %s src %-7s %d MiB ×%-2d depth %d hash in %s", shortDst(dst), c.src, c.size, c.n, c.depth, c.where)
			if profCachedWrites {
				label += ", cached writes"
			}
			e.measure(t, label, true, e.bytes, func(parts map[string]time.Duration) error {
				dir, err := os.MkdirTemp(dst, "pipe-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(dir)
				return profPipeline(ctx, e, dir, c.size*mib, c.n, c.where, c.src, c.depth, parts)
			})
		}
	}
	profCachedWrites = false
}
