package doctor

// whole.go holds the rows beyond configuration, gateway, cache and
// conversion: the binary and its stable link, the daemon, search in full,
// output, the tool surface a new session gets, and the log directory.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/daemon"
	"github.com/rezzminator/harvester/internal/harvestmcp"
	"github.com/rezzminator/harvester/internal/paths"
	"github.com/rezzminator/harvester/internal/searxng"
)

// The seams tests replace; production reads the real binary, daemon and Docker.
var (
	executable       = os.Executable
	daemonStatus     = daemon.Status
	newSearchManager = searxng.NewManager
	probeSearch      = searxng.ProbeJSON
)

// stableLink is the stable link the plugin's launcher publishes,
// {HARVESTER_HOME:-$HOME/.local/share/harvester}/bin/harvester, as
// plugins/harvester/libexec/harvester spells it.
func stableLink(home string) string {
	root := os.Getenv("HARVESTER_HOME")
	if root == "" {
		root = filepath.Join(home, ".local", "share", "harvester")
	}
	return filepath.Join(root, "bin", "harvester")
}

func printBinaryDoctor(stdout io.Writer, runtime config.Runtime) int {
	path, err := executable()
	if err != nil {
		path = "unknown (" + err.Error() + ")"
	}
	link := stableLink(runtime.Paths.Home)
	row := fmt.Sprintf("doctor: binary version=%s path=%s link=%s", runtime.Version, path, link)
	target, err := os.Readlink(link)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stdout, "%s absent (the plugin's launcher creates it at its first run)\n", row)
		return 0
	}
	if err == nil {
		row += " target=" + target
		_, err = os.Stat(link)
	}
	if err != nil {
		fmt.Fprintf(stdout, "%s error=%v hint=remove %s; the plugin's launcher relinks it at its next run\n", row, err, link)
		return 1
	}
	fmt.Fprintln(stdout, row)
	return 0
}

func printDaemonDoctor(ctx context.Context, stdout io.Writer, runtime config.Runtime) int {
	socket := paths.DaemonSocket(runtime.Paths.Home)
	status, err := daemonStatus(ctx, runtime)
	if errors.Is(err, daemon.ErrDaemonAbsent) {
		fmt.Fprintf(stdout, "doctor: daemon running=false socket=%s (the first MCP session or harvester setup starts it)\n", socket)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stdout, "doctor: daemon running=unknown socket=%s error=%v hint=run harvester stop, then harvester setup\n", socket, err)
		return 1
	}
	uptime := time.Duration(status.UptimeSeconds * float64(time.Second)).Round(time.Second)
	row := fmt.Sprintf("doctor: daemon running=true pid=%d version=%s socket=%s uptime=%s gateway=%t draining=%t",
		status.PID, status.Version, socket, uptime, status.Gateway, status.Draining)
	switch {
	case status.CleanupError != "":
		fmt.Fprintf(stdout, "%s cleanup_error=%s hint=read %s, then run harvester stop and harvester setup\n",
			row, status.CleanupError, paths.DaemonLog(runtime.Paths.Home))
		return 1
	case status.Version != runtime.Version:
		fmt.Fprintf(stdout, "%s this_binary=%s hint=run harvester setup; it replaces the daemon with this binary's\n", row, runtime.Version)
		return 1
	}
	fmt.Fprintln(stdout, row)
	return 0
}

// printSearchDoctor prints search in full: the backend in effect and where it
// comes from, Docker and the managed container when the managed backend
// applies or was opted out of, and one live JSON search. unhealthy skips
// the JSON search when the health row already found the origin down.
func printSearchDoctor(ctx context.Context, stdout io.Writer, home string, search config.HarvesterSearch, backend searxng.Backend, unhealthy bool) int {
	warnings := 0
	switch backend.Source {
	case searxng.SourceOperator:
		fmt.Fprintf(stdout, "doctor: search source=%s url=%s\n", backend.Source, backend.URL)
	case searxng.SourceBrave:
		fmt.Fprintf(stdout, "doctor: search source=%s backend=brave\n", backend.Source)
	case searxng.SourceManaged, searxng.SourceAdopted:
		fmt.Fprintf(stdout, "doctor: search source=%s url=%s container=%s record=%s\n",
			backend.Source, backend.URL, backend.Record.Container, searxng.RecordPath(home))
	case searxng.SourceNone:
		fmt.Fprintf(stdout, "doctor: search source=none detail=%s record=%s hint=%s\n", backend.Detail, searxng.RecordPath(home), searxng.Hint)
		warnings++
	default:
		fmt.Fprintf(stdout, "doctor: search source=%s detail=%s\n", backend.Source, backend.Detail)
	}
	wanted := searxng.Wanted(search)
	if wanted || backend.Source == searxng.SourceOptOut {
		warnings += printSearchDockerDoctor(ctx, stdout, home, wanted, backend.Record)
	}
	return warnings + printSearchProbeDoctor(ctx, stdout, backend, unhealthy)
}

