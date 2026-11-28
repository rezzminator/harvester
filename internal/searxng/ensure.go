package searxng

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/harvest"
	"github.com/rezzminator/harvester/internal/obs"
)

// Manager ensures the managed backend: one container, by name, on Docker.
type Manager struct {
	Runner deps.Runner
	Home   string
	Name   string
	Port   int
	Image  string
	// Offline never pulls: a missing pinned image is reported, never fetched.
	Offline bool
	// DockerPaths are tried in order when docker is not on PATH, which a client
	// started from a desktop launcher often lacks.
	DockerPaths []string
	// Probe runs one JSON search against origin and returns its result count.
	Probe     func(ctx context.Context, origin string) (int, error)
	ReadyWait time.Duration
	PollEvery time.Duration
	// ReachWait bounds the docker version call, so a Docker daemon that
	// accepts and never answers reads as out of reach.
	ReachWait time.Duration
	// KeepReady leaves a ready record as it is when a check finds the
	// recorded container on the recorded URL but no JSON answer: the daemon's
	// boot check, so a backend that answers on and off never moves the
	// daemon's identity (Origin) back and forth. Setup records what it finds.
	KeepReady bool

	bin string
}

// State is how an Ensure ended.
type State string

const (
	StateReady    State = "ready"
	StateNoDocker State = "no-docker"
	StateFailed   State = "failed"
)

// Result is one Ensure: Action names what it did to the container, Count is
// the probe's result count, Detail what is missing or failed.
type Result struct {
	State  State
	Action string
	Pulled bool
	Count  int
	Record Record
	Detail string
}

func (r Result) fail(err error) Result {
	r.State = StateFailed
	r.Detail = err.Error()
	return r
}

// Summary is the result as one row's fields.
func (r Result) Summary() string {
	if r.State != StateReady {
		return string(r.State) + ": " + r.Detail
	}
	backend := SourceManaged
	if r.Record.Adopted {
		backend = SourceAdopted
	}
	return fmt.Sprintf("ready backend=%s container=%s url=%s action=%q pulled=%t results=%d",
		backend, r.Record.Container, r.Record.URL, r.Action, r.Pulled, r.Count)
}

const probeQuery = "searxng"

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

var errRefused = errors.New("the real Docker is never run inside tests")

// refusedRunner stands in for the real runner inside a test binary, so a test
// that reaches NewManager meets no Docker instead of the operator's.
type refusedRunner struct{}

func (refusedRunner) Run(context.Context, []string, deps.RunOptions) (deps.RunResult, error) {
	return deps.RunResult{ExitCode: -1}, errRefused
}
func (refusedRunner) LookPath(string) (string, error) { return "", errRefused }
func (refusedRunner) Start(context.Context, []string, deps.StartOptions) (deps.Process, error) {
	return nil, errRefused
}

// NewManager is the managed backend as the binary runs it: the real Docker,
// the pinned image, and the name and first port EnvName and EnvPort give.
func NewManager(home string, offline bool) (Manager, error) {
	manager := Manager{
		Runner: obs.Runner(deps.RealRunner{}), Home: home, Name: DefaultName, Port: DefaultPort, Image: Pin,
		Offline: offline, Probe: ProbeJSON,
		DockerPaths: []string{
			"/usr/local/bin/docker", "/opt/homebrew/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker",
		},
	}
	if testing.Testing() {
		manager.Runner, manager.DockerPaths = refusedRunner{}, nil
	}
	if value, ok := os.LookupEnv(EnvName); ok {
		name := strings.TrimSpace(value)
		if !namePattern.MatchString(name) {
			return Manager{}, fmt.Errorf("%s=%q is not a Docker container name", EnvName, value)
		}
		manager.Name = name
	}
	if value, ok := os.LookupEnv(EnvPort); ok {
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port < 1 || port > 65535 {
			return Manager{}, fmt.Errorf("%s=%q is not a TCP port", EnvPort, value)
		}
		manager.Port = port
	}
	return manager, nil
}

