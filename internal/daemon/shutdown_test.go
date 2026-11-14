package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/config"
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
    print(json.dumps({"id": request["id"], "ok": True, "markdown": "fixture inherited stderr conversion"}), flush=True)
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

// TestStopRetainsSingletonUntilAdmittedWorkEnds: harvester stop (mode shutdown) drains in place. While
// an admitted call still runs after the drain window cancelled it, the socket stays the draining daemon's
// and the lock stays held, so no successor takes over: a call that ends within the cleanup bound releases
// both once it answered, one that outlives the bound leaves the daemon in unsafe cleanup until it ends.
// The window's cancellation is the observation point: it comes after serve chose how to stop.
func TestStopRetainsSingletonUntilAdmittedWorkEnds(t *testing.T) {
	const drainCut = "daemon drain did not finish active work: context deadline exceeded"
	const unsafeCleanup = "unsafe cleanup: owned work remains after cancellation; retaining daemon ownership: " + drainCut
	for _, row := range []struct {
		name      string
		unsafe    bool     // the call outlives the cleanup bound
		wantExits []string // the exit is one of these
	}{
		{name: "call ending within the cleanup bound", wantExits: []string{drainCut}},
		// After the unsafe wait the cleanup context has already expired, so serve's select between the
		// closed resources and that context takes either: the second exit adds the resource line.
		{name: "call outliving the cleanup bound", unsafe: true, wantExits: []string{
			unsafeCleanup + "\n" + drainCut,
			unsafeCleanup + "\n" + drainCut + "\nunsafe cleanup: owned work remains after cancellation; retaining daemon ownership: close owned worker resources: context deadline exceeded",
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			cfg := daemonFixture(t)
			entered, cleanupStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
				close(cleanupStarted)
				<-release
				io.WriteString(w, "finished")
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- serve(ctx, cfg, 20*time.Millisecond, 20*time.Millisecond, handler) }()
			waitCtx, waitCancel := context.WithTimeout(t.Context(), time.Second)
			defer waitCancel()
			var draining StatusInfo
			for {
				status, err := Status(waitCtx, cfg)
				if err == nil {
					draining = status
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
			answered := make(chan string, 1)
			go func() {
				response, err := client.Post("http://localhost/mcp", "application/json", strings.NewReader("{}"))
				if err != nil {
					answered <- "error: " + err.Error()
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					answered <- "error: " + err.Error()
					return
				}
				answered <- string(body)
			}()
			<-entered
			stopped := make(chan error, 1)
			go func() { stopped <- Stop(t.Context(), cfg) }()
			waitStopped := func() error {
				select {
				case err := <-stopped:
					return err
				case <-time.After(10 * time.Second):
					t.Fatal("harvester stop never returned")
					return nil
				}
			}
			select {
			case <-cleanupStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("the drain window never cancelled the admitted call")
			}
			if row.unsafe {
				want := fmt.Sprintf("daemon unsafe cleanup; see %s: %s", paths.DaemonLog(cfg.Paths.Home), unsafeCleanup)
				if stopErr := waitStopped(); stopErr == nil || stopErr.Error() != want {
					t.Fatalf("harvester stop = %v, want %q", stopErr, want)
				}
			}
			status, err := Status(t.Context(), cfg)
			if err != nil || !sameInstance(status, draining) || !status.Draining {
				t.Fatalf("socket while the admitted call runs = %+v err=%v, want the draining daemon %+v", status, err, draining)
			}
			if released, err := lockReleased(cfg); err != nil || released {
				t.Fatalf("lock while the admitted call runs: released=%t err=%v, want held", released, err)
			}
			duplicateCtx, duplicateCancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer duplicateCancel()
			// A successor finds the lock held: it waits out the drain, or names the unsafe cleanup.
			wantDuplicate := fmt.Sprintf("daemon lock held without healthy daemon; see %s: context deadline exceeded", paths.DaemonLog(cfg.Paths.Home))
			if row.unsafe {
				wantDuplicate = fmt.Sprintf("daemon unsafe cleanup; see %s: %s", paths.DaemonLog(cfg.Paths.Home), unsafeCleanup)
			}
			if err := Serve(duplicateCtx, cfg); err == nil || err.Error() != wantDuplicate {
				t.Fatalf("successor during the drain = %v, want %q", err, wantDuplicate)
			}
			once.Do(func() { close(release) })
			select {
			case body := <-answered:
				if body != "finished" {
					t.Fatalf("admitted call answered %q, want %q", body, "finished")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("admitted call never answered")
			}
			if !row.unsafe {
				if err := waitStopped(); err != nil {
					t.Fatalf("harvester stop after the call answered = %v, want nil", err)
				}
			}
			select {
			case err := <-done:
				exit := ""
				if err != nil {
					exit = err.Error()
				}
				if !slices.Contains(row.wantExits, exit) {
					t.Fatalf("stopped daemon exit %q, want one of %q", exit, row.wantExits)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stopped daemon did not exit after its call ended")
			}
			if released, err := lockReleased(cfg); err != nil || !released {
				t.Fatalf("lock after the drain ended: released=%t err=%v, want released", released, err)
			}
		})
	}
}

// TestReplacementLetsTheAdmittedCallFinish: a replacement (Ensure after a config change) takes the
// socket and the lock at once, so the successor serves new calls while the call the replaced daemon
// admitted finishes on it within the handoff window; past that window the call is cut. With the gateway
// on, a register the replaced daemon admitted and that reaches the credential store after the successor
// took the lock is refused, and the credential state file the successor loaded stays as it was.
func TestReplacementLetsTheAdmittedCallFinish(t *testing.T) {
	for _, row := range []struct {
		name       string
		window     time.Duration
		olderProxy bool // the replacement's stop names no mode, as an older proxy's does
		gateway    bool // the external gateway is on, a register admitted before the replacement
		want       string
		wantExit   string
	}{
		{name: "call within the handoff window finishes", window: time.Minute, want: "finished"},
		{name: "handed-off gateway refuses an admitted credential write", window: time.Minute, gateway: true, want: "finished"},
		{name: "older proxy's replacement hands off too", window: time.Minute, olderProxy: true, want: "finished"},
		{name: "call past the handoff window is cut", window: 20 * time.Millisecond, want: "", wantExit: "daemon drain did not finish active work: context deadline exceeded"},
	} {
		t.Run(row.name, func(t *testing.T) {
			cfg := daemonFixture(t)
			replacementConfig := `{"search":{"enabled":false},"output":{"maxInlineChars":1234}}`
			gateway, statePath := "", ""
			if row.gateway {
				probe, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				gateway = probe.Addr().String()
				port := probe.Addr().(*net.TCPAddr).Port
				if err := probe.Close(); err != nil {
					t.Fatal(err)
				}
				stateDir := filepath.Join(cfg.Paths.Home, "gateway")
				statePath = filepath.Join(stateDir, "auth.json")
				external := fmt.Sprintf(`"external":{"enabled":true,"host":"127.0.0.1","port":%d,"publicURL":"http://%s","auth":{"passphrase":"fixture gateway passphrase"},"stateDir":%q}`, port, gateway, stateDir)
				replacementConfig = `{"search":{"enabled":false},"output":{"maxInlineChars":1234},` + external + `}`
				if err := os.WriteFile(cfg.Config.Path, []byte(`{"search":{"enabled":false},`+external+`}`), 0o600); err != nil {
					t.Fatal(err)
				}
				gatewayConfig, err := config.LoadRuntime("")
				if err != nil {
					t.Fatal(err)
				}
				gatewayConfig.Version = cfg.Version
				cfg = gatewayConfig
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
					io.WriteString(w, "finished")
				case <-r.Context().Done():
				}
			})
			done := make(chan error, 1)
			go func() { done <- serve(t.Context(), cfg, 20*time.Millisecond, row.window, handler) }()
			ready, cancelReady := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancelReady()
			for {
				if _, err := Status(ready, cfg); err == nil {
					break
				}
				select {
				case <-ready.Done():
					t.Fatal("daemon did not start")
				case <-time.After(5 * time.Millisecond):
				}
			}
			client := unixClient(paths.DaemonSocket(cfg.Paths.Home))
			defer client.CloseIdleConnections()
			answered := make(chan string, 1)
			go func() {
				response, err := client.Post("http://localhost/mcp", "application/json", strings.NewReader("{}"))
				if err != nil {
					answered <- "error: " + err.Error()
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					answered <- "error: " + err.Error()
					return
				}
				answered <- string(body)
			}()
			<-entered
			registration := []byte(`{"client_name":"fixture","redirect_uris":["http://127.0.0.1:9/callback"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none","scope":"harvest"}`)
			var credentialsBefore []byte
			var held net.Conn
			var heldAnswer *bufio.Reader
			if row.gateway {
				// One register lands before the replacement, so the state file holds a client.
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+gateway+"/register", bytes.NewReader(registration))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				request.Close = true
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatalf("register before the replacement: %v", err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusCreated {
					t.Fatalf("register before the replacement = %d %s err=%v, want 201", response.StatusCode, body, err)
				}
				if credentialsBefore, err = os.ReadFile(statePath); err != nil {
					t.Fatalf("read %s: %v", statePath, err)
				}
				// A second register is admitted and waits on its body: its 100 Continue shows the gateway's
				// handler reading it, past admission, before the replacement starts.
				held, err = net.Dial("tcp", gateway)
				if err != nil {
					t.Fatal(err)
				}
				defer held.Close()
				if err := held.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := fmt.Fprintf(held, "POST /register HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", gateway, len(registration)); err != nil {
					t.Fatal(err)
				}
				heldAnswer = bufio.NewReader(held)
				continued, err := http.ReadResponse(heldAnswer, nil)
				if err != nil || continued.StatusCode != http.StatusContinue {
					t.Fatalf("held register was not admitted: %+v err=%v, want 100 Continue", continued, err)
				}
			}
			if row.olderProxy {
				replaced, err := Status(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := requestStop(t.Context(), cfg, replaced, ""); err != nil {
					t.Fatalf("older proxy's stop: %v", err)
				}
			} else if err := os.WriteFile(cfg.Config.Path, []byte(replacementConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			successor, err := Ensure(t.Context(), cfg)
			if err != nil || successor.PID == os.Getpid() {
				t.Fatalf("replacement did not start a successor while a call was in flight: %+v %v", successor, err)
			}
			if row.gateway {
				// The successor holds the lock and has loaded the state file: the held register lands now.
				if _, err := held.Write(registration); err != nil {
					t.Fatal(err)
				}
				refused, err := http.ReadResponse(heldAnswer, nil)
				if err != nil {
					t.Fatalf("held register answer: %v", err)
				}
				body, err := io.ReadAll(refused.Body)
				refused.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				var answer map[string]string
				if err := json.Unmarshal(body, &answer); err != nil || refused.StatusCode != http.StatusServiceUnavailable || len(answer) != 2 ||
					answer["error"] != "temporarily_unavailable" || answer["error_description"] != "the harvester gateway is handing off to a restarted daemon; retry" {
					t.Fatalf("register the handed-off daemon admitted = %d %s, want 503 temporarily_unavailable", refused.StatusCode, body)
				}
				credentialsAfter, err := os.ReadFile(statePath)
				if err != nil {
					t.Fatalf("read %s: %v", statePath, err)
				}
				if !bytes.Equal(credentialsAfter, credentialsBefore) {
					t.Fatalf("handed-off daemon rewrote %s under its successor:\nbefore %s\nafter  %s", statePath, credentialsBefore, credentialsAfter)
				}
			}
			successorClient := unixClient(paths.DaemonSocket(cfg.Paths.Home))
			defer successorClient.CloseIdleConnections()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/mcp", strings.NewReader(proxyTestInitialize))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			response, err := successorClient.Do(request)
			if err != nil {
				t.Fatalf("successor did not serve a new call: %v", err)
			}
			var initialized struct {
				ID     int `json:"id"`
				Result struct {
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"result"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&initialized)
			response.Body.Close()
			if decodeErr != nil || response.StatusCode != http.StatusOK || initialized.ID != 1 || initialized.Result.ProtocolVersion != "2025-06-18" {
				t.Fatalf("successor initialize: status=%d answer=%+v err=%v", response.StatusCode, initialized, decodeErr)
			}
			if row.want != "" {
				once.Do(func() { close(release) })
			}
			select {
			case body := <-answered:
				if body != row.want {
					t.Fatalf("admitted call answered %q, want %q", body, row.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("admitted call never answered")
			}
			select {
			case err := <-done:
				exit := ""
				if err != nil {
					exit = err.Error()
				}
				if exit != row.wantExit {
					t.Fatalf("replaced daemon exit %q, want %q", exit, row.wantExit)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("replaced daemon did not exit after its call finished")
			}
		})
	}
}
