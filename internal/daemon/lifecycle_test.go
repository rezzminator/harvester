package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/paths"
	"github.com/rezzminator/harvester/internal/testjail"
)

func TestMain(m *testing.M) {
	if os.Getenv("HARVESTER_DAEMON_TEST_CHILD") == "1" {
		version := os.Getenv("HARVESTER_DAEMON_TEST_VERSION")
		if len(os.Args) > 1 && os.Args[1] == "version" {
			fmt.Println("harvester", version)
			os.Exit(0)
		}
		runtime, err := config.LoadRuntime("")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		runtime.Version = version
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := Serve(ctx, runtime); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testjail.Run(m))
}

func daemonFixture(t *testing.T) config.Runtime {
	t.Helper()
	home, err := os.MkdirTemp("", "hd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(paths.EnvHome, home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "s"))
	t.Setenv("HARVESTER_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("HARVESTER_PYTHON_ROOT", "")
	t.Setenv("HARVESTER_DAEMON_TEST_CHILD", "1")
	t.Setenv("HARVESTER_DAEMON_TEST_VERSION", "test-v1")
	if err := os.WriteFile(config.Path(home), []byte(`{"search":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := config.LoadRuntime("")
	if err != nil {
		t.Fatal(err)
	}
	runtime.Version = "test-v1"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), drainTimeout+cleanupTimeout+time.Second)
		defer cancel()
		if err := Stop(ctx, runtime); err != nil {
			t.Error(err)
		}
	})
	return runtime
}

func foreground(t *testing.T, runtime config.Runtime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, runtime) }()
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	})
	ready, cancelReady := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelReady()
	for {
		if _, err := Status(ready, runtime); err == nil {
			return
		}
		select {
		case err := <-done:
			once.Do(func() { cancel() })
			t.Fatalf("daemon stopped before ready: %v", err)
		case <-ready.Done():
			t.Fatal("daemon did not become healthy")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestServeSingletonPermissionsAndNoIdleExit(t *testing.T) {
	runtime := daemonFixture(t)
	foreground(t, runtime)
	if err := Serve(t.Context(), runtime); err != nil {
		t.Fatalf("duplicate serve: %v", err)
	}
	for _, path := range []string{paths.DaemonLock(runtime.Paths.Home), paths.DaemonSocket(runtime.Paths.Home)} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private daemon path %s info=%v err=%v", path, info, err)
		}
	}
	first, err := Status(t.Context(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(100 * time.Millisecond)
	defer deadline.Stop()
	<-deadline.C
	second, err := Status(t.Context(), runtime)
	if err != nil || second.PID != first.PID || second.UptimeSeconds <= first.UptimeSeconds {
		t.Fatalf("daemon did not persist: %+v %v", second, err)
	}
}

func TestEnsureStartsDetachedRecoversAndDoesNotDowngradeOldProxy(t *testing.T) {
	runtime := daemonFixture(t)
	first, err := Ensure(t.Context(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if first.PID == os.Getpid() {
		t.Fatal("daemon did not detach into another process")
	}
	oldProxy := newStdioProxy(t.Context(), first.Endpoint, io.Discard)
	oldProxy.endpoint = "http://localhost/mcp"
	oldProxy.client = unixClient(first.Endpoint)
	defer oldProxy.client.CloseIdleConnections()
	oldProxy.ensure = func(ctx context.Context) error { _, err := Ensure(ctx, runtime); return err }
	harness := startProxyTestHarness(t, oldProxy)
	harness.write(t, proxyTestInitialize)
	if _, ok := harness.read(t)["result"]; !ok {
		t.Fatal("initialize failed")
	}
	harness.write(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	harness.write(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if _, ok := harness.read(t)["result"]; !ok {
		t.Fatal("tools/list failed")
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	harness.write(t, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	response := harness.read(t)
	if _, ok := response["result"]; !ok {
		t.Fatalf("proxy did not recover killed daemon: %+v", response)
	}
	recovered, err := Status(t.Context(), runtime)
	if err != nil || recovered.PID == first.PID {
		t.Fatalf("recovery status=%+v err=%v", recovered, err)
	}
	t.Setenv("HARVESTER_DAEMON_TEST_VERSION", "test-v2")
	upgraded, err := Ensure(t.Context(), runtime)
	if err != nil || upgraded.Version != "test-v2" || upgraded.PID == recovered.PID {
		t.Fatalf("upgrade=%+v err=%v", upgraded, err)
	}
	harness.write(t, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if _, ok := harness.read(t)["result"]; !ok {
		t.Fatal("old proxy failed after upgrade")
	}
	after, err := Ensure(t.Context(), runtime)
	if err != nil || after.PID != upgraded.PID {
		t.Fatalf("old proxy downgraded/flapped daemon: %+v err=%v", after, err)
	}
	if err := os.WriteFile(runtime.Config.Path, []byte(`{"search":{"enabled":false},"output":{"maxInlineChars":1234}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := Ensure(t.Context(), runtime)
	if err != nil || changed.PID == after.PID {
		t.Fatalf("same-version config change was not applied: %+v %v", changed, err)
	}
	harness.write(t, `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
	if _, ok := harness.read(t)["result"]; !ok {
		t.Fatal("old proxy failed after config change")
	}
	stable, err := Status(t.Context(), runtime)
	if err != nil || stable.PID != changed.PID {
		t.Fatalf("old proxy restored stale config: %+v %v", stable, err)
	}
}

func TestEnsureRejectsDifferentConfigAndRefreshesEffectiveRoots(t *testing.T) {
	runtime := daemonFixture(t)
	first, err := Ensure(t.Context(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	other := runtime
	other.Config.Path = filepath.Join(runtime.Paths.Home, "other.json")
	if err := os.WriteFile(other.Config.Path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(t.Context(), other); err == nil || !strings.Contains(err.Error(), "configuration mismatch") {
		t.Fatalf("config mismatch=%v", err)
	}
	t.Setenv("HARVESTER_PYTHON_ROOT", filepath.Join(runtime.Paths.Home, "different-python"))
	changed, err := Ensure(t.Context(), runtime)
	if err != nil || changed.PID == first.PID {
		t.Fatalf("effective root change reused worker: %+v %v", changed, err)
	}
	if err := Refresh(t.Context(), runtime); err != nil {
		t.Fatal(err)
	}
	refreshed, err := Status(t.Context(), runtime)
	if err != nil || refreshed.PID == changed.PID {
		t.Fatalf("explicit refresh reused daemon: %+v %v", refreshed, err)
	}
}

func TestEnsureStartFailureAnswersMCPWithLogPath(t *testing.T) {
	runtime := daemonFixture(t)
	binary := filepath.Join(runtime.Paths.Home, ".local", "bin", "harvester")
	script := []byte("#!/bin/sh\nif [ \"$1\" = version ]; then printf 'harvester test-v1\\n'; else printf 'fixture startup failed\\n' >&2; exit 1; fi\n")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := testjail.WriteExecutable(binary, script, 0o700); err != nil {
		t.Fatal(err)
	}
	proxy := newStdioProxy(t.Context(), "unused", io.Discard)
	proxy.ensure = func(ctx context.Context) error {
		bounded, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		_, err := Ensure(bounded, runtime)
		return err
	}
	harness := startProxyTestHarness(t, proxy)
	harness.write(t, proxyTestInitialize)
	_, message := proxyRecoveryError(t, harness.read(t))
	if !strings.Contains(message, paths.DaemonLog(runtime.Paths.Home)) {
		t.Fatalf("MCP startup failure hides log: %s", message)
	}
	body, err := os.ReadFile(paths.DaemonLog(runtime.Paths.Home))
	if err != nil || !strings.Contains(string(body), "fixture startup failed") {
		t.Fatalf("daemon failure log=%s err=%v", body, err)
	}
	info, err := os.Stat(paths.DaemonLog(runtime.Paths.Home))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log permissions=%v %v", info, err)
	}
}

func TestDaemonRefusesUnsafeSocketAndHeldUnresponsiveLock(t *testing.T) {
	runtime := daemonFixture(t)
	if err := privateRoot(runtime.Paths.Home); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(runtime.Config.Path, paths.DaemonSocket(runtime.Paths.Home)); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(t.Context(), runtime); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe socket accepted: %v", err)
	}
	if err := os.Remove(paths.DaemonSocket(runtime.Paths.Home)); err != nil {
		t.Fatal(err)
	}
	lock, held, err := acquire(runtime)
	if err != nil || !held {
		t.Fatalf("acquire=%t %v", held, err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := Serve(ctx, runtime); err == nil || !strings.Contains(err.Error(), "lock held") {
		t.Fatalf("held dead endpoint hidden: %v", err)
	}
}

// TestDaemonSocketPathLimitMatchesTheKernel: a socket path the kernel cannot
// bind (sun_path: 104 bytes on macOS, 108 on Linux, its NUL included) is
// refused by name, never left to bind's bare "invalid argument"; the longest
// path the kernel binds passes.
func TestDaemonSocketPathLimitMatchesTheKernel(t *testing.T) {
	base, err := os.MkdirTemp("", "hs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	limit := len(syscall.RawSockaddrUnix{}.Path)
	for _, length := range []int{limit - 1, limit} {
		dir := filepath.Join(base, strings.Repeat("d", length-len(base)-len("/")-len("/daemon.sock")))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		socket := filepath.Join(dir, "daemon.sock")
		if len(socket) != length {
			t.Fatalf("fixture socket path is %d bytes, want %d: %s", len(socket), length, socket)
		}
		guardErr := checkSocketPath(socket)
		listener, listenErr := net.Listen("unix", socket)
		if listenErr == nil {
			listener.Close()
		}
		if length < limit {
			if guardErr != nil || listenErr != nil {
				t.Fatalf("%d-byte socket path: guard %v, bind %v; want both to pass", length, guardErr, listenErr)
			}
			continue
		}
		want := fmt.Sprintf("daemon socket path %s is %d bytes, over the %d-byte unix socket path limit; set XDG_STATE_HOME to a shorter directory", socket, length, limit-1)
		if guardErr == nil || guardErr.Error() != want || listenErr == nil {
			t.Fatalf("%d-byte socket path: guard %v, bind %v; want guard %q and a refused bind", length, guardErr, listenErr, want)
		}
	}
}

func TestDrainRejectsNewAdmissionAndWaitsForActiveWork(t *testing.T) {
	gate := &admission{}
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(gate.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.WriteHeader(http.StatusOK) })))
	defer server.Close()
	go func() {
		response, err := server.Client().Get(server.URL)
		if err == nil {
			response.Body.Close()
		}
		close(finished)
	}()
	<-started
	gate.drain()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new admission status=%d", response.StatusCode)
	}
	waited := make(chan struct{})
	go func() { gate.active.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("drain ignored active work")
	default:
	}
	close(release)
	<-finished
	<-waited
}

func TestConcurrentEnsureConvergesOnOnePID(t *testing.T) {
	runtime := daemonFixture(t)
	results := make(chan StatusInfo, 4)
	failures := make(chan error, 4)
	for range 4 {
		go func() { status, err := Ensure(t.Context(), runtime); results <- status; failures <- err }()
	}
	pid := 0
	for range 4 {
		status := <-results
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
		if pid != 0 && status.PID != pid {
			t.Fatalf("concurrent starts returned PIDs %d and %d", pid, status.PID)
		}
		pid = status.PID
	}
}

func TestDrainBoundDoesNotWaitForeverForNoncooperativeWork(t *testing.T) {
	gate := &admission{}
	gate.active.Add(1)
	gate.drain()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- gate.wait(ctx) }()
	select {
	case err := <-done:
		gate.active.Done()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("drain timeout=%v", err)
		}
	case <-time.After(150 * time.Millisecond):
		gate.active.Done()
		<-done
		t.Fatal("daemon drain ignored its deadline while waiting for noncooperative work")
	}
}

func TestStopDoesNotReportSuccessWhileSingletonLockIsStillHeld(t *testing.T) {
	runtime := daemonFixture(t)
	lock, held, err := acquire(runtime)
	if err != nil || !held {
		t.Fatalf("lock=%t err=%v", held, err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	if err := Stop(ctx, runtime); err == nil {
		t.Fatal("stop reported success before draining process released singleton lock")
	}
}

// TestStopAlsoStopsTheSuccessorThatTookOverTheDrain: a daemon started while
// the stopped one drains waits that drain out and takes the lock (serve's
// takeover), as a second proxy's start does when Stop lands mid-startup. Stop
// asks that successor to drain too and returns once no daemon is left, never
// waiting out its whole window on a healthy successor.
func TestStopAlsoStopsTheSuccessorThatTookOverTheDrain(t *testing.T) {
	runtime := daemonFixture(t)
	if err := privateRoot(runtime.Paths.Home); err != nil {
		t.Fatal(err)
	}
	digest, err := identity(runtime)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := paths.DaemonSocket(runtime.Paths.Home)
	stopped := StatusInfo{Service: serviceMarker, PID: os.Getpid(), Version: runtime.Version, Started: time.Now().UTC(), Endpoint: endpoint, ConfigPath: runtime.Config.Path, ConfigDigest: digest}
	successor := stopped
	successor.PID++
	successor.Started = stopped.Started.Add(time.Second)
	var mu sync.Mutex
	healthy, drains := stopped, []int{}
	drainPolls := 0
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/stop" {
			var expected StatusInfo
			if err := json.NewDecoder(r.Body).Decode(&expected); err != nil || !sameInstance(expected, healthy) {
				http.Error(w, "daemon instance changed before drain", http.StatusConflict)
				return
			}
			drains = append(drains, expected.PID)
			healthy.Draining = true
			if expected.PID == successor.PID {
				// The successor drains and exits: its socket goes, and no lock file was ever taken.
				os.Remove(endpoint)
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		snapshot := healthy
		if healthy.PID == stopped.PID && healthy.Draining {
			if drainPolls++; drainPolls == 3 {
				// The stopped daemon has exited and the waiting successor holds the lock.
				healthy = successor
			}
		}
		json.NewEncoder(w).Encode(snapshot)
	})}
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(endpoint, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Close(); <-done; os.Remove(endpoint) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stopErr := Stop(ctx, runtime)
	mu.Lock()
	defer mu.Unlock()
	if want := []int{stopped.PID, successor.PID}; stopErr != nil || fmt.Sprint(drains) != fmt.Sprint(want) {
		t.Fatalf("Stop = %v, drains requested for PIDs %v; want nil and %v", stopErr, drains, want)
	}
}

func TestConcurrentEnsureUpgradeJoinsReplacementWithoutStoppingIt(t *testing.T) {
	for _, change := range []string{"version", "config"} {
		for _, failure := range []string{"stale-target", "response-lost"} {
			t.Run(change+"/"+failure, func(t *testing.T) {
				runtime := daemonFixture(t)
				if err := privateRoot(runtime.Paths.Home); err != nil {
					t.Fatal(err)
				}
				oldDigest, err := identity(runtime)
				if err != nil {
					t.Fatal(err)
				}
				old := StatusInfo{Service: serviceMarker, PID: os.Getpid(), Version: runtime.Version, Started: time.Now().Add(-time.Minute).UTC(), Endpoint: paths.DaemonSocket(runtime.Paths.Home), ConfigPath: runtime.Config.Path, ConfigDigest: oldDigest}
				if change == "version" {
					t.Setenv("HARVESTER_DAEMON_TEST_VERSION", "test-v2")
				} else {
					if err := os.WriteFile(runtime.Config.Path, []byte(`{"search":{"enabled":false},"output":{"maxInlineChars":1234}}`), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				currentRuntime, err := loadCurrent(runtime)
				if err != nil {
					t.Fatal(err)
				}
				desiredDigest, err := identity(currentRuntime)
				if err != nil {
					t.Fatal(err)
				}
				replacement := old
				replacement.PID++
				replacement.Started = time.Now().UTC()
				replacement.ConfigDigest = desiredDigest
				if change == "version" {
					replacement.Version = "test-v2"
				}
				current := old
				var mutex sync.Mutex
				var healths, stops, accepted atomic.Int32
				healthReady := make(chan struct{})
				var readyOnce sync.Once
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/healthz":
						mutex.Lock()
						snapshot := current
						mutex.Unlock()
						if n := healths.Add(1); n <= 2 {
							if n == 2 {
								readyOnce.Do(func() { close(healthReady) })
							}
							select {
							case <-healthReady:
							case <-r.Context().Done():
								return
							}
						}
						w.Header().Set("Content-Type", "application/json")
						json.NewEncoder(w).Encode(snapshot)
					case "/stop":
						n := stops.Add(1)
						if n > 1 && failure == "response-lost" {
							connection, _, err := w.(http.Hijacker).Hijack()
							if err == nil {
								connection.Close()
							}
							return
						}
						var expected StatusInfo
						if err := json.NewDecoder(r.Body).Decode(&expected); err != nil {
							http.Error(w, "missing expected instance", http.StatusConflict)
							return
						}
						mutex.Lock()
						matches := expected.PID == current.PID && expected.Started.Equal(current.Started) && expected.Version == current.Version && expected.ConfigDigest == current.ConfigDigest && expected.ConfigPath == current.ConfigPath
						if matches {
							accepted.Add(1)
							current = replacement
						}
						mutex.Unlock()
						if !matches {
							http.Error(w, "daemon instance changed", http.StatusConflict)
							return
						}
						w.WriteHeader(http.StatusAccepted)
					default:
						http.NotFound(w, r)
					}
				})}
				listener, err := net.Listen("unix", old.Endpoint)
				if err != nil {
					t.Fatal(err)
				}
				listener.(*net.UnixListener).SetUnlinkOnClose(false)
				if err := os.Chmod(old.Endpoint, 0o600); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- server.Serve(listener) }()
				t.Cleanup(func() { readyOnce.Do(func() { close(healthReady) }); server.Close(); <-done; os.Remove(old.Endpoint) })
				results := make(chan StatusInfo, 2)
				failures := make(chan error, 2)
				for range 2 {
					go func() { status, err := Ensure(t.Context(), runtime); results <- status; failures <- err }()
				}
				for range 2 {
					status := <-results
					if err := <-failures; err != nil {
						t.Fatalf("concurrent %s upgrade did not converge: %v", change, err)
					}
					if status.PID != replacement.PID {
						t.Fatalf("upgrade PID=%d want %d", status.PID, replacement.PID)
					}
				}
				if accepted.Load() != 1 {
					t.Fatalf("accepted %d stops, want one old-instance drain", accepted.Load())
				}
				status, err := Ensure(t.Context(), runtime)
				if err != nil || status.PID != replacement.PID {
					t.Fatalf("replacement churned: %+v %v", status, err)
				}
			})
		}
	}
}

func TestProxyStartsDaemonBeforeAnyFrameAndLeavesItRunningAtEOF(t *testing.T) {
	runtime := daemonFixture(t)
	input, feed := io.Pipe()
	defer feed.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Proxy(ctx, runtime, input, io.Discard, io.Discard) }()
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var running StatusInfo
	for running.PID == 0 {
		status, err := Status(t.Context(), runtime)
		if err == nil {
			running = status
			break
		}
		select {
		case err := <-done:
			t.Fatalf("proxy startup failed: %v", err)
		case <-deadline.C:
			cancel()
			feed.Close()
			<-done
			t.Fatal("open-input proxy did not eagerly start its daemon")
		case <-ticker.C:
		}
	}
	feed.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	status, err := Status(t.Context(), runtime)
	if err != nil || status.PID != running.PID {
		t.Fatalf("daemon did not survive proxy EOF: %+v %v", status, err)
	}
}

func TestStopGuardRefusesReusedPIDWithDifferentStart(t *testing.T) {
	runtime := daemonFixture(t)
	foreground(t, runtime)
	status, err := Status(t.Context(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	stale := status
	stale.Started = stale.Started.Add(-time.Nanosecond)
	if err := requestStop(t.Context(), runtime, stale); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("stale same-PID stop was accepted: %v", err)
	}
	current, err := Status(t.Context(), runtime)
	if err != nil || current.Draining || !sameInstance(status, current) {
		t.Fatalf("stale request drained replacement: %+v %v", current, err)
	}
}

func TestEnsureReloadsDiskConfigWhileOldDaemonDrains(t *testing.T) {
	runtime := daemonFixture(t)
	lock, held, err := acquire(runtime)
	if err != nil || !held {
		t.Fatalf("lock=%t %v", held, err)
	}
	digest, err := identity(runtime)
	if err != nil {
		t.Fatal(err)
	}
	old := StatusInfo{Service: serviceMarker, PID: os.Getpid(), Version: runtime.Version, Started: time.Now().UTC(), Endpoint: paths.DaemonSocket(runtime.Paths.Home), ConfigPath: runtime.Config.Path, ConfigDigest: digest, Draining: true}
	probed := make(chan struct{})
	var once sync.Once
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(probed) })
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(old)
	})}
	listener, err := net.Listen("unix", old.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(old.Endpoint, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { server.Close(); lock.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan StatusInfo, 1)
	failure := make(chan error, 1)
	go func() { status, err := Ensure(ctx, runtime); result <- status; failure <- err }()
	<-probed
	if err := os.WriteFile(runtime.Config.Path, []byte(`{"search":{"enabled":false},"output":{"maxInlineChars":2468}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server.Close()
	<-done
	os.Remove(old.Endpoint)
	lock.Close()
	status := <-result
	if err := <-failure; err != nil {
		t.Fatalf("config edit during drain failed: %v", err)
	}
	current, err := loadCurrent(runtime)
	if err != nil {
		t.Fatal(err)
	}
	latestDigest, err := identity(current)
	if err != nil || status.ConfigDigest != latestDigest {
		t.Fatalf("daemon kept stale config: %+v %v", status, err)
	}
}

func TestConcurrentDrainDoesNotHideUnprovenFailures(t *testing.T) {
	for _, failure := range []string{"denied-health", "foreign-health", "unsafe-socket", "old-still-healthy", "wrong-config-replacement"} {
		t.Run(failure, func(t *testing.T) {
			runtime := daemonFixture(t)
			if err := privateRoot(runtime.Paths.Home); err != nil {
				t.Fatal(err)
			}
			digest, err := identity(runtime)
			if err != nil {
				t.Fatal(err)
			}
			expected := StatusInfo{Service: serviceMarker, PID: os.Getpid(), Version: runtime.Version, Started: time.Now().UTC(), Endpoint: paths.DaemonSocket(runtime.Paths.Home), ConfigPath: runtime.Config.Path, ConfigDigest: digest}
			snapshot := expected
			switch failure {
			case "foreign-health":
				snapshot.Service = "foreign"
			case "wrong-config-replacement":
				snapshot.PID++
				snapshot.ConfigPath = "different-config"
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/stop" {
					http.Error(w, "not authorized to drain", http.StatusForbidden)
					return
				}
				if failure == "denied-health" {
					http.Error(w, "permission denied", http.StatusForbidden)
					return
				}
				json.NewEncoder(w).Encode(snapshot)
			})}
			listener, err := net.Listen("unix", expected.Endpoint)
			if err != nil {
				t.Fatal(err)
			}
			listener.(*net.UnixListener).SetUnlinkOnClose(false)
			mode := os.FileMode(0o600)
			if failure == "unsafe-socket" {
				mode = 0o666
			}
			if err := os.Chmod(expected.Endpoint, mode); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			defer func() { server.Close(); <-done; os.Remove(expected.Endpoint) }()
			if err := joinDrain(t.Context(), runtime, expected, func(current config.Runtime) (string, error) {
				return installedVersion(t.Context(), current)
			}); err == nil {
				t.Fatalf("%s was mistaken for confirmed concurrent drain", failure)
			}
		})
	}
}
