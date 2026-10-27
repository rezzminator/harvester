package paths

import (
	"path/filepath"
	"testing"
)

func TestStateRootsRespectXDGAndKeepInstallHomeSeparate(t *testing.T) {
	home := t.TempDir()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("PLUGIN_LOG_DIR", "")
	t.Setenv("HARVESTER_HOME", filepath.Join(home, "binaries"))
	if got := StateRoot(home); got != filepath.Join(state, "harvester") {
		t.Fatalf("state=%q", got)
	}
	if got := PythonRoot(home); got != filepath.Join(state, "harvester", "python") {
		t.Fatalf("python=%q", got)
	}
	for _, entry := range []struct{ actual, want string }{{DaemonSocket(home), filepath.Join(state, "harvester", "daemon.sock")}, {DaemonLock(home), filepath.Join(state, "harvester", "daemon.lock")}, {DaemonLog(home), filepath.Join(state, "harvester", "logs", "daemon.log")}} {
		if entry.actual != entry.want {
			t.Fatalf("daemon path=%q want=%q", entry.actual, entry.want)
		}
	}
	t.Setenv("XDG_STATE_HOME", "")
	if got := AskRoot(home); got != filepath.Join(home, ".local", "state", "harvester", "ask") {
		t.Fatalf("ask=%q", got)
	}
}

func TestLogDirIsPluginLogDirWhenSetElseTheStateLogs(t *testing.T) {
	home, state, shared := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("PLUGIN_LOG_DIR", shared)
	if got := LogDir(home); got != filepath.Join(shared, "harvester") {
		t.Fatalf("shared=%q", got)
	}
	if got := DaemonLog(home); got != filepath.Join(shared, "harvester", "daemon.log") {
		t.Fatalf("daemon log=%q", got)
	}
	t.Setenv("PLUGIN_LOG_DIR", " ")
	if got := LogDir(home); got != filepath.Join(state, "harvester", "logs") {
		t.Fatalf("fallback=%q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if got := LogDir(home); got != filepath.Join(home, ".local", "state", "harvester", "logs") {
		t.Fatalf("home fallback=%q", got)
	}
}
