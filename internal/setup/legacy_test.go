package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/engine"
	"github.com/rezzminator/harvester/internal/paths"
)

func TestSetupImportsLegacyConfigBesideRecordedOrExplicitConfig(t *testing.T) {
	for _, locator := range []string{"marker", "override", "default"} {
		t.Run(locator, func(t *testing.T) {
			cfg, old, root := setupFixture(t)
			home := cfg.Paths.Home
			t.Setenv(paths.EnvHome, home)
			t.Setenv("PFM_CONFIG", "")
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			legacy := filepath.Join(home, ".professor")
			if locator != "default" {
				legacy = filepath.Join(home, "old-install")
			}
			putState(t, legacy, "pfm.config.json", `{"ask":{"engine":"claude","codex":{"model":"gpt-legacy","effort":"high"}},"claude":{"binary":"old-claude","theme":"irrelevant"}}`)
			switch locator {
			case "marker":
				putState(t, filepath.Join(home, ".local", "share", "pfm", "install"), "source-repo", legacy+"\n")
			case "override":
				t.Setenv("PFM_CONFIG", filepath.Join(legacy, "pfm.config.json"))
			}
			original := `{"enabled":true,"external":{"enabled":true,"publicURL":"https://gateway.example","auth":{"passphrase":"fixture-secret","staticToken":"fixture-token"}},"search":{"braveApiKey":"fixture-key"},"ask":{"codex":{"model":"gpt-plugin"}}}`
			putState(t, legacy, "harvester.config.json", original)
			putState(t, filepath.Join(legacy, ".harvester-cache"), "read.md", "cached document")
			putState(t, filepath.Join(legacy, ".harvester-state"), "auth.json", "persisted OAuth state")
			ops := fixtureOperations(t, old, root, false)
			ops.service = func(_ context.Context, got config.Runtime) error {
				if !got.Config.Harvester.External.Enabled || got.Config.Harvester.External.Passphrase != "fixture-secret" || got.Config.Harvester.External.StaticToken != "fixture-token" || got.Config.Harvester.Search.BraveAPIKey != "fixture-key" {
					t.Fatalf("setup service did not use imported config: %+v", got.Config.Harvester)
				}
				if got.Config.Claude.Binary != "old-claude" || got.Config.Ask.PrefsFor(engine.Codex).Model != "gpt-plugin" || got.Config.Ask.PrefsFor(engine.Codex).Effort != "high" || got.Config.Ask.Engine != engine.Claude {
					t.Fatalf("legacy ask preferences lost: %+v", got.Config)
				}
				return nil
			}
			if err := run(t.Context(), cfg, true, io.Discard, ops); err != nil {
				t.Fatal(err)
			}
			destination := config.Path(home)
			body, err := os.ReadFile(destination)
			if err != nil {
				t.Fatalf("legacy config was not imported: %v", err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(body, &document); err != nil {
				t.Fatal(err)
			}
			if _, exists := document["enabled"]; exists {
				t.Fatal("retired client enablement carried into plugin config")
			}
			if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("imported config permissions: %v %v", info, err)
			}
			if body, err := os.ReadFile(filepath.Join(legacy, "harvester.config.json")); err != nil || string(body) != original {
				t.Fatalf("legacy credential source changed: %s %v", body, err)
			}
			for path, want := range map[string]string{filepath.Join(paths.HarvesterCacheDir(home), "read.md"): "cached document", filepath.Join(paths.StateRoot(home), ".harvester-state", "auth.json"): "persisted OAuth state"} {
				if body, err := os.ReadFile(path); err != nil || string(body) != want {
					t.Fatalf("state import %s: %s %v", path, body, err)
				}
			}
			current, err := config.LoadRuntime(destination)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := run(t.Context(), current, true, &output, ops); err != nil {
				t.Fatalf("repeat import: %v", err)
			}
			after, err := os.ReadFile(destination)
			if err != nil || !bytes.Equal(after, body) {
				t.Fatalf("repeat import changed config: %v", err)
			}
			if !strings.Contains(output.String(), "legacy config retained") {
				t.Fatalf("retained legacy source not reported: %s", output.String())
			}
		})
	}
}

func TestSetupLegacyImportPreservesExistingAndExplicitLocations(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "import explicit paths", true: "existing config wins"}[existing], func(t *testing.T) {
			cfg, old, root := setupFixture(t)
			home := cfg.Paths.Home
			t.Setenv(paths.EnvHome, home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			legacy := filepath.Join(home, "previous")
			t.Setenv("PFM_CONFIG", filepath.Join(legacy, "pfm.config.json"))
			cache := filepath.Join(home, "operator-cache")
			auth := filepath.Join(home, "operator-auth")
			body, err := json.Marshal(map[string]any{"external": map[string]any{"enabled": true, "publicURL": "https://gateway.example", "stateDir": auth, "auth": map[string]any{"passphrase": "plugin secret"}}, "cache": map[string]any{"dir": cache}})
			if err != nil {
				t.Fatal(err)
			}
			putState(t, legacy, "harvester.config.json", string(body))
			putState(t, filepath.Join(legacy, ".harvester-cache"), "item", "unselected cache")
			putState(t, filepath.Join(legacy, ".harvester-state"), "auth.json", "unselected auth")
			putState(t, cache, "item", "explicit cache")
			putState(t, auth, "auth.json", "explicit auth")
			target := config.Path(home)
			if existing {
				putState(t, filepath.Dir(target), filepath.Base(target), string(body))
				putState(t, legacy, "harvester.config.json", `{"external":{"auth":{"passphrase":"old secret"}}}`)
				cfg, err = config.LoadRuntime(target)
				if err != nil {
					t.Fatal(err)
				}
			}
			ops := fixtureOperations(t, old, root, false)
			ops.service = func(_ context.Context, got config.Runtime) error {
				if got.Config.Harvester.Cache.Dir != cache || got.Config.Harvester.External.StateDir != auth || got.Config.Harvester.External.Passphrase != "plugin secret" {
					t.Fatalf("explicit config changed: %+v", got.Config.Harvester)
				}
				return nil
			}
			if err := run(t.Context(), cfg, true, io.Discard, ops); err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]string{filepath.Join(cache, "item"): "explicit cache", filepath.Join(auth, "auth.json"): "explicit auth", filepath.Join(legacy, ".harvester-cache", "item"): "unselected cache", filepath.Join(legacy, ".harvester-state", "auth.json"): "unselected auth"} {
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Fatalf("explicit/legacy state changed: %s %v", got, err)
				}
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if existing && !bytes.Equal(got, body) {
				t.Fatal("existing config was rewritten")
			}
		})
	}
}

