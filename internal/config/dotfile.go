package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Setting is one line of the dotfile: a flag's default, for every command that has
// the flag (Section "") or for one command.
type Setting struct {
	Section string // a command name, or "" for every command
	Key     string // the flag's name, without dashes
	Value   string
	Line    int
}

// Settings is the user's dotfile of flag defaults.
type Settings struct {
	Path     string
	Settings []Setting // in file order; a repeatable flag may appear more than once
}

// SettingsPath is the dotfile: $CULL_CONFIG when set, else ~/.cull.
func SettingsPath() string {
	if p := os.Getenv("CULL_CONFIG"); p != "" {
		return expandHome(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cull")
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// LoadSettings reads the dotfile. Its format is a line per flag, "name = value" (a
// leading "--" and "=" without spaces are fine too), '#' comments, and optional
// "[command]" sections whose settings apply to that command only. A missing file is
// no settings; a malformed line is an error naming the file and line.
func LoadSettings() (*Settings, error) {
	s := &Settings{Path: SettingsPath()}
	if s.Path == "" {
		return s, nil
	}
	f, err := os.Open(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	section := ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		bad := func(why string) error { return fmt.Errorf("%s:%d: %s: %q", s.Path, n, why, line) }
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "["):
			if !strings.HasSuffix(line, "]") || strings.TrimSpace(line[1:len(line)-1]) == "" {
				return nil, bad("a section is [command]")
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimLeft(strings.TrimSpace(key), "-")
		if !ok || key == "" {
			return nil, bad("want name = value")
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		s.Settings = append(s.Settings, Setting{Section: section, Key: key, Value: value, Line: n})
	}
	return s, sc.Err()
}
