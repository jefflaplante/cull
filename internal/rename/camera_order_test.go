package rename_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/group"
	"github.com/jefflaplante/cull/internal/offload"
	"github.com/jefflaplante/cull/internal/pipeline"
	"github.com/jefflaplante/cull/internal/rename"
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
