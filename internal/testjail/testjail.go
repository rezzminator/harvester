// Package testjail gives every plugin test process an isolated home.
package testjail

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func Run(m *testing.M) int {
	root, err := os.MkdirTemp("", "harvester-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testjail:", err)
		return 1
	}
	defer os.RemoveAll(root)
	for name, value := range map[string]string{"HOME": root, "HARVESTER_HOME_DIR": root, "XDG_CONFIG_HOME": filepath.Join(root, ".config"), "XDG_STATE_HOME": filepath.Join(root, ".local", "state"), "XDG_CACHE_HOME": filepath.Join(root, ".cache"), "CLAUDE_CONFIG_DIR": "", "CODEX_HOME": "", "BASH_ENV": "", "HARVESTER_CONFIG": "", "HARVESTER_PYTHON_ROOT": "", "PLUGIN_LOG_DIR": ""} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintln(os.Stderr, "testjail:", name, err)
			return 1
		}
	}
	return m.Run()
}
