package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jefflaplante/cull/internal/eval"
	"github.com/jefflaplante/cull/internal/focus"
	"github.com/jefflaplante/cull/internal/imageprep"
	"github.com/jefflaplante/cull/internal/llm"
	"github.com/jefflaplante/cull/internal/report"
)

// perFileBackend answers each frame with the sharpness status for its file name.
type perFileBackend struct {
	status map[string]string
	calls  int32 // workers call concurrently
}

func (p *perFileBackend) Name() string { return "fake" }
func (p *perFileBackend) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	atomic.AddInt32(&p.calls, 1)
	status := "sharp"
	for name, s := range p.status {
		if strings.Contains(req.Parts[0].Text, name) {
			status = s
		}
	}
	return (&fakeBackend{status: status}).Call(ctx, req)
}

// shoot writes L1000001 (cull), L1000002 (keep), L1000003 (review).
func shoot(t *testing.T) (dir string, b *perFileBackend) {
	t.Helper()
	dir = t.TempDir()
	for _, n := range []string{"L1000001", "L1000002", "L1000003"} {
		minimalDNG(t, filepath.Join(dir, n+".DNG"))
	}
	return dir, &perFileBackend{status: map[string]string{"L1000001": "missed_focus", "L1000003": "soft"}}
}

func moveCfg(dir string) Config {
	c := cfg(dir)
	c.MoveCulled = true
	c.detect = func(*imageprep.Frame) []focus.Face { return nil }
	return c
}

func result(t *testing.T, rep *report.Report, name string) report.Result {
	t.Helper()
	for _, r := range rep.Results {
		if filepath.Base(r.File) == name {
			return r
		}
	}
	t.Fatalf("no result for %s", name)
	return report.Result{}
}

func TestMoveCulledMovesOnlyCullsWithTheirSidecars(t *testing.T) {
	dir, b := shoot(t)
	rep, _, err := Run(context.Background(), moveCfg(dir), b)
	if err != nil {
		t.Fatal(err)
	}
	culled := filepath.Join(dir, "cull")
	if exists(filepath.Join(dir, "L1000001.DNG")) || exists(filepath.Join(dir, "L1000001.xmp")) {
		t.Fatal("culled frame or its sidecar left in the shoot folder")
	}
	if !exists(filepath.Join(culled, "L1000001.DNG")) || !exists(filepath.Join(culled, "L1000001.xmp")) {
		t.Fatal("culled frame or its sidecar missing from culled/")
	}
	for _, n := range []string{"L1000002", "L1000003"} { // keep and review stay put
		if !exists(filepath.Join(dir, n+".DNG")) || !exists(filepath.Join(dir, n+".xmp")) {
			t.Fatalf("%s moved or lost its sidecar", n)
		}
	}
	r := result(t, rep, "L1000001.DNG")
	if r.Decision != eval.Cull || r.MovedTo != filepath.Join(culled, "L1000001.DNG") || r.XMP != filepath.Join(culled, "L1000001.xmp") {
		t.Fatalf("result decision=%s moved_to=%q xmp=%q", r.Decision, r.MovedTo, r.XMP)
	}
	saved, _ := os.ReadFile(filepath.Join(dir, "r.json"))
	if !strings.Contains(string(saved), `"moved_to"`) {
		t.Fatal("report on disk does not record the move")
	}
}

