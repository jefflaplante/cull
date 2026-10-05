package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeDotfile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cull")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CULL_CONFIG", p)
	return p
}

func TestLoadSettingsParsesSectionsAndComments(t *testing.T) {
	writeDotfile(t, `# my defaults
keep-best = 3
--outranked=cull
backend = "claude-code"

[review]
sort = true
[judge]
keyword = leica
keyword = m11-p
`)
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := []Setting{
		{Key: "keep-best", Value: "3", Line: 2},
		{Key: "outranked", Value: "cull", Line: 3},
		{Key: "backend", Value: "claude-code", Line: 4},
		{Section: "review", Key: "sort", Value: "true", Line: 7},
		{Section: "judge", Key: "keyword", Value: "leica", Line: 9},
		{Section: "judge", Key: "keyword", Value: "m11-p", Line: 10},
	}
	if !reflect.DeepEqual(s.Settings, want) {
		t.Fatalf("got %+v\nwant %+v", s.Settings, want)
	}
}

// A missing dotfile is no settings, not an error.
func TestLoadSettingsMissingFile(t *testing.T) {
	t.Setenv("CULL_CONFIG", filepath.Join(t.TempDir(), "nope"))
	s, err := LoadSettings()
	if err != nil || len(s.Settings) != 0 {
		t.Fatalf("%+v %v", s, err)
	}
}

// A malformed line is an error naming the file and line: a typo must not be silently
// ignored.
func TestLoadSettingsRejectsMalformedLines(t *testing.T) {
	for _, body := range []string{"keep-best 3\n", "[review\n", " = 3\n", "[]\n"} {
		p := writeDotfile(t, "# ok\n"+body)
		_, err := LoadSettings()
		if err == nil || !strings.Contains(err.Error(), p+":2") {
			t.Errorf("%q: %v", body, err)
		}
	}
}

// Without CULL_CONFIG the dotfile is ~/.cull.
func TestSettingsPathDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CULL_CONFIG", "")
	if p := SettingsPath(); p != filepath.Join(home, ".cull") {
		t.Fatalf("path %s", p)
	}
}