func printSearchDockerDoctor(ctx context.Context, stdout io.Writer, home string, wanted bool, record *searxng.Record) int {
	manager, err := newSearchManager(home, true)
	if err != nil {
		fmt.Fprintf(stdout, "doctor: search docker=unchecked error=%v hint=unset %s and %s\n", err, searxng.EnvName, searxng.EnvPort)
		return 1
	}
	if record != nil && record.Container != "" {
		// The record names the container behind the backend in effect.
		manager.Name = record.Container
	}
	look := manager.Look(ctx)
	switch {
	case look.Missing != "" && !wanted:
		fmt.Fprintf(stdout, "doctor: search docker=unreachable missing=%s (search.managed is false, nothing needs it)\n", look.Missing)
		return 0
	case look.Missing != "":
		fmt.Fprintf(stdout, "doctor: search docker=unreachable missing=%s hint=%s\n", look.Missing, searxng.Hint)
		return 1
	case look.Err != nil:
		fmt.Fprintf(stdout, "doctor: search docker=%s container=%s error=%v hint=check that docker container inspect %s works for this user\n",
			look.Docker, look.Name, look.Err, look.Name)
		return 1
	}
	fmt.Fprintf(stdout, "doctor: search docker=%s\n", look.Docker)
	if !look.Found {
		if wanted {
			fmt.Fprintf(stdout, "doctor: search container=%s absent (harvester setup or the next daemon start creates it)\n", look.Name)
		} else {
			fmt.Fprintf(stdout, "doctor: search container=%s absent\n", look.Name)
		}
		return 0
	}
	owner := "adopted"
	if look.Managed {
		owner = "harvester"
	}
	row := fmt.Sprintf("doctor: search container=%s state=%s image=%s pinned=%t owner=%s", look.Name, look.Status, look.Image, look.Pinned, owner)
	switch {
	case !wanted:
		fmt.Fprintf(stdout, "%s left from the managed backend (search.managed is false) remove=docker rm -f %s\n", row, look.Name)
	case look.Status != "running":
		fmt.Fprintf(stdout, "%s hint=run harvester setup; it starts the container\n", row)
		return 1
	case look.Managed && !look.Pinned:
		fmt.Fprintf(stdout, "%s pin=%s hint=run harvester setup; it recreates the container on the pin\n", row, searxng.Pin)
		return 1
	default:
		fmt.Fprintln(stdout, row)
	}
	return 0
}

func printSearchProbeDoctor(ctx context.Context, stdout io.Writer, backend searxng.Backend, unhealthy bool) int {
	switch {
	case backend.Source == searxng.SourceBrave:
		fmt.Fprintln(stdout, "doctor: search probe=skipped reason=every Brave probe spends quota")
		return 0
	case backend.URL == "":
		return 0
	case unhealthy:
		fmt.Fprintf(stdout, "doctor: search probe=skipped url=%s reason=the health check failed\n", backend.URL)
		return 0
	}
	count, err := probeSearch(ctx, backend.URL)
	if err != nil {
		fix := "check that the SearXNG at search.searxngURL lists json in search.formats"
		if backend.Source != searxng.SourceOperator {
			fix = "run harvester setup with Docker running"
		}
		fmt.Fprintf(stdout, "doctor: search probe=failed url=%s error=%s hint=%s\n", backend.URL, strings.ReplaceAll(err.Error(), "\n", " "), fix)
		return 1
	}
	fmt.Fprintf(stdout, "doctor: search probe=json url=%s results=%d\n", backend.URL, count)
	return 0
}

func printOutputDoctor(stdout io.Writer, harvester config.HarvesterConfig) {
	fmt.Fprintf(stdout, "doctor: output maxResultBytes=%d maxInlineChars=%d\n", harvester.Output.MaxResultBytes, harvester.Output.MaxInlineChars)
}

// printToolsDoctor names the tools a session started now lists; the backend
// row above carries any warning.
func printToolsDoctor(stdout io.Writer, home string, harvester config.HarvesterConfig, backend searxng.Backend) {
	names := harvestmcp.RegisteredToolNames(harvestmcp.RuntimeFromConfig(home, harvester))
	listed := "not_listed"
	if slices.Contains(names, "search_web") {
		listed = "listed"
	}
	fmt.Fprintf(stdout, "doctor: tools new_session=%s search_web=%s reason=%s\n", strings.Join(names, ","), listed, backend.Describe())
}

func printLogsDoctor(stdout io.Writer, home string) int {
	dir := paths.LogDir(home)
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stdout, "doctor: logs dir=%s absent (the first log write creates it)\n", dir)
		return 0
	}
	if err == nil && !info.IsDir() {
		err = errors.New("not a directory")
	}
	if err == nil {
		var probe *os.File
		if probe, err = os.CreateTemp(dir, ".doctor-write-*"); err == nil {
			err = errors.Join(probe.Close(), os.Remove(probe.Name()))
		}
	}
	if err != nil {
		fmt.Fprintf(stdout, "doctor: logs dir=%s writable=false error=%v hint=make %s writable by this user, or set %s to a writable directory\n",
			dir, err, dir, paths.EnvPluginLogDir)
		return 1
	}
	fmt.Fprintf(stdout, "doctor: logs dir=%s writable=true\n", dir)
	return 0
}
