package pipeline

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/cull/internal/report"
)

func TestTagPrecedence(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.Tags = &report.Tags{Project: "A", Location: "L"}
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	c.Resume, c.Tags = true, nil
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil || rep.Tags == nil || rep.Tags.Project != "A" || rep.Tags.Location != "L" {
		t.Fatalf("resume without tags: %v %+v", err, rep.Tags)
	}
	c.Tags = &report.Tags{Project: "B"}
	rep, _, err = Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil || rep.Tags.Project != "B" || rep.Tags.Location != "L" {
		t.Fatalf("resume with --project: %v %+v", err, rep.Tags)
	}
}

// Tags given at offload (stored by its scan) survive the judge that replaces the scan
// report.
func TestTagsSurviveJudgeAfterScan(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.DryRun, c.Tags = true, &report.Tags{Event: "Ceremony"}
	if _, _, err := Run(context.Background(), c, nil); err != nil {
		t.Fatal(err)
	}
	c.DryRun, c.Tags = false, nil
	rep, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"})
	if err != nil || rep.Tags == nil || rep.Tags.Event != "Ceremony" {
		t.Fatalf("judge after scan: %v %+v", err, rep.Tags)
	}
	loaded, _ := report.Load(c.ReportPath)
	if loaded.Tags == nil || loaded.Tags.Event != "Ceremony" {
		t.Fatalf("saved report lost the tags: %+v", loaded.Tags)
	}
}

// judge and decide write the same sidecar for a frame whose decision didn't change:
// tags and content keywords included.
func TestSidecarWritersAgree(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.XMPDevelop = false
	c.Tags = &report.Tags{Project: "Smith & Jones <2026>", Event: "Ceremony"}
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "L1000001.xmp")
	byJudge, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<rdf:li>Smith &amp; Jones &lt;2026&gt;</rdf:li>", "<rdf:li>portrait</rdf:li>", "<rdf:li>content|forest</rdf:li>", "<rdf:li>event|Ceremony</rdf:li>"} {
		if !strings.Contains(string(byJudge), want) {
			t.Fatalf("judge's sidecar lacks %q:\n%s", want, byJudge)
		}
	}
	if _, err := Decide(context.Background(), c.ReportPath, DecideOptions{Policy: c.Policy, WriteXMP: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	byDecide, _ := os.ReadFile(sidecar)
	if string(byDecide) != string(byJudge) {
		t.Fatalf("decide wrote a different sidecar:\njudge:\n%s\ndecide:\n%s", byJudge, byDecide)
	}
}

// cull tag changes the stored tags; the next decide --write-xmp carries them.
func TestTagThenDecideRewrites(t *testing.T) {
	dir := fourFiles(t)
	c := cfg(dir)
	c.Tags = &report.Tags{Project: "Old"}
	if _, _, err := Run(context.Background(), c, &fakeBackend{status: "sharp"}); err != nil {
		t.Fatal(err)
	}
	rep, _ := report.Load(c.ReportPath)
	rep.Tags = &report.Tags{Project: "New"}
	rep.Save(c.ReportPath)
	if _, err := Decide(context.Background(), c.ReportPath, DecideOptions{Policy: c.Policy, WriteXMP: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "L1000001.xmp"))
	if !strings.Contains(string(b), "project|New") || strings.Contains(string(b), "Old") {
		t.Fatalf("sidecar not rewritten with the new tags:\n%s", b)
	}
}
