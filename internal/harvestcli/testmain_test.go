package harvestcli

import (
	"os"
	"testing"

	"github.com/rezzminator/harvester/internal/ask"
	"github.com/rezzminator/harvester/internal/engine"
	claudeengine "github.com/rezzminator/harvester/internal/engine/claude"
	codexengine "github.com/rezzminator/harvester/internal/engine/codex"
	"github.com/rezzminator/harvester/internal/harvest"
	"github.com/rezzminator/harvester/internal/testjail"
)

// TestMain jails the package and registers the two ask runners cmd/harvester's
// main.go registers at startup, so `harvester ask` resolves them here too.
func TestMain(m *testing.M) {
	restore := harvest.StubPublicResolverForTest(harvest.RefusePublicLookupsForTest)
	ask.RegisterRunner(engine.Claude, claudeengine.AskRunner{})
	ask.RegisterRunner(engine.Codex, codexengine.AskRunner{})
	code := testjail.Run(m)
	restore()
	os.Exit(code)
}
