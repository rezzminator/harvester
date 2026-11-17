package installping

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/clock"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/paths"
	"github.com/rezzminator/harvester/internal/testjail"
)

func TestMain(m *testing.M) { os.Exit(testjail.Run(m)) }

const storedID = "3f9c2a7e-1b4d-4e8a-9c2f-6d1e0b7a5c43"

// counter is a stand-in install counter: it keeps every body and answers status.
type counter struct {
	mu      sync.Mutex
	bodies  []string
	types   []string
	status  int
	release chan struct{}
}

func serve(t *testing.T, status int) *counter {
	t.Helper()
	c := &counter{status: status}
	t.Setenv("XDG_STATE_HOME", "") // each test's home owns its state root
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.release != nil {
			<-c.release
		}
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, r.Method+" "+r.URL.Path+" "+string(body))
		c.types = append(c.types, r.Header.Get("Content-Type"))
		c.mu.Unlock()
		w.WriteHeader(c.status)
		if c.status >= 400 {
			io.WriteString(w, "invalid body: version\n")
		}
	}))
	t.Cleanup(server.Close)
	previous := endpoint
	endpoint = server.URL + "/v1/ping"
	t.Cleanup(func() { endpoint = previous })
	return c
}

func (c *counter) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

func body(id, event string) string {
	return `POST /v1/ping {"id":"` + id + `","product":"harvester","version":"0.1.0-dev.1","os":"` + runtime.GOOS + `","arch":"` + runtime.GOARCH + `","plugins":[],"event":"` + event + `"}`
}

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func readFile(t *testing.T, path string) (string, bool) {
	t.Helper()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content), true
}

