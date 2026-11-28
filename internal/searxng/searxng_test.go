package searxng

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/testjail"
)

func TestMain(m *testing.M) { os.Exit(testjail.Run(m)) }

const fakeBin = "/fake/docker"

// step is one scripted docker call: args after the binary, "*" matching any one argument.
type step struct {
	args   []string
	result deps.RunResult
	err    error
	hang   bool // the call answers only when its context ends, as a stuck Docker daemon does
}

type fakeDocker struct {
	t       *testing.T
	lookErr error
	steps   []step
	calls   [][]string
}

func (f *fakeDocker) LookPath(name string) (string, error) {
	if name != "docker" {
		f.t.Errorf("LookPath(%q), want docker", name)
	}
	if f.lookErr != nil {
		return "", f.lookErr
	}
	return fakeBin, nil
}

func (f *fakeDocker) Run(ctx context.Context, argv []string, _ deps.RunOptions) (deps.RunResult, error) {
	f.calls = append(f.calls, argv[1:])
	if argv[0] != fakeBin {
		f.t.Errorf("docker binary %q, want %q", argv[0], fakeBin)
	}
	if len(f.steps) == 0 {
		f.t.Errorf("unscripted docker call %q", argv[1:])
		return deps.RunResult{ExitCode: 1, Stderr: []byte("unscripted")}, nil
	}
	next := f.steps[0]
	f.steps = f.steps[1:]
	if !matches(next.args, argv[1:]) {
		f.t.Errorf("docker call %q, want %q", argv[1:], next.args)
	}
	if next.hang {
		<-ctx.Done()
		return deps.RunResult{ExitCode: -1}, ctx.Err()
	}
	return next.result, next.err
}

func (f *fakeDocker) Start(context.Context, []string, deps.StartOptions) (deps.Process, error) {
	return nil, errors.New("fake docker starts no process")
}

func matches(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != "*" && want[i] != got[i] {
			return false
		}
	}
	return true
}

func ok(stdout string) deps.RunResult { return deps.RunResult{Stdout: []byte(stdout)} }
func exit1(stderr string) deps.RunResult {
	return deps.RunResult{ExitCode: 1, Stderr: []byte(stderr)}
}