// ProbeJSON runs one real JSON search on a SearXNG origin: the proof that a
// backend can answer search_web, an empty answer included. A presence probe:
// its failure, a starting container's reset connections included, logs as a
// warning and is reported by its caller.
func ProbeJSON(ctx context.Context, origin string) (int, error) {
	results, _, err := harvest.Search(obs.Presence(ctx), probeQuery, harvest.SearchOptions{SearXNGURL: origin})
	return len(results), err
}

func (m Manager) withDefaults() Manager {
	if m.Name == "" {
		m.Name = DefaultName
	}
	if m.Port == 0 {
		m.Port = DefaultPort
	}
	if m.Image == "" {
		m.Image = Pin
	}
	if m.Probe == nil {
		m.Probe = ProbeJSON
	}
	if m.ReadyWait <= 0 {
		m.ReadyWait = 60 * time.Second
	}
	if m.PollEvery <= 0 {
		m.PollEvery = time.Second
	}
	if m.ReachWait <= 0 {
		m.ReachWait = 20 * time.Second
	}
	return m
}

func (m Manager) validate() error {
	switch {
	case m.Runner == nil:
		return errors.New("search backend: no command runner")
	case m.Home == "" || !filepath.IsAbs(m.Home):
		return fmt.Errorf("search backend: home must be an absolute path, got %q", m.Home)
	case !filepath.IsAbs(Dir(m.Home)):
		return fmt.Errorf("search backend: state directory must be absolute, got %q", Dir(m.Home))
	case !namePattern.MatchString(m.Name):
		return fmt.Errorf("search backend: %q is not a Docker container name", m.Name)
	case m.Port < 1 || m.Port > 65535:
		return fmt.Errorf("search backend: port %d is not a TCP port", m.Port)
	}
	return nil
}

// Ensure adopts, starts or provisions the managed backend and records it.
// Without a reachable Docker it records nothing, so a ready record stays ready
// for the container Docker restarts itself (restart unless-stopped); with
// Docker reachable it records what it found, Ready only once a JSON search
// answered. An interrupted Ensure records nothing either, nor does one under
// KeepReady that found the recorded container on its URL but no answer.
func (m Manager) Ensure(ctx context.Context) Result {
	m = m.withDefaults()
	if err := m.validate(); err != nil {
		return Result{State: StateFailed, Detail: err.Error()}
	}
	if _, missing := m.reach(ctx); missing != "" {
		return Result{State: StateNoDocker, Detail: missing}
	}
	unlock, err := lockEnsure(ctx, m.Home)
	if err != nil {
		return Result{State: StateFailed, Detail: err.Error()}
	}
	defer func() {
		if err := unlock(); err != nil {
			obs.Logger(ctx).Warn("release search ensure lock", obs.FieldErr, err.Error())
		}
	}()
	result := m.ensure(ctx)
	if ctx.Err() != nil {
		return Result{State: StateFailed, Action: result.Action, Detail: "interrupted: " + ctx.Err().Error()}
	}
	result.Record.Ready = result.State == StateReady
	result.Record.Checked = time.Now().UTC()
	if !result.Record.Ready && m.KeepReady {
		if kept, err := ReadRecord(m.Home); err == nil && kept.Ready && kept.URL != "" &&
			kept.URL == result.Record.URL && kept.Container == result.Record.Container {
			return result
		}
	}
	if !result.Record.Ready {
		result.Record.URL, result.Record.Error = "", result.Detail
	}
	if err := writeRecord(m.Home, result.Record); err != nil {
		return result.fail(err)
	}
	return result
}

// reach finds the docker command and asks its daemon for its version; missing
// names what keeps Docker out of reach.
func (m *Manager) reach(ctx context.Context) (version, missing string) {
	bin, err := m.Runner.LookPath("docker")
	if err != nil {
		for _, candidate := range m.DockerPaths {
			if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				bin, err = candidate, nil
				break
			}
		}
	}
	if err != nil {
		return "", "Docker is not installed (no docker command: " + err.Error() + ")"
	}
	m.bin = bin
	versionCtx, cancel := context.WithTimeout(ctx, m.ReachWait)
	defer cancel()
	out, err := m.run(versionCtx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", "the Docker daemon is not reachable (" + err.Error() + ")"
	}
	return strings.TrimSpace(out), ""
}