func TestMoveCulledNeverOverwrites(t *testing.T) {
	for _, clash := range []string{"L1000001.DNG", "L1000001.xmp"} {
		dir, b := shoot(t)
		culled := filepath.Join(dir, "cull")
		os.MkdirAll(culled, 0o755)
		os.WriteFile(filepath.Join(culled, clash), []byte("already here"), 0o644)
		rep, _, err := Run(context.Background(), moveCfg(dir), b)
		if err != nil {
			t.Fatal(err)
		}
		if !exists(filepath.Join(dir, "L1000001.DNG")) || !exists(filepath.Join(dir, "L1000001.xmp")) {
			t.Fatalf("%s clash: frame or sidecar moved anyway", clash)
		}
		if got, _ := os.ReadFile(filepath.Join(culled, clash)); string(got) != "already here" {
			t.Fatalf("%s clash: existing file overwritten", clash)
		}
		r := result(t, rep, "L1000001.DNG")
		if r.MovedTo != "" || !strings.Contains(strings.Join(r.Fixups, ";"), "not moved") {
			t.Fatalf("%s clash: moved_to=%q fixups=%v", clash, r.MovedTo, r.Fixups)
		}
	}
}

func TestMoveCulledLeavesFileWhenFolderCannotBeMade(t *testing.T) {
	dir, b := shoot(t)
	os.WriteFile(filepath.Join(dir, "cull"), []byte("a file, not a folder"), 0o644)
	rep, _, err := Run(context.Background(), moveCfg(dir), b)
	if err != nil {
		t.Fatal(err)
	}
	r := result(t, rep, "L1000001.DNG")
	if !exists(filepath.Join(dir, "L1000001.DNG")) || r.MovedTo != "" || len(r.Fixups) == 0 {
		t.Fatalf("moved_to=%q fixups=%v", r.MovedTo, r.Fixups)
	}
}

func TestDiscoverSkipsCulledFolders(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"A.DNG", "culled/B.DNG", "sub/C.DNG", "sub/culled/D.DNG"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), nil, 0o644)
	}
	got, err := Discover(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, filepath.Base(p))
	}
	if strings.Join(names, " ") != "A.DNG C.DNG" {
		t.Fatalf("discovered %v", names)
	}
}

func TestResumeDoesNotReprocessMovedFrames(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	c.Resume = true
	b2 := &perFileBackend{}
	rep, _, err := Run(context.Background(), c, b2)
	if err != nil || b2.calls != 0 || len(rep.Results) != 3 {
		t.Fatalf("err=%v calls=%d results=%d", err, b2.calls, len(rep.Results))
	}
	if r := result(t, rep, "L1000001.DNG"); r.MovedTo == "" {
		t.Fatal("resume lost the recorded move")
	}
}

func TestRestoreUndoesTheMove(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.WriteXMP = false
	os.WriteFile(filepath.Join(dir, "L1000001.xmp"), []byte("user sidecar"), 0o644) // pre-existing, not ours
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "cull", "L1000001.xmp")) {
		t.Fatal("pre-existing sidecar did not travel with its frame")
	}

	var log bytes.Buffer
	n, err := Restore(c.ReportPath, "", &log, nil)
	if err != nil || n != 1 {
		t.Fatalf("restored %d, err %v\n%s", n, err, log.String())
	}
	if !exists(filepath.Join(dir, "L1000001.DNG")) || exists(filepath.Join(dir, "cull")) {
		t.Fatal("frame not back, or empty culled/ left behind")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "L1000001.xmp")); string(got) != "user sidecar" {
		t.Fatalf("sidecar not restored intact: %q", got)
	}
	saved, _ := report.Load(c.ReportPath)
	if r := result(t, saved, "L1000001.DNG"); r.MovedTo != "" {
		t.Fatalf("moved_to not cleared: %q", r.MovedTo)
	}
	if n, err := Restore(c.ReportPath, "", io.Discard, nil); err != nil || n != 0 {
		t.Fatalf("second restore: %d, %v", n, err)
	}
}

func TestRestoreNeverOverwrites(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "L1000001.DNG"), []byte("new file at the old path"), 0o644)
	var log bytes.Buffer
	if n, err := Restore(c.ReportPath, "", &log, nil); err != nil || n != 0 {
		t.Fatalf("restored %d, err %v", n, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "L1000001.DNG")); string(got) != "new file at the old path" {
		t.Fatal("restore overwrote a file")
	}
	if !exists(filepath.Join(dir, "cull", "L1000001.DNG")) || !strings.Contains(log.String(), "exists") {
		t.Fatalf("culled copy should stay, with a note:\n%s", log.String())
	}
}

