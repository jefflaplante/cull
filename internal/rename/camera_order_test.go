package rename_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/rename"
	"github.com/jefflaplante/cull/internal/report"
)

// The live case (2026-10-07, an M11-P card): the camera numbers its M… frames and its
// L… (Content Credentials) frames from one counter, M1102767–M1102771 then
// L1002772–L1002776, all with one capture time (a clock that wasn't running). {n}
// numbers them in that order, not by name (which put every L first); judge, ranking
// on, makes no calls after the rename.
func TestRenameNumbersSharedCounterInCameraOrder(t *testing.T) {
	var m, l []string
	files := map[string]dngtest.Fixture{}
	for i := 0; i < 5; i++ {
		m = append(m, fmt.Sprintf("M110%04d.DNG", 2767+i))
		l = append(l, fmt.Sprintf("L100%04d.DNG", 2772+i))
	}
	for i, n := range append(append([]string(nil), l...), m...) {
		fx := frame(int64(i+1), 4000+100*i)
		fx.Preview = lookJPEG(0) // one look: the ten frames are one set
		files[n] = fx
	}
	c, dest := t.TempDir(), t.TempDir()
	card(t, c, files)
	dir := offloadCard(t, c, dest)
	b := &ranker{}
	cfg := judgeCfg(dir)
	cfg.Resume, cfg.Rank = true, true
	cfg.Seq = group.Options{Gap: time.Hour, MaxLook: group.DefaultLook}
	cfg.Policy.KeepBest = 2
	if _, _, err := pipeline.Run(context.Background(), cfg, b); err != nil {
		t.Fatal(err)
	}
	if b.ranks == 0 {
		t.Fatal("nothing ranked")
	}

	runRename(t, rename.Options{Dir: dir, Pattern: "{date}_{name}_{n:4}"})
	es, err := offload.CurrentManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range es {
		got[e.Orig] = e.Name
	}
	for i, n := range append(m, l...) {
		want := fmt.Sprintf("20251228_test_%04d.DNG", i+1)
		if got[n] != want {
			t.Errorf("%s is %s, want %s", n, got[n], want)
		}
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Error(err)
		}
	}
	if n := calls(t, b, cfg); n != 0 {
		t.Fatalf("judge (ranking on) after the rename made %d calls", n)
	}
}

// oldReport is a mixed-prefix shoot with one capture time, offloaded, then judged and
// ranked by a cull that didn't record card names (simulated: the manifest hidden
// during that judge). With rename first, the frames carry x_<camera name> names.
func oldReport(t *testing.T, renameFirst bool) (string, *ranker, pipeline.Config) {
	t.Helper()
	mLook, lLook := []int{0, 0, 1, 1, 2}, []int{2, 0, 0, 1, 1}
	files := map[string]dngtest.Fixture{}
	i := 0
	for k := 0; k < 5; k++ {
		for _, x := range []struct {
			name string
			look int
		}{{fmt.Sprintf("M110%04d.DNG", 2767+k), mLook[k]}, {fmt.Sprintf("L100%04d.DNG", 2772+k), lLook[k]}} {
			fx := frame(int64(i+1), 4000+100*i)
			fx.Preview = lookJPEG(x.look)
			files[x.name] = fx
			i++
		}
	}
	c, dest := t.TempDir(), t.TempDir()
	card(t, c, files)
	dir := offloadCard(t, c, dest)
	if renameFirst {
		runRename(t, rename.Options{Dir: dir, Pattern: "x_{orig}"}) // no report yet
	}
	man := filepath.Join(dir, offload.ManifestName)
	if err := os.Rename(man, man+".bak"); err != nil {
		t.Fatal(err)
	}
	b := &ranker{}
	cfg := judgeCfg(dir)
	cfg.Resume, cfg.Rank = true, true
	cfg.Seq = group.Options{Gap: time.Hour, MaxLook: group.DefaultLook}
	cfg.Policy.KeepBest = 1
	if _, _, err := pipeline.Run(context.Background(), cfg, b); err != nil {
		t.Fatal(err)
	}
	if b.ranks == 0 {
		t.Fatal("nothing ranked")
	}
	if err := os.Rename(man+".bak", man); err != nil {
		t.Fatal(err)
	}
	return dir, b, cfg
}

// On a report without card names, rename's order check fills them from the manifest
// (as the next judge will), and the rename writes them into the report: a rename that
// keeps camera order is accepted, and judge then makes no calls.
func TestRenameOldReportFillsCardNames(t *testing.T) {
	t.Run("x_{orig} on camera names", func(t *testing.T) {
		dir, b, cfg := oldReport(t, false)
		runRename(t, rename.Options{Dir: dir, Pattern: "x_{orig}"})
		rep, err := report.Load(cfg.ReportPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range rep.Results {
			if want := "100LEICA/" + strings.TrimPrefix(filepath.Base(x.File), "x_"); x.CardName != want {
				t.Errorf("%s: card name %q, want %q", x.File, x.CardName, want)
			}
		}
		if n := calls(t, b, cfg); n != 0 {
			t.Fatalf("judge after the rename made %d calls", n)
		}
	})
	// Names that aren't camera names (x_M1102767) were ordered as text, every x_L
	// before every x_M; card names put them in camera order, so the first judge after
	// the upgrade regroups and ranks again whether or not anything is renamed. The
	// rename is accepted and adds nothing to that.
	t.Run("y_{orig} on renamed names", func(t *testing.T) {
		_, b0, cfg0 := oldReport(t, true)
		upgrade := calls(t, b0, cfg0)
		if upgrade == 0 {
			t.Fatal("premise: card names regroup this shoot's sets")
		}
		dir, b, cfg := oldReport(t, true)
		runRename(t, rename.Options{Dir: dir, Pattern: "y_{orig}"})
		rep, err := report.Load(cfg.ReportPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range rep.Results {
			if want := "100LEICA/" + strings.TrimPrefix(filepath.Base(x.File), "y_"); x.CardName != want {
				t.Errorf("%s: card name %q, want %q", x.File, x.CardName, want)
			}
		}
		if n := calls(t, b, cfg); n != upgrade {
			t.Fatalf("judge after the rename made %d calls; without it, %d", n, upgrade)
		}
		if n := calls(t, b, cfg); n != 0 {
			t.Fatalf("a second judge made %d calls", n)
		}
	})
}

// New names of the DCF shape (8 characters ending in 4 digits) are ordered by those
// digits: when that changes the order, the refusal says so (not "pad {n}").
func TestRenameRefusesCameraShapedNamesThatReorder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-10-02 test")
	loose(t, dir, map[string]dngtest.Fixture{"A9999.DNG": frame(1, 4000), "B0001.DNG": frame(2, 4100)})
	judge(t, dir, &counting{}, nil)
	_, err := rename.Run(context.Background(), rename.Options{Dir: dir, Pattern: "zz{n}{orig}", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "zz1A9999.DNG") || !strings.Contains(err.Error(), "4 digits") || strings.Contains(err.Error(), "{n:") {
		t.Fatalf("%v", err)
	}
}
