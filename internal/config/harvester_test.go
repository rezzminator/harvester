package config

import (
	"encoding/json"
	"github.com/rezzminator/harvester/internal/paths"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHarvesterSchemaValidationAndSecretPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	for _, tc := range []struct {
		body, want string
		mode       os.FileMode
	}{
		{`{"external":{"port":0}}`, "external.port", 0600},
		{`{"external":{"host":"bad host"}}`, "external.host", 0600},
		{`{"external":{"enabled":true}}`, "external.publicURL", 0600},
		{`{"external":{"enabled":true,"publicURL":"https://example.test"}}`, "external.auth", 0600},
		{`{"external":{"publicURL":"https://example.test/path"}}`, "external.publicURL", 0600},
		{`{"search":{"searxngURL":"file:///private"}}`, "search.searxngURL", 0600},
		{`{"cache":{"ttlSeconds":-1}}`, "cache.ttlSeconds", 0600},
		{`{"cache":{"dir":"relative"}}`, "cache.dir", 0600},
		{`{"fetch":{"userAgent":"bad\nagent"}}`, "fetch.userAgent", 0600},
		{`{"convert":{"workers":-1}}`, "convert.workers", 0600},
		{`{"harvest":{"maxResourceBytes":-1}}`, "harvest.maxResourceBytes", 0600},
		{`{"output":{"maxInlineChars":0}}`, "output.maxInlineChars", 0600},
		{`{"output":{"maxResultBytes":0}}`, "output.maxResultBytes", 0600},
		{`{"output":{"maxResultBytes":-5}}`, "output.maxResultBytes must be at least 1, got -5", 0600},
		{`{"scholarly":{"mirrors":{"unknown":"https://example.test"}}}`, "unknown provider", 0600},
		{`{"search":{"braveApiKey":"secret"}}`, "readable by others", 0644},
	} {
		path := filepath.Join(home, "config.json")
		if err := os.WriteFile(path, []byte(tc.body), tc.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRuntime(path); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("body=%s err=%v want=%s", tc.body, err, tc.want)
		}
	}
}
func TestMarshalHarvesterPreservesSchemaAndRedacts(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	config := DefaultHarvester()
	config.Cache.TTL = 0
	config.Convert.Timeout = 3 * time.Second
	config.External.Enabled = true
	config.External.PublicURL = "https://example.test"
	config.External.Passphrase = "private-passphrase"
	config.Search.BraveAPIKey = "private-key"
	data, err := MarshalHarvester(config, true)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["enabled"]; ok || strings.Contains(string(data), "private-") || !strings.Contains(string(data), "redacted") {
		t.Fatalf("redacted config=%s", data)
	}
	var external struct {
		Auth struct {
			Passphrase string `json:"passphrase"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(top["external"], &external); err != nil {
		t.Fatal(err)
	}
	if external.Auth.Passphrase != "<redacted>" {
		t.Fatalf("redacted passphrase=%q", external.Auth.Passphrase)
	}
	data, err = MarshalHarvester(config, false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "roundtrip.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	runtime, err := LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Config.Harvester.External.Passphrase != config.External.Passphrase || runtime.Config.Harvester.Cache.TTL != 0 || runtime.Config.Harvester.Convert.Timeout != config.Convert.Timeout {
		t.Fatalf("roundtrip=%+v", runtime.Config.Harvester)
	}
}

// TestHarvesterOutputMaxResultBytes: output.maxResultBytes defaults to 50000,
// a config written before the key keeps the default beside its own
// maxInlineChars, a set value loads as written from 1 up, and config show
// names both output keys.
func TestHarvesterOutputMaxResultBytes(t *testing.T) {
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	for _, tc := range []struct {
		body              string
		inline, maxResult int
	}{
		{`{}`, 50000, 50000},
		{`{"output":{"maxInlineChars":40000}}`, 40000, 50000},
		{`{"output":{"maxResultBytes":1}}`, 50000, 1},
		{`{"output":{"maxResultBytes":500000}}`, 50000, 500000},
	} {
		path := filepath.Join(home, "config.json")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		runtime, err := LoadRuntime(path)
		if err != nil {
			t.Fatalf("body=%s: %v", tc.body, err)
		}
		if got := runtime.Config.Harvester.Output; got.MaxInlineChars != tc.inline || got.MaxResultBytes != tc.maxResult {
			t.Errorf("body=%s output = %+v, want maxInlineChars %d maxResultBytes %d", tc.body, got, tc.inline, tc.maxResult)
		}
	}
	if got := DefaultHarvester().Output.MaxResultBytes; got != 50000 {
		t.Errorf("default output.maxResultBytes = %d, want 50000", got)
	}
	shown, err := MarshalHarvester(DefaultHarvester(), true)
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Output map[string]int `json:"output"`
	}
	if err := json.Unmarshal(shown, &value); err != nil ||
		value.Output["maxInlineChars"] != 50000 || value.Output["maxResultBytes"] != 50000 {
		t.Errorf("config show output = %v (%v), want both keys at 50000", value.Output, err)
	}
}
