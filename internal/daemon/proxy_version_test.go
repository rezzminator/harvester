package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/testjail"
)

func countedVersionExecutable(t *testing.T, runtime config.Runtime) (versionFile, logFile string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	versionFile = filepath.Join(runtime.Paths.Home, "installed-version")
	logFile = filepath.Join(runtime.Paths.Home, "version-checks")
	if err := os.WriteFile(versionFile, []byte("test-v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_VERSION_TEST_FILE", versionFile)
	t.Setenv("HARVESTER_VERSION_TEST_LOG", logFile)
	t.Setenv("HARVESTER_VERSION_TEST_BINARY", binary)
	installed := filepath.Join(runtime.Paths.Home, ".local", "bin", "harvester")
	if err := os.MkdirAll(filepath.Dir(installed), 0700); err != nil {
		t.Fatal(err)
	}
	script := []byte(`#!/bin/sh
version=$(cat "$HARVESTER_VERSION_TEST_FILE") || exit 1
if [ "$1" = version ]; then
  printf '%s\n' "$version" >> "$HARVESTER_VERSION_TEST_LOG"
  printf 'harvester %s\n' "$version"
  exit 0
fi
export HARVESTER_DAEMON_TEST_VERSION="$version"
exec "$HARVESTER_VERSION_TEST_BINARY" "$@"
`)
	if err := testjail.WriteExecutable(installed, script, 0700); err != nil {
		t.Fatal(err)
	}
	return versionFile, logFile
}

func startRuntimeProxy(t *testing.T, runtime config.Runtime) *proxyTestHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	input, feed := io.Pipe()
	output, receive := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Proxy(ctx, runtime, input, receive, io.Discard)
		receive.Close()
	}()
	harness := &proxyTestHarness{input: feed, output: bufio.NewReader(output), cancel: cancel, done: done}
	t.Cleanup(func() {
		feed.Close()
		go io.Copy(io.Discard, output)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			cancel()
			<-done
			t.Error("runtime proxy did not stop")
		}
		cancel()
	})
	return harness
}

