package setup

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/paths"
	"github.com/rezzminator/harvester/internal/testjail"
)

func TestLinuxServiceInstallRefreshRemoveAndFailures(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cfg := config.Runtime{Paths: paths.Values{Home: home}, Config: config.Config{Path: filepath.Join(home, "custom config.json"), Harvester: config.DefaultHarvester()}}
	cfg.Config.Harvester.External.Enabled = true
	var runner deps.FakeRunner
	runner.Script([]string{}, deps.RunResult{}, nil)
	if err := configureService(t.Context(), cfg, &runner, "linux"); err != nil {
		t.Fatal(err)
	}
	path, _, _ := serviceContent(cfg, "linux")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{binary, " serve", "HARVESTER_CONFIG=", "custom config.json", "Restart=on-failure"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	var unchanged deps.FakeRunner
	unchanged.Script([]string{}, deps.RunResult{}, nil)
	if err := configureService(t.Context(), cfg, &unchanged, "linux"); err != nil {
		t.Fatal(err)
	}
	cfg.Config.Path = filepath.Join(home, "other.json")
	var refresh deps.FakeRunner
	refresh.Script([]string{}, deps.RunResult{}, nil)
	if err := configureService(t.Context(), cfg, &refresh, "linux"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range refresh.Calls() {
		if strings.Contains(strings.Join(call.Argv, " "), "restart") {
			found = true
		}
	}
	if !found {
		t.Fatal("changed service not refreshed")
	}
	cfg.Config.Harvester.External.Enabled = false
	var failed deps.FakeRunner
	failed.Script([]string{"systemctl", "--user", "disable"}, deps.RunResult{ExitCode: 1, Stderr: []byte("no bus")}, errors.New("exit 1"))
	if err := configureService(t.Context(), cfg, &failed, "linux"); err == nil || !strings.Contains(err.Error(), "no bus") {
		t.Fatalf("service manager failure=%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unit deleted after stop failure: %v", err)
	}
	var remove deps.FakeRunner
	remove.Script([]string{}, deps.RunResult{}, nil)
	if err := configureService(t.Context(), cfg, &remove, "linux"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled unit remains: %v", err)
	}
}

func TestDarwinServiceEscapesPathsAndBootstraps(t *testing.T) {
	home := filepath.Join(t.TempDir(), "space & quote\"")
	cfg := config.Runtime{Paths: paths.Values{Home: home}, Config: config.Config{Path: filepath.Join(home, "operator.json"), Harvester: config.DefaultHarvester()}}
	cfg.Config.Harvester.External.Enabled = true
	_, body, err := serviceContent(cfg, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		_, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var runner deps.FakeRunner
	runner.Script([]string{}, deps.RunResult{}, nil)

	if err := configureService(t.Context(), cfg, &runner, "darwin"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range runner.Calls() {
		if len(call.Argv) > 1 && call.Argv[1] == "bootstrap" {
			found = true
		}
	}
	if !found {
		t.Fatal("LaunchAgent was not bootstrapped")
	}
}

func TestServiceQuotesSystemdSpecifiers(t *testing.T) {
	home := filepath.Join(t.TempDir(), "100% done")
	cfg := config.Runtime{Paths: paths.Values{Home: home}, Config: config.Config{Path: filepath.Join(home, "config.json")}}
	_, body, err := serviceContent(cfg, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "100%% done") {
		t.Fatalf("systemd specifier expanded: %s", body)
	}
	cfg.Config.Path = "bad\nconfig"
	if _, _, err := serviceContent(cfg, "linux"); err == nil {
		t.Fatal("newline accepted in service environment")
	}
}

func TestServiceRetainsStateOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "custom state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "uv cache"))
	t.Setenv("HARVESTER_PYTHON_ROOT", filepath.Join(home, "custom python"))
	cfg := config.Runtime{Paths: paths.Values{Home: home}}
	for _, goos := range []string{"linux", "darwin"} {
		_, body, err := serviceContent(cfg, goos)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"XDG_STATE_HOME", "XDG_CACHE_HOME", "HARVESTER_PYTHON_ROOT"} {
			if !strings.Contains(string(body), name) || !strings.Contains(string(body), os.Getenv(name)) {
				t.Fatalf("%s unit lost %s: %s", goos, name, body)
			}
		}
	}
}

func TestServiceRefreshResumesAfterFailedRestart(t *testing.T) {
	home := t.TempDir()
	cfg := config.Runtime{Paths: paths.Values{Home: home}, Config: config.Config{Harvester: config.DefaultHarvester()}}
	cfg.Config.Harvester.External.Enabled = true
	var initial deps.FakeRunner
	initial.Script([]string{}, deps.RunResult{}, nil)
	if err := configureService(t.Context(), cfg, &initial, "linux"); err != nil {
		t.Fatal(err)
	}
	cfg.Config.Harvester.External.Port++
	var failed deps.FakeRunner
	failed.Script([]string{}, deps.RunResult{}, nil)
	failed.Script([]string{"systemctl", "--user", "restart"}, deps.RunResult{ExitCode: 1}, errors.New("restart failed"))
	if err := configureService(t.Context(), cfg, &failed, "linux"); err == nil {
		t.Fatal("restart failure hidden")
	}
	var retry deps.FakeRunner
	retry.Script([]string{}, deps.RunResult{}, nil)
	if err := configureService(t.Context(), cfg, &retry, "linux"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range retry.Calls() {
		if strings.Contains(strings.Join(call.Argv, " "), "restart") {
			found = true
		}
	}
	if !found {
		t.Fatal("configuration change was not retried after failed restart")
	}
}

func TestLaunchLoadedDistinguishesAbsenceFromProbeFailure(t *testing.T) {
	const target = "gui/501/com.harvester.gateway"
	for _, tc := range []struct {
		name           string
		code           int
		stderr         string
		loaded, failed bool
	}{
		{name: "loaded", loaded: true},
		{name: "service absent", code: 113, stderr: `Bad request.
Could not find service "com.harvester.gateway" in domain for user gui: 501`},
		{name: "denied", code: 13, stderr: "Permission denied", failed: true},
		{name: "manager unavailable", code: 1, stderr: "Could not connect to service manager", failed: true},
		{name: "domain unavailable", code: 113, stderr: "Could not find domain for user gui: 501", failed: true},
		{name: "different service", code: 113, stderr: `Could not find service "other.gateway" in domain for user gui: 501`, failed: true},
		{name: "unspecified failure", code: 113, failed: true},
		{name: "executable unavailable", code: -1, stderr: "executable not found", failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runner deps.FakeRunner
			var commandErr error
			if tc.code != 0 {
				commandErr = errors.New("launchctl failed")
			}
			runner.Script([]string{"launchctl", "print", target}, deps.RunResult{ExitCode: tc.code, Stderr: []byte(tc.stderr)}, commandErr)
			loaded, err := launchLoaded(t.Context(), &runner, target)
			if loaded != tc.loaded || (err != nil) != tc.failed {
				t.Fatalf("loaded=%t error=%v; want loaded=%t failed=%t", loaded, err, tc.loaded, tc.failed)
			}
			if tc.failed && !errors.Is(err, commandErr) {
				t.Fatalf("command error lost: %v", err)
			}
		})
	}
}

func TestDarwinDisableRetainsUnitWhenProbeFails(t *testing.T) {
	home := t.TempDir()
	cfg := config.Runtime{Paths: paths.Values{Home: home}, Config: config.Config{Harvester: config.DefaultHarvester()}}
	cfg.Config.Harvester.External.Enabled = false
	path, body, err := serviceContent(cfg, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var runner deps.FakeRunner
	runner.Script([]string{"launchctl", "print"}, deps.RunResult{ExitCode: 13, Stderr: []byte("Permission denied")}, errors.New("exit status 13"))
	if err := configureService(t.Context(), cfg, &runner, "darwin"); err == nil {
		t.Fatal("probe permission failure hidden as absence")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unit removed after failed probe: %v", err)
	}
}

func TestDarwinServiceDoesNotRestartSuccessfulStop(t *testing.T) {
	cfg := config.Runtime{Paths: paths.Values{Home: t.TempDir()}}
	_, body, err := serviceContent(cfg, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	nextElement := func() xml.StartElement {
		for {
			token, err := decoder.Token()
			if err != nil {
				t.Fatalf("decode KeepAlive policy: %v", err)
			}
			if element, ok := token.(xml.StartElement); ok {
				return element
			}
		}
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("KeepAlive missing: %v", err)
		}
		if text, ok := token.(xml.CharData); ok && string(text) == "KeepAlive" {
			break
		}
	}
	if element := nextElement(); element.Name.Local != "dict" {
		t.Fatalf("KeepAlive=%s, want failure-only dict so clean stop remains stopped", element.Name.Local)
	}
	if element := nextElement(); element.Name.Local != "key" {
		t.Fatalf("KeepAlive child=%s", element.Name.Local)
	}
	var key string
	if err := decoder.DecodeElement(&key, &xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		t.Fatal(err)
	}
	if key != "SuccessfulExit" {
		t.Fatalf("KeepAlive condition=%q", key)
	}
	if element := nextElement(); element.Name.Local != "false" {
		t.Fatalf("SuccessfulExit=%s, want false", element.Name.Local)
	}
}

func TestGatewayServiceUsesTheInvokedExecutable(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HARVESTER_HOME", filepath.Join(home, "custom plugin storage"))
	cfg := config.Runtime{Paths: paths.Values{Home: home}, Config: config.Config{Harvester: config.DefaultHarvester()}}
	for _, goos := range []string{"linux", "darwin"} {
		_, body, err := serviceContent(cfg, goos)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), binary) {
			t.Fatalf("%s service must launch the invoked executable %s, not assume a separate install: %s", goos, binary, body)
		}
	}
}

func TestGatewayServiceUsesPluginStableLink(t *testing.T) {
	if stable := os.Getenv("HARVESTER_SERVICE_BINARY_HELPER"); stable != "" {
		cfg := config.Runtime{Paths: paths.Values{Home: t.TempDir()}, Config: config.Config{Harvester: config.DefaultHarvester()}}
		for _, goos := range []string{"linux", "darwin"} {
			_, body, err := serviceContent(cfg, goos)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), stable) {
				t.Fatalf("%s service must follow the native plugin stable link %s: %s", goos, stable, body)
			}
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(t.TempDir(), "custom plugin storage")
	versioned := filepath.Join(plugin, "bin", "v1", "harvester")
	if err := os.MkdirAll(filepath.Dir(versioned), 0700); err != nil {
		t.Fatal(err)
	}
	if err := testjail.WriteExecutable(versioned, body, 0700); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(plugin, "bin", "harvester")
	if err := os.Symlink(filepath.Join("v1", "harvester"), stable); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(versioned, "-test.run=^TestGatewayServiceUsesPluginStableLink$", "-test.v")
	command.Env = append(os.Environ(), "HARVESTER_SERVICE_BINARY_HELPER="+stable)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native plugin service: %v\n%s", err, output)
	}
	upgraded := filepath.Join(plugin, "bin", "v2", "harvester")
	if err := os.MkdirAll(filepath.Dir(upgraded), 0700); err != nil {
		t.Fatal(err)
	}
	if err := testjail.WriteExecutable(upgraded, body, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stable); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("v2", "harvester"), stable); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(stable, "-test.run=^TestGatewayServiceUsesPluginStableLink$", "-test.v")
	command.Env = append(os.Environ(), "HARVESTER_SERVICE_BINARY_HELPER="+stable)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("upgraded native service: %v\n%s", err, output)
	}
}
