package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/searxng"
)

// TestServeEnsuresSearchBesideTheSocket: the search ensure runs once per start, the socket answers
// while it is still running, and serve returns only after the ensure saw its context end.
func TestServeEnsuresSearchBesideTheSocket(t *testing.T) {
	cfg := daemonFixture(t)
	var calls atomic.Int32
	started, ended := make(chan config.Runtime, 1), make(chan error, 1)
	previous := bootSearch
	bootSearch = func(ctx context.Context, runtime config.Runtime) {
		calls.Add(1)
		started <- runtime
		<-ctx.Done()
		ended <- ctx.Err()
	}
	t.Cleanup(func() { bootSearch = previous })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, 20*time.Millisecond, 20*time.Millisecond, nil) }()
	select {
	case runtime := <-started:
		if runtime.Paths.Home != cfg.Paths.Home {
			t.Fatalf("search ensure got home %q, want %q", runtime.Paths.Home, cfg.Paths.Home)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve never started its search ensure")
	}
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer waitCancel()
	for {
		if _, err := Status(waitCtx, cfg); err == nil {
			break
		}
		select {
		case <-waitCtx.Done():
			t.Fatal("the socket did not answer while the search ensure ran")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("search ensure ended with %v, want context.Canceled", err)
		}
	default:
		t.Fatal("serve returned before its search ensure ended")
	}
	if calls.Load() != 1 {
		t.Fatalf("search ensure ran %d times, want once per start", calls.Load())
	}
}

