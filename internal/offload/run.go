package offload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jefflaplante/cull/internal/ui"
)

// retryDelays are the waits before each retry of a failed file (a flaky reader, a
// card reseated); tests shorten them.
var retryDelays = []time.Duration{time.Second, 3 * time.Second}

// Result is what a run did.
type Result struct {
	Plan       *Plan
	Copied     int
	Skipped    int
	Unverified int      // skipped files never checked against the card (see File.Unverified)
	Failed     []string // "<name>: <error>"
	Bytes      int64
	Elapsed    time.Duration
	SyncErrs   []string // destinations whose final F_FULLFSYNC failed
	// Safe: every planned file is on every destination and was verified against the
	// card (now, or by an earlier run's manifest, or by --checksum), and each
	// destination's drive cache was flushed. Only then may the card be formatted.
	Safe bool
}

// Run carries out p. Files that fail after retries are listed and the run goes on; a
// cancelled ctx stops after the file in flight (its temp files removed) and returns
// what was done with ctx.Err().
func Run(ctx context.Context, p *Plan, sink ui.Sink) (*Result, error) {
	start := time.Now()
	res := &Result{Plan: p}
	emit := func(e ui.Event) {
		if sink != nil {
			sink.Emit(e)
		}
	}
	warn := func(format string, args ...any) {
		emit(ui.Event{Note: &ui.Note{Sev: ui.Warn, Text: fmt.Sprintf(format, args...)}})
	}
	for _, d := range p.Dests {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return res, err
		}
		stale, _ := filepath.Glob(filepath.Join(d, ".*.cull-*.tmp"))
		for _, s := range stale {
			os.Remove(s)
		}
	}
	emit(ui.Event{Stage: &ui.Stage{Name: "offload", Unit: "bytes", Total: p.Bytes}})
	defer emit(ui.Event{Stage: &ui.Stage{Name: "offload", Done: true}})

	for _, f := range p.Files {
		if f.Skip != "" {
			res.Skipped++
			if f.Unverified {
				res.Unverified++
			}
			continue
		}
		if ctx.Err() != nil {
			break
		}
		sum, err := copyWithRetries(ctx, f, p.h, warn)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			break
		}
		if err != nil {
			res.Failed = append(res.Failed, f.Name+": "+err.Error())
			warn("%s not copied: %v", f.Name, err)
			continue
		}
		e := Entry{Src: f.Src, Orig: filepath.Base(f.Src), Name: f.Name, Size: f.Size, ModTime: f.ModTime,
			SHA256: hexOf(sum[:]), At: time.Now().UTC()}
		for _, d := range f.To {
			if err := appendManifest(d, e); err != nil {
				res.Failed = append(res.Failed, f.Name+": manifest: "+err.Error())
				warn("%s copied but not recorded in %s: %v", f.Name, d, err)
			}
		}
		if f.Unverified {
			res.Unverified++
		}
		res.Copied++
		res.Bytes += f.Size
		emit(ui.Event{Stage: &ui.Stage{Name: "offload", Add: f.Size}})
		emit(ui.Event{Note: &ui.Note{Level: ui.Verbose, Text: fmt.Sprintf("%s → %s (sha256 %s…)", filepath.Base(f.Src), f.Name, hexOf(sum[:4]))}})
	}
	// The drive's own cache, flushed once per destination: fsync stops at the drive.
	for _, d := range p.Dests {
		if err := flushDrive(d); err != nil {
			res.SyncErrs = append(res.SyncErrs, d+": "+err.Error())
			warn("couldn't flush %s's drive cache: %v", d, err)
		}
	}
	res.Elapsed = time.Since(start)
	res.Safe = ctx.Err() == nil && len(res.Failed) == 0 && len(res.SyncErrs) == 0 &&
		res.Unverified == 0 && res.Copied+res.Skipped == len(p.Files)
	return res, ctx.Err()
}

func copyWithRetries(ctx context.Context, f File, h hooks, warn func(string, ...any)) ([32]byte, error) {
	for try := 0; ; try++ {
		sum, err := copyFile(ctx, f.Src, f.Name, f.To, f.ModTime, h)
		if err == nil || ctx.Err() != nil || try == len(retryDelays) {
			return sum, err
		}
		warn("%s: %v; retrying in %s", f.Name, err, retryDelays[try])
		select {
		case <-time.After(retryDelays[try]):
		case <-ctx.Done():
			return sum, ctx.Err()
		}
	}
}

func flushDrive(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return fullSync(d)
}

// Verify re-hashes every file folder's manifest records, from the disk, and reports
// how many match and how many are missing or differ (each with a warning).
func Verify(ctx context.Context, folder string, sink ui.Sink) (ok, bad int, err error) {
	man, err := readManifest(folder)
	if err != nil {
		return 0, 0, err
	}
	if len(man) == 0 {
		return 0, 0, fmt.Errorf("no %s in %s: nothing to verify", ManifestName, folder)
	}
	if sink != nil {
		sink.Emit(ui.Event{Stage: &ui.Stage{Name: "verify", Unit: "files", Total: int64(len(man))}})
		defer sink.Emit(ui.Event{Stage: &ui.Stage{Name: "verify", Done: true}})
	}
	for _, e := range man {
		if err := ctx.Err(); err != nil {
			return ok, bad, err
		}
		p := filepath.Join(folder, e.Name)
		problem := ""
		if err := dropCache(p); err != nil {
			problem = err.Error()
		} else if sum, err := hashUncached(ctx, p); err != nil {
			problem = err.Error()
		} else if hexOf(sum[:]) != e.SHA256 {
			problem = "differs from the card (checksum mismatch)"
		}
		if problem != "" {
			bad++
			if sink != nil {
				sink.Emit(ui.Event{Note: &ui.Note{Sev: ui.Warn, Text: e.Name + ": " + problem}})
			}
		} else {
			ok++
		}
		if sink != nil {
			sink.Emit(ui.Event{Stage: &ui.Stage{Name: "verify", Add: 1}})
		}
	}
	return ok, bad, nil
}
