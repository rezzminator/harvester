package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/daemon"
	"github.com/rezzminator/harvester/internal/harvestmcp"
	"github.com/rezzminator/harvester/internal/paths"
	"github.com/rezzminator/harvester/internal/testjail"
)

func TestMain(m *testing.M) {
	if os.Getenv("HARVESTER_TEST_MAIN") == "1" {
		main()
	}
	os.Exit(testjail.Run(m))
}

func TestHarvestRuntimeCarriesConfiguredScholarlyProviders(t *testing.T) {
	cfg := config.Config{Harvester: config.DefaultHarvester()}
	cfg.Harvester.Cache.Dir = t.TempDir()
	cfg.Harvester.Scholarly = config.HarvesterScholarly{DOIMirrorURL: "https://mirror.example/doi", IPFSCatalogURL: "https://ipfs.example", DOIViewerURL: "https://viewer.example", MD5CatalogURL: "https://catalog.example", GoogleScholarURL: "https://scholar.example", ContactEmail: "ops@example.com"}
	got := harvestmcp.RuntimeFromConfig("", cfg.Harvester)
	for key, pair := range map[string][2]string{"DOIMirrorURL": {got.DOIMirrorURL, cfg.Harvester.Scholarly.DOIMirrorURL}, "IPFSCatalogURL": {got.IPFSCatalogURL, cfg.Harvester.Scholarly.IPFSCatalogURL}, "DOIViewerURL": {got.DOIViewerURL, cfg.Harvester.Scholarly.DOIViewerURL}, "MD5CatalogURL": {got.MD5CatalogURL, cfg.Harvester.Scholarly.MD5CatalogURL}, "GoogleScholarURL": {got.GoogleScholarURL, cfg.Harvester.Scholarly.GoogleScholarURL}, "ContactEmail": {got.ContactEmail, cfg.Harvester.Scholarly.ContactEmail}} {
		if pair[0] != pair[1] {
			t.Errorf("%s=%q want %q", key, pair[0], pair[1])
		}
	}
}

func TestHarvestDispatchReachesHarvestcli(t *testing.T) {
	for _, verb := range []string{"download-file", "search", "ask"} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{verb}, strings.NewReader(""), &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "usage: harvester "+verb) {
			t.Errorf("%s: code=%d stderr=%s", verb, code, stderr.String())
		}
	}
}

func TestDoctorHelpExitStatus(t *testing.T) {
	for _, argument := range []string{"--help", "-h", "--unknown-flag"} {
		t.Run(argument, func(t *testing.T) {
			command := exec.Command(os.Args[0], "doctor", argument)
			command.Env = append(os.Environ(), "HARVESTER_TEST_MAIN=1")
			output, err := command.CombinedOutput()
			if argument == "--unknown-flag" {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
					t.Fatalf("invalid doctor flag: error=%v output=%s", err, output)
				}
				return
			}
			if err != nil || !strings.Contains(string(output), "Usage of harvester doctor:") {
				t.Fatalf("doctor help: error=%v output=%s", err, output)
			}
		})
	}
}

func externalRuntime(t *testing.T) config.Runtime {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{Harvester: config.DefaultHarvester()}
	cfg.Harvester.Cache.Dir = filepath.Join(home, "cache")
	cfg.Harvester.External = config.HarvesterExternal{Enabled: true, Host: "127.0.0.1", PublicURL: "https://harvester.example.test", StaticToken: "example-gateway-token", StateDir: filepath.Join(home, "auth")}
	return config.Runtime{Config: cfg, Paths: paths.Values{Home: home}, Version: version}
}

