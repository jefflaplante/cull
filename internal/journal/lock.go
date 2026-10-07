package journal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The folder lock, built from exclusive flocks only: SMB shares (the user's NAS) treat a
// shared flock as exclusive, so two readers holding one would exclude each other.
//
//   - A reader (judge, decide, the review server, restore, offload, scan, tag, rank,
//     import-labels) registers: it creates its own holder file, .cull-holder-<pid>-<hex>,
//     locks it (LOCK_EX) for its whole run, and only then probes the gate, .cull.lock,
//     taking and at once dropping it. A gate held for more than a moment (RetryFor)
//     means a redate or rename is running: the reader removes its holder file and is
//     refused.
//   - A writer (redate, rename) takes the gate for its whole run, and only then tries
//     every holder file: one it can't lock belongs to a live reader, and it lets the
//     gate go and is refused; one it can lock was left by a reader that crashed, and is
//     removed.
//
// Readers never exclude each other. A reader registers before it probes and a writer
// takes the gate before it looks, so of a reader and a writer at most one proceeds:
// whichever comes second sees the first. The gate is never removed (a removed lock file
// would let a later writer lock another file of the same name).
const (
	LockName     = ".cull.lock"
	HolderPrefix = ".cull-holder-"
)

// RetryFor is how long a gate held by someone else is retried: another reader's probe
// holds it for a moment only.
var RetryFor = 2 * time.Second

// lockHook is a test seam: called by each side at each step ("reader create", "reader
// lock", "reader verify", "reader probe", "writer gate", "writer scan", "writer check").
var lockHook func(step string)

func hook(step string) {
	if lockHook != nil {
		lockHook(step)
	}
}

// IsLockFile reports the folder lock's own files (the gate and holder files): hidden,
// never frames, temps or records.
func IsLockFile(name string) bool {
	return name == LockName || strings.HasPrefix(name, HolderPrefix)
}

// Lock takes dir's folder lock: a writer's (exclusive) or a reader's. holder names the
// command ("rename"); each lock file holds its holder's line, so a refused command names
// exactly who is in the way. release lets go (a reader's holder file is removed). Only a
// lock held by another command refuses: a volume without locks, any other flock error, or
// a folder the lock files can't be made in proceeds unlocked, with note saying so.
func Lock(dir string, exclusive bool, holder string) (release func(), note string, err error) {
	line := fmt.Sprintf("cull %s (pid %d, since %s)", holder, os.Getpid(), time.Now().Format("2006-01-02 15:04:05"))
	if exclusive {
		return lockWriter(dir, line)
	}
	return lockReader(dir, line)
}

func noLock(dir string, err error) (func(), string, error) {
	return func() {}, fmt.Sprintf("%s: folder lock unavailable (%v); make sure no other cull command uses this folder meanwhile", dir, err), nil
}

// flockNB is a non-blocking exclusive flock, retried on EINTR.
func flockNB(f *os.File) error {
	for {
		err := flockFn(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// gate takes the gate exclusively, retrying for RetryFor while someone holds it.
func gate(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, LockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(RetryFor)
	for {
		err = flockNB(f)
		if !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			who := readLine(f)
			f.Close()
			return nil, inUse(dir, who)
		}
		f.Close()
		return nil, err
	}
	return f, nil
}

var errInUse = errors.New("in use")

type inUseError struct{ dir, who string }

func (e *inUseError) Error() string {
	who := e.who
	if who == "" {
		who = "another cull command"
	}
	return fmt.Sprintf("%s is in use by %s: wait for it to finish (or stop it), then run this again", e.dir, who)
}

func (e *inUseError) Unwrap() error { return errInUse }

func inUse(dir, who string) error { return &inUseError{dir, who} }

func readLine(f *os.File) string {
	b, _ := io.ReadAll(io.NewSectionReader(f, 0, 300))
	return strings.TrimSpace(string(b))
}

func writeLine(f *os.File, line string) {
	if f.Truncate(0) == nil {
		f.WriteAt([]byte(line+"\n"), 0)
	}
}

// lockWriter takes the gate for the run, then makes sure no reader is live: a holder
// file it can't lock is one; one it can lock was left by a crash and is removed.
func lockWriter(dir, line string) (func(), string, error) {
	hook("writer gate")
	g, err := gate(dir)
	switch {
	case errors.Is(err, errInUse):
		return func() {}, "", err
	case err != nil:
		return noLock(dir, err)
	}
	writeLine(g, line)
	release := func() {
		flockFn(int(g.Fd()), syscall.LOCK_UN)
		g.Close()
	}
	hook("writer scan")
	ents, err := os.ReadDir(dir)
	if err != nil {
		release()
		return func() {}, "", err
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), HolderPrefix) || !e.Type().IsRegular() {
			continue
		}
		hook("writer check")
		p := filepath.Join(dir, e.Name())
		h, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			continue // removed meanwhile: its reader finished
		}
		switch err := flockNB(h); {
		case errors.Is(err, syscall.EWOULDBLOCK):
			who := readLine(h)
			h.Close()
			release()
			return func() {}, "", inUse(dir, who)
		case err == nil: // a crashed reader's: nobody holds it
			os.Remove(p)
			flockFn(int(h.Fd()), syscall.LOCK_UN)
		}
		h.Close()
	}
	return release, "", nil
}

