package harvestpy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOCRSetupGuidanceAtRuntime(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "converter.py"), ConverterSource(), 0o600); err != nil {
		t.Fatal(err)
	}
	const probe = `
import pathlib, sys
sys.path.insert(0, sys.argv[1])
import converter as c
try:
    if sys.argv[2] == 'unconfigured':
        c._MODEL_ROOT = ''
        c._model_root()
    else:
        c.require_staged(pathlib.Path(sys.argv[1]), 'latin')
except c.OCRModelsNotStaged as exc:
    assert sys.argv[3] in str(exc), str(exc)
else:
    raise AssertionError('missing OCR models must produce a named failure')
`
	for _, mode := range []string{"unconfigured", "unstaged"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(python, "-c", probe, dir, mode, "run `harvester setup`")
			cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("OCR recovery guidance: %v\n%s", err, out)
			}
		})
	}
}
