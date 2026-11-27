package daemon

import (
	"context"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/searxng"
)

// bootSearch runs once at every daemon start, beside the socket and never in
// its way; a variable so tests watch serve call it.
var bootSearch = ensureSearch

// newSearchManager builds the backend ensureSearch keeps; a variable so tests
// give it a fake Docker.
var newSearchManager = searxng.NewManager

// ensureSearch keeps the managed web-search backend up: an existing container
// is adopted or started at once, a host with none gets one provisioned, pull
// included, all logged in the daemon log. Without Docker it logs one row and
// stops; nothing retries before the next start. A backend that turns ready
// changes the daemon's identity, so the next call any session's proxy makes
// replaces this daemon with one that lists search_web for the sessions that
// start after it; running sessions keep the tools they listed. The check
// keeps a ready record whose container it finds but gets no answer from
// (KeepReady), so a backend that answers on and off never makes daemons
// replace each other back and forth.
func ensureSearch(ctx context.Context, runtime config.Runtime) {
	if !searxng.Wanted(runtime.Config.Harvester.Search) {
		return
	}
	logger := obs.Logger(ctx)
	manager, err := newSearchManager(runtime.Paths.Home, false)
	if err != nil {
		logger.Warn("harvester search backend not ensured", obs.FieldErr, err.Error(), "next", searxng.Hint)
		return
	}
	manager.KeepReady = true
	result := manager.Ensure(ctx)
	switch result.State {
	case searxng.StateReady:
		logger.Info("harvester search backend ready", "state", result.Summary())
	case searxng.StateNoDocker:
		logger.Info("harvester search backend unavailable", "reason", result.Detail, "next", searxng.Hint)
	default:
		logger.Warn("harvester search backend not ready", obs.FieldErr, result.Detail, "next", searxng.FailedHint)
	}
}
