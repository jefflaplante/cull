package offload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/dng/dngtest"
)

// patchedDNG is a synthetic DNG with every kind of capture date, a payload big enough
// to span several of streamPatched's chunks, and the patches that set its dates to t.
func patchedDNG(t *testing.T) (src []byte, ps []dng.Patch) {
	t.Helper()
	payload := make([]byte, 9<<20)
	rand.New(rand.NewSource(1)).Read(payload)
	src = dngtest.Build(t, dngtest.Fixture{
		DateTime: "2025:12:28 00:05:59", DTO: "2025:12:28 00:05:59", DTD: "2025:12:28 00:05:59",
		SubSecOrig: "42",
		XMP:        `<x:xmpmeta><rdf:Description xmp:CreateDate="2025-12-28T00:05:59" xmp:ModifyDate="2025-12-28T00:05:59"/></x:xmpmeta>`,
		Payload:    payload,
	})
	ps, _, err := dng.PatchDates(bytes.NewReader(src), int64(len(src)), time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local))
	if err != nil || len(ps) == 0 {
		t.Fatalf("PatchDates: %d patches, %v", len(ps), err)
	}
	return src, ps
}

// spanningPatches puts patches across streamPatched's 4 MiB chunk boundaries, at the
// first and last bytes, and back to back: the cases a per-chunk loop gets wrong.
func spanningPatches(src []byte) []dng.Patch {
	mk := func(off, n int) dng.Patch {
		old := append([]byte(nil), src[off:off+n]...)
		nw := make([]byte, n)
		for i := range nw {
			nw[i] = old[i] ^ 0xFF
		}
		return dng.Patch{Off: int64(off), Old: old, New: nw}
	}
	n := len(src)
	return []dng.Patch{
		mk(0, 3),
		mk(3, 5), // touches the previous one
		mk(bufSize-4, 9),
		mk(2*bufSize-1, 2),
		mk(2*bufSize+1, bufSize+10), // longer than a whole chunk
		mk(n-6, 6),
	}
}

func sum(b []byte) [32]byte { return sha256.Sum256(b) }

func TestStreamPatchedHashes(t *testing.T) {
	src, ps := patchedDNG(t)
	random := make([]byte, 3*bufSize+123)
	rand.New(rand.NewSource(2)).Read(random)
	for name, c := range map[string]struct {
		src []byte
		ps  []dng.Patch
	}{
		"dng":      {src, ps},
		"spanning": {random, spanningPatches(random)},
		"none":     {random, nil},
	} {
		t.Run(name, func(t *testing.T) {
			want := dngtest.Apply(c.src, c.ps)
			var w bytes.Buffer
			o, p, err := streamPatched(context.Background(), bytes.NewReader(c.src), c.ps, &w)
			if err != nil {
				t.Fatal(err)
			}
			if o != sum(c.src) {
				t.Error("orig isn't the hash of the bytes read")
			}
			if p != sum(want) {
				t.Error("want isn't the hash of the patched bytes")
			}
			if !bytes.Equal(w.Bytes(), want) {
				t.Error("written bytes aren't the patched bytes")
			}
			// Without a writer the hashes are the same.
			o2, p2, err := streamPatched(context.Background(), bytes.NewReader(c.src), c.ps, nil)
			if err != nil || o2 != o || p2 != p {
				t.Errorf("no writer: %v", err)
			}
		})
	}
}

