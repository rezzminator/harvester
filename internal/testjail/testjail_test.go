package testjail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Run(m)) }
func TestRunSeparatesOperatorState(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" || !strings.HasPrefix(filepath.Base(home), "harvester-test-") || os.Getenv("HARVESTER_HOME_DIR") != home {
		t.Fatalf("jail home=%q override=%q", home, os.Getenv("HARVESTER_HOME_DIR"))
	}
	for name, suffix := range map[string]string{"XDG_CONFIG_HOME": ".config", "XDG_STATE_HOME": filepath.Join(".local", "state"), "XDG_CACHE_HOME": ".cache"} {
		if got := os.Getenv(name); got != filepath.Join(home, suffix) {
			t.Errorf("%s=%q", name, got)
		}
	}
	if got := os.Getenv("HARVESTER_INSTALL_PING_OFFLINE"); got != "1" {
		t.Errorf("HARVESTER_INSTALL_PING_OFFLINE=%q, want 1 so no process the jail starts sends the install count", got)
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "BASH_ENV", "HARVESTER_CONFIG", "HARVESTER_PYTHON_ROOT", "PLUGIN_LOG_DIR"} {
		if got := os.Getenv(name); got != "" {
			t.Errorf("%s leaked %q", name, got)
		}
	}
}

// TestTrustExecutableTakesGroupAndWorldWriteOff: the daemon starts only a
// binary neither group nor world may write, whatever umask linked it.
func TestTrustExecutableTakesGroupAndWorldWriteOff(t *testing.T) {
	for _, test := range []struct{ linked, want os.FileMode }{
		{0o775, 0o755}, {0o777, 0o755}, {0o755, 0o755}, {0o700, 0o700},
	} {
		path := filepath.Join(t.TempDir(), "binary")
		if err := WriteExecutable(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, test.linked); err != nil {
			t.Fatal(err)
		}
		if err := TrustExecutable(path); err != nil {
			t.Fatalf("TrustExecutable(%s linked %o): %v", path, test.linked, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != test.want {
			t.Errorf("%s linked %o: mode %o, want %o", path, test.linked, got, test.want)
		}
	}
}
