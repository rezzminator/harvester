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
	EnvLogMirror     = "HARVESTER_LOG_MIRROR"
)

type Values struct {
	Home    string
	LogFile string
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
	return Values{Home: home, LogFile: filepath.Join(StateRoot(home), "logs", "harvester.jsonl")}, nil
}

func DaemonSocket(home string) string { return filepath.Join(StateRoot(home), "daemon.sock") }
func DaemonLock(home string) string   { return filepath.Join(StateRoot(home), "daemon.lock") }
func DaemonLog(home string) string    { return filepath.Join(StateRoot(home), "logs", "daemon.log") }