// TestIdentityFollowsTheManagedBackend: the daemon's identity, and the runtime its sessions get, carry the
// managed backend's origin exactly while its record is ready; a re-check alone changes neither, and
// search.managed false ignores the record.
func TestIdentityFollowsTheManagedBackend(t *testing.T) {
	cfg := daemonFixture(t)
	home := cfg.Paths.Home
	cfg.Config.Harvester.Search = config.HarvesterSearch{Enabled: true, Managed: true}
	record := func(body string) {
		t.Helper()
		if err := os.MkdirAll(searxng.Dir(home), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(searxng.RecordPath(home), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := func() string {
		t.Helper()
		value, err := identity(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	const readyAt = `{"url":"http://127.0.0.1:8888","container":"harvester-searxng","ready":true,"checked":"%s"}`
	none := digest()
	if got := runtimeForService(cfg).SearXNGURL; got != "" {
		t.Fatalf("no record: session SearXNG origin %q, want none", got)
	}
	record(fmt.Sprintf(readyAt, "2026-11-27T10:00:00Z"))
	ready := digest()
	if ready == none {
		t.Fatal("a ready managed backend left the daemon identity unchanged")
	}
	if got := runtimeForService(cfg).SearXNGURL; got != "http://127.0.0.1:8888" {
		t.Fatalf("ready record: session SearXNG origin %q", got)
	}
	record(fmt.Sprintf(readyAt, "2026-11-27T11:00:00Z"))
	if digest() != ready {
		t.Fatal("a re-check of the same backend changed the daemon identity")
	}
	record(`{"url":"","container":"harvester-searxng","ready":false,"error":"no JSON search answered"}`)
	if digest() != none || runtimeForService(cfg).SearXNGURL != "" {
		t.Fatal("a backend that is not ready still reached the daemon identity or its sessions")
	}
	cfg.Config.Harvester.Search.Managed = false
	optOut := digest()
	record(fmt.Sprintf(readyAt, "2026-11-27T12:00:00Z"))
	if digest() != optOut || runtimeForService(cfg).SearXNGURL != "" {
		t.Fatal("search.managed false still followed the managed record")
	}
}

type noDocker struct{}

func (noDocker) Run(context.Context, []string, deps.RunOptions) (deps.RunResult, error) {
	return deps.RunResult{ExitCode: -1}, exec.ErrNotFound
}
func (noDocker) LookPath(string) (string, error) { return "", exec.ErrNotFound }
func (noDocker) Start(context.Context, []string, deps.StartOptions) (deps.Process, error) {
	return nil, exec.ErrNotFound
}

// TestEnsureSearchOnlyWhenWanted: the boot ensure builds a manager only when the managed backend applies,
// never offline, and without Docker returns quietly, writing no search state.
func TestEnsureSearchOnlyWhenWanted(t *testing.T) {
	cfg := daemonFixture(t)
	home := cfg.Paths.Home
	var built []string
	previous := newSearchManager
	newSearchManager = func(home string, offline bool) (searxng.Manager, error) {
		built = append(built, fmt.Sprintf("%s offline=%t", home, offline))
		return searxng.Manager{Runner: noDocker{}, Home: home}, nil
	}
	t.Cleanup(func() { newSearchManager = previous })
	for _, tc := range []struct {
		name   string
		search config.HarvesterSearch
		builds int
	}{
		{"search off", config.HarvesterSearch{Managed: true}, 0},
		{"operator URL", config.HarvesterSearch{Enabled: true, Managed: true, SearXNGURL: "http://127.0.0.1:9"}, 0},
		{"brave key", config.HarvesterSearch{Enabled: true, Managed: true, BraveAPIKey: "key"}, 0},
		{"managed off", config.HarvesterSearch{Enabled: true}, 0},
		{"managed", config.HarvesterSearch{Enabled: true, Managed: true}, 1},
	} {
		built = nil
		cfg.Config.Harvester.Search = tc.search
		ctx, recorder := obs.Test(t)
		ensureSearch(ctx, cfg)
		if len(built) != tc.builds || (tc.builds == 1 && built[0] != home+" offline=false") {
			t.Fatalf("%s: managers built %q, want %d for %s", tc.name, built, tc.builds, home)
		}
		if tc.builds == 0 {
			continue
		}
		// The one row a host without Docker logs carries its values past the
		// log's allow-list: what is missing and how to get search.
		want := []string{"harvester search backend unavailable", "Docker is not installed (no docker command: " + exec.ErrNotFound.Error() + ")", searxng.Hint}
		var got [][]string
		for _, record := range recorder.Records() {
			reason, _ := record.Field("reason")
			next, _ := record.Field("next")
			got = append(got, []string{record.Message, fmt.Sprint(reason), fmt.Sprint(next)})
		}
		if len(got) != 1 || !slices.Equal(got[0], want) {
			t.Fatalf("%s: log rows %q, want one %q", tc.name, got, want)
		}
	}
	if _, err := os.Stat(searxng.Dir(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an ensure without Docker wrote search state: %v", err)
	}
}

// silentDocker answers docker version and shows the recorded container running on 127.0.0.1:8888.
type silentDocker struct{ t *testing.T }

func (d silentDocker) Run(_ context.Context, argv []string, _ deps.RunOptions) (deps.RunResult, error) {
	switch fmt.Sprint(argv[1:]) {
	case "[version --format {{.Server.Version}}]":
		return deps.RunResult{Stdout: []byte("29.9.2\n")}, nil
	case "[container inspect " + searxng.DefaultName + "]":
		bound := `{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"8888"}]}`
		return deps.RunResult{Stdout: []byte(`[{"State":{"Status":"running","Running":true},"Config":{"Image":"` + searxng.Pin +
			`"},"HostConfig":{"PortBindings":` + bound + `},"NetworkSettings":{"Ports":` + bound + `}}]`)}, nil
	}
	d.t.Errorf("unscripted docker call %q", argv[1:])
	return deps.RunResult{ExitCode: 1}, nil
}
func (silentDocker) LookPath(string) (string, error) { return "/fake/docker", nil }
func (silentDocker) Start(context.Context, []string, deps.StartOptions) (deps.Process, error) {
	return nil, errors.New("fake docker starts no process")
}

// TestEnsureSearchKeepsAReadyBackendThatFallsSilent: the boot check finds the recorded container but
// no JSON answer, logs it, and leaves the ready record and so the daemon's identity as they were.
func TestEnsureSearchKeepsAReadyBackendThatFallsSilent(t *testing.T) {
	cfg := daemonFixture(t)
	cfg.Config.Harvester.Search = config.HarvesterSearch{Enabled: true, Managed: true}
	home := cfg.Paths.Home
	if err := os.MkdirAll(searxng.Dir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	ready := `{"url":"http://127.0.0.1:8888","container":"` + searxng.DefaultName + `","image":"` + searxng.Pin +
		`","adopted":true,"ready":true,"checked":"2026-11-27T12:00:00Z"}` + "\n"
	if err := os.WriteFile(searxng.RecordPath(home), []byte(ready), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := identity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	previous := newSearchManager
	newSearchManager = func(home string, offline bool) (searxng.Manager, error) {
		silent := func(context.Context, string) (int, error) { return 0, errors.New("searxng: status 502") }
		return searxng.Manager{Runner: silentDocker{t}, Home: home, Probe: silent, ReadyWait: 10 * time.Millisecond, PollEvery: time.Millisecond}, nil
	}
	t.Cleanup(func() { newSearchManager = previous })
	ctx, recorder := obs.Test(t)
	ensureSearch(ctx, cfg)
	var logged []string
	for _, record := range recorder.Records() {
		logged = append(logged, record.Message)
	}
	after, err := os.ReadFile(searxng.RecordPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(logged, []string{"harvester search backend not ready"}) || string(after) != ready {
		t.Fatalf("log rows %q, record %s; want one not-ready row and the record %s", logged, after, ready)
	}
	if kept, err := identity(cfg); err != nil || kept != digest {
		t.Fatalf("identity %q (%v), want it unchanged %q", kept, err, digest)
	}
}
