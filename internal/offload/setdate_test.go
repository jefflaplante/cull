package offload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dng"
	"github.com/jefflaplante/cull/internal/dng/dngtest"
	"github.com/jefflaplante/cull/internal/ui"
)

// setTarget is the corrected capture time the --set-date tests fix every copy to.
var setTarget = time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)

const setTargetISO = "2026-10-04T12:00:00"

// stopped is a dead clock's capture time: every fixture frame carries it.
const stopped = "2025:12:28 00:05:59"

// dngCard writes fixture DNGs under root/DCIM/100LEICA with the card mtime t0 and
// returns each file's bytes by name.
func dngCard(t *testing.T, root string, files map[string]dngtest.Fixture) map[string][]byte {
	t.Helper()
	dir := filepath.Join(root, "DCIM", "100LEICA")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{}
	for name, fx := range files {
		b := dngtest.Build(t, fx)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, t0, t0)
		data[name] = b
	}
	return data
}

// dated is a fixture with every kind of capture date (TIFF, EXIF, sub-seconds,
// embedded XMP) and a payload of n random bytes.
func dated(seed int64, n int) dngtest.Fixture {
	payload := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(payload)
	return dngtest.Fixture{
		DateTime: stopped, DTO: stopped, DTD: stopped, SubSecOrig: "42",
		XMP:     `<x:xmpmeta><rdf:Description xmp:CreateDate="2025-12-28T00:05:59" xmp:ModifyDate="2025-12-28T00:05:59.00+01:00"/></x:xmpmeta>`,
		Payload: payload,
	}
}

// signed is a Content Credentials frame (an M11-P L… file): never byte-patched.
func signed(seed int64) dngtest.Fixture {
	fx := dated(seed, 2000)
	fx.C2PA = []byte("jumbf c2pa manifest stand-in")
	return fx
}

// threeDNGs is a card with two patchable frames (one spanning several 4 MiB chunks)
// and one Content Credentials frame.
func threeDNGs(t *testing.T) (src string, data map[string][]byte) {
	src = t.TempDir()
	data = dngCard(t, src, map[string]dngtest.Fixture{
		"M1.DNG": dated(1, 9<<20),
		"M2.DNG": dated(2, 3000),
		"L3.DNG": signed(3),
	})
	return src, data
}

func setDateOpts(t *testing.T, src string) Options {
	o := opts(t, src)
	o.Backup = t.TempDir()
	o.SetDate, o.SetDateSet = setTarget, true
	return o
}

// applyAll is b with exactly the patches PatchDates computes for setTarget.
func applyAll(t *testing.T, b []byte) []byte {
	t.Helper()
	ps, _, err := dng.PatchDates(bytes.NewReader(b), int64(len(b)), setTarget)
	if err != nil {
		t.Fatal(err)
	}
	return dngtest.Apply(b, ps)
}

// notes collects every note a run emits.
type notes struct {
	mu   sync.Mutex
	list []string
}

func (n *notes) Emit(e ui.Event) {
	if e.Note != nil {
		n.mu.Lock()
		n.list = append(n.list, e.Note.Text)
		n.mu.Unlock()
	}
}
func (n *notes) Close() error { return nil }

func (n *notes) with(s ...string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, l := range n.list {
		ok := true
		for _, x := range s {
			ok = ok && strings.Contains(l, x)
		}
		if ok {
			out = append(out, l)
		}
	}
	return out
}

func runPlan(t *testing.T, o Options, sink ui.Sink) (*Plan, *Result) {
	t.Helper()
	p, err := MakePlan(o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), p, sink)
	if err != nil {
		t.Fatal(err)
	}
	return p, res
}