func TestFreshRefusedWhileFramesAreMoved(t *testing.T) {
	dir, b := shoot(t)
	if _, _, err := Run(context.Background(), moveCfg(dir), b); err != nil {
		t.Fatal(err)
	}
	c := moveCfg(dir)
	c.Fresh = true
	_, _, err := Run(context.Background(), c, b)
	if err == nil || !strings.Contains(err.Error(), "cull restore") {
		t.Fatalf("want a refusal naming cull restore, got %v", err)
	}
}

// A move whose report save never happened: the frame is in culled/, the report
// says it isn't. restore must still bring it back.
func TestRestoreAdoptsUnrecordedMove(t *testing.T) {
	for _, folder := range []string{CullDir, CulledDir} { // CulledDir: a crash under v0.1
		t.Run(folder, func(t *testing.T) {
			dir, b := shoot(t)
			c := moveCfg(dir)
			c.MoveCulled = false
			if _, _, err := Run(context.Background(), c, b); err != nil {
				t.Fatal(err)
			}
			culled := filepath.Join(dir, folder)
			os.MkdirAll(culled, 0o755)
			for _, n := range []string{"L1000001.DNG", "L1000001.xmp"} { // the move, without the save
				if err := os.Rename(filepath.Join(dir, n), filepath.Join(culled, n)); err != nil {
					t.Fatal(err)
				}
			}
			n, err := Restore(filepath.Join(dir, "r.json"), "", io.Discard, nil)
			if err != nil || n != 1 {
				t.Fatalf("restored %d, err %v", n, err)
			}
			if !exists(filepath.Join(dir, "L1000001.DNG")) || !exists(filepath.Join(dir, "L1000001.xmp")) {
				t.Fatal("frame or sidecar not restored")
			}
		})
	}
}

