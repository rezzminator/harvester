package ask

import (
	"github.com/rezzminator/harvester/internal/engine"
	"strings"
	"testing"
)

func TestRunnerForNamesUnsupportedEngine(t *testing.T) {
	if _, err := RunnerFor(engine.OpenCode); err == nil || !strings.Contains(err.Error(), "OpenCode does not support ask") {
		t.Fatalf("error=%v", err)
	}
}
