package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// contractKeys is plugins/CLAUDE.md § Logging's row, in order (session when known).
var contractKeys = []string{"ts", "plugin", "level", "event", "msg", "where", "detail"}

// openJail points the harvester home at a temp tree and returns the two places a log can land: the
// shared PLUGIN_LOG_DIR and the state fallback, with the plugin's older location variable set too.
func openJail(t *testing.T) (shared, state string) {
	t.Helper()
	root := t.TempDir()
	shared, state = filepath.Join(root, "shared"), filepath.Join(root, "state")
	t.Setenv("HARVESTER_HOME_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("PLUGIN_LOG_DIR", shared)
	t.Setenv("HARVESTER_LOG_LEVEL", "debug")
	t.Setenv("HARVESTER_LOG_MIRROR", "stderr")
	return shared, state
}

func readRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		row := map[string]any{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func keyOrder(t *testing.T, line string) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(line))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("row %q does not open an object: %v", line, err)
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, token.(string))
		var skipped json.RawMessage
		if err := decoder.Decode(&skipped); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestOpenLogWritesLevelFilesUnderPluginLogDir(t *testing.T) {
	shared, state := openJail(t)
	var stderr bytes.Buffer
	ctx, closeLog := OpenLog(context.Background(), Settings{Cmd: "harvester", Version: "1.0.0", Stderr: &stderr})
	Logger(ctx).Info("harvest.read", "count", 2)
	Logger(ctx).Warn("harvest.slow", "reason", "retry")
	Logger(ctx).Error("harvest.failed", "err", "upstream refused")
	closeLog(0)

	dir := filepath.Join(shared, "harvester")
	for name, wantEvent := range map[string]string{"info.jsonl": "harvest.read", "warnings.jsonl": "harvest.slow", "errors.jsonl": "harvest.failed"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode=%o", name, info.Mode().Perm())
		}
		rows := readRows(t, filepath.Join(dir, name))
		found := false
		for _, row := range rows {
			found = found || row["event"] == wantEvent
			if row["plugin"] != "harvester" {
				t.Errorf("%s plugin=%v", name, row["plugin"])
			}
		}
		if !found {
			t.Errorf("%s has no %s row: %v", name, wantEvent, rows)
		}
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if got := keyOrder(t, line); strings.Join(got, ",") != strings.Join(contractKeys, ",") {
				t.Errorf("%s keys=%v want %v", name, got, contractKeys)
			}
			if len(line)+1 > 4096 {
				t.Errorf("%s row is %d bytes", name, len(line)+1)
			}
		}
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(state, "harvester", "logs")); err == nil {
		t.Error("the state fallback was written although PLUGIN_LOG_DIR is set")
	}
	if got := stderr.String(); !strings.Contains(got, "upstream refused") || strings.Contains(got, "harvest.read") || strings.Contains(got, "harvest.slow") {
		t.Errorf("the screen shows only the error, got %q", got)
	}
}

func TestOpenLogFallsBackToTheStateLogs(t *testing.T) {
	_, state := openJail(t)
	t.Setenv("PLUGIN_LOG_DIR", "")
	_, closeLog := OpenLog(context.Background(), Settings{Cmd: "harvester", Version: "1.0.0", Stderr: &bytes.Buffer{}})
	closeLog(0)
	if rows := readRows(t, filepath.Join(state, "harvester", "logs", "info.jsonl")); len(rows) == 0 {
		t.Fatal("no fallback rows")
	}
}

func TestLevelFileOverTheCapMovesAsideWithALogCappedRow(t *testing.T) {
	shared, _ := openJail(t)
	dir := filepath.Join(shared, "harvester")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "info.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, "info.jsonl"), 20971521); err != nil {
		t.Fatal(err)
	}
	_, closeLog := OpenLog(context.Background(), Settings{Cmd: "harvester", Version: "1.0.0", Stderr: &bytes.Buffer{}})
	closeLog(0)
	if info, err := os.Stat(filepath.Join(dir, "info.jsonl.1")); err != nil || info.Size() != 20971521 {
		t.Fatalf("rotated file: %v %v", info, err)
	}
	rows := readRows(t, filepath.Join(dir, "warnings.jsonl"))
	if len(rows) == 0 || rows[0]["event"] != "log-capped" || rows[0]["level"] != "warning" {
		t.Fatalf("warnings=%v", rows)
	}
}

func TestOversizedRowIsShortenedToTheBound(t *testing.T) {
	shared, _ := openJail(t)
	ctx, closeLog := OpenLog(context.Background(), Settings{Cmd: "harvester", Version: "1.0.0", Stderr: &bytes.Buffer{}})
	Logger(ctx).Info("harvest.big", "reason", strings.Repeat("é", 2048), "target", strings.Repeat("x", 2048), "path", strings.Repeat("y", 2048))
	closeLog(0)
	raw, err := os.ReadFile(filepath.Join(shared, "harvester", "info.jsonl"))
	if err != nil || !strings.Contains(string(raw), "harvest.big") {
		t.Fatalf("the long row was not written: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if len(line)+1 > 4096 {
			t.Errorf("row is %d bytes", len(line)+1)
		}
	}
}

func TestUnwritableLogSaysItOncePerFailingStreak(t *testing.T) {
	shared, _ := openJail(t)
	if err := os.WriteFile(shared, []byte("a file where the directory should be"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	ctx, closeLog := OpenLog(context.Background(), Settings{Cmd: "harvester", Version: "1.0.0", Stderr: &stderr})
	Logger(ctx).Info("harvest.one")
	Logger(ctx).Info("harvest.two")
	if got := strings.Count(stderr.String(), "harvester: plugin log"); got != 1 {
		t.Fatalf("streak lines=%d: %q", got, stderr.String())
	}
	if err := os.Remove(shared); err != nil {
		t.Fatal(err)
	}
	Logger(ctx).Info("harvest.three")
	if err := os.RemoveAll(shared); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("blocked again"), 0o600); err != nil {
		t.Fatal(err)
	}
	Logger(ctx).Info("harvest.four")
	closeLog(0)
	if got := strings.Count(stderr.String(), "harvester: plugin log"); got != 2 {
		t.Fatalf("a success re-arms the line: %d: %q", got, stderr.String())
	}
}
