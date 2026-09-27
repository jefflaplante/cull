// Package config resolves runtime configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeyFileCandidates are checked in order (relative to $HOME) when neither
// --api-key-file nor ANTHROPIC_API_KEY is set.
var KeyFileCandidates = []string{
	".anthropic/api_key",
	".config/anthropic/api_key",
	".anthropic_api_key",
	".anthropic",
}

// LoadAPIKey resolves the key: explicit file > ANTHROPIC_API_KEY > candidate files.
// source describes where it came from; the key itself is never logged.
func LoadAPIKey(explicitFile string) (key, source string, warnings []string, err error) {
	if explicitFile != "" {
		return readKeyFile(expand(explicitFile))
	}
	if k := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); k != "" {
		return k, "env:ANTHROPIC_API_KEY", nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", nil, err
	}
	for _, rel := range KeyFileCandidates {
		p := filepath.Join(home, rel)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return readKeyFile(p)
		}
	}
	return "", "", nil, errors.New("no API key: set ANTHROPIC_API_KEY, pass --api-key-file, or create ~/" + KeyFileCandidates[0])
}

func readKeyFile(p string) (string, string, []string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", "", nil, err
	}
	var warnings []string
	if st.Mode().Perm()&0o077 != 0 {
		warnings = append(warnings, fmt.Sprintf("%s is readable by group/others (mode %04o); chmod 600 it", p, st.Mode().Perm()))
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", nil, err
	}
	k := strings.TrimSpace(string(b))
	// Tolerate a single `[export ]KEY=value` line.
	if i := strings.LastIndex(k, "="); i >= 0 && !strings.Contains(k, "\n") {
		k = strings.Trim(strings.TrimSpace(k[i+1:]), `"'`)
	}
	if k == "" {
		return "", "", nil, fmt.Errorf("%s is empty", p)
	}
	if !strings.HasPrefix(k, "sk-ant-") {
		warnings = append(warnings, p+": contents don't start with sk-ant-; verify it's an Anthropic API key")
	}
	return k, "file:" + p, warnings, nil
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}