func TestHarvesterExternalGatewayServesHarvesterBehindAuthOnly(t *testing.T) {
	remote, server, err := daemon.NewGateway(externalRuntime(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := remote.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, token := range []string{"", "wrong", "example-gateway-token"} {
		request := httptest.NewRequest(http.MethodPost, "https://harvester.example.test/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		recorder := httptest.NewRecorder()
		server.Handler.ServeHTTP(recorder, request)
		if token == "example-gateway-token" {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"harvester"`) {
				t.Fatalf("authenticated: %d %s", recorder.Code, recorder.Body.String())
			}
		} else if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", token, recorder.Code)
		}
	}
	for _, path := range []string{"/mcp/chat-professor", "/mcp/harvester-professor", "/status"} {
		r := httptest.NewRequest(http.MethodGet, "https://harvester.example.test"+path, http.NoBody)
		r.Header.Set("Authorization", "Bearer example-gateway-token")
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestHarvesterExternalGatewayReportsBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	runtime := externalRuntime(t)
	runtime.Config.Harvester.External.Port = listener.Addr().(*net.TCPAddr).Port
	var stdout, stderr bytes.Buffer
	if code := serveGateway(context.Background(), runtime, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "address already in use") || stdout.Len() != 0 {
		t.Fatalf("bind failure code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestVersionAndConfigPathDoNotRequireValidConfiguration(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "broken.json")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(paths.EnvHome, home)
	t.Setenv("HARVESTER_CONFIG", path)
	for _, args := range [][]string{{"version"}, {"config", "path"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("%v code=%d stderr=%s", args, code, stderr.String())
		}
		if stdout.Len() == 0 {
			t.Fatalf("%v empty output", args)
		}
	}
}

func TestHarvestAskE2E(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "source.txt")
	if err := os.WriteFile(source, []byte("full cached evidence reaches the adapter\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(paths.EnvHome, home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(home, name)
			capture := filepath.Join(home, name+".prompt")
			reply := `{"result":"fixture answer","usage":{"input_tokens":100,"output_tokens":20}}`
			model := "claude-fixture-model"
			if name == "codex" {
				model = "gpt-fixture-model"
				reply = "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"fixture answer\"}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20}}"
			}
			body := "#!/bin/sh\ncat > \"$ASK_CAPTURE\"\nprintf '%s\\n' '" + reply + "'\n"
			if err := testjail.WriteExecutable(binary, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ASK_CAPTURE", capture)
			path := filepath.Join(home, "harvester.config.json")
			raw := `{"` + name + `":{"binary":"` + binary + `"},"ask":{"engine":"` + name + `","` + name + `":{"model":"` + model + `","effort":"low"}}}`
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HARVESTER_CONFIG", path)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"ask", "-p", "State the evidence", "--engine", name, source}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 || strings.TrimSpace(stdout.String()) != "fixture answer" || !strings.Contains(stderr.String(), "usage input=100") {
				t.Fatalf("%s code=%d stdout=%s stderr=%s", name, code, stdout.String(), stderr.String())
			}
			prompt, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"source: " + source, "TASK: State the evidence", "EVIDENCE"} {
				if !strings.Contains(string(prompt), want) {
					t.Errorf("prompt missing %q: %s", want, prompt)
				}
			}
		})
	}
}

func TestCLIStopsOnTerminationWhileAskIsRunning(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	binary := filepath.Join(root, "engine")
	pidPath := filepath.Join(root, "engine.pid")
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(source, []byte("fixture evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	script := []byte("#!/bin/sh\nprintf '%s\n' \"$$\" > \"$TEST_ENGINE_PID\"\ncat >/dev/null\nwhile :; do sleep 30; done\n")
	if err := testjail.WriteExecutable(binary, script, 0700); err != nil {
		t.Fatal(err)
	}
	configBytes, err := json.Marshal(map[string]any{"claude": map[string]string{"binary": binary}, "ask": map[string]any{"engine": "claude", "claude": map[string]string{"model": "haiku"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "ask", "-p", "question", "--engine", "claude", source)
	command.Env = append(os.Environ(), "HARVESTER_TEST_MAIN=1", "HARVESTER_CONFIG="+configPath, "TEST_ENGINE_PID="+pidPath)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	enginePID := 0
	t.Cleanup(func() {
		_ = command.Process.Kill()
		if enginePID > 0 {
			_ = syscall.Kill(-enginePID, syscall.SIGKILL)
		}
	})
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for enginePID == 0 {
		raw, err := os.ReadFile(pidPath)
		if err == nil {
			enginePID, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		select {
		case <-ticker.C:
		case err := <-done:
			t.Fatalf("ask stopped before ready: %v %s", err, output.String())
		case <-deadline.C:
			t.Fatal("ask engine did not start")
		}
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("CLI swallowed termination")
		}
	case <-time.After(time.Second):
		_ = command.Process.Kill()
		<-done
		t.Fatal("CLI ignored SIGTERM while its ask engine was running")
	}
}

func TestServeWithoutExternalKeepsDaemonUntilCancelled(t *testing.T) {
	home, err := os.MkdirTemp("", "hd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(paths.EnvHome, home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("HARVESTER_CONFIG", filepath.Join(home, "missing.json"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve"}, strings.NewReader(""), &stdout, &stderr) }()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(paths.StateRoot(home), "daemon.sock")); err == nil {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("serve rejected internal-only daemon: code=%d stderr=%s", code, &stderr)
		case <-timer.C:
			cancel()
			<-done
			t.Fatal("daemon socket was never ready")
		case <-ticker.C:
		}
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve stop code=%d stderr=%s", code, &stderr)
	}
}

func TestStatusReportsUnsafeCleanupAsFailure(t *testing.T) {
	home, err := os.MkdirTemp("", "hc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv(paths.EnvHome, home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "s"))
	t.Setenv("HARVESTER_CONFIG", filepath.Join(home, "config.json"))
	if err := os.MkdirAll(paths.StateRoot(home), 0o700); err != nil {
		t.Fatal(err)
	}
	socket := paths.DaemonSocket(home)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(daemon.StatusInfo{Service: "harvester-daemon", PID: os.Getpid(), Version: "test", Started: time.Now().UTC(), Endpoint: socket, Draining: true, CleanupError: "unsafe cleanup: fixture worker remains", ConfigDigest: "private-digest"})
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "unsafe cleanup") {
			t.Fatalf("status hid unsafe cleanup: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
		}
		if strings.Contains(stdout.String(), "private-digest") {
			t.Fatal("public status disclosed private config digest")
		}
		if !strings.Contains(stdout.String(), "draining") {
			t.Fatalf("status omitted drain state: %s", &stdout)
		}
	}
}