func (m Manager) ensure(ctx context.Context) Result {
	info, found, err := m.inspect(ctx)
	if err != nil {
		return Result{Record: Record{Container: m.Name, Image: m.Image}}.fail(err)
	}
	if !found {
		return m.create(ctx, "created", false)
	}
	if info.managed() && !samePin(info.Config.Image, m.Image) {
		// Ours from an older pin: replaced once the pin is at hand, kept until then.
		pulled, err := m.image(ctx)
		if err == nil {
			if _, err := m.run(ctx, "rm", "--force", "--volumes", m.Name); err != nil {
				return Result{Action: "recreated", Pulled: pulled, Record: Record{Container: m.Name, Image: m.Image}}.fail(err)
			}
			return m.create(ctx, "recreated", pulled)
		}
		obs.Logger(ctx).Warn("harvester search keeps its container on an older image", "target", m.Name,
			"state", "image "+info.Config.Image+", pin "+m.Image, obs.FieldErr, err.Error())
	}
	return m.adopt(ctx, info)
}

// adopt keeps an existing container as it is: started when stopped, never
// recreated or removed when it is not ours.
func (m Manager) adopt(ctx context.Context, info container) Result {
	managed := info.managed()
	result := Result{Action: "adopted", Record: Record{Container: m.Name, Image: info.Config.Image, Adopted: !managed}}
	if managed {
		result.Action, result.Record.Settings = "running", SettingsPath(m.Home)
	}
	if !info.State.Running {
		if _, err := m.run(ctx, "start", m.Name); err != nil {
			return result.fail(err)
		}
		result.Action = map[bool]string{true: "started", false: "adopted and started"}[managed]
		started, found, err := m.inspect(ctx)
		if err == nil && !found {
			err = fmt.Errorf("container %s vanished after docker start", m.Name)
		}
		if err != nil {
			return result.fail(err)
		}
		info = started
	}
	origin, err := info.origin()
	if err != nil {
		return result.fail(fmt.Errorf("container %s %w", m.Name, err))
	}
	result.Record.URL = origin
	return m.ready(ctx, result)
}

func (m Manager) create(ctx context.Context, action string, pulled bool) Result {
	settings := SettingsPath(m.Home)
	result := Result{Action: action, Pulled: pulled, Record: Record{Container: m.Name, Image: m.Image, Settings: settings}}
	fetched, err := m.image(ctx)
	result.Pulled = result.Pulled || fetched
	if err != nil {
		return result.fail(err)
	}
	if err := writeSettings(settings); err != nil {
		return result.fail(err)
	}
	port, err := freePort(m.Port)
	if err != nil {
		return result.fail(err)
	}
	if _, err := m.run(ctx, runArgs(m.Name, m.Image, settings, port)...); err != nil {
		// A run that fails after its create leaves a stopped container under the
		// name carrying the harvester's label: it goes, and the next ensure
		// starts clean. One without the label was made by another hand since
		// the inspect, so it stays.
		if info, found, inspectErr := m.inspect(ctx); inspectErr != nil {
			err = errors.Join(err, inspectErr)
		} else if found && info.managed() {
			if _, rmErr := m.run(ctx, "rm", "--force", "--volumes", m.Name); rmErr != nil && !isNoSuch(rmErr) {
				err = errors.Join(err, rmErr)
			}
		}
		return result.fail(err)
	}
	result.Record.URL = origin("127.0.0.1", port)
	return m.ready(ctx, result)
}

