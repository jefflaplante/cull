package develop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// line is one line of `lightcraft-cli run` output: one per command, in order.
type line struct {
	Command string          `json:"command"`
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
	Ms      float64         `json:"ms"`
}

// errText is the line's error as text (LightCraft sends a string; anything else is
// shown as JSON).
func (l line) errText() string {
	var s string
	if json.Unmarshal(l.Error, &s) == nil {
		return s
	}
	return string(l.Error)
}

// lightcraft runs `bin run --library lib --keep-going --script <name>.jsonl` in dir,
// headless, and calls each with every output line as it arrives (i counts commands),
// so a frame's export is recorded the moment it lands. The script, its output and
// stderr stay in dir as <name>.jsonl, <name>.out and <name>.err. It returns how many
// lines came, and the process's error with stderr's tail; with --keep-going a failed
// command makes the exit status non-zero, so the caller judges by the lines. On ctx's
// cancellation LightCraft gets SIGINT, then SIGKILL after a grace period.
func lightcraft(ctx context.Context, bin, lib, dir, name string, cmds []Command, each func(i int, l line)) (int, error) {
	var script bytes.Buffer
	enc := json.NewEncoder(&script)
	for _, c := range cmds {
		if err := enc.Encode(c); err != nil {
			return 0, err
		}
	}
	scriptPath := filepath.Join(dir, name+".jsonl")
	if err := os.WriteFile(scriptPath, script.Bytes(), 0o644); err != nil {
		return 0, err
	}
	outLog, err := os.Create(filepath.Join(dir, name+".out"))
	if err != nil {
		return 0, err
	}
	defer outLog.Close()
	errLog, err := os.Create(filepath.Join(dir, name+".err"))
	if err != nil {
		return 0, err
	}
	defer errLog.Close()
	var tail tailBuffer
	cmd := exec.CommandContext(ctx, bin, "run", "--library", lib, "--keep-going", "--script", scriptPath)
	cmd.Dir = dir
	cmd.Stderr = io.MultiWriter(errLog, &tail)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s: %w", bin, err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 256<<20) // a catalog.query of a big chunk is one long line
	n := 0
	for sc.Scan() {
		b := sc.Bytes()
		outLog.Write(append(b, '\n'))
		var l line
		if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) || json.Unmarshal(b, &l) != nil || l.Command == "" {
			continue // not a command's result line
		}
		each(n, l)
		n++
	}
	err = cmd.Wait()
	if err != nil {
		if t := strings.TrimSpace(tail.String()); t != "" {
			err = fmt.Errorf("%w: %s", err, t)
		}
	}
	return n, err
}

// tailBuffer keeps the last 2 KB written: enough of stderr for an error message.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// version is `bin --version`: a check that LightCraft runs at all before a long
// develop starts, and what the state records.
func version(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w: %s", bin, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