func TestMoveCulledAdoptsUnrecordedMove(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled = false
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	culled := filepath.Join(dir, "cull")
	os.MkdirAll(culled, 0o755)
	os.Rename(filepath.Join(dir, "L1000001.DNG"), filepath.Join(culled, "L1000001.DNG"))
	sum, err := Decide(context.Background(), filepath.Join(dir, "r.json"), DecideOptions{MoveCulled: true, Policy: eval.Policy{MinCropArea: 0.6}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := report.Load(filepath.Join(dir, "r.json"))
	r := result(t, rep, "L1000001.DNG")
	if r.MovedTo != filepath.Join(culled, "L1000001.DNG") || len(r.Fixups) != 0 || sum.Moved != 0 {
		t.Fatalf("moved_to=%q fixups=%v moved=%d", r.MovedTo, r.Fixups, sum.Moved)
	}
}

// A restore whose report save never happened: the frame is back, the report
// still says culled/. The next restore must not fail on it.
func TestRestoreForgetsAlreadyRestoredMove(t *testing.T) {
	dir, b := shoot(t)
	if _, _, err := Run(context.Background(), moveCfg(dir), b); err != nil {
		t.Fatal(err)
	}
	culled := filepath.Join(dir, "cull")
	os.Rename(filepath.Join(culled, "L1000001.DNG"), filepath.Join(dir, "L1000001.DNG"))
	os.Rename(filepath.Join(culled, "L1000001.xmp"), filepath.Join(dir, "L1000001.xmp"))
	var log bytes.Buffer
	if _, err := Restore(filepath.Join(dir, "r.json"), "", &log, nil); err != nil {
		t.Fatal(err)
	}
	rep, _ := report.Load(filepath.Join(dir, "r.json"))
	if r := result(t, rep, "L1000001.DNG"); r.MovedTo != "" || r.XMP != filepath.Join(dir, "L1000001.xmp") || strings.Contains(log.String(), "not restored") {
		t.Fatalf("moved_to=%q xmp=%q log=%s", r.MovedTo, r.XMP, log.String())
	}
}

func TestRenameNoReplaceRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(src, []byte("src"), 0o644)
	os.WriteFile(dst, []byte("dst"), 0o644)
	if err := renameNoReplace(src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("want ErrExist, got %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "dst" || !exists(src) {
		t.Fatal("destination replaced or source lost")
	}
	os.Remove(dst)
	if err := renameNoReplace(src, dst); err != nil || exists(src) || !exists(dst) {
		t.Fatalf("plain move: err=%v", err)
	}
}

func TestMoveFixupRecordedOnce(t *testing.T) {
	dir, b := shoot(t)
	culled := filepath.Join(dir, "cull")
	os.MkdirAll(culled, 0o755)
	os.WriteFile(filepath.Join(culled, "L1000001.DNG"), []byte("already here"), 0o644)
	if _, _, err := Run(context.Background(), moveCfg(dir), b); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		Decide(context.Background(), filepath.Join(dir, "r.json"), DecideOptions{MoveCulled: true, Policy: eval.Policy{MinCropArea: 0.6}}, io.Discard)
	}
	rep, _ := report.Load(filepath.Join(dir, "r.json"))
	n := 0
	for _, f := range result(t, rep, "L1000001.DNG").Fixups {
		if strings.HasPrefix(f, "move:") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d move fixups, want 1", n)
	}
}

// Link succeeded, Remove failed (the source folder is read-only): the new name
// must not be left behind, or every later move and restore sees "already exists".
func TestRenameNoReplaceUndoesLinkWhenSourceCannotBeRemoved(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	src, dst := filepath.Join(a, "x"), filepath.Join(b, "x")
	os.WriteFile(src, []byte("src"), 0o644)
	os.Chmod(a, 0o555)
	defer os.Chmod(a, 0o755)
	if err := renameNoReplace(src, dst); err == nil {
		t.Fatal("want an error: the source can't be removed")
	}
	if exists(dst) || !exists(src) {
		t.Fatalf("dst left: %v, src kept: %v", exists(dst), exists(src))
	}
}

// A frame moved into culled/ by a run whose report was never saved is still a
// moved frame: --fresh must refuse, or the new report forgets it.
func TestFreshRefusedForUnrecordedMove(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled = false
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	culled := filepath.Join(dir, "cull")
	os.MkdirAll(culled, 0o755)
	os.Rename(filepath.Join(dir, "L1000001.DNG"), filepath.Join(culled, "L1000001.DNG"))
	c.Fresh = true
	if _, _, err := Run(context.Background(), c, b); err == nil || !strings.Contains(err.Error(), "cull restore") {
		t.Fatalf("want a refusal naming cull restore, got %v", err)
	}
}

// An unrelated file at the frame's culled/ path (the frame itself deleted by
// hand) is not the frame: it is never adopted, restored or moved.
func TestReconcileNeverAdoptsAStranger(t *testing.T) {
	dir, b := shoot(t)
	c := moveCfg(dir)
	c.MoveCulled = false
	if _, _, err := Run(context.Background(), c, b); err != nil {
		t.Fatal(err)
	}
	culled := filepath.Join(dir, "cull")
	os.MkdirAll(culled, 0o755)
	os.Remove(filepath.Join(dir, "L1000001.DNG"))
	os.WriteFile(filepath.Join(culled, "L1000001.DNG"), []byte("someone else's file"), 0o644)
	if n, err := Restore(filepath.Join(dir, "r.json"), "", io.Discard, nil); err != nil || n != 0 {
		t.Fatalf("restored %d, err %v", n, err)
	}
	if exists(filepath.Join(dir, "L1000001.DNG")) {
		t.Fatal("stranger moved into the shoot folder")
	}
}
