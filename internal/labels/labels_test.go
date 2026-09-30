package labels

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendReadFoldsLastWins(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	for _, e := range []Entry{
		{File: "A.DNG", Label: "keep"},
		{File: "B.DNG", Label: "cull", Stars: 2},
		{File: "A.DNG", Label: "review", Stars: 4},
		{File: "B.DNG"}, // cleared
	} {
		e.At = time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
		if err := Append(p, e); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m["A.DNG"].Label != "review" || m["A.DNG"].Stars != 4 {
		t.Fatalf("fold: %+v", m)
	}
	b, _ := os.ReadFile(p)
	if n := strings.Count(string(b), "\n"); n != 4 {
		t.Fatalf("log must keep history: %d lines\n%s", n, b)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	m, err := Read(filepath.Join(t.TempDir(), FileName))
	if err != nil || len(m) != 0 {
		t.Fatalf("%v %v", m, err)
	}
}

func TestReadSkipsTornFinalLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(p, []byte(`{"file":"A.DNG","label":"keep","stars":0,"at":"2026-09-27T20:00:00Z"}`+"\n"+`{"file":"B.DNG","la`), 0o644)
	m, err := Read(p)
	if err != nil || len(m) != 1 || m["A.DNG"].Label != "keep" {
		t.Fatalf("%v %v", m, err)
	}
}

func TestReadRejectsBadLinesNamingThem(t *testing.T) {
	ok := `{"file":"A.DNG","label":"keep","stars":0,"at":"2026-09-27T20:00:00Z"}`
	for name, body := range map[string]string{
		"not json":          ok + "\nnot json\n" + ok + "\n",
		"bad label":         ok + "\n" + `{"file":"B.DNG","label":"maybe","stars":0}` + "\n",
		"bad stars":         ok + "\n" + `{"file":"B.DNG","label":"","stars":6}` + "\n",
		"bad final with \n": ok + "\n" + `{"file":` + "\n",
	} {
		p := filepath.Join(t.TempDir(), FileName)
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := Read(p); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAppendValidates(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	for _, e := range []Entry{
		{File: "A.DNG", Label: "maybe"}, {File: "A.DNG", Stars: 6}, {File: "A.DNG", Stars: -1},
		{File: "sub/A.DNG"}, {File: ".."}, {File: ""},
	} {
		if err := Append(p, e); err == nil {
			t.Errorf("accepted %+v", e)
		}
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("rejected entries created the log")
	}
}

func TestConcurrentAppendsStayWhole(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Append(p, Entry{File: fmt.Sprintf("L%d.DNG", i%7), Label: "keep", Stars: i % 6, At: time.Now()}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("%d lines", len(lines))
	}
	for i, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("line %d torn: %q", i+1, l)
		}
	}
}

func TestDefaultPathAndVerdicts(t *testing.T) {
	if FileName != "cull-labels.jsonl" {
		t.Fatalf("log name %q", FileName)
	}
	if got := DefaultPath("/shoot/cull-report.json"); got != "/shoot/"+FileName {
		t.Fatal(got)
	}
	v := Verdicts(map[string]Entry{"A.DNG": {Label: "cull"}, "B.DNG": {Stars: 3}})
	if len(v) != 1 || v["A.DNG"] != "cull" {
		t.Fatalf("stars-only entries are not verdicts: %v", v)
	}
}

func TestAppendAfterTornLineKeepsLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	if err := Append(p, Entry{File: "A.DNG", Label: "keep"}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"file":"B.DNG","lab`) // a crash mid-append
	f.Close()
	if err := Append(p, Entry{File: "C.DNG", Label: "cull"}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatalf("log unreadable after a torn line: %v", err)
	}
	if got["A.DNG"].Label != "keep" || got["C.DNG"].Label != "cull" {
		t.Fatalf("got %+v", got)
	}
}

// A log that is nothing but one torn line: the next append leaves just itself.
func TestAppendAfterOnlyATornLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(p, []byte(`{"file":"B.DNG","lab`), 0o644)
	if err := Append(p, Entry{File: "C.DNG", Label: "cull"}); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(p); err != nil || got["C.DNG"].Label != "cull" || len(got) != 1 {
		t.Fatalf("got %+v err %v", got, err)
	}
}
