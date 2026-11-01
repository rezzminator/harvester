package harvestcli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// harvestLocal runs `harvester` over args with a fresh local text file
// appended, returning the exit code, stdout and stderr.
func harvestLocal(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	runtime := downloadRuntime(t)
	source := filepath.Join(runtime.Paths.Home, "plain.txt")
	if err := os.WriteFile(source, []byte("plain harvest body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Harvest(append(args, source), &stdout, &stderr, runtime)
	return code, stdout.String(), stderr.String()
}

// TestHarvestPrintsTheReadToolsBlock: harvester prints each source as the
// read tool's block — its header, its artifact's path, then the content, or
// with --include-content=false the size line in place of the body.
func TestHarvestPrintsTheReadToolsBlock(t *testing.T) {
	sizeLine := regexp.MustCompile(`^size: 18 chars$`)
	for _, test := range []struct {
		name string
		args []string
		last func(string) bool
	}{
		{"content", nil, func(line string) bool { return line == "plain harvest body" }},
		{"size only", []string{"--include-content=false"}, sizeLine.MatchString},
	} {
		code, stdout, stderr := harvestLocal(t, test.args...)
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		if code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "=== [1/1] ") ||
			!strings.HasSuffix(lines[0], "plain.txt") || !filepath.IsAbs(lines[1]) || !test.last(lines[2]) {
			t.Errorf("%s: code=%d stdout=%q stderr=%q, want the header, path and %s line", test.name, code, stdout,
				stderr, test.name)
		}
	}
}

// TestHarvestReadsAShortLocalFileThatNamesACaptcha: a short file whose text
// names a captcha is the person's own content, never a challenge wall (only
// the fetch ladder judges a wall, and a local read has none): its text is
// printed and the read exits 0.
func TestHarvestReadsAShortLocalFileThatNamesACaptcha(t *testing.T) {
	runtime := downloadRuntime(t)
	source := filepath.Join(runtime.Paths.Home, "challenge.txt")
	if err := os.WriteFile(source, []byte("Please complete the captcha to continue.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Harvest([]string{source}, &stdout, &stderr, runtime)
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if code != 0 || len(lines) != 3 || lines[2] != "Please complete the captcha to continue." {
		t.Fatalf("code=%d stdout=%q stderr=%q, want the file's text and exit 0", code, stdout.String(), stderr.String())
	}
}

// TestHarvestOCRLanguageIsValidatedByName: --ocr-language takes a staged
// script and refuses anything else naming the flag, before any read.
func TestHarvestOCRLanguageIsValidatedByName(t *testing.T) {
	if code, stdout, stderr := harvestLocal(t, "--ocr-language", "ja"); code != 0 {
		t.Fatalf("--ocr-language ja code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr := harvestLocal(t, "--ocr-language", "klingon")
	if code != 2 || !strings.Contains(stderr, "--ocr-language") || !strings.Contains(stderr, "klingon") ||
		stdout != "" {
		t.Fatalf("--ocr-language klingon code=%d stdout=%q stderr=%q, want 2 naming the flag", code, stdout, stderr)
	}
}

// TestHarvestDownloadVerbPointsAtDownloadFile keeps the CLI's recovery hint
// for callers who use the download verb.
func TestHarvestDownloadVerbPointsAtDownloadFile(t *testing.T) {
	code, stdout, stderr := harvestLocal(t, "download")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "download-file") {
		t.Fatalf(
			"harvest download: code=%d stdout=%q stderr=%q, want exit 2 and the download-file hint",
			code,
			stdout,
			stderr,
		)
	}
}