func TestSetupRejectsInvalidLegacyConfigWithoutPublishing(t *testing.T) {
	for _, body := range []string{`null`, `{"enabled":null}`, `{"external":{"port":0}}`, `{"ask":null}`} {
		t.Run(body, func(t *testing.T) {
			cfg, old, root := setupFixture(t)
			home := cfg.Paths.Home
			t.Setenv(paths.EnvHome, home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			legacy := filepath.Join(home, ".professor")
			putState(t, legacy, "harvester.config.json", body)
			// Force object validation when ask was explicitly null.
			putState(t, legacy, "pfm.config.json", `{"ask":{"engine":"codex"}}`)
			if err := run(t.Context(), cfg, true, io.Discard, fixtureOperations(t, old, root, false)); err == nil {
				t.Fatal("invalid legacy config accepted")
			}
			if _, err := os.Stat(config.Path(home)); !os.IsNotExist(err) {
				t.Fatalf("invalid config published: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(legacy, "harvester.config.json")); err != nil || string(got) != body {
				t.Fatalf("invalid credential source changed: %s %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(old, "env", "interpreter")); err != nil {
				t.Fatalf("legacy environment lost before config validation: %v", err)
			}
		})
	}
}

func TestSetupLegacyLocationRejectsMalformedOverridesAndMarkers(t *testing.T) {
	for _, value := range []string{"relative/path", "/absolute\n/another"} {
		t.Run(value, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("PFM_CONFIG", "")
			putState(t, filepath.Join(home, ".local", "share", "pfm", "install"), "source-repo", value)
			if _, err := legacyConfigRoot(home); err == nil {
				t.Fatal("malformed legacy source marker accepted")
			}
		})
	}
	t.Setenv("PFM_CONFIG", "relative/config.json")
	if _, err := legacyConfigRoot(t.TempDir()); err == nil {
		t.Fatal("relative legacy config override accepted")
	}
}