func TestSend(t *testing.T) {
	day17 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		env       map[string]string
		id, day   string // prior file contents; "" is no file
		now       time.Time
		want      string
		wantEvent string // the event of the one body sent; "" means no request
		wantNew   bool   // a new id was created, so the notice is printed
		wantDay   string // the day file after the run; "" means no file
	}{
		{name: "first run sends install and prints the notice", now: day17, want: "sent install", wantEvent: "install", wantNew: true, wantDay: "2026-11-17\n"},
		{name: "a stored id and an earlier day send daily", id: storedID + "\n", day: "2026-11-16\n", now: day17, want: "sent daily", wantEvent: "daily", wantDay: "2026-11-17\n"},
		{name: "the same UTC day is skipped", id: storedID + "\n", day: "2026-11-17\n", now: day17, want: "skipped: sent 2026-11-17", wantDay: "2026-11-17\n"},
		{name: "the last second of the stored day is skipped", id: storedID + "\n", day: "2026-11-16\n", now: time.Date(2026, 11, 16, 23, 59, 59, 0, time.UTC), want: "skipped: sent 2026-11-16", wantDay: "2026-11-16\n"},
		{name: "midnight UTC starts the next day", id: storedID + "\n", day: "2026-11-16\n", now: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), want: "sent daily", wantEvent: "daily", wantDay: "2026-11-17\n"},
		{name: "a local clock past midnight is still the UTC day before", id: storedID + "\n", day: "2026-11-16\n", now: time.Date(2026, 11, 17, 1, 0, 0, 0, time.FixedZone("EET", 2*3600)), want: "skipped: sent 2026-11-16", wantDay: "2026-11-16\n"},
		{name: "a stored id with no ping stored yet sends install without the notice", id: storedID + "\n", now: day17, want: "sent install", wantEvent: "install", wantDay: "2026-11-17\n"},
		{name: "a malformed id file is replaced", id: "not-an-id\n", day: "2026-11-16\n", now: day17, want: "sent install", wantEvent: "install", wantNew: true, wantDay: "2026-11-17\n"},
		{name: "offline 1 refuses", env: map[string]string{EnvOffline: "1"}, now: day17, want: "refused: HARVESTER_INSTALL_PING_OFFLINE"},
		{name: "offline 0 sends", env: map[string]string{EnvOffline: "0"}, now: day17, want: "sent install", wantEvent: "install", wantNew: true, wantDay: "2026-11-17\n"},
		{name: "DO_NOT_TRACK 1 refuses", env: map[string]string{"DO_NOT_TRACK": "1"}, now: day17, want: "refused: DO_NOT_TRACK"},
		{name: "DO_NOT_TRACK true refuses", env: map[string]string{"DO_NOT_TRACK": "true"}, now: day17, want: "refused: DO_NOT_TRACK"},
		{name: "DO_NOT_TRACK 0 sends", env: map[string]string{"DO_NOT_TRACK": "0"}, now: day17, want: "sent install", wantEvent: "install", wantNew: true, wantDay: "2026-11-17\n"},
		{name: "DO_NOT_TRACK false sends", env: map[string]string{"DO_NOT_TRACK": "false"}, now: day17, want: "sent install", wantEvent: "install", wantNew: true, wantDay: "2026-11-17\n"},
		{name: "CI true refuses", env: map[string]string{"CI": "true"}, now: day17, want: "refused: CI"},
		{name: "CI 1 refuses", env: map[string]string{"CI": "1"}, now: day17, want: "refused: CI"},
		{name: "CI false sends", env: map[string]string{"CI": "false"}, now: day17, want: "sent install", wantEvent: "install", wantNew: true, wantDay: "2026-11-17\n"},
		{name: "CI 0 sends", env: map[string]string{"CI": "0"}, now: day17, want: "sent install", wantEvent: "install", wantNew: true, wantDay: "2026-11-17\n"},
		{name: "GITHUB_ACTIONS true refuses", env: map[string]string{"GITHUB_ACTIONS": "true"}, now: day17, want: "refused: GITHUB_ACTIONS"},
		{name: "GITHUB_ACTIONS false sends", env: map[string]string{"GITHUB_ACTIONS": "false"}, now: day17, want: "sent install", wantEvent: "install", wantNew: true, wantDay: "2026-11-17\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, http.StatusNoContent)
			home := t.TempDir()
			if tc.id != "" {
				writeState(t, paths.InstallID(home), tc.id)
			}
			if tc.day != "" {
				writeState(t, paths.InstallPingDay(home), tc.day)
			}
			var stderr bytes.Buffer
			stdout := captureStdout(t)
			got, err := Send(context.Background(), Settings{Home: home, Version: "0.1.0-dev.1", Getenv: env(tc.env), Clock: clock.NewFake(tc.now), Stderr: &stderr})
			if printed := stdout(); printed != "" {
				t.Fatalf("stdout = %q, want nothing", printed)
			}
			if err != nil || got != tc.want {
				t.Fatalf("Send = %q, %v; want %q, nil", got, err, tc.want)
			}
			idFile, hasID := readFile(t, paths.InstallID(home))
			id := strings.TrimSpace(idFile)
			if strings.HasPrefix(tc.want, "refused") {
				if _, err := os.Stat(paths.StateRoot(home)); !os.IsNotExist(err) {
					t.Fatalf("refused run left %s (stat err %v), want no state dir", paths.StateRoot(home), err)
				}
			} else if !hasID || !idPattern.MatchString(id) {
				t.Fatalf("id file %s = %q, want a lowercase UUIDv4", paths.InstallID(home), idFile)
			}
			if hasID && tc.id != "" && (id == strings.TrimSpace(tc.id)) == tc.wantNew {
				t.Fatalf("id = %q, prior %q, wantNew %v", id, tc.id, tc.wantNew)
			}
			wantStderr := ""
			if tc.wantNew {
				wantStderr = "harvester: anonymous install count on (random id, version, OS, arch); set HARVESTER_INSTALL_PING_OFFLINE=1 to turn it off\n"
			}
			if stderr.String() != wantStderr {
				t.Fatalf("stderr = %q, want %q", stderr.String(), wantStderr)
			}
			var wantBodies []string
			if tc.wantEvent != "" {
				wantBodies = []string{body(id, tc.wantEvent)}
			}
			if seen := server.seen(); strings.Join(seen, "\n") != strings.Join(wantBodies, "\n") {
				t.Fatalf("requests = %q, want %q", seen, wantBodies)
			}
			if tc.wantEvent != "" && server.types[0] != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", server.types[0])
			}
			if day, _ := readFile(t, paths.InstallPingDay(home)); day != tc.wantDay {
				t.Fatalf("day file %s = %q, want %q", paths.InstallPingDay(home), day, tc.wantDay)
			}
		})
	}
}

func TestSendFailureWarnsAndKeepsTheDayOpen(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL + "/v1/ping"
	closed.Close()
	for _, tc := range []struct {
		name    string
		status  int
		url     string
		wantErr string
	}{
		{name: "a server error", status: http.StatusInternalServerError, wantErr: "the install counter answered 500: invalid body: version"},
		{name: "a refused body", status: http.StatusBadRequest, wantErr: "the install counter answered 400: invalid body: version"},
		{name: "an unreachable counter", url: closedURL, wantErr: "send the ping: Post \"" + closedURL + "\": dial tcp " + strings.TrimPrefix(strings.TrimSuffix(closedURL, "/v1/ping"), "http://") + ": connect: connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serve(t, tc.status)
			if tc.url != "" {
				endpoint = tc.url
			}
			ctx, recorder := obs.Test(t)
			home := t.TempDir()
			writeState(t, paths.InstallID(home), storedID+"\n")
			writeState(t, paths.InstallPingDay(home), "2026-11-16\n")
			got, err := Send(ctx, Settings{Home: home, Version: "0.1.0-dev.1", Getenv: env(nil), Clock: clock.NewFake(time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC))})
			if got != "" || err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Send = %q, %v; want \"\", %q", got, err, tc.wantErr)
			}
			var warned []obs.Record
			for _, record := range recorder.Records() {
				if record.Message == "installping.failed" {
					warned = append(warned, record)
				}
			}
			if len(warned) != 1 {
				t.Fatalf("installping.failed records = %d, want 1 (log %s)", len(warned), recorder.Raw())
			}
			fields := warned[0].Fields
			if warned[0].Level != "WARN" || fields["step"] != "post" || fields["target"] != endpoint || fields["kind"] != "daily" || fields[obs.FieldErr] != tc.wantErr {
				t.Fatalf("record = %s %+v, want WARN step=post target=%s kind=daily err=%q", warned[0].Level, fields, endpoint, tc.wantErr)
			}
			if day, _ := readFile(t, paths.InstallPingDay(home)); day != "2026-11-16\n" {
				t.Fatalf("day file = %q, want the unsent day left at 2026-11-16", day)
			}
		})
	}
}