func TestStreamPatchedRejectsWrongOld(t *testing.T) {
	src, ps := patchedDNG(t)
	random := make([]byte, 3*bufSize+123)
	rand.New(rand.NewSource(2)).Read(random)
	sp := spanningPatches(random)

	clone := func(ps []dng.Patch) []dng.Patch {
		out := make([]dng.Patch, len(ps))
		for i, p := range ps {
			out[i] = dng.Patch{Off: p.Off, Old: append([]byte(nil), p.Old...), New: append([]byte(nil), p.New...), What: p.What}
		}
		return out
	}
	cases := map[string]struct {
		src []byte
		ps  []dng.Patch
	}{}
	bad := clone(ps)
	bad[len(bad)-1].Old[0] ^= 1
	cases["dng old"] = struct {
		src []byte
		ps  []dng.Patch
	}{src, bad}
	// Old differs only in its part past a chunk boundary.
	bad = clone(sp)
	bad[2].Old[8] ^= 1
	cases["spanning old"] = struct {
		src []byte
		ps  []dng.Patch
	}{random, bad}
	bad = clone(sp)
	bad[0], bad[1] = bad[1], bad[0]
	cases["unsorted"] = struct {
		src []byte
		ps  []dng.Patch
	}{random, bad}
	bad = clone(sp)
	bad[1].Off--
	cases["overlapping"] = struct {
		src []byte
		ps  []dng.Patch
	}{random, bad}
	bad = clone(sp)
	bad[0].New = bad[0].New[:2]
	cases["length change"] = struct {
		src []byte
		ps  []dng.Patch
	}{random, bad}
	cases["past the end"] = struct {
		src []byte
		ps  []dng.Patch
	}{random[:len(random)-1], clone(sp)}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var w bytes.Buffer
			if _, _, err := streamPatched(context.Background(), bytes.NewReader(c.src), c.ps, &w); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestStreamPatchedStopsOnCancel(t *testing.T) {
	src, ps := patchedDNG(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := streamPatched(ctx, bytes.NewReader(src), ps, nil); err == nil {
		t.Fatal("ran after cancel")
	}
}

// The proof: a patched file re-read from the device must hash to want. A stray byte
// anywhere else, or a patch left out, fails it.
func TestProveCatchesStrayByte(t *testing.T) {
	src, ps := patchedDNG(t)
	_, want, err := streamPatched(context.Background(), bytes.NewReader(src), ps, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(ps []dng.Patch) string {
		p := filepath.Join(dir, "M1.DNG")
		os.Remove(p)
		if err := os.WriteFile(p, src, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := applyPatches(p, ps); err != nil {
			t.Fatal(err)
		}
		return p
	}

	p := write(ps)
	if err := proveFrom(context.Background(), p, want); err != nil {
		t.Fatalf("exact patches: %v", err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, dngtest.Apply(src, ps)) {
		t.Fatal("applyPatches didn't write exactly the patches")
	}

	// One byte flipped outside every patch, in the payload.
	f, _ := os.OpenFile(p, os.O_WRONLY, 0)
	stray := int64(len(src) - 5000)
	for _, q := range ps {
		if stray >= q.Off && stray < q.Off+int64(len(q.New)) {
			t.Fatal("stray byte inside a patch")
		}
	}
	b := []byte{src[stray] ^ 0x10}
	f.WriteAt(b, stray)
	f.Close()
	if err := proveFrom(context.Background(), p, want); err == nil {
		t.Fatal("a stray byte passed the proof")
	}

	// One patch left out.
	p = write(ps[1:])
	if err := proveFrom(context.Background(), p, want); err == nil {
		t.Fatal("a missing patch passed the proof")
	}
}

// applyPatches never grows or truncates the file: a patch past its end is refused.
func TestApplyPatchesKeepsLength(t *testing.T) {
	p := filepath.Join(t.TempDir(), "M1.DNG")
	os.WriteFile(p, []byte("0123456789"), 0o644)
	if err := applyPatches(p, []dng.Patch{{Off: 8, Old: []byte("89x"), New: []byte("abc")}}); err == nil {
		t.Fatal("a patch past the end was written")
	}
	if b, _ := os.ReadFile(p); string(b) != "0123456789" {
		t.Fatalf("file changed: %q", b)
	}
	if err := applyPatches(p, []dng.Patch{{Off: 2, Old: []byte("23"), New: []byte("ab")}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "01ab456789" {
		t.Fatalf("got %q", b)
	}
}

// Verify checks each file against its current (last) manifest entry, and against
// file_sha256 when the entry has one: a redated, renamed file is OK, and its old name
// isn't reported missing.
func TestVerifyUsesFileSHAAndLastEntry(t *testing.T) {
	src, ps := patchedDNG(t)
	patched := dngtest.Apply(src, ps)
	folder := t.TempDir()
	p := filepath.Join(folder, "Trip_0001.DNG")
	if err := os.WriteFile(p, patched, 0o644); err != nil {
		t.Fatal(err)
	}
	cardSum, fileSum := sum(src), sum(patched)
	at := time.Now().UTC().Truncate(time.Second)
	for _, e := range []Entry{
		{Src: "/card/M1.DNG", Orig: "M1.DNG", Name: "M1.DNG", Size: int64(len(src)), SHA256: hexOf(cardSum[:]), At: at},
		{Src: "/card/M2.DNG", Orig: "M2.DNG", Name: "M2.DNG", Size: 3, SHA256: hexOf(cardSum[:]), At: at}, // never copied here
		{Src: "/card/M1.DNG", Orig: "m1.dng", Name: "Trip_0001.DNG", Size: int64(len(src)), SHA256: hexOf(cardSum[:]),
			FileSHA256: hexOf(fileSum[:]), DatesSet: "2026-10-04T12:00:00", PatchedAt: at, At: at},
	} {
		if err := appendManifest(folder, e); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(folder, "M2.DNG"), []byte("abc"), 0o644)
	m2 := sha256.Sum256([]byte("abc"))
	// M2's entry carries the wrong sum on purpose above; supersede it with the right one.
	appendManifest(folder, Entry{Src: "/card/M2.DNG", Orig: "M2.DNG", Name: "M2.DNG", Size: 3, SHA256: hexOf(m2[:]), At: at})

	v, err := Verify(context.Background(), folder, nil)
	if err != nil || v.OK != 2 || v.Bad != 0 || len(v.Unrecorded) != 0 {
		t.Fatalf("%+v %v", v, err)
	}

	f, _ := os.OpenFile(p, os.O_WRONLY, 0)
	f.WriteAt([]byte{patched[len(patched)-100] ^ 1}, int64(len(patched)-100))
	f.Close()
	v, err = Verify(context.Background(), folder, nil)
	if err != nil || v.OK != 1 || v.Bad != 1 {
		t.Fatalf("after corruption: %+v %v", v, err)
	}
}

func TestCurrentKeepsLastEntry(t *testing.T) {
	es := []Entry{
		{Orig: "M1.DNG", Size: 10, Name: "a"},
		{Orig: "M2.DNG", Size: 10, Name: "b"},
		{Orig: "M1.DNG", Size: 11, Name: "c"}, // same name, other size: another file
		{Orig: "m1.dng", Size: 10, Name: "d"},
	}
	got := current(es)
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	if want := []string{"d", "b", "c"}; !equalStrings(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Unpatched entries stay as they were written: no empty patched fields on the line.
func TestManifestOmitsUnpatchedFields(t *testing.T) {
	folder := t.TempDir()
	appendManifest(folder, Entry{Orig: "M1.DNG", Name: "M1.DNG", Size: 1, SHA256: "ab"})
	b, _ := os.ReadFile(filepath.Join(folder, ManifestName))
	for _, k := range []string{"file_sha256", "dates_set", "patched_at"} {
		if bytes.Contains(b, []byte(k)) {
			t.Errorf("unpatched line has %s: %s", k, b)
		}
	}
	at := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	appendManifest(folder, Entry{Orig: "M1.DNG", Name: "M1.DNG", Size: 1, SHA256: "ab", FileSHA256: "cd", DatesSet: "2026-10-04T12:00:00", PatchedAt: at})
	es, err := readManifest(folder)
	if err != nil || len(es) != 2 || es[1].FileSHA256 != "cd" || es[1].DatesSet != "2026-10-04T12:00:00" || !es[1].PatchedAt.Equal(at) {
		t.Fatalf("%+v %v", es, err)
	}
}

func TestSetFileTimes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "M1.DNG")
	os.WriteFile(p, []byte("x"), 0o644)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)
	crErr, err := setFileTimes(p, at)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if !st.ModTime().Equal(at) {
		t.Fatalf("mtime %v, want %v", st.ModTime(), at)
	}
	if runtime.GOOS == "darwin" {
		if crErr != nil {
			t.Logf("creation time not set: %v", crErr)
			return
		}
		if bt := birthtime(t, p); !bt.Equal(at) {
			t.Fatalf("birthtime %v, want %v", bt, at)
		}
	}
}
