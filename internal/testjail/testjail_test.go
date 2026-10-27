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
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "BASH_ENV", "HARVESTER_CONFIG", "HARVESTER_PYTHON_ROOT", "PLUGIN_LOG_DIR"} {
		if got := os.Getenv(name); got != "" {
			t.Errorf("%s leaked %q", name, got)
		}
	}
}