func inspectJSON(t *testing.T, running bool, image string, labels map[string]string, hostIP, hostPort string) string {
	t.Helper()
	ports := map[string]any{}
	if hostPort != "" {
		ports["8080/tcp"] = []map[string]string{{"HostIp": hostIP, "HostPort": hostPort}}
	}
	live := ports
	if !running {
		live = map[string]any{}
	}
	body, err := json.Marshal([]map[string]any{{
		"State":           map[string]any{"Status": map[bool]string{true: "running", false: "exited"}[running], "Running": running},
		"Config":          map[string]any{"Image": image, "Labels": labels},
		"HostConfig":      map[string]any{"PortBindings": ports},
		"NetworkSettings": map[string]any{"Ports": live},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

var (
	version     = step{args: []string{"version", "--format", "{{.Server.Version}}"}, result: ok("29.9.2\n")}
	inspect     = []string{"container", "inspect", DefaultName}
	noContainer = step{args: inspect, result: exit1("Error response from daemon: No such container: " + DefaultName)}
	imageCheck  = []string{"image", "inspect", "--format", "{{.Id}}", Pin}
	imageAbsent = step{args: imageCheck, result: exit1("Error response from daemon: No such image: " + Pin)}
	imageHere   = step{args: imageCheck, result: ok("sha256:e0e0\n")}
	pull        = step{args: []string{"pull", Pin}, result: ok("")}
	managedTag  = map[string]string{ManagedLabel: "true"}
)

func isolate(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return t.TempDir()
}

func runStep(home string, port int) step {
	return step{args: runArgs(DefaultName, Pin, SettingsPath(home), port), result: ok("c0ffee\n")}
}

// freeLoopbackPort is a port nothing holds when the test starts.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestEnsure(t *testing.T) {
	answer := func(count int) func(context.Context, string) (int, error) {
		return func(context.Context, string) (int, error) { return count, nil }
	}
	never := func(context.Context, string) (int, error) { return 0, errors.New("searxng: status 403") }
	for _, tc := range []struct {
		name       string
		lookErr    error
		steps      func(t *testing.T, home string, port int) []step
		probe      func(context.Context, string) (int, error)
		offline    bool
		holdPort   bool
		state      State
		action     string
		pulled     bool
		url        string // "PORT" is replaced by the preferred port
		adopted    bool
		detail     string
		recordOnly bool // no record may be written
	}{
		{
			name: "no docker command", lookErr: errors.New(`exec: "docker": executable file not found in $PATH`),
			steps: func(*testing.T, string, int) []step { return nil }, probe: answer(1),
			state: StateNoDocker, detail: "Docker is not installed (no docker command: exec: \"docker\"", recordOnly: true,
		},
		{
			name: "docker daemon unreachable",
			steps: func(*testing.T, string, int) []step {
				return []step{{args: version.args, result: exit1("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")}}
			},
			probe: answer(1), state: StateNoDocker,
			detail: "the Docker daemon is not reachable (docker version: exit 1: Cannot connect to the Docker daemon", recordOnly: true,
		},
		{
			name: "docker daemon that never answers",
			steps: func(*testing.T, string, int) []step {
				return []step{{args: version.args, hang: true}}
			},
			probe: answer(1), state: StateNoDocker,
			detail: "the Docker daemon is not reachable (docker version: context deadline exceeded)", recordOnly: true,
		},
		{
			name: "this host's shape: running unlabelled container adopted without a pull",
			steps: func(t *testing.T, _ string, _ int) []step {
				return []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, nil, "127.0.0.1", "8888"))}}
			},
			probe: answer(7), state: StateReady, action: "adopted", url: "http://127.0.0.1:8888", adopted: true,
		},
		{
			name: "stopped adopted container started, published on all interfaces",
			steps: func(t *testing.T, _ string, _ int) []step {
				return []step{
					version,
					{args: inspect, result: ok(inspectJSON(t, false, "searxng/searxng:latest", nil, "0.0.0.0", "8080"))},
					{args: []string{"start", DefaultName}, result: ok(DefaultName + "\n")},
					{args: inspect, result: ok(inspectJSON(t, true, "searxng/searxng:latest", nil, "0.0.0.0", "8080"))},
				}
			},
			probe: answer(3), state: StateReady, action: "adopted and started", url: "http://127.0.0.1:8080", adopted: true,
		},
		{
			name: "own running container kept",
			steps: func(t *testing.T, _ string, _ int) []step {
				return []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, managedTag, "127.0.0.1", "8899"))}}
			},
			probe: answer(2), state: StateReady, action: "running", url: "http://127.0.0.1:8899",
		},
		{
			name: "fresh host: pulled by digest, created on the preferred port",
			steps: func(_ *testing.T, home string, port int) []step {
				return []step{version, noContainer, imageAbsent, pull, runStep(home, port)}
			},
			probe: answer(8), state: StateReady, action: "created", pulled: true, url: "http://127.0.0.1:PORT",
		},
		{
			name: "offline with the pin present: created without a pull",
			steps: func(_ *testing.T, home string, port int) []step {
				return []step{version, noContainer, imageHere, runStep(home, port)}
			},
			offline: true, probe: answer(0), state: StateReady, action: "created", url: "http://127.0.0.1:PORT",
		},
		{
			name:    "offline with the pin absent: never pulls",
			steps:   func(*testing.T, string, int) []step { return []step{version, noContainer, imageAbsent} },
			offline: true, probe: answer(1), state: StateFailed, action: "created",
			detail: "the pinned image " + Pin + " is not present and --offline never pulls",
		},
		{
			name: "pull refused",
			steps: func(*testing.T, string, int) []step {
				return []step{version, noContainer, imageAbsent, {args: pull.args, result: exit1("Error response from daemon: Get \"https://registry-1.docker.io/v2/\": dial tcp: lookup registry-1.docker.io: no such host")}}
			},
			probe: answer(1), state: StateFailed, action: "created",
			detail: "docker pull: exit 1: Error response from daemon: Get",
		},
		{
			name:     "preferred port taken: another free loopback port",
			holdPort: true,
			steps: func(_ *testing.T, home string, _ int) []step {
				run := runStep(home, 1)
				run.args[11] = "*"
				return []step{version, noContainer, imageHere, run}
			},
			probe: answer(4), state: StateReady, action: "created", url: "OTHER",
		},
		{
			name: "run fails after create: its container removed",
			steps: func(t *testing.T, home string, port int) []step {
				run := runStep(home, port)
				run.result = exit1("docker: Error response from daemon: driver failed programming external connectivity: Bind for 127.0.0.1:" + strconv.Itoa(port) + " failed: port is already allocated")
				left := step{args: inspect, result: ok(inspectJSON(t, false, Pin, managedTag, "127.0.0.1", strconv.Itoa(port)))}
				return []step{version, noContainer, imageHere, run, left, {args: []string{"rm", "--force", "--volumes", DefaultName}, result: ok(DefaultName)}}
			},
			probe: answer(1), state: StateFailed, action: "created", detail: "port is already allocated",
		},
		{
			name: "run refused for a name another hand took since the inspect: that container kept",
			steps: func(t *testing.T, home string, port int) []step {
				run := runStep(home, port)
				run.result = exit1(`docker: Error response from daemon: Conflict. The container name "/` + DefaultName + `" is already in use by container "abc123".`)
				theirs := step{args: inspect, result: ok(inspectJSON(t, true, "searxng/searxng:latest", nil, "127.0.0.1", "8888"))}
				return []step{version, noContainer, imageHere, run, theirs}
			},
			probe: answer(1), state: StateFailed, action: "created", detail: "is already in use",
		},
		{
			name: "no JSON answer within the wait",
			steps: func(t *testing.T, _ string, _ int) []step {
				return []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, nil, "127.0.0.1", "8888"))}}
			},
			probe: never, state: StateFailed, action: "adopted", adopted: true,
			detail: "no JSON search answered on http://127.0.0.1:8888 within 30ms: searxng: status 403",
		},
		{
			name: "own container on an older image: recreated from the pin",
			steps: func(t *testing.T, home string, port int) []step {
				return []step{
					version,
					{args: inspect, result: ok(inspectJSON(t, true, "searxng/searxng@sha256:0ld", managedTag, "127.0.0.1", "8888"))},
					imageHere,
					{args: []string{"rm", "--force", "--volumes", DefaultName}, result: ok(DefaultName)},
					imageHere,
					runStep(home, port),
				}
			},
			probe: answer(5), state: StateReady, action: "recreated", url: "http://127.0.0.1:PORT",
		},
		{
			name: "adopted container on an older image: kept as it is",
			steps: func(t *testing.T, _ string, _ int) []step {
				return []step{version, {args: inspect, result: ok(inspectJSON(t, true, "searxng/searxng@sha256:0ld", nil, "127.0.0.1", "8888"))}}
			},
			probe: answer(5), state: StateReady, action: "adopted", url: "http://127.0.0.1:8888", adopted: true,
		},
		{
			name: "adopted container published on no loopback address",
			steps: func(t *testing.T, _ string, _ int) []step {
				return []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, nil, "192.0.2.10", "8888"))}}
			},
			probe: answer(1), state: StateFailed, action: "adopted", adopted: true,
			detail: "container " + DefaultName + " publishes 8080/tcp on no loopback address",
		},
		{
			name: "adopted container publishing no port",
			steps: func(t *testing.T, _ string, _ int) []step {
				return []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, nil, "", ""))}}
			},
			probe: answer(1), state: StateFailed, action: "adopted", adopted: true,
			detail: "container " + DefaultName + " publishes no host port for 8080/tcp",
		},
		{
			name: "inspect fails for another reason",
			steps: func(*testing.T, string, int) []step {
				return []step{version, {args: inspect, result: exit1("permission denied while trying to connect")}}
			},
			probe: answer(1), state: StateFailed, detail: "docker container inspect: exit 1: permission denied",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolate(t)
			port := freeLoopbackPort(t)
			if tc.holdPort {
				held, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = held.Close() })
			}
			docker := &fakeDocker{t: t, lookErr: tc.lookErr, steps: tc.steps(t, home, port)}
			var probed []string
			probe := func(ctx context.Context, origin string) (int, error) {
				probed = append(probed, origin)
				return tc.probe(ctx, origin)
			}
			manager := Manager{Runner: docker, Home: home, Port: port, Offline: tc.offline, Probe: probe,
				ReadyWait: 30 * time.Millisecond, PollEvery: time.Millisecond, ReachWait: 30 * time.Millisecond}
			result := manager.Ensure(context.Background())
			if len(docker.steps) != 0 {
				t.Errorf("docker calls left unmade: %v", docker.steps)
			}
			if result.State != tc.state || result.Action != tc.action || result.Pulled != tc.pulled {
				t.Fatalf("result = %+v, want state=%s action=%q pulled=%t", result, tc.state, tc.action, tc.pulled)
			}
			if !strings.Contains(result.Detail, tc.detail) {
				t.Fatalf("detail = %q, want it to hold %q", result.Detail, tc.detail)
			}
			record, err := ReadRecord(home)
			if tc.recordOnly {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("record written without Docker: %+v err=%v", record, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := strings.ReplaceAll(tc.url, "PORT", strconv.Itoa(port))
			if tc.url == "OTHER" {
				publish := docker.calls[len(docker.calls)-1][11]
				if publish == "127.0.0.1:"+strconv.Itoa(port)+":8080" {
					t.Fatalf("publish %q took the held port", publish)
				}
				want = "http://" + strings.TrimSuffix(publish, ":8080")
			}
			if record.URL != want || record.Ready != (tc.state == StateReady) || record.Adopted != tc.adopted || record.Container != DefaultName {
				t.Fatalf("record = %+v, want url=%q ready=%t adopted=%t", record, want, tc.state == StateReady, tc.adopted)
			}
			if tc.state == StateReady {
				if record.Error != "" || len(probed) == 0 || probed[len(probed)-1] != want {
					t.Fatalf("record error=%q probed=%q want probe of %q", record.Error, probed, want)
				}
			} else if record.Error != result.Detail {
				t.Fatalf("record error = %q, want the result detail %q", record.Error, result.Detail)
			}
			if record.Checked.IsZero() {
				t.Fatal("record carries no check time")
			}
		})
	}
}

