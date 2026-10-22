package claude

import (
	"github.com/rezzminator/harvester/internal/config"
	"strings"
	"testing"
)

func TestAskRunnerNamesMissingBinary(t *testing.T) {
	machine := config.Config{Claude: config.Claude{Binary: "/missing/claude"}}
	if _, err := (AskRunner{}).Resolve(machine); err == nil || !strings.Contains(err.Error(), "binary MISSING") {
		t.Fatalf("error=%v", err)
	}
}
