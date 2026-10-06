//go:build cardbench

package offload

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCardBench compares offload's Run with cp, rsync and ditto, card → destination:
//
//	CULL_BENCH_DST=/Volumes/X go test -tags cardbench -run CardBench -v ./internal/offload
//
// It only reads the card. Settings, all from the environment:
//   - CULL_BENCH_SRC: the card's DNG folder (default /Volumes/LEICA M/DCIM/100LEICA)
//   - CULL_BENCH_DST: where the copies go, in a fresh folder removed afterwards
//     (default t.TempDir())
//   - CULL_BENCH_N: frames per tool and round (default 20)
//   - CULL_BENCH_FROM: index of the first frame, in file-name order (default 0)
//   - CULL_BENCH_ROUNDS: rounds; every second round runs the tools in reverse (default 1)
//
// Every read must come from the card, not RAM. Each tool gets its own frames when the
// card has enough (N × tools × rounds from FROM); otherwise the tools share frames.
// Either way, before every run the frames' pages are evicted from the page cache
// (msync MS_INVALIDATE on a read-only mapping: nothing is written), checked gone
// (mincore), and how many were still cached at the start is logged.
//
// What each tool checks: cull hashes the card while reading it and re-reads every byte
// of the copy from the device to compare; cp, rsync -a and ditto check nothing. cull
// runs as shipped, offload.Run on a plan of the frames ("cull pipeline": each file's
// fsync and verify overlap the next file's card read), and with the serial test seam
// ("cull serial": the engine before that). Each other tool is timed alone, then with
// sync and F_FULLFSYNC on the destination, so each ends as durable as cull's "safe to
// format"; Run fsyncs every file and flushes the destination itself.
func TestCardBench(t *testing.T) {
	src := env("CULL_BENCH_SRC", "/Volumes/LEICA M/DCIM/100LEICA")
	n, from, rounds := envInt(t, "CULL_BENCH_N", 20), envInt(t, "CULL_BENCH_FROM", 0), envInt(t, "CULL_BENCH_ROUNDS", 1)
	root := os.Getenv("CULL_BENCH_DST")
	if root == "" {
		root = t.TempDir()
	}
	dst, err := os.MkdirTemp(root, "cardbench-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dst) })

	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	var frames []string
	for _, e := range ents {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".dng") && !strings.HasPrefix(e.Name(), ".") {
			frames = append(frames, filepath.Join(src, e.Name()))
		}
	}
	sort.Strings(frames)

	tools := []struct {
		name string
		run  func(fs []string, dir string) error
	}{
		{"cull pipeline", cullRun(false)},
		{"cull serial", cullRun(true)},
		{"cp", func(fs []string, dir string) error { return command("cp", append(fs, dir)...) }},
		{"rsync -a", func(fs []string, dir string) error {
			return command("/usr/bin/rsync", append(append([]string{"-a"}, fs...), dir+"/")...)
		}},
		{"ditto", func(fs []string, dir string) error { return command("ditto", append(fs, dir)...) }},
	}
	distinct := len(frames) >= from+n*len(tools)*rounds
	if !distinct && len(frames) < from+n {
		n = len(frames) - from
	}
	if n <= 0 {
		t.Fatalf("no frames in %s from index %d", src, from)
	}
	t.Logf("%d frames on the card; %d per run, %d round(s), to %s; distinct frames per run: %v", len(frames), n, rounds, root, distinct)

	type total struct {
		bytes        int64
		tool, synced time.Duration
	}
	totals := map[string]*total{}
	run := 0
	for r := 0; r < rounds; r++ {
		order := make([]int, len(tools))
		for i := range order {
			order[i] = i
			if r%2 == 1 {
				order[i] = len(tools) - 1 - i
			}
		}
		for _, i := range order {
			tool := tools[i]
			fs := frames[from : from+n]
			if distinct {
				fs = frames[from+run*n : from+(run+1)*n]
			}
			run++
			for _, f := range fs {
				if err := dropCache(f); err != nil { // evict, check, retry: as before a verify read
					t.Logf("%s: %v", tool.name, err)
				}
			}
			var res, pages int
			for _, f := range fs {
				r, p, err := residentPages(f)
				if err != nil {
					t.Fatal(err)
				}
				res, pages = res+r, pages+p
			}
			dir := filepath.Join(dst, fmt.Sprintf("r%d-%s", r, strings.ReplaceAll(tool.name, " ", "-")))
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			var bytes int64
			for _, f := range fs {
				bytes += sizeOf(t, f)
			}
			start := time.Now()
			if err := tool.run(fs, dir); err != nil {
				t.Fatalf("%s: %v", tool.name, err)
			}
			tt := time.Since(start)
			s0 := time.Now()
			var flushErr error
			if !strings.HasPrefix(tool.name, "cull") { // Run fsyncs every file and flushes the destination
				command("sync")
			}
			s1 := time.Now()
			if !strings.HasPrefix(tool.name, "cull") {
				_, flushErr = flushDrive(dir) // F_FULLFSYNC, as offload does once per destination
			}
			st := time.Since(start)
			note := ""
			if flushErr != nil {
				note = " (F_FULLFSYNC: " + flushErr.Error() + ")"
			}
			t.Logf("round %d %-13s %s→%s  %4d MB  tool %6.2fs %4.0f MB/s  sync %5.2fs  F_FULLFSYNC %5.2fs  total %6.2fs %4.0f MB/s  card pages cached at start %d/%d%s",
				r+1, tool.name, filepath.Base(fs[0]), filepath.Base(fs[len(fs)-1]), bytes/1e6,
				tt.Seconds(), mbps(bytes, tt), s1.Sub(s0).Seconds(), time.Since(s1).Seconds(),
				st.Seconds(), mbps(bytes, st), res, pages, note)
			if totals[tool.name] == nil {
				totals[tool.name] = &total{}
			}
			tot := totals[tool.name]
			tot.bytes += bytes
			tot.tool += tt
			tot.synced += st
		}
	}
	for _, tool := range tools {
		tot := totals[tool.name]
		t.Logf("TOTAL %-13s %5d MB  %7.2fs  %4.0f MB/s (tool alone %4.0f MB/s)", tool.name, tot.bytes/1e6, tot.synced.Seconds(), mbps(tot.bytes, tot.synced), mbps(tot.bytes, tot.tool))
	}
	t.Logf("cull serial time split: card read + hash + write + fsync %.2fs, evict %.2fs, the rest (verify re-read, link, manifest; the destination flush in the last file's) %.2fs",
		split.copy.Seconds(), split.evict.Seconds(), split.rest.Seconds())
}

