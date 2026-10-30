package ask

import (
	"fmt"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/engine"
)

type Runner interface {
	Resolve(machine config.Config) (Engine, error)
}

var runners = map[engine.ID]Runner{}

func RegisterRunner(id engine.ID, runner Runner) {
	if _, duplicate := runners[id]; duplicate {
		panic(fmt.Sprintf("ask: runner for engine %q registered twice", id))
	}
	runners[id] = runner
}

func RunnerFor(id engine.ID) (Runner, error) {
	runner, ok := runners[id]
	if !ok {
		if descriptor, err := engine.Lookup(id); err == nil {
			return nil, fmt.Errorf("%s does not support ask", descriptor.Short)
		}
		return nil, fmt.Errorf("engine %s: no ask runner registered", id)
	}
	return runner, nil
}
