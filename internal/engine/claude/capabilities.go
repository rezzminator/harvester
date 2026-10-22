package claude

import (
	"github.com/rezzminator/harvester/internal/ask"
	"github.com/rezzminator/harvester/internal/config"
)

type AskRunner struct{}

func (AskRunner) Resolve(machine config.Config) (ask.Engine, error) {
	return ask.ResolveClaude(machine)
}
