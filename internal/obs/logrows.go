package obs

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rezzminator/harvester/internal/pluginlog"
)

// logRows is the activity log's destination: the io.Writer behind the JSON handler. Each write is one slog
// record, turned into a plugin-log row and handed to the vendored plugin-log library, which owns the level
// files, the row bound, rotation and the rows held across a failed write (plugins/CLAUDE.md § Logging).
type logRows struct {
	logger *pluginlog.Logger
	stderr io.Writer
}

// newLogRows writes harvester's rows under dir through the process's one library Logger for it.
func newLogRows(dir string, stderr io.Writer) *logRows {
	return &logRows{logger: pluginlog.For("harvester", dir, stderr), stderr: stderr}
}

// Write receives one encoded slog record. A record that does not decode is evidence, not absence: it is
// recorded as an error row of its own and the write fails.
func (rows *logRows) Write(line []byte) (int, error) {
	row, err := rowOf(line)
	if err != nil {
		err = fmt.Errorf("write plugin log row: %w", err)
		row = pluginlog.Row{Level: "error", Event: "log-record-invalid", Msg: err.Error(), Where: "internal/obs/logrows.go#logRows.Write"}
	}
	rows.logger.Record(row)
	if pluginlog.OnScreen(row.Level) {
		fmt.Fprintf(rows.stderr, "harvester: %s: %s\n", row.Event, row.Msg)
	}
	if err != nil {
		return 0, err
	}
	return len(line), nil
}

// rowOf turns a scrubbed slog JSON record into its row: the message is the event, an err field the msg,
// the component (else the command) the where, and every other field the detail.
func rowOf(line []byte) (pluginlog.Row, error) {
	fields := map[string]any{}
	if err := json.Unmarshal(line, &fields); err != nil {
		return pluginlog.Row{}, fmt.Errorf("decode record: %w", err)
	}
	text := func(key string) string {
		value, _ := fields[key].(string)
		delete(fields, key)
		return value
	}
	levelText, event, stamp := text("level"), text("msg"), text(FieldTime)
	level, ok := rowLevel(levelText)
	if !ok {
		return pluginlog.Row{}, fmt.Errorf("record %q has unknown level %q", event, levelText)
	}
	row := pluginlog.Row{Level: level, Event: event, Msg: event, Session: text("session"), Detail: fields}
	if failure := text(FieldErr); failure != "" {
		row.Msg = failure
	}
	if row.Where = text(FieldComp); row.Where == "" {
		row.Where, _ = fields[FieldCmd].(string)
	}
	// An unparsable time stays empty: the Logger stamps the writing time.
	if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
		row.TS = parsed.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return row, nil
}

// rowLevel maps a record's slog level text to the row level, each with its own level file.
func rowLevel(text string) (string, bool) {
	for _, level := range [][2]string{{"ERROR", "error"}, {"WARN", "warning"}, {"INFO", "info"}, {"DEBUG", "debug"}} {
		if strings.HasPrefix(text, level[0]) {
			return level[1], true
		}
	}
	return "", false
}