// TestEnsureKeepsTheReadyRecord: a ready record stays as it is without Docker,
// and under KeepReady (the daemon's boot check) when the recorded container
// is found on its URL but answers no search; setup's check, or a container
// found on another URL, records it not ready.
func TestEnsureKeepsTheReadyRecord(t *testing.T) {
	never := func(context.Context, string) (int, error) { return 0, errors.New("searxng: status 502") }
	for _, tc := range []struct {
		name      string
		steps     func(t *testing.T) []step
		keepReady bool
		state     State
		kept      bool
	}{
		{name: "Docker unreachable", state: StateNoDocker, kept: true,
			steps: func(*testing.T) []step {
				return []step{{args: version.args, result: exit1("Cannot connect to the Docker daemon")}}
			}},
		{name: "boot check, the recorded container silent", keepReady: true, state: StateFailed, kept: true,
			steps: func(t *testing.T) []step {
				return []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, nil, "127.0.0.1", "8888"))}}
			}},
		{name: "setup check, the recorded container silent", state: StateFailed,
			steps: func(t *testing.T) []step {
				return []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, nil, "127.0.0.1", "8888"))}}
			}},
		{name: "boot check, the container silent on another port", keepReady: true, state: StateFailed,
			steps: func(t *testing.T) []step {
				return []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, nil, "127.0.0.1", "9999"))}}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolate(t)
			ready := Record{URL: "http://127.0.0.1:8888", Container: DefaultName, Image: Pin, Adopted: true, Ready: true}
			if err := writeRecord(home, ready); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(RecordPath(home))
			if err != nil {
				t.Fatal(err)
			}
			docker := &fakeDocker{t: t, steps: tc.steps(t)}
			result := Manager{Runner: docker, Home: home, Probe: never, KeepReady: tc.keepReady,
				ReadyWait: 10 * time.Millisecond, PollEvery: time.Millisecond}.Ensure(context.Background())
			if len(docker.steps) != 0 {
				t.Errorf("docker calls left unmade: %v", docker.steps)
			}
			after, err := os.ReadFile(RecordPath(home))
			if err != nil {
				t.Fatal(err)
			}
			if result.State != tc.state || (string(after) == string(before)) != tc.kept {
				t.Fatalf("state=%s, want %s; record kept=%t, want %t\nbefore=%s\nafter=%s", result.State, tc.state,
					string(after) == string(before), tc.kept, before, after)
			}
			if !tc.kept {
				record, err := ReadRecord(home)
				if err != nil || record.Ready || record.URL != "" || record.Error != result.Detail {
					t.Fatalf("record = %+v (%v), want not ready, no URL, error %q", record, err, result.Detail)
				}
			}
			if tc.state == StateNoDocker {
				if _, err := os.Stat(filepath.Join(Dir(home), "ensure.lock")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("an unreachable Docker still took the ensure lock: %v", err)
				}
			}
		})
	}
}