// runArgs is the managed container: loopback only, restarted with Docker,
// capabilities and logs as SearXNG's Docker deployment sets them, settings
// mounted read-only and relabelled for the container on an SELinux host (z,
// a no-op elsewhere).
func runArgs(name, image, settings string, port int) []string {
	return []string{
		"run", "--detach", "--pull", "never",
		"--name", name,
		"--label", ManagedLabel + "=true",
		"--restart", "unless-stopped",
		"--publish", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + ":8080",
		"--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "SETGID", "--cap-add", "SETUID",
		"--log-driver", "json-file", "--log-opt", "max-size=1m", "--log-opt", "max-file=1",
		"--volume", settings + ":/etc/searxng/settings.yml:ro,z",
		image,
	}
}

func (m Manager) ready(ctx context.Context, result Result) Result {
	deadline := time.Now().Add(m.ReadyWait)
	for {
		count, err := m.Probe(ctx, result.Record.URL)
		if err == nil {
			result.State, result.Count = StateReady, count
			return result
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return result.fail(fmt.Errorf("no JSON search answered on %s within %s: %w", result.Record.URL, m.ReadyWait, err))
		}
		timer := time.NewTimer(m.PollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// image makes the pinned image present: pulled reports a pull.
func (m Manager) image(ctx context.Context) (pulled bool, err error) {
	_, err = m.run(ctx, "image", "inspect", "--format", "{{.Id}}", m.Image)
	if err == nil {
		return false, nil
	}
	if !isNoSuch(err) {
		return false, err
	}
	if m.Offline {
		return false, fmt.Errorf("the pinned image %s is not present and --offline never pulls", m.Image)
	}
	if _, err := m.run(ctx, "pull", m.Image); err != nil {
		return false, err
	}
	return true, nil
}

type container struct {
	State struct {
		Status  string
		Running bool
	}
	Config struct {
		Image  string
		Labels map[string]string
	}
	HostConfig struct {
		PortBindings map[string][]binding
	}
	NetworkSettings struct {
		Ports map[string][]binding
	}
}

type binding struct {
	HostIp   string // Docker's own spelling.
	HostPort string
}

func (c container) managed() bool { return c.Config.Labels[ManagedLabel] == "true" }

// origin is the loopback origin the container's SearXNG port answers on.
func (c container) origin() (string, error) {
	bindings := c.NetworkSettings.Ports[containerPort]
	if len(bindings) == 0 {
		bindings = c.HostConfig.PortBindings[containerPort]
	}
	ipv6 := ""
	for _, bound := range bindings {
		port, err := strconv.Atoi(bound.HostPort)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		switch bound.HostIp {
		case "127.0.0.1", "0.0.0.0", "":
			return origin("127.0.0.1", port), nil
		case "::1", "::":
			if ipv6 == "" {
				ipv6 = origin("::1", port)
			}
		}
	}
	if ipv6 != "" {
		return ipv6, nil
	}
	if len(bindings) == 0 {
		return "", fmt.Errorf("publishes no host port for %s", containerPort)
	}
	return "", fmt.Errorf("publishes %s on no loopback address", containerPort)
}

func origin(host string, port int) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func samePin(image, pin string) bool {
	return strings.TrimPrefix(image, "docker.io/") == strings.TrimPrefix(pin, "docker.io/")
}

func (m Manager) inspect(ctx context.Context) (container, bool, error) {
	out, err := m.run(ctx, "container", "inspect", m.Name)
	if err != nil {
		if isNoSuch(err) {
			return container{}, false, nil
		}
		return container{}, false, err
	}
	var found []container
	if err := json.Unmarshal([]byte(out), &found); err != nil {
		return container{}, false, fmt.Errorf("decode docker container inspect %s: %w", m.Name, err)
	}
	if len(found) != 1 {
		return container{}, false, fmt.Errorf("docker container inspect %s returned %d containers", m.Name, len(found))
	}
	return found[0], true, nil
}

type dockerError struct {
	verb   string
	code   int
	stderr string
}

func (e *dockerError) Error() string {
	line, _, _ := strings.Cut(strings.TrimSpace(e.stderr), "\n")
	if line == "" {
		return fmt.Sprintf("docker %s: exit %d", e.verb, e.code)
	}
	return fmt.Sprintf("docker %s: exit %d: %s", e.verb, e.code, line)
}

func isNoSuch(err error) bool {
	var docker *dockerError
	return errors.As(err, &docker) && strings.Contains(docker.stderr, "No such ")
}

func (m Manager) run(ctx context.Context, args ...string) (string, error) {
	verb := args[0]
	if (verb == "container" || verb == "image") && len(args) > 1 {
		verb += " " + args[1]
	}
	result, err := m.Runner.Run(ctx, append([]string{m.bin}, args...), deps.RunOptions{})
	if err != nil {
		return "", fmt.Errorf("docker %s: %w", verb, err)
	}
	if result.ExitCode != 0 {
		return string(result.Stdout), &dockerError{verb: verb, code: result.ExitCode, stderr: string(result.Stderr)}
	}
	return string(result.Stdout), nil
}

// freePort is preferred when nothing answers or holds it on loopback, else a
// free loopback port the kernel picks.
func freePort(preferred int) (int, error) {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(preferred))
	if conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond); err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			return 0, fmt.Errorf("probe loopback port %d: %w", preferred, closeErr)
		}
	} else if listener, err := net.Listen("tcp", address); err == nil {
		return preferred, listener.Close()
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free loopback port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}

