package obs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CapBytes is the size a level file may pass before it moves aside to ".1"; RowMaxBytes bounds one row,
// its newline included (plugins/CLAUDE.md § Logging).
const (
	CapBytes    = 20971520
	RowMaxBytes = 4096
)

// levelFiles is the activity log's destination: the io.Writer behind the JSON handler. Each write is one
// slog record, turned into a plugin-log row and appended to the level file its level names, in the
// plugin's log directory. It keeps no handle: every append opens the file, takes an exclusive flock on it,
// confirms the path still names that file (another process may have moved it aside), rotates it when it
// is over the cap, and appends, so processes of one home share a directory without a lock sidecar.
type levelFiles struct {
	dir    string
	stderr io.Writer
	mutex  sync.Mutex
	// failing is set by the first failed write of a streak and cleared by the next success: the screen
	// hears about a broken log once per streak.
	failing bool
}

// wireRow is the row of plugins/CLAUDE.md § Logging, its keys in contract order.
type wireRow struct {
	TS      string         `json:"ts"`
	Plugin  string         `json:"plugin"`
	Level   string         `json:"level"`
	Event   string         `json:"event"`
	Msg     string         `json:"msg"`
	Where   string         `json:"where"`
	Session string         `json:"session,omitempty"`
	Detail  map[string]any `json:"detail"`
}

// levelTarget maps a record's slog level text to the row level and the level file: a debug record has no
// file of its own and goes to info.jsonl under its own level.
func levelTarget(text string) (level, file string, ok bool) {
	switch {
	case strings.HasPrefix(text, "ERROR"):
		return "error", "errors.jsonl", true
	case strings.HasPrefix(text, "WARN"):
		return "warning", "warnings.jsonl", true
	case strings.HasPrefix(text, "INFO"):
		return "info", "info.jsonl", true
	case strings.HasPrefix(text, "DEBUG"):
		return "debug", "info.jsonl", true
	}
	return "", "", false
}

// Write receives one encoded slog record. A record that does not decode is evidence, not absence: it is
// reported like any failed write.
func (files *levelFiles) Write(line []byte) (int, error) {
	files.mutex.Lock()
	defer files.mutex.Unlock()
	row, file, err := files.rowOf(line)
	if err == nil {
		err = files.appendRow(file, row)
	}
	if err != nil {
		wrapped := fmt.Errorf("write plugin log row: %w", err)
		files.fail(wrapped)
		return 0, wrapped
	}
	files.failing = false
	if row.Level == "error" {
		fmt.Fprintf(files.stderr, "harvester: %s: %s\n", row.Event, row.Msg)
	}
	return len(line), nil
}

// fail says once per failing streak that the log is failing.
func (files *levelFiles) fail(err error) {
	if files.failing {
		return
	}
	files.failing = true
	fmt.Fprintf(files.stderr, "harvester: plugin log %s: %v\n", files.dir, err)
}

// rowOf turns a scrubbed slog JSON record into its row: the message is the event, an err field the msg,
// the component (else the command) the where, and every other field the detail.
func (files *levelFiles) rowOf(line []byte) (wireRow, string, error) {
	fields := map[string]any{}
	if err := json.Unmarshal(line, &fields); err != nil {
		return wireRow{}, "", fmt.Errorf("decode record: %w", err)
	}
	text := func(key string) string {
		value, _ := fields[key].(string)
		delete(fields, key)
		return value
	}
	levelText, event, stamp := text("level"), text("msg"), text(FieldTime)
	level, file, ok := levelTarget(levelText)
	if !ok {
		return wireRow{}, "", fmt.Errorf("record %q has unknown level %q", event, levelText)
	}
	row := wireRow{Plugin: "harvester", Level: level, Event: event, Msg: event, Session: text("session"), Detail: fields}
	if failure := text(FieldErr); failure != "" {
		row.Msg = failure
	}
	if row.Where = text(FieldComp); row.Where == "" {
		row.Where, _ = fields[FieldCmd].(string)
	}
	if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
		row.TS = parsed.UTC().Format("2006-01-02T15:04:05.000Z")
	} else {
		row.TS = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return row, file, nil
}

