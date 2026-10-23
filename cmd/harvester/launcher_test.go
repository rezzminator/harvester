package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/testjail"
)

func TestLauncherVerifiesDownloadsAndPublishesStableBinary(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			home := t.TempDir()
			plugin := filepath.Join(home, "plugin")
			bin := filepath.Join(home, "tools")
			for _, dir := range []string{filepath.Join(plugin, "libexec"), filepath.Join(plugin, ".claude-plugin"), bin} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			launcher, err := os.ReadFile("../../plugins/harvester/libexec/harvester")
			if err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(plugin, "libexec", "harvester")
			if err := testjail.WriteExecutable(entry, launcher, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(plugin, ".claude-plugin", "plugin.json"), []byte("{\n  \"version\": \"0.1.0-dev.1\"\n}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			body := []byte("#!/bin/sh\nprintf 'fixture %s\\n' \"$*\"\n")
			download := filepath.Join(home, "download")
			if err := os.WriteFile(download, body, 0600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			if !valid {
				sum = sha256.Sum256([]byte("different"))
			}
			sums := fmt.Sprintf("%x  harvester_0.1.0-dev.1_%s_%s\n", sum, runtime.GOOS, runtime.GOARCH)
			if err := os.WriteFile(filepath.Join(plugin, "libexec", "SHA256SUMS"), []byte(sums), 0600); err != nil {
				t.Fatal(err)
			}
			fakeCurl := filepath.Join(bin, "curl")
			if err := testjail.WriteExecutable(fakeCurl, []byte("#!/bin/sh\nfor last do :; done\ncp \"$DOWNLOAD_FIXTURE\" \"$last\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			install := filepath.Join(home, "installed")
			command := exec.Command("sh", entry, "version")
			command.Env = append(os.Environ(), "HARVESTER_HOME="+install, "DOWNLOAD_FIXTURE="+download, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := command.CombinedOutput()
			binary := filepath.Join(install, "bin", "0.1.0-dev.1", "harvester")
			if !valid {
				if err == nil || !strings.Contains(string(output), "checksum mismatch") {
					t.Fatalf("bad checksum: err=%v output=%s", err, output)
				}
				if _, err := os.Stat(binary); !os.IsNotExist(err) {
					t.Fatalf("unverified binary published: %v", err)
				}
				return
			}
			if err != nil || string(output) != "fixture version\n" {
				t.Fatalf("install: err=%v output=%s", err, output)
			}
			if target, err := os.Readlink(filepath.Join(install, "bin", "harvester")); err != nil || target != "0.1.0-dev.1/harvester" {
				t.Fatalf("stable link=%q err=%v", target, err)
			}
			if err := os.Remove(fakeCurl); err != nil {
				t.Fatal(err)
			}
			command = exec.Command("sh", entry, "cached")
			command.Env = append(os.Environ(), "HARVESTER_HOME="+install)
			output, err = command.CombinedOutput()
			if err != nil || string(output) != "fixture cached\n" {
				t.Fatalf("cached: err=%v output=%s", err, output)
			}
		})
	}
}

func TestReleaseCheckRejectsVersionDrift(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"scripts", "plugins/harvester/.claude-plugin", "plugins/harvester/.codex-plugin", "plugins/harvester/libexec"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	check, err := os.ReadFile("../../scripts/release-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "release-check.sh")
	if err := os.WriteFile(script, check, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins/harvester/.claude-plugin/plugin.json"), []byte(`{"version":"0.1.0-dev.1","mcpServers":{"harvester":{"type":"stdio","command":"${CLAUDE_PLUGIN_ROOT}/libexec/harvester","args":["mcp"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	sums := ""
	for _, system := range []string{"darwin", "linux"} {
		for _, arch := range []string{"amd64", "arm64"} {
			sums += strings.Repeat("a", 64) + "  harvester_0.1.0-dev.1_" + system + "_" + arch + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(root, "plugins/harvester/libexec/SHA256SUMS"), []byte(sums), 0600); err != nil {
		t.Fatal(err)
	}
	metadata := []string{"plugins/harvester/.codex-plugin/plugin.json", "plugins/harvester/package.json"}
	for _, path := range metadata {
		if err := os.WriteFile(filepath.Join(root, path), []byte(`{"version":"0.1.0-dev.1"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	versionPath := filepath.Join(root, "VERSION")
	if err := os.WriteFile(versionPath, []byte("0.1.0-dev.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("matching metadata err=%v output=%s", err, output)
	}
	for _, path := range metadata {
		if err := os.WriteFile(filepath.Join(root, path), []byte(`{"version":"0.1.1"}`), 0600); err != nil {
			t.Fatal(err)
		}
		output, err = exec.Command("bash", script).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "VERSION and "+path+" disagree") {
			t.Fatalf("engine version drift %s err=%v output=%s", path, err, output)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(`{"version":"0.1.0-dev.1"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(versionPath, []byte("0.1.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err = exec.Command("bash", script).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "VERSION and plugin.json disagree") {
		t.Fatalf("drift err=%v output=%s", err, output)
	}
}

func TestNativePluginRegistersHarvester(t *testing.T) {
	body, err := os.ReadFile("../../plugins/harvester/.claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	server, ok := manifest.MCPServers["harvester"]
	if !ok || server.Command != "${CLAUDE_PLUGIN_ROOT}/libexec/harvester" || len(server.Args) != 1 || server.Args[0] != "mcp" {
		t.Fatalf("native plugin must register the standalone harvester MCP door: %#v", manifest.MCPServers)
	}
}

func TestCodexPluginRegistersTheSharedLauncher(t *testing.T) {
	body, err := os.ReadFile("../../plugins/harvester/.codex-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		MCPServers string `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.MCPServers != "./mcp.json" {
		t.Fatalf("Codex MCP declaration = %q", manifest.MCPServers)
	}
	body, err = os.ReadFile("../../plugins/harvester/mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
			Cwd     string
			EnvVars []string `json:"env_vars"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	server, ok := config.MCPServers["harvester"]
	if server.Cwd != "./" || !strings.Contains(strings.Join(server.EnvVars, " "), "HARVESTER_CONFIG") || !strings.Contains(strings.Join(server.EnvVars, " "), "XDG_STATE_HOME") {
		t.Fatalf("Codex launcher must run from its plugin root and retain operator overrides: %#v", server)
	}
	if !ok || server.Command != "./libexec/harvester" || len(server.Args) != 1 || server.Args[0] != "mcp" {
		t.Fatalf("Codex native plugin must register the shared launcher: %#v", config.MCPServers)
	}
}
