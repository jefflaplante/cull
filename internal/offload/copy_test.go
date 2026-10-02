package offload

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func randomFile(t *testing.T, n int) (string, []byte) {
	t.Helper()
	b := make([]byte, n)
	rand.Read(b)
	p := filepath.Join(t.TempDir(), "M1.DNG")
	if err := os.WriteFile(p, b, 0o777); err != nil {
		t.Fatal(err)
	}
	return p, b
}

func noTemps(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		m, _ := filepath.Glob(filepath.Join(d, ".*.cull-*.tmp"))
		if len(m) > 0 {
			t.Fatalf("temp files left: %v", m)
		}
	}
}

func TestCopyFileTwoDestinations(t *testing.T) {
	src, data := randomFile(t, 9<<20+123)
	a, b := t.TempDir(), t.TempDir()
	mt := time.Date(2025, 12, 27, 23, 56, 5, 0, time.UTC)
	sum, err := copyFile(context.Background(), src, "M1.DNG", []string{a, b}, mt, hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if sum != sha256.Sum256(data) {
		t.Fatal("wrong sum")
	}
	for _, d := range []string{a, b} {
		p := filepath.Join(d, "M1.DNG")
		got, _ := os.ReadFile(p)
		st, _ := os.Stat(p)
		if !bytes.Equal(got, data) || st.Mode().Perm() != 0o644 || !st.ModTime().Equal(mt) {
			t.Fatalf("%s: equal=%v mode=%v mtime=%v", p, bytes.Equal(got, data), st.Mode().Perm(), st.ModTime())
		}
	}
	noTemps(t, a, b)
}

func TestCopyFileCatchesCorruption(t *testing.T) {
	src, _ := randomFile(t, 5<<20)
	a, b := t.TempDir(), t.TempDir()
	h := hooks{afterWrite: func(tmp string) {
		if filepath.Dir(tmp) != a {
			return
		}
		f, _ := os.OpenFile(tmp, os.O_RDWR, 0)
		buf := []byte{0}
		f.ReadAt(buf, 1234)
		buf[0] ^= 0xFF
		f.WriteAt(buf, 1234)
		f.Close()
	}}
	_, err := copyFile(context.Background(), src, "M1.DNG", []string{a, b}, time.Now(), h)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte(a)) {
		t.Fatalf("corruption not caught or not named: %v", err)
	}
	for _, d := range []string{a, b} {
		if _, err := os.Stat(filepath.Join(d, "M1.DNG")); err == nil {
			t.Fatalf("final name exists in %s after a failed verify", d)
		}
	}
	noTemps(t, a, b)
}

func TestCopyFileNeverReplaces(t *testing.T) {
	src, _ := randomFile(t, 1<<20)
	a := t.TempDir()
	os.WriteFile(filepath.Join(a, "M1.DNG"), []byte("theirs"), 0o644)
	if _, err := copyFile(context.Background(), src, "M1.DNG", []string{a}, time.Now(), hooks{}); err == nil {
		t.Fatal("replaced an existing file")
	}
	if got, _ := os.ReadFile(filepath.Join(a, "M1.DNG")); string(got) != "theirs" {
		t.Fatal("existing file changed")
	}
	noTemps(t, a)
}

type failingReader struct {
	r     io.Reader
	after int
	n     int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n >= f.after {
		return 0, errors.New("input/output error")
	}
	if len(p) > f.after-f.n {
		p = p[:f.after-f.n]
	}
	n, err := f.r.Read(p)
	f.n += n
	return n, err
}
func (f *failingReader) Close() error { return nil }

func TestCopyFileSourceErrorCleansUp(t *testing.T) {
	src, data := randomFile(t, 9<<20)
	a := t.TempDir()
	h := hooks{open: func(string) (io.ReadCloser, error) {
		return &failingReader{r: bytes.NewReader(data), after: 5 << 20}, nil
	}}
	if _, err := copyFile(context.Background(), src, "M1.DNG", []string{a}, time.Now(), h); err == nil {
		t.Fatal("source error ignored")
	}
	if _, err := os.Stat(filepath.Join(a, "M1.DNG")); err == nil {
		t.Fatal("partial file under the real name")
	}
	noTemps(t, a)
}

type cancelAfterFirst struct {
	r      io.Reader
	cancel func()
	reads  int
}

func (c *cancelAfterFirst) Read(p []byte) (int, error) {
	c.reads++
	if c.reads == 2 {
		c.cancel()
	}
	return c.r.Read(p)
}
func (c *cancelAfterFirst) Close() error { return nil }

func TestCopyFileCancel(t *testing.T) {
	src, data := randomFile(t, 20<<20)
	a := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	h := hooks{open: func(string) (io.ReadCloser, error) {
		return &cancelAfterFirst{r: bytes.NewReader(data), cancel: cancel}, nil
	}}
	if _, err := copyFile(ctx, src, "M1.DNG", []string{a}, time.Now(), h); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if ents, _ := os.ReadDir(a); len(ents) != 0 {
		t.Fatalf("files left after cancel: %v", ents)
	}
}

func TestCopyFileWriteError(t *testing.T) {
	src, _ := randomFile(t, 9<<20)
	a := t.TempDir()
	h := hooks{write: func(f *os.File, p []byte) (int, error) { return 0, errors.New("no space left on device") }}
	if _, err := copyFile(context.Background(), src, "M1.DNG", []string{a}, time.Now(), h); err == nil {
		t.Fatal("write error ignored")
	}
	if ents, _ := os.ReadDir(a); len(ents) != 0 {
		t.Fatalf("files left after a write error: %v", ents)
	}
}

// warmCache reads p normally, pulling every page into the page cache.
func warmCache(t *testing.T, p string) {
	t.Helper()
	if _, err := os.ReadFile(p); err != nil {
		t.Fatal(err)
	}
}