// encodeRow marshals row, shortening it to RowMaxBytes: detail becomes a truncation marker and the msg is
// cut to what room is left.
func encodeRow(row wireRow) ([]byte, error) {
	raw, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", row.Event, err)
	}
	if len(raw)+1 <= RowMaxBytes {
		return append(raw, '\n'), nil
	}
	row.Detail = map[string]any{"truncated": true, "bytes": len(raw) + 1}
	runes := []rune(row.Msg)
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		row.Msg = string(runes[:middle])
		candidate, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("encode truncated %s: %w", row.Event, err)
		}
		if len(candidate)+1 <= RowMaxBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	row.Msg = string(runes[:low])
	if raw, err = json.Marshal(row); err != nil {
		return nil, fmt.Errorf("encode truncated %s: %w", row.Event, err)
	}
	if len(raw)+1 > RowMaxBytes {
		return nil, fmt.Errorf("encode %s: metadata exceeds %d bytes", row.Event, RowMaxBytes)
	}
	return append(raw, '\n'), nil
}

// appendRow appends row to the level file, then, if that file was moved aside at the cap, a log-capped row
// to warnings.jsonl.
func (files *levelFiles) appendRow(file string, row wireRow) error {
	raw, err := encodeRow(row)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(files.dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", files.dir, err)
	}
	capped, err := files.appendLine(file, raw)
	if err != nil || !capped {
		return err
	}
	notice, err := encodeRow(wireRow{
		TS: row.TS, Plugin: "harvester", Level: "warning", Event: "log-capped", Session: row.Session,
		Msg:    file + " reached the cap; previous rows moved to " + file + ".1",
		Where:  "internal/obs/levelfiles.go#appendRow",
		Detail: map[string]any{"file": file, "cap_bytes": CapBytes},
	})
	if err != nil {
		return err
	}
	// A notice never starts a rotation of its own: appendLine moves a file aside only when it is over the cap.
	_, err = files.appendLine("warnings.jsonl", notice)
	return err
}

// appendLine appends raw to the named level file under its flock, moving the file to ".1" first when it is
// over the cap; capped reports that move.
func (files *levelFiles) appendLine(name string, raw []byte) (capped bool, err error) {
	path := filepath.Join(files.dir, name)
	for attempt := 0; attempt < 3; attempt++ {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return capped, fmt.Errorf("open %s: %w", path, err)
		}
		done, moved, err := files.appendOpen(file, path, raw)
		err = errors.Join(err, file.Close())
		capped = capped || moved
		if err != nil || done {
			return capped, err
		}
	}
	return capped, fmt.Errorf("append %s: the file kept moving", path)
}

// appendOpen does one attempt on an open, flocked file: done once the row is written; not done when the
// path no longer names the file or the file was just moved aside, and the caller reopens.
func (files *levelFiles) appendOpen(file *os.File, path string, raw []byte) (done, moved bool, err error) {
	for {
		if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX); !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return false, false, fmt.Errorf("lock %s: %w", path, err)
	}
	defer func() {
		if unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("unlock %s: %w", path, unlockErr))
		}
	}()
	held, err := file.Stat()
	if err != nil {
		return false, false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if onDisk, statErr := os.Stat(path); statErr != nil || !os.SameFile(held, onDisk) {
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return false, false, fmt.Errorf("inspect %s: %w", path, statErr)
		}
		return false, false, nil
	}
	if held.Size() > CapBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return false, false, fmt.Errorf("move %s aside: %w", path, err)
		}
		return false, true, nil
	}
	written, err := file.Write(raw)
	if err == nil && written != len(raw) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return false, false, fmt.Errorf("append %s: %w", path, err)
	}
	return true, false, nil
}
