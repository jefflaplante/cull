//go:build cardbench

package offload

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// go test -tags cardbench -run CardBench ./internal/offload (needs the LEICA M card mounted)
func TestCardBench(t *testing.T) {
	card := "/Volumes/LEICA M/DCIM/100LEICA"
	dst := t.TempDir()
	var engine, plain []string
	for i := 3170; i < 3190; i++ {
		engine = append(engine, filepath.Join(card, fmt.Sprintf("M110%d.DNG", i)))
	}
	for i := 3190; i < 3210; i++ {
		plain = append(plain, filepath.Join(card, fmt.Sprintf("M110%d.DNG", i)))
	}
	size := func(fs []string) (n int64) {
		for _, f := range fs {
			st, _ := os.Stat(f)
			n += st.Size()
		}
		return
	}
	a := filepath.Join(dst, "engine")
	os.Mkdir(a, 0o755)
	start := time.Now()
	for _, f := range engine {
		st, _ := os.Stat(f)
		if _, err := copyFile(context.Background(), f, filepath.Base(f), []string{a}, st.ModTime(), hooks{}); err != nil {
			t.Fatal(err)
		}
	}
	d := filepath.Join(a)
	flushDrive(d)
	et := time.Since(start)
	b := filepath.Join(dst, "cp")
	os.Mkdir(b, 0o755)
	start = time.Now()
	if out, err := exec.Command("cp", append(plain, b)...).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	ct := time.Since(start)
	es, cs := size(engine), size(plain)
	t.Logf("cull engine (read once, hash, write uncached, evict, verify from disk, F_FULLFSYNC): %d MB in %s = %.0f MB/s", es/1e6, et.Round(time.Millisecond), float64(es)/1e6/et.Seconds())
	t.Logf("cp (no verify):                                                                  %d MB in %s = %.0f MB/s", cs/1e6, ct.Round(time.Millisecond), float64(cs)/1e6/ct.Seconds())
}