func TestProxyChecksInstalledVersionOncePerSessionAndReconnect(t *testing.T) {
	runtime := daemonFixture(t)
	versionFile, logFile := countedVersionExecutable(t, runtime)
	document := filepath.Join(runtime.Paths.Home, "document.txt")
	if err := os.WriteFile(document, []byte("VERSION-CHECK-READ"), 0600); err != nil {
		t.Fatal(err)
	}
	harness := startRuntimeProxy(t, runtime)
	harness.write(t, proxyTestInitialize)
	if _, ok := harness.read(t)["result"]; !ok {
		t.Fatal("initialize failed")
	}
	harness.write(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	traffic := func(first int) {
		t.Helper()
		for id := first; id < first+8; id++ {
			harness.write(t, `{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`)
			frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list"}`, id)
			if id%2 == 0 {
				frame = proxyTestToolCall(id, "read", fmt.Sprintf(`{"files":[%q]}`, document))
			}
			harness.write(t, frame)
			response := harness.read(t)
			if _, ok := response["result"]; !ok {
				t.Fatalf("frame %d failed: %+v", id, response)
			}
			if id%2 == 0 && !strings.Contains(fmt.Sprint(response), "VERSION-CHECK-READ") {
				t.Fatalf("read %d lost content: %+v", id, response)
			}
		}
	}
	checks := func(want int) {
		t.Helper()
		body, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(strings.Fields(string(body))); got != want {
			t.Errorf("installed version subprocesses = %d, want %d: %s", got, want, body)
		}
	}
	traffic(2)
	checks(1)
	first, err := Status(t.Context(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), runtime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versionFile, []byte("test-v2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	traffic(10)
	checks(2)
	upgraded, err := Status(t.Context(), runtime)
	if err != nil || upgraded.Version != "test-v2" || upgraded.PID == first.PID {
		t.Fatalf("reconnect did not load installed update: %+v: %v", upgraded, err)
	}
	// An old proxy and a new session both use the current installed build.
	newProxy := startRuntimeProxy(t, runtime)
	newProxy.write(t, proxyTestInitialize)
	if _, ok := newProxy.read(t)["result"]; !ok {
		t.Fatal("second initialize failed")
	}
	checks(3)
	after, err := Status(t.Context(), runtime)
	if err != nil || after.PID != upgraded.PID || after.Version != "test-v2" {
		t.Fatalf("old runtime downgraded installed daemon: %+v: %v", after, err)
	}
}

func TestConcurrentProxyStartupChecksEachSessionOnce(t *testing.T) {
	runtime := daemonFixture(t)
	_, logFile := countedVersionExecutable(t, runtime)
	first := startRuntimeProxy(t, runtime)
	second := startRuntimeProxy(t, runtime)
	first.write(t, proxyTestInitialize)
	second.write(t, proxyTestInitialize)
	for _, harness := range []*proxyTestHarness{first, second} {
		if _, ok := harness.read(t)["result"]; !ok {
			t.Fatal("concurrent initialize failed")
		}
	}
	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(body))); got != 2 {
		t.Fatalf("concurrent startup version subprocesses = %d, want 2: %s", got, body)
	}
}

func TestProxyConcurrentReconnectChecksInstalledVersionOnce(t *testing.T) {
	runtime := daemonFixture(t)
	versionFile, logFile := countedVersionExecutable(t, runtime)
	harness := startRuntimeProxy(t, runtime)
	harness.write(t, proxyTestInitialize)
	if _, ok := harness.read(t)["result"]; !ok {
		t.Fatal("initialize failed")
	}
	harness.write(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	harness.write(t, `{"jsonrpc":"2.0","id":0,"method":"tools/list"}`)
	if response := harness.read(t); response["result"] == nil {
		t.Fatalf("initial session was not ready: %+v", response)
	}
	if err := Stop(t.Context(), runtime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versionFile, []byte("test-v2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for id := 2; id < 14; id++ {
		harness.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list"}`, id))
	}
	seen := make(map[float64]bool)
	for range 12 {
		response := harness.read(t)
		if _, ok := response["result"]; !ok {
			t.Fatalf("concurrent recovery failed: %+v", response)
		}
		seen[response["id"].(float64)] = true
	}
	if len(seen) != 12 {
		t.Fatalf("recovery lost or duplicated requests: %v", seen)
	}
	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(body))); got != 2 {
		t.Fatalf("concurrent reconnect version subprocesses = %d, want 2: %s", got, body)
	}
	status, err := Status(t.Context(), runtime)
	if err != nil || status.Version != "test-v2" {
		t.Fatalf("concurrent reconnect missed installed version: %+v: %v", status, err)
	}
}

func TestProxyRestartsKilledIdleDaemonWithOneVersionCheck(t *testing.T) {
	runtime := daemonFixture(t)
	_, logFile := countedVersionExecutable(t, runtime)
	harness := startRuntimeProxy(t, runtime)
	harness.write(t, proxyTestInitialize)
	if _, ok := harness.read(t)["result"]; !ok {
		t.Fatal("initialize failed")
	}
	harness.write(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	harness.write(t, `{"jsonrpc":"2.0","id":0,"method":"tools/list"}`)
	if response := harness.read(t); response["result"] == nil {
		t.Fatalf("initial session was not ready: %+v", response)
	}
	first, err := Status(t.Context(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	harness.write(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if response := harness.read(t); response["result"] == nil {
		t.Fatalf("idle kill recovery failed: %+v", response)
	}
	after, err := Status(t.Context(), runtime)
	if err != nil || after.PID == first.PID {
		t.Fatalf("idle kill did not restart daemon: %+v: %v", after, err)
	}
	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if checks := len(strings.Fields(string(body))); checks != 2 {
		t.Fatalf("idle kill version subprocesses = %d, want 2: %s", checks, body)
	}
}

func TestEnsureNoticesExecutableReplacementDuringVersionCheck(t *testing.T) {
	runtime := daemonFixture(t)
	versionFile, logFile := countedVersionExecutable(t, runtime)
	installed := filepath.Join(runtime.Paths.Home, ".local", "bin", "harvester")
	script, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	entered := filepath.Join(runtime.Paths.Home, "version-entered")
	release := filepath.Join(runtime.Paths.Home, "version-release")
	t.Setenv("HARVESTER_VERSION_TEST_ENTERED", entered)
	t.Setenv("HARVESTER_VERSION_TEST_RELEASE", release)
	blocked := strings.Replace(string(script), "  printf 'harvester", `  : > "$HARVESTER_VERSION_TEST_ENTERED"
  while [ ! -e "$HARVESTER_VERSION_TEST_RELEASE" ]; do sleep 0.01; done
  printf 'harvester`, 1)
	if err := testjail.WriteExecutable(installed, []byte(blocked), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan StatusInfo, 1)
	failure := make(chan error, 1)
	go func() {
		status, err := Ensure(ctx, runtime)
		result <- status
		failure <- err
	}()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("first installed-version check never started")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := os.WriteFile(versionFile, []byte("test-v2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := testjail.WriteExecutable(installed+".new", script, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(installed+".new", installed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	status := <-result
	if err := <-failure; err != nil || status.Version != "test-v2" {
		t.Fatalf("installed replacement was missed: %+v: %v", status, err)
	}
	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if checks := len(strings.Fields(string(body))); checks != 2 {
		t.Errorf("installed replacement version checks = %d, want 2: %s", checks, body)
	}
}

func TestProxyVersionCheckWaiterHonorsCancellation(t *testing.T) {
	proxy := newStdioProxy(t.Context(), "unused", io.Discard)
	entered, release := make(chan struct{}), make(chan struct{})
	proxy.ready = func(context.Context) bool { return true }
	proxy.ensure = func(context.Context) error {
		close(entered)
		<-release
		return nil
	}
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- proxy.ensureDaemon(t.Context()) }()
	<-entered
	defer func() { close(release); <-ownerDone }()
	ctx, cancel := context.WithCancel(t.Context())
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- proxy.ensureDaemon(ctx) }()
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled version-check waiter returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request stayed blocked behind another request's version check")
	}
}
