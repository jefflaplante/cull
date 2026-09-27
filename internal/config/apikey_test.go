package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOpenAIKeyIsOptional(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	key, source, warnings, err := LoadOpenAIKey("")
	if err != nil || key != "" || source != "none" || len(warnings) != 0 {
		t.Fatalf("absent key: key=%q source=%q warnings=%v err=%v", key, source, warnings, err)
	}

	t.Setenv("OPENAI_API_KEY", "sk-local")
	if key, source, _, err = LoadOpenAIKey(""); err != nil || key != "sk-local" || source != "env:OPENAI_API_KEY" {
		t.Fatalf("env key: key=%q source=%q err=%v", key, source, err)
	}

	p := filepath.Join(t.TempDir(), "key")
	os.WriteFile(p, []byte("sk-openai-xyz\n"), 0o600)
	key, source, warnings, err = LoadOpenAIKey(p)
	if err != nil || key != "sk-openai-xyz" || source != "file:"+p || len(warnings) != 0 {
		t.Fatalf("file key: key=%q source=%q warnings=%v err=%v", key, source, warnings, err)
	}
}

func TestLoadAPIKeyStillWarnsOnNonAnthropicKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	os.WriteFile(p, []byte("sk-openai-xyz"), 0o600)
	_, _, warnings, err := LoadAPIKey(p)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "sk-ant-") {
		t.Fatalf("warnings=%v err=%v", warnings, err)
	}
}
