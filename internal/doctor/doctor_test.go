package doctor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/paths"
	"github.com/rezzminator/harvester/internal/testjail"
)

func TestMain(m *testing.M) { os.Exit(testjail.Run(m)) }

func TestDoctorJSONAndFailureExit(t *testing.T) {
	home := t.TempDir()
	runtime := config.Runtime{Paths: paths.Values{Home: home}, Config: config.Config{Harvester: config.DefaultHarvester()}}
	runtime.Config.Harvester.Search.Enabled = false
	runtime.Config.Harvester.Cache.Dir = filepath.Join(home, "cache")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--json", "--skip-conversion"}, &stdout, &stderr, runtime); code != 0 {
		t.Fatalf("code=%d stderr=%s rows=%s", code, &stderr, &stdout)
	}
	var report struct {
		Healthy bool
		Rows    []string
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || !report.Healthy {
		t.Fatalf("report=%s error=%v", &stdout, err)
	}
	if !strings.Contains(strings.Join(report.Rows, "\n"), "skipped (--skip-conversion)") {
		t.Fatal(report.Rows)
	}
	if err := os.WriteFile(runtime.Config.Harvester.Cache.Dir, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime.Config.Harvester.Cache.Dir = filepath.Join(runtime.Config.Harvester.Cache.Dir, "nested")
	stdout.Reset()
	if code := Run([]string{"--json", "--skip-conversion"}, &stdout, &stderr, runtime); code != 1 || !strings.Contains(stdout.String(), "error=") {
		t.Fatalf("code=%d report=%s", code, &stdout)
	}
}

func TestCacheRowsRetainPersistentEntriesAndNeverTTL(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "item.md"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultHarvester()
	settings.Cache.Dir, settings.Cache.TTL = root, 0
	var rows bytes.Buffer
	if warnings := printHarvestCacheDoctor(&rows, settings); warnings != 0 || !strings.Contains(rows.String(), "entries=1 ttl=never") {
		t.Fatalf("warnings=%d rows=%s", warnings, &rows)
	}
}

func TestSearchDisabledRow(t *testing.T) {
	settings := config.DefaultHarvester()
	settings.Search.Enabled = false
	var rows bytes.Buffer
	if warnings := printHarvestSearchDoctor(t.Context(), &rows, settings); warnings != 0 || !strings.Contains(rows.String(), "state=off") {
		t.Fatalf("warnings=%d rows=%s", warnings, &rows)
	}
}
