package pipeline

import (
	"context"
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