func TestRefusalGuardsTheRealEndpointInTests(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		want     string
	}{
		{name: "the real endpoint in a test binary", endpoint: productionEndpoint, want: "test binary"},
		{name: "a test's own endpoint", endpoint: "http://127.0.0.1:1/v1/ping", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := endpoint
			endpoint = tc.endpoint
			t.Cleanup(func() { endpoint = previous })
			if got := Refusal(env(nil)); got != tc.want {
				t.Fatalf("Refusal = %q, want %q", got, tc.want)
			}
		})
	}
	if got := Refusal(os.Getenv); got != EnvOffline {
		t.Fatalf("Refusal under the jail's own environment = %q, want %q", got, EnvOffline)
	}
}

func TestStartReturnsBeforeThePingAnswers(t *testing.T) {
	server := serve(t, http.StatusNoContent)
	server.release = make(chan struct{})
	home := t.TempDir()
	writeState(t, paths.InstallID(home), storedID+"\n")
	writeState(t, paths.InstallPingDay(home), "2026-11-16\n")
	stdout := captureStdout(t)
	returned := make(chan struct{})
	go func() {
		Start(context.Background(), Settings{Home: home, Version: "0.1.0-dev.1", Getenv: env(nil), Clock: clock.NewFake(time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC))})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return while the counter held the ping")
	}
	close(server.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if day, _ := readFile(t, paths.InstallPingDay(home)); day == "2026-11-17\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("day file never reached 2026-11-17; requests %q", server.seen())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if seen := server.seen(); len(seen) != 1 || seen[0] != body(storedID, "daily") {
		t.Fatalf("requests = %q, want one daily ping", seen)
	}
	if printed := stdout(); printed != "" {
		t.Fatalf("stdout = %q, want nothing", printed)
	}
}

func TestSendRacingFirstStartsShareOneID(t *testing.T) {
	server := serve(t, http.StatusNoContent)
	home := t.TempDir()
	const starts = 8
	var wait sync.WaitGroup
	got := make([]string, starts)
	var stderr [starts]bytes.Buffer
	for i := range starts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := Send(context.Background(), Settings{Home: home, Version: "0.1.0-dev.1", Getenv: env(nil), Clock: clock.NewFake(time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)), Stderr: &stderr[i]})
			if err != nil {
				result = "error: " + err.Error()
			}
			got[i] = result
		}()
	}
	wait.Wait()
	idFile, _ := readFile(t, paths.InstallID(home))
	id := strings.TrimSpace(idFile)
	if !idPattern.MatchString(id) {
		t.Fatalf("id file %s = %q, want a lowercase UUIDv4", paths.InstallID(home), idFile)
	}
	notices := 0
	for i := range starts {
		if got[i] != "sent install" && got[i] != "skipped: sent 2026-11-17" {
			t.Fatalf("start %d: Send = %q, want sent install or skipped: sent 2026-11-17", i, got[i])
		}
		if stderr[i].String() != "" {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("notices printed = %d, want 1", notices)
	}
	for _, seen := range server.seen() {
		if seen != body(id, "install") {
			t.Fatalf("request %q, want every ping to carry the one stored id %s", seen, id)
		}
	}
	entries, err := os.ReadDir(paths.StateRoot(home))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, " ") != "install-id install-ping-day" {
		t.Fatalf("state dir %s holds %q, want install-id install-ping-day and no scratch", paths.StateRoot(home), names)
	}
}

func writeState(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// captureStdout swaps os.Stdout for a pipe and returns what reached it.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	done := make(chan string)
	go func() {
		captured, _ := io.ReadAll(reader)
		done <- string(captured)
	}()
	restored := false
	restore := func() string {
		if restored {
			return ""
		}
		restored = true
		os.Stdout = original
		writer.Close()
		return <-done
	}
	t.Cleanup(func() { restore() })
	return restore
}
