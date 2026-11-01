package harvestmcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/harvest"
	"github.com/rezzminator/harvester/internal/harvestpy"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/testjail"
)

// inflateWorker stands in for converter.py's inflate op: a body starting
// "bomb" is a DecompressionBomb, a good one is written to out as "inner:<body>".
// A conversion request answers "converted:<body>".
const inflateWorker = `
import json, pathlib, sys
for line in sys.stdin:
    req = json.loads(line)
    if req.get("op") == "smoke":
        print(json.dumps({"ok": True, "runtime": "test"}), flush=True)
        continue
    body = pathlib.Path(req["path"]).read_bytes()
    if req.get("op") != "inflate":
        print(json.dumps({"ok": True, "markdown": "converted:" + body.decode()}), flush=True)
        continue
    if body.startswith(b"bomb"):
        print(json.dumps({"ok": False, "error_class": "DecompressionBomb",
                          "error": "DecompressionBomb: more than %d bytes" % req["limit"]}), flush=True)
        continue
    pathlib.Path(req["out"]).write_bytes(b"inner:" + body)
    print(json.dumps({"ok": True}), flush=True)
`

// TestInflateHandsTheBodyToTheSidecarAndRemovesTheScratch: pythonConverter
// (harvest.Inflater) writes the body to a private scratch, answers the inner
// bytes the sidecar wrote, maps its cap to harvest.ErrDecompressionBomb, and
// leaves no scratch behind either way.
func TestInflateHandsTheBodyToTheSidecarAndRemovesTheScratch(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	converter := fakeSidecar(t)

	inner, err := converter.Inflate(context.Background(), "xz", []byte("notes"), 4096)
	if err != nil || string(inner) != "inner:notes" {
		t.Errorf("Inflate = %q, %v; want the sidecar's inner bytes", inner, err)
	}
	_, err = converter.Inflate(context.Background(), "xz", []byte("bomb"), 4096)
	if !errors.Is(err, harvest.ErrDecompressionBomb) || !strings.Contains(err.Error(), "more than 4096 bytes") {
		t.Errorf("bomb: err = %v, want harvest.ErrDecompressionBomb naming the sidecar's cap", err)
	}
	// harvester-pycache is the sidecar's fixed bytecode home under TMPDIR
	// (harvestpy withPythonBytecodeHome), not inflate scratch.
	left, err := os.ReadDir(scratch)
	left = slices.DeleteFunc(left, func(entry os.DirEntry) bool { return entry.Name() == "harvester-pycache" })
	if err != nil || len(left) != 0 {
		t.Errorf("scratch left behind: %v, %v; want none", left, err)
	}
}

// fakeSidecar is a pythonConverter over inflateWorker.
func fakeSidecar(t *testing.T) pythonConverter {
	t.Helper()
	dir := t.TempDir()
	python := filepath.Join(dir, "fake-python")
	launcher := "#!/bin/sh\nexec python3 -c '" + strings.ReplaceAll(inflateWorker, "'", "'\\''") + "'\n"
	if err := testjail.WriteExecutable(python, []byte(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "converter.py")
	if err := os.WriteFile(script, harvestpy.ConverterSource(), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := harvestpy.NewConverter(harvestpy.Runtime{Python: python, Script: script})
	t.Cleanup(func() { _ = worker.Close() })
	return pythonConverter{worker: worker}
}

// TestScratchCleanupFailureKeepsTheResult: a conversion or an inflate whose
// scratch directory cannot be removed answers its result, and the leftover is
// logged at warn naming the scratch.
func TestScratchCleanupFailureKeepsTheResult(t *testing.T) {
	ctx, recorder := obs.Test(t)
	t.Setenv("TMPDIR", t.TempDir()) // the scratch this test leaves is the test's own
	converter := fakeSidecar(t)
	restore := scratchRemoveAll
	scratchRemoveAll = func(string) error { return errors.New("device busy") }
	t.Cleanup(func() { scratchRemoveAll = restore })
	for _, tc := range []struct {
		label, want string
		run         func() (string, error)
	}{
		{"conversion", "converted:<p>notes</p>", func() (string, error) {
			return converter.Convert(ctx, "html", "https://fixture.example/page.html", []byte("<p>notes</p>"))
		}},
		{"inflate", "inner:notes", func() (string, error) {
			inner, err := converter.Inflate(ctx, "xz", []byte("notes"), 4096)
			return string(inner), err
		}},
	} {
		got, err := tc.run()
		if err != nil || got != tc.want {
			t.Errorf("%s with an unremovable scratch = %q, %v; want %q and no error", tc.label, got, err, tc.want)
		}
		logged := false
		for _, record := range recorder.Records() {
			if op, _ := record.Field("op"); record.Message == "harvester.scratch.remove" && op == tc.label {
				cause, _ := record.Field(obs.FieldErr)
				logged = record.Level == "WARN" && cause == "device busy"
			}
		}
		if !logged {
			t.Errorf("%s: no warn record naming the leftover scratch; log: %s", tc.label, recorder.Raw())
		}
	}
}