// writeSettings writes settings.yml once; an existing one keeps its secret.
// The file is 0644 inside the 0700 state root: SearXNG reads it as its own
// non-root user inside the container.
func writeSettings(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("search settings %s is not a regular file", path)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat search settings %s: %w", path, err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generate search secret: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create search state %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".settings-*.yml")
	if err != nil {
		return fmt.Errorf("write search settings: %w", err)
	}
	_, writeErr := temp.WriteString(settingsYAML(hex.EncodeToString(secret)))
	if closeErr := temp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Chmod(temp.Name(), 0o644)
	}
	if writeErr == nil {
		writeErr = os.Rename(temp.Name(), path)
	}
	if writeErr != nil {
		return errors.Join(fmt.Errorf("write search settings %s: %w", path, writeErr), os.Remove(temp.Name()))
	}
	return nil
}

func settingsYAML(secret string) string {
	return "# The harvester's managed SearXNG: SearXNG's defaults, its own secret, the\n" +
		"# limiter off for one local client, and the JSON format search_web reads.\n" +
		"use_default_settings: true\n" +
		"server:\n" +
		"  secret_key: \"" + secret + "\"\n" +
		"  limiter: false\n" +
		"search:\n" +
		"  formats:\n" +
		"    - html\n" +
		"    - json\n"
}

func lockEnsure(ctx context.Context, home string) (func() error, error) {
	dir := Dir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create search state %s: %w", dir, err)
	}
	path := filepath.Join(dir, "ensure.lock")
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open search ensure lock %s: %w", path, err)
	}
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock.Close, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(fmt.Errorf("lock search ensure %s: %w", path, err), lock.Close())
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(fmt.Errorf("wait for search ensure lock %s: %w", path, ctx.Err()), lock.Close())
		case <-timer.C:
		}
	}
}

// Look is the managed container as Docker reports it.
type Look struct {
	// Name is the container looked for.
	Name string
	// Docker is the Docker server version; Missing names what keeps it out of reach.
	Docker  string
	Missing string
	Found   bool
	Status  string
	Image   string
	Managed bool
	Pinned  bool
	Err     error
}

// Look reads Docker and the managed container, changing nothing.
func (m Manager) Look(ctx context.Context) Look {
	m = m.withDefaults()
	if err := m.validate(); err != nil {
		return Look{Name: m.Name, Err: err}
	}
	version, missing := m.reach(ctx)
	if missing != "" {
		return Look{Name: m.Name, Missing: missing}
	}
	look := Look{Name: m.Name, Docker: version}
	info, found, err := m.inspect(ctx)
	if err != nil {
		look.Err = err
		return look
	}
	look.Found = found
	if found {
		look.Status, look.Image, look.Managed = info.State.Status, info.Config.Image, info.managed()
		look.Pinned = samePin(info.Config.Image, m.Image)
	}
	return look
}
