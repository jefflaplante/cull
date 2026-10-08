// Package lctest is a fake headless LightCraft (lightcraft-cli) for tests: a test
// binary whose TestMain calls Main when Env is set stands in for lightcraft-cli, so
// cull develop runs end to end without LightCraft or a raw.
//
//   - FAKE_LC_LOG: every invocation's script is appended there, one JSON array per run.
//   - FAKE_LC_CRASH_AFTER=n: the process dies (exit 3, no summary) right after its n-th export.
//   - FAKE_LC_TAG: written into every JPEG, so a test can tell exports apart.
//   - A photo whose name contains BADIMPORT fails to import; BADEXPORT fails app.export;
//     SLOW sleeps 30 s in app.export (for cancellation).
package lctest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Env set to "1" makes a test binary act as lightcraft-cli.
const Env = "CULL_FAKE_LIGHTCRAFT"

type command struct {
	Command string         `json:"command"`
	Params  map[string]any `json:"params,omitempty"`
}

type photo struct {
	ID       int    `json:"id"`
	Path     string `json:"path"`
	FileName string `json:"fileName"`
}

// Main is the fake lightcraft-cli: args as the real one gets them, the exit status as
// it returns. It understands --version and run --library DIR [--keep-going] --script FILE.
func Main(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("lightcraft-cli 0.4.0-fake")
		return 0
	}
	var lib, script string
	keepGoing := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "run":
		case "--library":
			i++
			lib = args[i]
		case "--script":
			i++
			script = args[i]
		case "--keep-going":
			keepGoing = true
		default:
			fmt.Fprintf(os.Stderr, "fake lightcraft: unexpected argument %q\n", args[i])
			return 2
		}
	}
	if lib == "" || script == "" {
		fmt.Fprintln(os.Stderr, "fake lightcraft: want run --library DIR --script FILE")
		return 2
	}
	os.MkdirAll(lib, 0o755)
	var photos []photo
	if b, err := os.ReadFile(filepath.Join(lib, "fake.json")); err == nil {
		json.Unmarshal(b, &photos)
	}
	var cmds []command
	f, err := os.Open(script)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var c command
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			fmt.Fprintln(os.Stderr, "fake lightcraft: bad script line:", err)
			return 2
		}
		cmds = append(cmds, c)
	}
	f.Close()
	if p := os.Getenv("FAKE_LC_LOG"); p != "" {
		b, _ := json.Marshal(cmds)
		lf, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		lf.Write(append(b, '\n'))
		lf.Close()
	}
	crashAfter, _ := strconv.Atoi(os.Getenv("FAKE_LC_CRASH_AFTER"))
	byID := func(id int) *photo {
		for i := range photos {
			if photos[i].ID == id {
				return &photos[i]
			}
		}
		return nil
	}
	firstID := func(p map[string]any) int {
		ids, _ := p["ids"].([]any)
		if len(ids) == 0 {
			return 0
		}
		v, _ := ids[0].(float64)
		return int(v)
	}
	failed, exports := 0, 0
	for _, c := range cmds {
		var res any
		var cerr string
		switch c.Command {
		case "library.xmpPreferences":
			res = map[string]any{"autoWrite": false, "naming": "stem"}
		case "library.import":
			var imported []int
			var bad [][2]string
			for _, p := range c.Params["paths"].([]any) {
				path := p.(string)
				if strings.Contains(path, "BADIMPORT") {
					bad = append(bad, [2]string{path, "unrecognized file format"})
					continue
				}
				id := len(photos) + 1
				photos = append(photos, photo{ID: id, Path: path, FileName: filepath.Base(path)})
				imported = append(imported, id)
			}
			res = map[string]any{"imported": imported, "failed": bad, "duplicates": []any{}}
		case "catalog.query":
			res = map[string]any{"photos": photos, "total": len(photos)}
		case "preset.import":
			for _, p := range c.Params["paths"].([]any) {
				if _, err := os.Stat(p.(string)); err != nil {
					cerr = err.Error()
				}
			}
			res = map[string]any{"imported": []any{}}
		case "library.select", "develop.wb", "develop.auto", "crop.autoStraighten", "preset.apply", "develop.set":
		case "app.export":
			ph := byID(firstID(c.Params))
			out, _ := c.Params["path"].(string)
			switch {
			case ph == nil:
				cerr = "no such photo"
			case strings.Contains(ph.FileName, "BADEXPORT"):
				cerr = "render failed: synthetic"
			default:
				if strings.Contains(ph.FileName, "SLOW") {
					time.Sleep(30 * time.Second)
				}
				// A tiny "JPEG" that says what it was made from.
				body := append([]byte{0xFF, 0xD8}, []byte(ph.FileName+" "+os.Getenv("FAKE_LC_TAG"))...)
				body = append(body, 0xFF, 0xD9)
				if err := os.WriteFile(out, body, 0o644); err != nil {
					cerr = err.Error()
					break
				}
				res = map[string]any{"bytes": len(body), "path": out, "files": []any{map[string]any{"path": out, "bytes": len(body)}}}
				exports++
			}
		default:
			cerr = "unknown command " + c.Command
		}
		line := map[string]any{"command": c.Command, "ok": cerr == "", "ms": 5.0}
		if cerr == "" {
			line["result"] = res
		} else {
			line["error"] = cerr
			failed++
		}
		b, _ := json.Marshal(line)
		fmt.Println(string(b))
		if crashAfter > 0 && exports == crashAfter && c.Command == "app.export" && cerr == "" {
			os.Exit(3)
		}
		if cerr != "" && !keepGoing {
			break
		}
	}
	b, _ := json.Marshal(photos)
	os.WriteFile(filepath.Join(lib, "fake.json"), b, 0o644)
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "lightcraft-cli: %d command(s) failed\n", failed)
		return 1
	}
	return 0
}
