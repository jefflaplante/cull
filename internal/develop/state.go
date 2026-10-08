package develop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// StateName is the shoot's develop record, beside its report: the recipe, and each
	// keep's export. It stays after a run, so a re-run develops only what changed.
	StateName = "cull-develop.json"
	// WorkName is the hidden folder beside it for LightCraft's per-chunk libraries and
	// logs, and the preset copies it imports. Removed after a clean run; a failed
	// chunk's folder is kept for its logs.
	WorkName     = ".cull-develop"
	stateVersion = 1

	statusDone   = "done"
	statusFailed = "failed"
)

// State is everything a re-run needs: like the batch state (pipeline/batch.go) it is
// saved after every frame LightCraft finishes, so an interrupted overnight run loses at
// most the frames it was working on.
type State struct {
	Version    int                    `json:"version"`
	Out        string                 `json:"out"`                  // the export folder
	Recipe     Recipe                 `json:"recipe"`               // the last run's
	LightCraft string                 `json:"lightcraft,omitempty"` // lightcraft-cli --version, the last run's
	Frames     map[string]*FrameState `json:"frames"`               // by base name
}

// FrameState is one keep's last develop.
type FrameState struct {
	File        string    `json:"file"`        // the DNG, when developed
	Fingerprint string    `json:"fingerprint"` // Recipe.Fingerprint: what the JPEG was made from
	Status      string    `json:"status"`      // done | failed
	Preset      string    `json:"preset,omitempty"`
	EV          *float64  `json:"ev,omitempty"` // the sidecar's, set after the auto stages
	JPEG        string    `json:"jpeg,omitempty"`
	Bytes       int64     `json:"bytes,omitempty"`
	SHA256      string    `json:"sha256,omitempty"` // the JPEG's: proves a JPEG at JPEG is cull's to replace
	Ms          float64   `json:"ms,omitempty"`     // LightCraft's time for its commands
	Error       string    `json:"error,omitempty"`
	At          time.Time `json:"at"`
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{Version: stateVersion, Frames: map[string]*FrameState{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("%s is version %d; this cull reads version %d", path, st.Version, stateVersion)
	}
	if st.Frames == nil {
		st.Frames = map[string]*FrameState{}
	}
	return &st, nil
}

// save writes the state atomically: a torn state would forget which JPEGs are cull's.
func (st *State) save(path string) error {
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Clean(path))
}

// lockWork takes the shoot's develop lock (an exclusive flock in the work folder,
// released by closing it, as journal's locks are), so two develops never share a state.
func lockWork(work string) (release func(), err error) {
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(work, "lock")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EACCES) {
		f.Close()
		return nil, fmt.Errorf("another cull develop is running on this shoot (%s is locked)", p)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}
