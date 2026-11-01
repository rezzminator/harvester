package pluginlog

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecordKeepsARowOfAnUnknownLevel: a row whose level names no level file
// is written to warnings.jsonl with its level kept, never dropped.
func TestRecordKeepsARowOfAnUnknownLevel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var screen bytes.Buffer
	now := func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	logger := New("fixture", dir, &screen, now)
	logger.Record(Row{Level: "notice", Event: "fixture-event", Msg: "fixture message", Where: "fixture.go"})
	logger.Record(Row{Level: "warning", Event: "known-level", Msg: "a known level", Where: "fixture.go"})
	path := filepath.Join(dir, "warnings.jsonl")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (screen %q)", path, err, screen.String())
	}
	want := `{"ts":"2026-01-02T03:04:05.000Z","plugin":"fixture","level":"notice","event":"fixture-event","msg":"fixture message","where":"fixture.go"}` + "\n" +
		`{"ts":"2026-01-02T03:04:05.000Z","plugin":"fixture","level":"warning","event":"known-level","msg":"a known level","where":"fixture.go"}` + "\n"
	if string(got) != want {
		t.Fatalf("%s =\n%s\nwant\n%s\nscreen %q", path, got, want, screen.String())
	}
}
