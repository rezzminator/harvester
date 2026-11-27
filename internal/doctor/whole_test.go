package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/daemon"
	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/paths"
	"github.com/rezzminator/harvester/internal/searxng"
)

func stubSeam[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	previous := *seam
	*seam = value
	t.Cleanup(func() { *seam = previous })
}

// stateHome is a home whose state root is {home}/.local/state/harvester.
func stateHome(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv(paths.EnvPluginLogDir, "")
	return t.TempDir()
}

func TestDoctorBinaryRow(t *testing.T) {
	stubSeam(t, &executable, func() (string, error) { return "/opt/harvester/bin/0.4.0/harvester", nil })
	const row = "doctor: binary version=0.4.0 path=/opt/harvester/bin/0.4.0/harvester link="
	for _, tc := range []struct {
		name     string
		lay      func(t *testing.T, bin string)
		want     func(link string) string
		warnings int
	}{
		{
			name: "no link yet",
			want: func(link string) string {
				return row + link + " absent (the plugin's launcher creates it at its first run)\n"
			},
		},
		{
			name: "link to the running version",
			lay: func(t *testing.T, bin string) {
				mustWrite(t, filepath.Join(bin, "0.4.0", "harvester"), "binary")
				mustLink(t, "0.4.0/harvester", filepath.Join(bin, "harvester"))
			},
			want: func(link string) string { return row + link + " target=0.4.0/harvester\n" },
		},
		{
			name: "link to a removed version",
			lay:  func(t *testing.T, bin string) { mustLink(t, "0.3.0/harvester", filepath.Join(bin, "harvester")) },
			want: func(link string) string {
				return row + link + " target=0.3.0/harvester error=stat " + link + ": no such file or directory hint=remove " + link +
					"; the plugin's launcher relinks it at its next run\n"
			},
			warnings: 1,
		},
		{
			name: "a directory where the link belongs",
			lay: func(t *testing.T, bin string) {
				if err := os.MkdirAll(filepath.Join(bin, "harvester"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: func(link string) string {
				return row + link + " error=readlink " + link + ": invalid argument hint=remove " + link +
					"; the plugin's launcher relinks it at its next run\n"
			},
			warnings: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, custom := range []bool{false, true} {
				home, root := t.TempDir(), t.TempDir()
				t.Setenv("HARVESTER_HOME", map[bool]string{true: root}[custom])
				bin := map[bool]string{false: filepath.Join(home, ".local", "share", "harvester", "bin"), true: filepath.Join(root, "bin")}[custom]
				if tc.lay != nil {
					tc.lay(t, bin)
				}
				var rows bytes.Buffer
				warnings := printBinaryDoctor(&rows, config.Runtime{Paths: paths.Values{Home: home}, Version: "0.4.0"})
				if want := tc.want(filepath.Join(bin, "harvester")); rows.String() != want || warnings != tc.warnings {
					t.Fatalf("HARVESTER_HOME set=%t\nrow  %q warnings=%d\nwant %q warnings=%d", custom, rows.String(), warnings, want, tc.warnings)
				}
			}
		})
	}
}

func TestDoctorDaemonRow(t *testing.T) {
	home := stateHome(t)
	socket := filepath.Join(home, ".local", "state", "harvester", "daemon.sock")
	running := "doctor: daemon running=true pid=42 version=%s socket=" + socket + " uptime=1m30s gateway=true draining=false"
	for _, tc := range []struct {
		name     string
		status   daemon.StatusInfo
		err      error
		want     string
		warnings int
	}{
		{
			name: "not running is information",
			err:  fmt.Errorf("%w at %s", daemon.ErrDaemonAbsent, socket),
			want: "doctor: daemon running=false socket=" + socket + " (the first MCP session or harvester setup starts it)\n",
		},
		{
			name:     "an unsafe socket",
			err:      errors.New("daemon socket mode 0644, want 0600"),
			want:     "doctor: daemon running=unknown socket=" + socket + " error=daemon socket mode 0644, want 0600 hint=run harvester stop, then harvester setup\n",
			warnings: 1,
		},
		{
			name:   "this binary's version",
			status: daemon.StatusInfo{PID: 42, Version: "0.4.0", UptimeSeconds: 90.4, Gateway: true},
			want:   fmt.Sprintf(running, "0.4.0") + "\n",
		},
		{
			name:     "an older version",
			status:   daemon.StatusInfo{PID: 42, Version: "0.3.0", UptimeSeconds: 90.4, Gateway: true},
			want:     fmt.Sprintf(running, "0.3.0") + " this_binary=0.4.0 hint=run harvester setup; it replaces the daemon with this binary's\n",
			warnings: 1,
		},
		{
			name:   "an unsafe cleanup",
			status: daemon.StatusInfo{PID: 42, Version: "0.4.0", UptimeSeconds: 90.4, Gateway: true, CleanupError: "remove daemon.new: permission denied"},
			want: fmt.Sprintf(running, "0.4.0") + " cleanup_error=remove daemon.new: permission denied hint=read " +
				filepath.Join(home, ".local", "state", "harvester", "logs", "daemon.log") + ", then run harvester stop and harvester setup\n",
			warnings: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubSeam(t, &daemonStatus, func(context.Context, config.Runtime) (daemon.StatusInfo, error) { return tc.status, tc.err })
			var rows bytes.Buffer
			warnings := printDaemonDoctor(t.Context(), &rows, config.Runtime{Paths: paths.Values{Home: home}, Version: "0.4.0"})
			if rows.String() != tc.want || warnings != tc.warnings {
				t.Fatalf("row  %q warnings=%d\nwant %q warnings=%d", rows.String(), warnings, tc.want, tc.warnings)
			}
		})
	}
}

func TestDoctorSearchRows(t *testing.T) {
	const (
		operator = "http://127.0.0.1:9999"
		managed  = "http://127.0.0.1:8888"
		older    = "searxng/searxng@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	)
	inspect := func(status, image string, ours bool) deps.RunResult {
		labels := map[string]string{}
		if ours {
			labels[searxng.ManagedLabel] = "true"
		}
		body, err := json.Marshal([]map[string]any{{
			"State":  map[string]any{"Status": status, "Running": status == "running"},
			"Config": map[string]any{"Image": image, "Labels": labels},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return deps.RunResult{Stdout: body}
	}
	noSuch := deps.RunResult{ExitCode: 1, Stderr: []byte("Error response from daemon: No such container: harvester-searxng\n")}
	denied := deps.RunResult{ExitCode: 1, Stderr: []byte("permission denied while trying to connect to the docker API\n")}
	noDocker := "Docker is not installed (no docker command: " + deps.UnscriptedError{Argv: []string{"docker"}}.Error() + ")"
	adopted := &searxng.Record{URL: managed, Container: "harvester-searxng", Image: searxng.Pin, Adopted: true, Ready: true}
	ours := &searxng.Record{URL: managed, Container: "harvester-searxng", Image: searxng.Pin, Ready: true}
	none := "doctor: search source=none detail=no managed backend yet record={record} hint=" + searxng.Hint + "\n"
	for _, tc := range []struct {
		name      string
		search    func(*config.HarvesterSearch)
		record    *searxng.Record
		docker    *deps.RunResult // nil: no docker command; else the container inspect answer
		probeErr  error
		unhealthy bool
		want      string // {record} is the record path
		warnings  int
		probes    int
	}{
		{
			name:   "search off",
			search: func(s *config.HarvesterSearch) { s.Enabled = false },
			want:   "doctor: search source=disabled detail=search.enabled is false\n",
		},
		{
			name:   "the operator's SearXNG answers",
			search: func(s *config.HarvesterSearch) { s.SearXNGURL = operator },
			want:   "doctor: search source=search.searxngURL url=" + operator + "\ndoctor: search probe=json url=" + operator + " results=3\n",
			probes: 1,
		},
		{
			name:     "the operator's SearXNG refuses json",
			search:   func(s *config.HarvesterSearch) { s.SearXNGURL = operator },
			probeErr: errors.New("searxng answered 403\nForbidden"),
			want: "doctor: search source=search.searxngURL url=" + operator + "\ndoctor: search probe=failed url=" + operator +
				" error=searxng answered 403 Forbidden hint=check that the SearXNG at search.searxngURL lists json in search.formats\n",
			warnings: 1,
			probes:   1,
		},
		{
			name:   "Brave",
			search: func(s *config.HarvesterSearch) { s.BraveAPIKey = "brave-key" },
			want:   "doctor: search source=search.braveApiKey backend=brave\ndoctor: search probe=skipped reason=every Brave probe spends quota\n",
		},
		{
			name:     "wanted without Docker",
			want:     none + "doctor: search docker=unreachable missing=" + noDocker + " hint=" + searxng.Hint + "\n",
			warnings: 2,
		},
		{
			name:     "wanted, Docker up, no container yet",
			docker:   &noSuch,
			want:     none + "doctor: search docker=29.9.2\ndoctor: search container=harvester-searxng absent (harvester setup or the next daemon start creates it)\n",
			warnings: 1,
		},
		{
			name:   "an adopted container answers",
			record: adopted,
			docker: ptr(inspect("running", searxng.Pin, false)),
			want: "doctor: search source=adopted url=" + managed + " container=harvester-searxng record={record}\ndoctor: search docker=29.9.2\n" +
				"doctor: search container=harvester-searxng state=running image=" + searxng.Pin + " pinned=true owner=adopted\n" +
				"doctor: search probe=json url=" + managed + " results=3\n",
			probes: 1,
		},
		{
			name:      "the managed container stopped",
			record:    ours,
			docker:    ptr(inspect("exited", searxng.Pin, true)),
			unhealthy: true,
			want: "doctor: search source=managed url=" + managed + " container=harvester-searxng record={record}\ndoctor: search docker=29.9.2\n" +
				"doctor: search container=harvester-searxng state=exited image=" + searxng.Pin + " pinned=true owner=harvester hint=run harvester setup; it starts the container\n" +
				"doctor: search probe=skipped url=" + managed + " reason=the health check failed\n",
			warnings: 1,
		},
		{
			name:   "the record names its own container",
			record: &searxng.Record{URL: managed, Container: "harvester-searxng-proof", Image: searxng.Pin, Ready: true},
			docker: ptr(inspect("running", searxng.Pin, true)),
			want: "doctor: search source=managed url=" + managed + " container=harvester-searxng-proof record={record}\ndoctor: search docker=29.9.2\n" +
				"doctor: search container=harvester-searxng-proof state=running image=" + searxng.Pin + " pinned=true owner=harvester\n" +
				"doctor: search probe=json url=" + managed + " results=3\n",
			probes: 1,
		},
		{
			name:   "the managed container on an older image",
			record: ours,
			docker: ptr(inspect("running", older, true)),
			want: "doctor: search source=managed url=" + managed + " container=harvester-searxng record={record}\ndoctor: search docker=29.9.2\n" +
				"doctor: search container=harvester-searxng state=running image=" + older + " pinned=false owner=harvester pin=" + searxng.Pin +
				" hint=run harvester setup; it recreates the container on the pin\n" +
				"doctor: search probe=json url=" + managed + " results=3\n",
			warnings: 1,
			probes:   1,
		},
		{
			name:     "Docker refuses the inspect",
			docker:   &denied,
			want:     none + "doctor: search docker=29.9.2 container=harvester-searxng error=docker container inspect: exit 1: permission denied while trying to connect to the docker API hint=check that docker container inspect harvester-searxng works for this user\n",
			warnings: 2,
		},
		{
			name:   "opted out with the container left",
			search: func(s *config.HarvesterSearch) { s.Managed = false },
			record: adopted,
			docker: ptr(inspect("running", searxng.Pin, false)),
			want: "doctor: search source=managed-off detail=search.managed is false\ndoctor: search docker=29.9.2\n" +
				"doctor: search container=harvester-searxng state=running image=" + searxng.Pin + " pinned=true owner=adopted" +
				" left from the managed backend (search.managed is false) remove=docker rm -f harvester-searxng\n",
		},
		{
			name:   "opted out without Docker",
			search: func(s *config.HarvesterSearch) { s.Managed = false },
			want: "doctor: search source=managed-off detail=search.managed is false\n" +
				"doctor: search docker=unreachable missing=" + noDocker + " (search.managed is false, nothing needs it)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := stateHome(t)
			search := config.DefaultHarvester().Search
			if tc.search != nil {
				tc.search(&search)
			}
			record := filepath.Join(home, ".local", "state", "harvester", "search", "managed.json")
			if tc.record != nil {
				body, err := json.Marshal(tc.record)
				if err != nil {
					t.Fatal(err)
				}
				mustWrite(t, record, string(body))
			}
			runner := &deps.FakeRunner{}
			var dockerPaths []string
			if tc.docker != nil {
				bin := filepath.Join(t.TempDir(), "docker")
				mustWrite(t, bin, "#!/bin/sh\n")
				if err := os.Chmod(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				dockerPaths = []string{bin}
				runner.Script([]string{bin, "version"}, deps.RunResult{Stdout: []byte("29.9.2\n")}, nil)
				runner.Script([]string{bin, "container", "inspect", "harvester-searxng"}, *tc.docker, nil)
				runner.Script([]string{bin, "container", "inspect", "harvester-searxng-proof"}, *tc.docker, nil)
			}
			stubSeam(t, &newSearchManager, func(home string, offline bool) (searxng.Manager, error) {
				if !offline {
					t.Error("doctor built a manager that may pull")
				}
				return searxng.Manager{Runner: runner, Home: home, DockerPaths: dockerPaths}, nil
			})
			probes := 0
			stubSeam(t, &probeSearch, func(context.Context, string) (int, error) { probes++; return 3, tc.probeErr })
			var rows bytes.Buffer
			warnings := printSearchDoctor(t.Context(), &rows, home, search, searxng.Resolve(home, search), tc.unhealthy)
			if want := strings.ReplaceAll(tc.want, "{record}", record); rows.String() != want || warnings != tc.warnings || probes != tc.probes {
				t.Fatalf("rows\n%s warnings=%d probes=%d\nwant\n%s warnings=%d probes=%d", &rows, warnings, probes, want, tc.warnings, tc.probes)
			}
		})
	}
}

func TestDoctorOutputAndToolRows(t *testing.T) {
	home := stateHome(t)
	settings := config.DefaultHarvester()
	var rows bytes.Buffer
	printOutputDoctor(&rows, settings)
	if want := "doctor: output maxResultBytes=50000 maxInlineChars=50000\n"; rows.String() != want {
		t.Fatalf("output row %q, want %q", &rows, want)
	}
	for _, tc := range []struct {
		name   string
		search func(*config.HarvesterSearch)
		want   string
	}{
		{"search off", func(s *config.HarvesterSearch) { s.Enabled = false },
			"doctor: tools new_session=read,download_file,search_literature search_web=not_listed reason=none (search.enabled is false)\n"},
		{"no backend yet", func(*config.HarvesterSearch) {},
			"doctor: tools new_session=read,download_file,search_literature search_web=not_listed reason=none (no managed backend yet)\n"},
		{"the operator's SearXNG", func(s *config.HarvesterSearch) { s.SearXNGURL = "http://127.0.0.1:9999" },
			"doctor: tools new_session=read,download_file,search_literature,search_web search_web=listed reason=search.searxngURL http://127.0.0.1:9999\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := config.DefaultHarvester()
			tc.search(&settings.Search)
			rows.Reset()
			printToolsDoctor(&rows, home, settings, searxng.Resolve(home, settings.Search))
			if rows.String() != tc.want {
				t.Fatalf("tools row %q, want %q", &rows, tc.want)
			}
		})
	}
}

func TestDoctorLogsRow(t *testing.T) {
	home := stateHome(t)
	dir := filepath.Join(home, ".local", "state", "harvester", "logs")
	check := func(want string, warnings int) {
		t.Helper()
		var rows bytes.Buffer
		got := printLogsDoctor(&rows, home)
		row := regexp.MustCompile(`\.doctor-write-\d+`).ReplaceAllString(rows.String(), ".doctor-write-N")
		if row != want || got != warnings {
			t.Fatalf("row  %q warnings=%d\nwant %q warnings=%d", row, got, want, warnings)
		}
	}
	check("doctor: logs dir="+dir+" absent (the first log write creates it)\n", 0)
	mustWrite(t, dir, "not a directory")
	check("doctor: logs dir="+dir+" writable=false error=not a directory hint=make "+dir+" writable by this user, or set PLUGIN_LOG_DIR to a writable directory\n", 1)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	check("doctor: logs dir="+dir+" writable=true\n", 0)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the write probe left %v behind (error %v)", entries, err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	check("doctor: logs dir="+dir+" writable=false error=open "+dir+"/.doctor-write-N: permission denied hint=make "+dir+" writable by this user, or set PLUGIN_LOG_DIR to a writable directory\n", 1)
}

func ptr[T any](value T) *T { return &value }

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
