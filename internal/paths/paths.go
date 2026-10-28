// Package paths owns standalone harvester roots and environment overrides.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	EnvHome          = "HARVESTER_HOME_DIR"
	EnvLogLevel      = "HARVESTER_LOG_LEVEL"
	EnvLogComponents = "HARVESTER_LOG_COMPONENTS"
	// EnvPluginLogDir is the shared log root every Professor plugin writes under, in a directory named for the plugin.
	EnvPluginLogDir = "PLUGIN_LOG_DIR"
)

type Values struct {
	Home   string
	LogDir string
}

func Home() (string, error) {
	if h, ok := os.LookupEnv(EnvHome); ok {
		if strings.TrimSpace(h) == "" {
			return "", fmt.Errorf("%s is empty", EnvHome)
		}
		return filepath.Clean(h), nil
	}
	if testing.Testing() {
		return "", fmt.Errorf("refusing to resolve operator home inside tests: set %s", EnvHome)
	}
	return os.UserHomeDir()
}
func StateRoot(home string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "harvester")
}
func PythonRoot(home string) string        { return filepath.Join(StateRoot(home), "python") }
func AskRoot(home string) string           { return filepath.Join(StateRoot(home), "ask") }
func HarvesterCacheDir(home string) string { return filepath.Join(StateRoot(home), "cache") }
func Resolve() (Values, error) {
	home, err := Home()
	if err != nil {
		return Values{}, fmt.Errorf("resolve harvester home: %w", err)
	}
	return Values{Home: home, LogDir: LogDir(home)}, nil
}

func DaemonSocket(home string) string { return filepath.Join(StateRoot(home), "daemon.sock") }
func DaemonLock(home string) string   { return filepath.Join(StateRoot(home), "daemon.lock") }
func DaemonLog(home string) string    { return filepath.Join(LogDir(home), "daemon.log") }

// LogDir is where harvester logs: $PLUGIN_LOG_DIR/harvester when PLUGIN_LOG_DIR is set, else the state root's logs.
// Nothing else sets the location.
func LogDir(home string) string {
	if dir := strings.TrimSpace(os.Getenv(EnvPluginLogDir)); dir != "" {
		return filepath.Join(dir, "harvester")
	}
	return filepath.Join(StateRoot(home), "logs")
}
