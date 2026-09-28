package review

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jefflaplante/gophotocull/internal/eval"
	"github.com/jefflaplante/gophotocull/internal/labels"
	"github.com/jefflaplante/gophotocull/internal/report"
)

const testAddr = "127.0.0.1:4567"

// serveFixture: L1 (model cull) and L2 (model keep), a saved report, a built sheet.
func serveFixture(t *testing.T, writeXMP bool) (dir string, h http.Handler, token string) {
	t.Helper()
	dir = t.TempDir()
	f1, f2 := filepath.Join(dir, "L1.DNG"), filepath.Join(dir, "L2.DNG")
	tinyDNG(t, f1)
	tinyDNG(t, f2)
	pv := &report.PreviewInfo{Width: 1600, Height: 1067, Orientation: 1, Source: "tiff-ifd"}
	ev := &eval.Evaluation{Sharpness: eval.Sharpness{Score: 2, Status: "missed_focus"}}
	rep := &report.Report{Dir: dir, Results: []report.Result{
		{File: f1, Preview: pv, Evaluation: ev, Decision: eval.Cull},
		{File: f2, Preview: pv, Evaluation: ev, Decision: eval.Keep},
	}}
	rp := filepath.Join(dir, "gophotocull-report.json")
	if err := rep.Save(rp); err != nil {
		t.Fatal(err)
	}
	sheet, err := Build(rep, rp, Options{Out: filepath.Join(dir, "gophotocull-review"), Concurrency: 1}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	token = strings.Repeat("ab", 16)
	s, err := NewServer(sheet, rep, ServeOptions{ReportPath: rp, LabelsPath: labels.DefaultPath(rp), WriteXMP: writeXMP, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return dir, s.Handler(testAddr), token
}

func call(h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = testAddr
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func api(token string) map[string]string {
	return map[string]string{"X-Gophotocull-Token": token, "Content-Type": "application/json"}
}

func dngHashes(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	m := map[string][32]byte{}
	files, _ := filepath.Glob(filepath.Join(dir, "*.DNG"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		m[f] = sha256.Sum256(b)
	}
	return m
}

func TestServerAccessControl(t *testing.T) {
	_, h, tok := serveFixture(t, false)
	for _, c := range []struct {
		name, method, target string
		hdr                  map[string]string
		want                 int // 0: anything but 200
	}{
		{"page", "GET", "/", nil, 200},
		{"foreign host", "GET", "/", map[string]string{"Host": "evil.example:4567"}, 403},
		{"rebinding host", "GET", "/api/labels", map[string]string{"Host": "attacker.test:4567", "X-Gophotocull-Token": tok}, 403},
		{"no token", "GET", "/api/labels", nil, 403},
		{"wrong token", "GET", "/api/labels", map[string]string{"X-Gophotocull-Token": strings.Repeat("0", 32)}, 403},
		{"foreign origin", "POST", "/api/labels", map[string]string{"X-Gophotocull-Token": tok, "Origin": "http://evil.example"}, 403},
		{"localhost", "GET", "/api/labels", map[string]string{"Host": "localhost:4567", "Origin": "http://localhost:4567", "X-Gophotocull-Token": tok}, 200},
		{"image", "GET", "/L1.thumb.jpg", nil, 200},
		{"escape", "GET", "/..%2Fgophotocull-report.json", nil, 0},
		{"not an image", "GET", "/index.json", nil, 404},
	} {
		rec := call(h, c.method, c.target, "{}", c.hdr)
		if (c.want == 0 && rec.Code == 200) || (c.want != 0 && rec.Code != c.want) {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
	}
	if rec := call(h, "GET", "/", "", nil); !strings.Contains(rec.Body.String(), `"serve":true`) {
		t.Error("served page not in serve mode")
	}
}

func TestServerSavesLabelsAndSidecars(t *testing.T) {
	dir, h, tok := serveFixture(t, true)
	before := dngHashes(t, dir)
	rec := call(h, "POST", "/api/labels", `{"file":"L1.DNG","label":"keep","stars":4}`, api(tok))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sidecar":"written"`) {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	sc, _ := os.ReadFile(filepath.Join(dir, "L1.xmp"))
	for _, want := range []string{`xmp:Rating="4"`, `xmp:Label="Green"`, "gophotocull:keep", "gophotocull:labeled"} {
		if !strings.Contains(string(sc), want) {
			t.Errorf("sidecar lacks %s:\n%s", want, sc)
		}
	}
	rep, _ := report.Load(filepath.Join(dir, "gophotocull-report.json"))
	if r := rep.Results[0]; r.XMP != filepath.Join(dir, "L1.xmp") || r.Decision != eval.Cull {
		t.Fatalf("report: xmp=%q decision=%s (must record ownership, keep the model's decision)", r.XMP, r.Decision)
	}
	if rec := call(h, "GET", "/api/labels", "", api(tok)); !strings.Contains(rec.Body.String(), `"L1.DNG":{"label":"keep","stars":4}`) {
		t.Fatalf("get: %s", rec.Body)
	}
	call(h, "POST", "/api/labels", `{"file":"L1.DNG","label":"","stars":0}`, api(tok)) // clear
	sc, _ = os.ReadFile(filepath.Join(dir, "L1.xmp"))
	if s := string(sc); strings.Contains(s, "xmp:Rating") || !strings.Contains(s, `xmp:Label="Red"`) || strings.Contains(s, "labeled") {
		t.Fatalf("cleared frame's sidecar should be the model's cull again:\n%s", s)
	}
	if rec := call(h, "GET", "/api/labels", "", api(tok)); strings.Contains(rec.Body.String(), "L1.DNG") {
		t.Fatalf("cleared frame still labeled: %s", rec.Body)
	}
	b, _ := os.ReadFile(filepath.Join(dir, labels.FileName))
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Fatalf("log lines %d", n)
	}
	if !reflect.DeepEqual(before, dngHashes(t, dir)) {
		t.Fatal("a DNG changed")
	}
}

func TestServerSkipsForeignSidecar(t *testing.T) {
	dir, h, tok := serveFixture(t, true)
	p := filepath.Join(dir, "L2.xmp")
	os.WriteFile(p, []byte("foreign"), 0o644) // appears after the server started
	for i := 0; i < 2; i++ {
		rec := call(h, "POST", "/api/labels", `{"file":"L2.DNG","label":"cull","stars":0}`, api(tok))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "skipped: sidecar exists and isn't gophotocull's") {
			t.Fatalf("post %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if b, _ := os.ReadFile(p); string(b) != "foreign" {
		t.Fatal("foreign sidecar overwritten")
	}
	if m, _ := labels.Read(filepath.Join(dir, labels.FileName)); m["L2.DNG"].Label != "cull" {
		t.Fatalf("label not saved: %+v", m)
	}
}

func TestServerRejectsBadInput(t *testing.T) {
	dir, h, tok := serveFixture(t, true)
	for _, body := range []string{
		`{"file":"../gophotocull-report.json","label":"keep","stars":0}`,
		`{"file":"X.DNG","label":"keep","stars":0}`,
		`{"file":"L1.DNG","label":"maybe","stars":0}`,
		`{"file":"L1.DNG","label":"","stars":6}`,
		`not json`,
	} {
		if rec := call(h, "POST", "/api/labels", body, api(tok)); rec.Code != 400 {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, labels.FileName)); err == nil {
		t.Fatal("a rejected request wrote the log")
	}
}

func TestServerWithoutWriteXMP(t *testing.T) {
	dir, h, tok := serveFixture(t, false)
	if rec := call(h, "POST", "/api/labels", `{"file":"L1.DNG","label":"keep","stars":1}`, api(tok)); !strings.Contains(rec.Body.String(), `"sidecar":"off"`) {
		t.Fatalf("%s", rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "L1.xmp")); err == nil {
		t.Fatal("sidecar written without --write-xmp")
	}
}

func TestServerConcurrentPosts(t *testing.T) {
	dir, h, tok := serveFixture(t, true)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			call(h, "POST", "/api/labels", fmt.Sprintf(`{"file":"L%d.DNG","label":"keep","stars":%d}`, 1+i%2, i%6), api(tok))
		}(i)
	}
	wg.Wait()
	b, _ := os.ReadFile(filepath.Join(dir, labels.FileName))
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 20 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("torn line %q", l)
		}
	}
}

func TestNewServerRefusesDuplicateNames(t *testing.T) {
	rep := &report.Report{Results: []report.Result{{File: "/a/L1.DNG"}, {File: "/b/L1.DNG"}}}
	_, err := NewServer(&Sheet{}, rep, ServeOptions{Token: strings.Repeat("ab", 16)})
	if err == nil || !strings.Contains(err.Error(), "/a/L1.DNG") || !strings.Contains(err.Error(), "/b/L1.DNG") {
		t.Fatalf("%v", err)
	}
}