func TestSetDatePatchesCopiesNotCard(t *testing.T) {
	for _, serial := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipeline", true: "serial"}[serial], func(t *testing.T) {
			src, data := threeDNGs(t)
			before := treeHash(t, src)
			o := setDateOpts(t, src)
			p, err := MakePlan(o)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(p.Folder, "2026-10-04 ") || p.Dated != "--set-date" {
				t.Fatalf("folder %q dated from %q", p.Folder, p.Dated)
			}
			p.h.serial = serial
			var n notes
			res, err := Run(context.Background(), p, &n)
			if err != nil || !res.Safe || res.Copied != 3 || len(res.Failed) != 0 {
				t.Fatalf("err %v result %+v", err, res)
			}
			if treeHash(t, src) != before {
				t.Fatal("the card changed")
			}
			for _, d := range p.Dests {
				man, _ := readManifest(d)
				if len(man) != 3 {
					t.Fatalf("%s manifest has %d lines", d, len(man))
				}
				for _, e := range current(man) {
					card := data[e.Name]
					got, err := os.ReadFile(filepath.Join(d, e.Name))
					if err != nil {
						t.Fatal(err)
					}
					want := applyAll(t, card)
					if e.Name == "L3.DNG" {
						want = card // Content Credentials: never byte-patched
					} else if bytes.Equal(want, card) {
						t.Fatalf("%s: the fixture needs no patch", e.Name)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("%s in %s isn't the card's bytes with exactly its date patches", e.Name, d)
					}
					st, _ := os.Stat(filepath.Join(d, e.Name))
					if !st.ModTime().Equal(setTarget) {
						t.Errorf("%s mtime %v, want %v", e.Name, st.ModTime(), setTarget)
					}
					if bt := birthtime(t, filepath.Join(d, e.Name)); !bt.IsZero() && !bt.Equal(setTarget) {
						t.Errorf("%s creation time %v, want %v", e.Name, bt, setTarget)
					}
					cs, fs := sha256.Sum256(card), sha256.Sum256(got)
					if e.SHA256 != hexOf(cs[:]) {
						t.Errorf("%s: sha256 isn't the card's hash", e.Name)
					}
					if e.DatesSet != setTargetISO {
						t.Errorf("%s: dates_set %q", e.Name, e.DatesSet)
					}
					switch e.Name {
					case "L3.DNG":
						if e.FileSHA256 != "" || !e.PatchedAt.IsZero() {
							t.Errorf("L3.DNG equals the card, but records file_sha256 %q patched_at %v", e.FileSHA256, e.PatchedAt)
						}
					default:
						if e.FileSHA256 != hexOf(fs[:]) || e.PatchedAt.IsZero() {
							t.Errorf("%s: file_sha256 %q (copy %x), patched_at %v", e.Name, e.FileSHA256, fs[:4], e.PatchedAt)
						}
					}
				}
				v, err := Verify(context.Background(), d, nil)
				if err != nil || v.OK != 3 || v.Bad != 0 || len(v.Unrecorded) != 0 {
					t.Fatalf("verify %s: %+v %v", d, v, err)
				}
				noTemps(t, d)
			}
			if c := n.with("L3.DNG", "Content Credentials"); len(c) != 1 {
				t.Errorf("Content Credentials notes %q (all: %q)", c, n.list)
			}
		})
	}
}

// Review Focus 1: a re-run on the same card after --set-date copies nothing and is
// safe to format, never "exists with different content", with camera names, with
// --rename and with --checksum.
func TestSetDateRerunSkips(t *testing.T) {
	modes := []struct {
		name     string
		rename   string
		checksum bool
	}{
		{"camera names", "", false},
		{"camera names --checksum", "", true},
		{"rename", "{date}_{n:4}", false},
		{"rename --checksum", "{date}_{n:4}", true},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			src, _ := threeDNGs(t)
			o := setDateOpts(t, src)
			o.Rename = m.rename
			if _, res := runPlan(t, o, nil); !res.Safe || res.Copied != 3 {
				t.Fatalf("first run %+v", res)
			}
			o.Checksum = m.checksum
			p, err := MakePlan(o)
			if err != nil {
				t.Fatalf("re-plan: %v", err)
			}
			if got := names(p, false); len(got) != 0 {
				t.Fatalf("re-run would copy %v", got)
			}
			res, err := Run(context.Background(), p, nil)
			if err != nil || !res.Safe || res.Copied != 0 || res.Skipped != 3 || res.Unverified != 0 {
				t.Fatalf("re-run: err %v result %+v", err, res)
			}
			// Without --set-date the copies are still the ones the manifest records.
			o.SetDateSet = false
			o.Date = "2026-10-04"
			p, err = MakePlan(o)
			if err != nil || len(names(p, false)) != 0 {
				t.Fatalf("re-plan without --set-date: %v, copying %v", err, names(p, false))
			}
		})
	}
}