// lockReader registers a holder file (locked for the run), then probes the gate.
func lockReader(dir, line string) (func(), string, error) {
	var h *os.File
	var p string
	deadline := time.Now().Add(RetryFor)
	for {
		var rb [4]byte
		rand.Read(rb[:])
		p = filepath.Join(dir, fmt.Sprintf("%s%d-%s", HolderPrefix, os.Getpid(), hex.EncodeToString(rb[:])))
		hook("reader create")
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return noLock(dir, err)
		}
		hook("reader lock")
		if err := flockNB(f); err != nil {
			f.Close()
			os.Remove(p)
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				return noLock(dir, err)
			}
			// A writer is trying it (and will remove it as a crashed reader's): register
			// again, or, past the deadline, stay out of the writer's way.
			if !time.Now().Before(deadline) {
				return func() {}, "", inUse(dir, "a redate or rename starting")
			}
			continue
		}
		// A writer may have taken the file for a crashed reader's and removed it before
		// the lock above: then it no longer registers this reader.
		hook("reader verify")
		if st, err := os.Stat(p); err == nil {
			if fst, err := f.Stat(); err == nil && os.SameFile(st, fst) {
				h = f
				break
			}
		}
		f.Close()
		if !time.Now().Before(deadline) {
			return func() {}, "", inUse(dir, "a redate or rename starting")
		}
	}
	writeLine(h, line)
	registered(p, true)
	release := func() {
		registered(p, false)
		os.Remove(p)
		flockFn(int(h.Fd()), syscall.LOCK_UN)
		h.Close()
	}
	hook("reader probe")
	g, err := gate(dir)
	switch {
	case errors.Is(err, errInUse):
		release()
		return func() {}, "", err
	case err != nil:
		return release, fmt.Sprintf("%s: folder lock unavailable (%v); make sure no redate or rename runs on this folder meanwhile", dir, err), nil
	}
	flockFn(int(g.Fd()), syscall.LOCK_UN)
	g.Close()
	return release, "", nil
}

// The holder files this process holds, so an exit path that skips the commands'
// deferred releases can still remove them (RemoveHolders).
var (
	holdersMu sync.Mutex
	holders   = map[string]bool{}
)

func registered(p string, on bool) {
	holdersMu.Lock()
	defer holdersMu.Unlock()
	if on {
		holders[p] = true
	} else {
		delete(holders, p)
	}
}

// RemoveHolders removes every holder file this process still holds: main calls it on
// the way out. A holder file left by a crash is harmless; the next writer clears it.
func RemoveHolders() {
	holdersMu.Lock()
	defer holdersMu.Unlock()
	for p := range holders {
		os.Remove(p)
		delete(holders, p)
	}
}

// flockFn is flock(2); tests act out volumes that refuse it.
var flockFn = syscall.Flock
