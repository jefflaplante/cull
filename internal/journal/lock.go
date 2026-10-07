package journal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// LockName is a shoot folder's lock file. Commands that change the files' names or
// bytes (redate, rename) hold it exclusively; commands that rely on them staying put
// (judge, decide, the review server, restore, offload into an existing folder) hold it
// shared, for their whole run. It is hidden, so no folder walk takes it for a frame,
// and it is never removed: a removed lock file would let a later holder lock another
// file of the same name.
const LockName = ".cull.lock"

// Lock takes dir's folder lock (flock, never waiting): exclusive or shared. holder
// names the command ("rename"); it is written into the file, best effort, so a refused
// command can say what holds the folder. release unlocks. A volume without locks
// (ENOTSUP) or a folder the lock file can't be made in proceeds unlocked, with note
// saying so.
func Lock(dir string, exclusive bool, holder string) (release func(), note string, err error) {
	none := func() {}
	p := filepath.Join(dir, LockName)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return none, fmt.Sprintf("%s: no folder lock (%v); make sure no other cull command uses this folder meanwhile", dir, err), nil
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, syscall.EWOULDBLOCK):
		b, _ := io.ReadAll(io.LimitReader(f, 200))
		f.Close()
		who := strings.TrimSpace(string(b))
		if who == "" {
			who = "another cull command"
		}
		return none, "", fmt.Errorf("%s is in use by %s: wait for it to finish (or stop it), then run this again", dir, who)
	case errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP):
		f.Close()
		return none, fmt.Sprintf("%s: this volume has no file locks; make sure no other cull command uses this folder meanwhile", dir), nil
	default:
		f.Close()
		return none, "", fmt.Errorf("lock %s: %w", p, err)
	}
	// Best effort: who holds it, for a refused command's message.
	line := []byte(fmt.Sprintf("cull %s (pid %d)\n", holder, os.Getpid()))
	if b, _ := io.ReadAll(io.LimitReader(f, 200)); string(b) != string(line) && f.Truncate(0) == nil {
		f.WriteAt(line, 0)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, "", nil
}