// The manifest stands in for the destination's changed bytes and mtime only for the
// card file it records: a different file under the same name is still refused, and
// --checksum still catches a damaged patched copy.
func TestSetDateRerunStillRefusesDifferentFiles(t *testing.T) {
	src, data := threeDNGs(t)
	o := setDateOpts(t, src)
	o.Backup = ""
	runPlan(t, o, nil)

	t.Run("another frame under the same name", func(t *testing.T) {
		for _, c := range []struct {
			n     int
			mtime time.Time
		}{{3001, t0}, {3000, t0.Add(time.Hour)}} { // another size; the same size, another mtime
			other := t.TempDir()
			dngCard(t, other, map[string]dngtest.Fixture{"M2.DNG": dated(99, c.n)})
			p := filepath.Join(other, "DCIM", "100LEICA", "M2.DNG")
			os.Chtimes(p, c.mtime, c.mtime)
			o2 := o
			o2.Sources = []string{other}
			if _, err := MakePlan(o2); err == nil || !strings.Contains(err.Error(), "different content") {
				t.Fatalf("a different M2.DNG (payload %d, mtime %v) wasn't refused: %v", c.n, c.mtime, err)
			}
		}
	})
	t.Run("--checksum on a damaged copy", func(t *testing.T) {
		p, _ := MakePlan(o)
		copyPath := filepath.Join(p.Dests[0], "M2.DNG")
		b := applyAll(t, data["M2.DNG"])
		b[len(b)-1] ^= 1
		os.WriteFile(copyPath, b, 0o644)
		os.Chtimes(copyPath, setTarget, setTarget)
		o2 := o
		o2.Checksum = true
		if _, err := MakePlan(o2); err == nil || !strings.Contains(err.Error(), "different content") {
			t.Fatalf("damaged patched copy passed --checksum: %v", err)
		}
	})
}

// A proof failure (the temp damaged after its patches were written) fails that
// attempt: the file is retried from the card, ends correct, and is never named
// before it is proven.
func TestSetDateProofFailureFailsFile(t *testing.T) {
	for _, serial := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipeline", true: "serial"}[serial], func(t *testing.T) {
			src, data := threeDNGs(t)
			o := setDateOpts(t, src)
			p, err := MakePlan(o)
			if err != nil {
				t.Fatal(err)
			}
			p.h.serial = serial
			var mu sync.Mutex
			patched := map[string]int{}
			p.h.afterPatch = func(tmp string) {
				name := strings.SplitN(strings.TrimPrefix(filepath.Base(tmp), "."), ".cull-", 2)[0]
				mu.Lock()
				patched[name]++
				first := patched[name] == 1
				mu.Unlock()
				if _, err := os.Lstat(filepath.Join(filepath.Dir(tmp), name)); err == nil {
					t.Errorf("%s named before its proof", name)
				}
				if name == "M1.DNG" && first {
					corrupt(t, tmp)
				}
			}
			var n notes
			res, err := Run(context.Background(), p, &n)
			if err != nil || !res.Safe || res.Copied != 3 || len(res.Failed) != 0 {
				t.Fatalf("err %v result %+v", err, res)
			}
			mu.Lock()
			if patched["M1.DNG"] < 3 { // 2 destinations, then the retry's 2
				t.Errorf("M1.DNG patched %d times: not retried", patched["M1.DNG"])
			}
			mu.Unlock()
			if len(n.with("M1.DNG", "retrying")) != 1 {
				t.Errorf("retry notes %q", n.list)
			}
			for _, d := range p.Dests {
				got, _ := os.ReadFile(filepath.Join(d, "M1.DNG"))
				if !bytes.Equal(got, applyAll(t, data["M1.DNG"])) {
					t.Fatalf("M1.DNG in %s is wrong after the retry", d)
				}
				if v, err := Verify(context.Background(), d, nil); err != nil || v.Bad != 0 || v.OK != 3 {
					t.Fatalf("verify %s: %+v %v", d, v, err)
				}
				noTemps(t, d)
			}
		})
	}
}

