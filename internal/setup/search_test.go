package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/harvestpy"
	"github.com/rezzminator/harvester/internal/searxng"
)

const hint = "for web search, run harvester setup with Docker running, or set search.searxngURL or search.braveApiKey in harvester.config.json"

// TestSetupSearchStage: after conversion and before the daemon refresh, setup readies web search when
// the managed backend applies and provisions nothing otherwise; every outcome is one row, and setup
// succeeds whatever search does.
func TestSetupSearchStage(t *testing.T) {
	ready := searxng.Result{State: searxng.StateReady, Action: "created", Pulled: true, Count: 8,
		Record: searxng.Record{URL: "http://127.0.0.1:8888", Container: "harvester-searxng", Ready: true}}
	adopted := searxng.Result{State: searxng.StateReady, Action: "adopted", Count: 3,
		Record: searxng.Record{URL: "http://127.0.0.1:8888", Container: "harvester-searxng", Adopted: true, Ready: true}}
	on := config.HarvesterSearch{Enabled: true, Managed: true}
	for _, tc := range []struct {
		name   string
		search config.HarvesterSearch
		result searxng.Result
		row    string
		calls  int
	}{
		{name: "fresh host provisioned", search: on, result: ready, calls: 1,
			row: `setup: search ready backend=managed container=harvester-searxng url=http://127.0.0.1:8888 action="created" pulled=true results=8`},
		{name: "existing container adopted", search: on, result: adopted, calls: 1,
			row: `setup: search ready backend=adopted container=harvester-searxng url=http://127.0.0.1:8888 action="adopted" pulled=false results=3`},
		{name: "no Docker", search: on, calls: 1,
			result: searxng.Result{State: searxng.StateNoDocker, Detail: "Docker is not installed (no docker command: not found)"},
			row:    "setup: search unavailable: Docker is not installed (no docker command: not found); " + hint},
		{name: "backend not ready", search: on, calls: 1,
			result: searxng.Result{State: searxng.StateFailed, Detail: "no JSON search answered on http://127.0.0.1:8888 within 1m0s"},
			row: "setup: search not ready: no JSON search answered on http://127.0.0.1:8888 within 1m0s; " +
				"fix that and run harvester setup again, or set search.searxngURL or search.braveApiKey in harvester.config.json"},
		{name: "operator URL", search: config.HarvesterSearch{Enabled: true, Managed: true, SearXNGURL: "http://searx.internal:8080"},
			row: "setup: search backend=search.searxngURL; nothing provisioned"},
		{name: "brave key", search: config.HarvesterSearch{Enabled: true, Managed: true, BraveAPIKey: "key"},
			row: "setup: search backend=search.braveApiKey; nothing provisioned"},
		{name: "managed off", search: config.HarvesterSearch{Enabled: true},
			row: "setup: search backend=managed-off (search.managed is false); nothing provisioned"},
		{name: "search off", search: config.HarvesterSearch{Managed: true},
			row: "setup: search backend=disabled (search.enabled is false); nothing provisioned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, root := setupFixture(t)
			cfg.Config.Harvester.Search = tc.search
			ops := fixtureOperations(t, root, false)
			var events []string
			check := ops.check
			ops.check = func(ctx context.Context, root string, platform harvestpy.Platform) (harvestpy.CheckReport, error) {
				events = append(events, "check")
				return check(ctx, root, platform)
			}
			ops.search = func(_ context.Context, got config.Runtime, offline bool) searxng.Result {
				events = append(events, "search")
				if got.Paths.Home != cfg.Paths.Home || !offline {
					t.Errorf("search stage got home=%q offline=%t, want %q offline=true", got.Paths.Home, offline, cfg.Paths.Home)
				}
				return tc.result
			}
			ops.ensure = func(context.Context, config.Runtime) error {
				events = append(events, "ensure")
				return nil
			}
			var stdout strings.Builder
			if err := run(t.Context(), cfg, true, &stdout, ops); err != nil {
				t.Fatalf("setup failed on its search stage: %v", err)
			}
			out := stdout.String()
			if strings.Count(out, "setup: search ") != 1 || !strings.Contains(out, "\n"+tc.row+"\n") || !strings.HasSuffix(out, "daemon ready\n") {
				t.Fatalf("setup output:\n%s\nwant the one search row\n%s", out, tc.row)
			}
			want := "check check ensure"
			if tc.calls == 1 {
				want = "check check search ensure"
			}
			if got := strings.Join(events, " "); got != want {
				t.Fatalf("stage order %q, want %q", got, want)
			}
		})
	}
}
