package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/engine"
	"github.com/rezzminator/harvester/internal/paths"
)

func TestLoadRuntimeStandalonePathsAndAsk(t *testing.T) {
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv("HARVESTER_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "settings"))
	runtime, err := LoadRuntime("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "settings", "harvester", HarvesterFileName)
	if runtime.Config.Path != want || runtime.Config.Harvester.Exists || runtime.Paths.Home != home {
		t.Fatalf("runtime=%+v", runtime)
	}
	path := filepath.Join(home, "override.json")
	t.Setenv("HARVESTER_CONFIG", path)
	if err := os.WriteFile(path, []byte(`{"ask":{"engine":"codex","codex":{"model":"gpt-fixture","effort":"high"}},"claude":{"binary":"custom-claude"},"codex":{"binary":"custom-codex"},"cache":{"dir":"~/cache"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	runtime, err = LoadRuntime("")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := runtime.Config.DefaultEngine()
	if err != nil || selected != engine.Codex {
		t.Fatalf("engine=%s err=%v", selected, err)
	}
	prefs := runtime.Config.Ask.PrefsFor(selected)
	if prefs.Model != "gpt-fixture" || prefs.Effort != "high" || runtime.Config.Claude.Binary != "custom-claude" || runtime.Config.Codex.Binary != "custom-codex" || runtime.Config.Harvester.Cache.Dir != filepath.Join(home, "cache") {
		t.Fatalf("config=%+v", runtime.Config)
	}
}
func TestLoadRuntimeRefusesInvalidAskAndJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	for _, body := range []string{`{"enabled":true}`, `{"ask":{"engine":"unknown"}}`, `{"ask":{"engine":"opencode"}}`, `{"ask":{"codex":{"unknown":true}}}`, `{"ask":{"codex":{"model":" "}}}`, `{"claude":{"binary":" "}}`, `{} {}`, `{"cache":`} {
		path := filepath.Join(home, "invalid.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRuntime(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("body=%s err=%v", body, err)
		}
	}
}