// A proof failure on every attempt fails the file, names nothing, and isn't safe.
func TestSetDateProofAlwaysFailing(t *testing.T) {
	src, _ := threeDNGs(t)
	p, err := MakePlan(setDateOpts(t, src))
	if err != nil {
		t.Fatal(err)
	}
	p.h.afterPatch = func(tmp string) {
		if strings.HasPrefix(filepath.Base(tmp), ".M2.DNG.") {
			corrupt(t, tmp)
		}
	}
	res, err := Run(context.Background(), p, nil)
	if err != nil || res.Safe || res.Copied != 2 || len(res.Failed) != 1 || !strings.Contains(res.Failed[0], "M2.DNG") {
		t.Fatalf("err %v result %+v", err, res)
	}
	for _, d := range p.Dests {
		if _, err := os.Lstat(filepath.Join(d, "M2.DNG")); err == nil {
			t.Fatalf("unproven M2.DNG named in %s", d)
		}
		man, _ := readManifest(d)
		for _, e := range man {
			if e.Name == "M2.DNG" {
				t.Fatal("unproven M2.DNG recorded")
			}
		}
		noTemps(t, d)
	}
}

func TestSetDateSkippedXMPNoted(t *testing.T) {
	src := t.TempDir()
	fx := dated(1, 2000)
	fx.XMP = `<x:xmpmeta><rdf:Description xmp:CreateDate="yesterday" xmp:ModifyDate="2025-12-28T00:05:59"/></x:xmpmeta>`
	data := dngCard(t, src, map[string]dngtest.Fixture{"M1.DNG": fx, "M2.DNG": dated(2, 2000)})
	var n notes
	p, res := runPlan(t, setDateOpts(t, src), &n)
	if !res.Safe || res.Copied != 2 {
		t.Fatalf("result %+v", res)
	}
	if got := n.with("M1.DNG", "xmp:CreateDate"); len(got) != 1 {
		t.Fatalf("notes naming M1.DNG's xmp:CreateDate: %q (all %q)", got, n.list)
	}
	if got := n.with("M2.DNG", "xmp:"); len(got) != 0 {
		t.Fatalf("M2.DNG noted: %q", got)
	}
	for _, d := range p.Dests {
		got, _ := os.ReadFile(filepath.Join(d, "M1.DNG"))
		if !bytes.Equal(got, applyAll(t, data["M1.DNG"])) || !bytes.Contains(got, []byte(`xmp:CreateDate="yesterday"`)) {
			t.Fatalf("M1.DNG in %s: the unparseable value must be left alone, the rest patched", d)
		}
	}
}

// A file whose dates can't be read at all (not a TIFF) is copied exactly as the card
// holds it, with its card mtime, verified as without --set-date, and named in a note.
func TestSetDateUnreadableFileCopiedAsIs(t *testing.T) {
	src := t.TempDir()
	card(t, src, map[string]spec{"DCIM/M9.DNG": {seed: 9, size: 4000}})
	dngCard(t, src, map[string]dngtest.Fixture{"M1.DNG": dated(1, 2000)})
	var n notes
	o := setDateOpts(t, src)
	p, res := runPlan(t, o, &n)
	if !res.Safe || res.Copied != 2 {
		t.Fatalf("result %+v", res)
	}
	if got := n.with("M9.DNG", "dates"); len(got) != 1 {
		t.Fatalf("notes %q", n.list)
	}
	for _, d := range p.Dests {
		got, _ := os.ReadFile(filepath.Join(d, "M9.DNG"))
		if !bytes.Equal(got, content(spec{seed: 9, size: 4000})) {
			t.Fatal("M9.DNG isn't the card's bytes")
		}
		if st, _ := os.Stat(filepath.Join(d, "M9.DNG")); !st.ModTime().Equal(t0) {
			t.Errorf("M9.DNG mtime %v, want the card's", st.ModTime())
		}
		for _, e := range current(mustManifest(t, d)) {
			if e.Name == "M9.DNG" && (e.DatesSet != "" || e.FileSHA256 != "") {
				t.Errorf("M9.DNG recorded as dated: %+v", e)
			}
		}
	}
	if _, res := runPlan(t, o, nil); !res.Safe || res.Copied != 0 {
		t.Fatalf("re-run %+v", res)
	}
}

