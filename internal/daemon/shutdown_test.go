package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/harvestpy"
	"github.com/rezzminator/harvester/internal/paths"
)

func TestServeDrainsKilledWorkerWithInheritedStderr(t *testing.T) {
	cfg := daemonFixture(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("fence requires python3 for worker cleanup proof: %v", err)
	}
	root := filepath.Join(cfg.Paths.Home, "python")
	t.Setenv("HARVESTER_PYTHON_ROOT", root)
	project := filepath.Join(harvestpy.RuntimeRoot(root, harvestpy.Platform{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}), "project")
	if err := os.MkdirAll(filepath.Join(project, ".venv", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(python, filepath.Join(project, ".venv", "bin", "python")); err != nil {
		t.Fatal(err)
	}
	childFile, workerFile := filepath.Join(cfg.Paths.Home, "escaped.pid"), filepath.Join(cfg.Paths.Home, "worker.pid")
	script := fmt.Sprintf(`import json, os, subprocess, sys
child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, start_new_session=True)
open(%q, "w").write(str(child.pid))
open(%q, "w").write(str(os.getpid()))
for line in sys.stdin:
    request = json.loads(line)
    print(json.dumps({"id": request["id"], "ok": True, "markdown": "fixture inherited stderr conversion", "kind": "pdf", "features": {}}), flush=True)
`, childFile, workerFile)
	if err := os.WriteFile(filepath.Join(project, "converter.py"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	document := filepath.Join(cfg.Paths.Home, "fixture.pdf")
	if err := os.WriteFile(document, []byte("%PDF-1.4\n% fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreground(t, cfg)
	proxy := newStdioProxy(t.Context(), paths.DaemonSocket(cfg.Paths.Home), io.Discard)
	proxy.endpoint = "http://localhost/mcp"
	proxy.client = unixClient(paths.DaemonSocket(cfg.Paths.Home))
	defer proxy.client.CloseIdleConnections()
	harness := startProxyTestHarness(t, proxy)
	harness.write(t, proxyTestInitialize)
	_ = harness.read(t)
	harness.write(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	harness.write(t, proxyTestToolCall(2, "read", fmt.Sprintf(`{"files":[%q]}`, document)))
	response := harness.read(t)
	if !strings.Contains(fmt.Sprint(response), "fixture inherited stderr conversion") {
		t.Fatalf("fixture did not start real conversion: %+v", response)
	}
	readPID := func(path string) int {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		return pid
	}
	escaped, worker := readPID(childFile), readPID(workerFile)
	t.Cleanup(func() { syscall.Kill(escaped, syscall.SIGKILL) })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := Stop(ctx, cfg); err != nil {
		t.Fatalf("Serve cleanup waited on inherited stderr after killing its own worker: %v", err)
	}
	if err := syscall.Kill(worker, 0); err != syscall.ESRCH {
		t.Fatalf("singleton released before own worker exited: pid=%d probe=%v", worker, err)
	}
	released, err := lockReleased(cfg)
	if err != nil || !released {
		t.Fatalf("cleanup retained singleton: released=%t err=%v", released, err)
	}
}

func TestServeRetainsSingletonDuringUnsafeAdmittedCleanup(t *testing.T) {
	cfg := daemonFixture(t)
	entered := make(chan struct{})
	cleanupStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cleanupStarted)
		<-release
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, 20*time.Millisecond, handler) }()
	waitCtx, waitCancel := context.WithTimeout(t.Context(), time.Second)
	defer waitCancel()
	for {
		if _, err := Status(waitCtx, cfg); err == nil {
			break
		}
		select {
		case <-waitCtx.Done():
			t.Fatal("daemon did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	client := unixClient(paths.DaemonSocket(cfg.Paths.Home))
	defer client.CloseIdleConnections()
	requestDone := make(chan struct{})
	go func() {
		response, _ := client.Post("http://localhost/mcp", "application/json", strings.NewReader("{}"))
		if response != nil {
			response.Body.Close()
		}
		close(requestDone)
	}()
	<-entered
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopCancel()
	stopErr := Stop(stopCtx, cfg)
	if stopErr == nil || !strings.Contains(stopErr.Error(), "unsafe cleanup") {
		once.Do(func() { close(release) })
		<-done
		t.Fatalf("stop must name unsafe cleanup rather than release live handler: %v", stopErr)
	}
	<-cleanupStarted
	status, err := Status(t.Context(), cfg)
	if err != nil || !status.Draining {
		t.Fatalf("exceptional drain is not observable: %+v %v", status, err)
	}
	if released, err := lockReleased(cfg); err != nil || released {
		t.Fatalf("singleton released during unsafe cleanup: released=%t %v", released, err)
	}
	duplicateCtx, duplicateCancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer duplicateCancel()
	if err := Serve(duplicateCtx, cfg); err == nil {
		t.Fatal("second daemon accepted unsafe cleanup as ready")
	}
	once.Do(func() { close(release) })
	<-requestDone
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unsafe cleanup must remain reported after work finishes")
		}
	case <-time.After(time.Second):
		t.Fatal("released handler did not finish cleanup")
	}
	if released, err := lockReleased(cfg); err != nil || !released {
		t.Fatalf("completed cleanup did not release singleton: %t %v", released, err)
	}
}