func TestEnsureInterruptedRecordsNothing(t *testing.T) {
	home := isolate(t)
	ctx, cancel := context.WithCancel(context.Background())
	docker := &fakeDocker{t: t, steps: []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, nil, "127.0.0.1", "8888"))}}}
	probe := func(context.Context, string) (int, error) { cancel(); return 0, context.Canceled }
	result := Manager{Runner: docker, Home: home, Probe: probe, ReadyWait: time.Minute, PollEvery: time.Millisecond}.Ensure(ctx)
	if result.State != StateFailed || !strings.HasPrefix(result.Detail, "interrupted: ") {
		t.Fatalf("result = %+v", result)
	}
	if _, err := ReadRecord(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an interrupted ensure wrote a record: %v", err)
	}
}

func TestSettingsWrittenOnceReadableAndPrivateTree(t *testing.T) {
	home := isolate(t)
	path := SettingsPath(home)
	if err := writeSettings(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"use_default_settings: true\n", "  limiter: false\n", "  formats:\n    - html\n    - json\n"} {
		if !strings.Contains(string(first), want) {
			t.Fatalf("settings lack %q:\n%s", want, first)
		}
	}
	if !regexp.MustCompile(`(?m)^  secret_key: "[0-9a-f]{64}"$`).Match(first) {
		t.Fatalf("settings carry no random 64-hex secret_key:\n%s", first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(Dir(home))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("settings mode %v in dir mode %v, want 0644 in 0700", info.Mode().Perm(), dirInfo.Mode().Perm())
	}
	if err := writeSettings(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Fatal("a second write replaced the settings and their secret")
	}
	other := isolate(t)
	if err := writeSettings(SettingsPath(other)); err != nil {
		t.Fatal(err)
	}
	third, err := os.ReadFile(SettingsPath(other))
	if err != nil {
		t.Fatal(err)
	}
	if string(third) == string(first) {
		t.Fatal("two state roots got the same secret")
	}
}

func TestRunArgsLeastPrivilegeLoopbackPinned(t *testing.T) {
	got := runArgs("harvester-searxng", Pin, "/state/search/settings.yml", 8888)
	want := []string{
		"run", "--detach", "--pull", "never", "--name", "harvester-searxng", "--label", "harvester.managed=true",
		"--restart", "unless-stopped", "--publish", "127.0.0.1:8888:8080",
		"--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "SETGID", "--cap-add", "SETUID",
		"--log-driver", "json-file", "--log-opt", "max-size=1m", "--log-opt", "max-file=1",
		"--volume", "/state/search/settings.yml:/etc/searxng/settings.yml:ro,z", Pin,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runArgs =\n%q\nwant\n%q", got, want)
	}
	if !regexp.MustCompile(`^searxng/searxng@sha256:[0-9a-f]{64}$`).MatchString(Pin) {
		t.Fatalf("Pin %q is not a digest", Pin)
	}
}

func TestResolve(t *testing.T) {
	ready := Record{URL: "http://127.0.0.1:8888", Container: DefaultName, Ready: true}
	adopted := ready
	adopted.Adopted = true
	notReady := Record{Container: DefaultName, Error: "the Docker daemon is not reachable"}
	on := config.HarvesterSearch{Enabled: true, Managed: true}
	with := func(change func(*config.HarvesterSearch)) config.HarvesterSearch {
		search := on
		change(&search)
		return search
	}
	for _, tc := range []struct {
		name    string
		search  config.HarvesterSearch
		record  *Record
		corrupt bool
		source  Source
		url     string
		detail  string
		wanted  bool
	}{
		{name: "search off", search: with(func(s *config.HarvesterSearch) { s.Enabled = false }), record: &ready, source: SourceDisabled, detail: "search.enabled is false"},
		{name: "operator URL wins over a ready record", search: with(func(s *config.HarvesterSearch) { s.SearXNGURL = "http://searx.internal:8080" }), record: &ready, source: SourceOperator, url: "http://searx.internal:8080"},
		{name: "brave key alone", search: with(func(s *config.HarvesterSearch) { s.BraveAPIKey = "key" }), record: &ready, source: SourceBrave},
		{name: "managed off ignores the record", search: with(func(s *config.HarvesterSearch) { s.Managed = false }), record: &ready, source: SourceOptOut, detail: "search.managed is false"},
		{name: "no record yet", search: on, source: SourceNone, detail: "no managed backend yet", wanted: true},
		{name: "ready managed record", search: on, record: &ready, source: SourceManaged, url: ready.URL, wanted: true},
		{name: "ready adopted record", search: on, record: &adopted, source: SourceAdopted, url: ready.URL, wanted: true},
		{name: "record not ready", search: on, record: &notReady, source: SourceNone, detail: "the managed backend is not ready: the Docker daemon is not reachable", wanted: true},
		{name: "corrupt record", search: on, corrupt: true, source: SourceNone, detail: "decode search record", wanted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolate(t)
			if tc.record != nil {
				if err := writeRecord(home, *tc.record); err != nil {
					t.Fatal(err)
				}
			}
			if tc.corrupt {
				if err := os.MkdirAll(Dir(home), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(RecordPath(home), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			backend := Resolve(home, tc.search)
			if backend.Source != tc.source || backend.URL != tc.url || !strings.Contains(backend.Detail, tc.detail) {
				t.Fatalf("Resolve = %+v, want source=%s url=%q detail~%q", backend, tc.source, tc.url, tc.detail)
			}
			if Origin(home, tc.search) != tc.url || Wanted(tc.search) != tc.wanted {
				t.Fatalf("Origin=%q Wanted=%t, want %q %t", Origin(home, tc.search), Wanted(tc.search), tc.url, tc.wanted)
			}
		})
	}
	info, err := os.Stat(RecordPath(isolateWithRecord(t, ready)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v, want 0600", info.Mode().Perm())
	}
}

func isolateWithRecord(t *testing.T, record Record) string {
	t.Helper()
	home := isolate(t)
	if err := writeRecord(home, record); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestNewManagerRefusesRealDockerAndReadsItsKnobs(t *testing.T) {
	home := isolate(t)
	manager, err := NewManager(home, true)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Name != DefaultName || manager.Port != DefaultPort || manager.Image != Pin || !manager.Offline || manager.DockerPaths != nil {
		t.Fatalf("NewManager = %+v", manager)
	}
	if result := manager.Ensure(context.Background()); result.State != StateNoDocker || !strings.Contains(result.Detail, errRefused.Error()) {
		t.Fatalf("a test reached Docker through NewManager: %+v", result)
	}
	if look := manager.Look(context.Background()); !strings.Contains(look.Missing, errRefused.Error()) {
		t.Fatalf("Look = %+v", look)
	}
	if _, err := os.Stat(Dir(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused ensure wrote search state: %v", err)
	}
	t.Setenv(EnvName, "harvester-searxng-proof")
	t.Setenv(EnvPort, "18888")
	manager, err = NewManager(home, false)
	if err != nil || manager.Name != "harvester-searxng-proof" || manager.Port != 18888 {
		t.Fatalf("NewManager with knobs = %+v err=%v", manager, err)
	}
	for _, bad := range []struct{ name, value, want string }{
		{EnvName, "bad name", "is not a Docker container name"},
		{EnvName, "", "is not a Docker container name"},
		{EnvPort, "0", "is not a TCP port"},
		{EnvPort, "65536", "is not a TCP port"},
		{EnvPort, "port", "is not a TCP port"},
	} {
		t.Setenv(EnvName, "harvester-searxng-proof")
		t.Setenv(EnvPort, "18888")
		t.Setenv(bad.name, bad.value)
		if _, err := NewManager(home, false); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Fatalf("%s=%q err=%v, want %q", bad.name, bad.value, err, bad.want)
		}
	}
}

func TestLook(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps func(t *testing.T) []step
		want  Look
	}{
		{name: "pinned managed container", steps: func(t *testing.T) []step {
			return []step{version, {args: inspect, result: ok(inspectJSON(t, true, Pin, managedTag, "127.0.0.1", "8888"))}}
		}, want: Look{Name: DefaultName, Docker: "29.9.2", Found: true, Status: "running", Image: Pin, Managed: true, Pinned: true}},
		{name: "adopted container off the pin", steps: func(t *testing.T) []step {
			return []step{version, {args: inspect, result: ok(inspectJSON(t, false, "searxng/searxng:latest", nil, "127.0.0.1", "8888"))}}
		}, want: Look{Name: DefaultName, Docker: "29.9.2", Found: true, Status: "exited", Image: "searxng/searxng:latest"}},
		{name: "no container", steps: func(*testing.T) []step { return []step{version, noContainer} }, want: Look{Name: DefaultName, Docker: "29.9.2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docker := &fakeDocker{t: t, steps: tc.steps(t)}
			got := Manager{Runner: docker, Home: isolate(t)}.Look(context.Background())
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Look = %+v, want %+v", got, tc.want)
			}
		})
	}
}