func mustManifest(t *testing.T, d string) []Entry {
	t.Helper()
	m, err := readManifest(d)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A filesystem that keeps no creation time gives one note per destination per run;
// the copies still count.
func TestSetDateCreationTimeFailureNotedOnce(t *testing.T) {
	src, _ := threeDNGs(t)
	old := setCreationTimeFn
	setCreationTimeFn = func(string, time.Time) error { return errors.New("not supported") }
	t.Cleanup(func() { setCreationTimeFn = old })
	var n notes
	p, res := runPlan(t, setDateOpts(t, src), &n)
	if !res.Safe || res.Copied != 3 {
		t.Fatalf("result %+v", res)
	}
	for _, d := range p.Dests {
		if got := n.with("creation time", d); len(got) != 1 {
			t.Errorf("creation-time notes for %s: %q (all %q)", d, got, n.list)
		}
	}
}

func TestSetDateConflictsWithDate(t *testing.T) {
	src, _ := threeDNGs(t)
	o := setDateOpts(t, src)
	o.Date = "2026-10-03"
	if _, err := MakePlan(o); err == nil || !strings.Contains(err.Error(), "--set-date") {
		t.Fatalf("a different --date accepted: %v", err)
	}
	o.Date = "2026-10-04"
	if p, err := MakePlan(o); err != nil || !strings.HasPrefix(p.Folder, "2026-10-04") {
		t.Fatalf("the same --date refused: %v", err)
	}
}

// Without --set-date nothing changes: copies equal the card, keep its mtime, and the
// manifest records no patch fields.
func TestNoSetDateLeavesCopiesAsCard(t *testing.T) {
	src, data := threeDNGs(t)
	o := setDateOpts(t, src)
	o.SetDateSet = false
	p, res := runPlan(t, o, nil)
	if !res.Safe || res.Copied != 3 {
		t.Fatalf("result %+v", res)
	}
	for _, e := range mustManifest(t, p.Dests[0]) {
		got, _ := os.ReadFile(filepath.Join(p.Dests[0], e.Name))
		st, _ := os.Stat(filepath.Join(p.Dests[0], e.Name))
		if !bytes.Equal(got, data[e.Name]) || !st.ModTime().Equal(t0) || e.DatesSet != "" || e.FileSHA256 != "" {
			t.Fatalf("%s changed without --set-date: %+v", e.Name, e)
		}
	}
}

func TestDatesSetReadsCurrentEntries(t *testing.T) {
	d := t.TempDir()
	for _, e := range []Entry{
		{Orig: "M1.DNG", Name: "M1.DNG", Size: 10, DatesSet: "2026-10-01T12:00:00"},
		{Orig: "M2.DNG", Name: "M2.DNG", Size: 10},
		{Orig: "M1.DNG", Name: "A_0001.DNG", Size: 10, DatesSet: "2026-10-04T12:00:00"}, // supersedes
	} {
		if err := appendManifest(d, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DatesSet(d)
	if err != nil || len(got) != 1 || got["A_0001.DNG"] != "2026-10-04T12:00:00" {
		t.Fatalf("DatesSet %v %v", got, err)
	}
	if got, err := DatesSet(t.TempDir()); err != nil || len(got) != 0 {
		t.Fatalf("no manifest: %v %v", got, err)
	}
}

// A crash after a dated copy is named but before its manifest line: a re-run doesn't
// refuse it as different. Without --checksum it is alike but unverified (not safe to
// format); --checksum proves it against the card with the date's patches applied.
func TestSetDateRerunAfterLostManifest(t *testing.T) {
	src, _ := threeDNGs(t)
	o := setDateOpts(t, src)
	o.Backup = ""
	p, _ := runPlan(t, o, nil)
	os.Remove(filepath.Join(p.Dests[0], ManifestName))

	p2, err := MakePlan(o)
	if err != nil {
		t.Fatalf("re-plan without the manifest: %v", err)
	}
	res, err := Run(context.Background(), p2, nil)
	if err != nil || res.Safe || res.Copied != 0 || res.Unverified != 3 {
		t.Fatalf("without --checksum: err %v result %+v", err, res)
	}
	o.Checksum = true
	p3, err := MakePlan(o)
	if err != nil {
		t.Fatalf("re-plan --checksum: %v", err)
	}
	if res, err := Run(context.Background(), p3, nil); err != nil || !res.Safe || res.Copied != 0 {
		t.Fatalf("--checksum: err %v result %+v", err, res)
	}
	// A damaged copy is still refused.
	b, _ := os.ReadFile(filepath.Join(p.Dests[0], "M2.DNG"))
	b[len(b)-1] ^= 1
	os.WriteFile(filepath.Join(p.Dests[0], "M2.DNG"), b, 0o644)
	if _, err := MakePlan(o); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("damaged unrecorded copy passed --checksum: %v", err)
	}
}