// split adds up where the serial engine's time goes, across its runs, from Run's hooks.
var split struct{ copy, evict, rest time.Duration }

// cullRun copies fs into dir with offload.Run, pipelined or serial, as `cull offload`
// would (without a backup), and fails unless the run ends safe to format.
func cullRun(serial bool) func(fs []string, dir string) error {
	return func(fs []string, dir string) error {
		p := &Plan{Dests: []string{dir}}
		for _, f := range fs {
			st, err := os.Stat(f)
			if err != nil {
				return err
			}
			p.Files = append(p.Files, File{Src: f, Size: st.Size(), ModTime: st.ModTime(), Name: filepath.Base(f), To: []string{dir}})
			p.Bytes += st.Size()
		}
		p.h.serial = serial
		var start, wrote time.Time
		if serial { // one file at a time, so hook times split each file's work
			end := func() {
				if !start.IsZero() {
					split.rest += time.Since(wrote)
				}
			}
			p.h.open = func(s string) (io.ReadCloser, error) {
				end()
				start = time.Now()
				return openSource(s, hooks{})
			}
			p.h.afterWrite = func(string) { wrote = time.Now(); split.copy += wrote.Sub(start) }
			p.h.beforeVerify = func(string) { v := time.Now(); split.evict += v.Sub(wrote); wrote = v }
			defer end()
		}
		res, err := Run(context.Background(), p, nil)
		if err != nil {
			return err
		}
		if !res.Safe || res.Copied != len(fs) {
			return fmt.Errorf("not safe to format: copied %d of %d, failed %v, sync errors %v", res.Copied, len(fs), res.Failed, res.SyncErrs)
		}
		return nil
	}
}

func command(name string, args ...string) error {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, out)
	}
	return nil
}

func mbps(b int64, d time.Duration) float64 { return float64(b) / 1e6 / d.Seconds() }

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(t *testing.T, k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s: %v", k, err)
	}
	return n
}
