package harvestcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rezzminator/harvester/internal/harvest"
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

// TestHarvestPagesALongSourceByOffset: harvester applies the read tool's
// answer budget; --json names the offset that continues a cut source
// (next_offset) and keeps chars, and following --offset to the end
// reproduces the source's whole text; --offset with two sources is refused
// by name.
func TestHarvestPagesALongSourceByOffset(t *testing.T) {
	runtime := downloadRuntime(t)
	runtime.Config.Harvester.Output.MaxResultBytes = 700
	var body strings.Builder
	for line := range 60 {
		fmt.Fprintf(&body, "Reading %d: Ünïcödé — 日本語 ✓ 🌊\n", line)
	}
	source := filepath.Join(runtime.Paths.Home, "long.txt")
	if err := os.WriteFile(source, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(body.String())
	var stdout, stderr bytes.Buffer
	if code := Harvest([]string{source}, &stdout, &stderr, runtime); code != 0 ||
		len(stdout.String()) > 701 || !strings.Contains(stdout.String(), "\ntruncated: chars 0-") {
		t.Fatalf("text read code=%d %d bytes stdout=%q stderr=%q", code, len(stdout.String()), stdout.String(), stderr.String())
	}
	var got strings.Builder
	offset, pages := 0, 0
	for {
		stdout.Reset()
		stderr.Reset()
		if code := Harvest([]string{source, "--json", "--offset", strconv.Itoa(offset)}, &stdout, &stderr, runtime); code != 0 {
			t.Fatalf("--offset %d code=%d stderr=%q", offset, code, stderr.String())
		}
		var results []struct {
			Content    string `json:"content"`
			Chars      int    `json:"chars"`
			Offset     int    `json:"offset"`
			NextOffset *int   `json:"next_offset"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &results); err != nil || len(results) != 1 {
			t.Fatalf("--offset %d JSON %v: %s", offset, err, stdout.String())
		}
		result := results[0]
		if result.Chars != utf8.RuneCountInString(want) || result.Offset != offset {
			t.Fatalf("--offset %d: chars %d offset %d, want %d and %d", offset, result.Chars, result.Offset,
				utf8.RuneCountInString(want), offset)
		}
		pages++
		if result.NextOffset == nil {
			got.WriteString(strings.TrimRight(result.Content, " \t\r\n"))
			break
		}
		content, cut := strings.CutSuffix(result.Content, harvest.InlineTruncationNote)
		if !cut || *result.NextOffset <= offset {
			t.Fatalf("--offset %d: next_offset %d, content %q", offset, *result.NextOffset, result.Content)
		}
		got.WriteString(content)
		offset = *result.NextOffset
	}
	if got.String() != want || pages < 3 {
		t.Fatalf("%d pages concatenated:\n%q\nwant:\n%q", pages, got.String(), want)
	}
	code, stdout2, stderr2 := harvestLocal(t, "--offset", "4", source)
	if code != 2 || !strings.Contains(stderr2, "--offset takes exactly one source") || stdout2 != "" {
		t.Fatalf("--offset with two sources code=%d stdout=%q stderr=%q, want 2 naming the flag", code, stdout2, stderr2)
	}
	// The floor: a budget the headers alone fill shows no page, so no
	// next_offset sends a JSON client back to the same call.
	runtime.Config.Harvester.Output.MaxResultBytes = 1
	stdout.Reset()
	stderr.Reset()
	if code := Harvest([]string{source, "--json"}, &stdout, &stderr, runtime); code != 0 {
		t.Fatalf("withheld read code=%d stderr=%q", code, stderr.String())
	}
	var withheld []struct {
		Content    string `json:"content"`
		Chars      int    `json:"chars"`
		NextOffset *int   `json:"next_offset"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &withheld); err != nil || len(withheld) != 1 ||
		withheld[0].Chars != utf8.RuneCountInString(want) || withheld[0].Content != "" || withheld[0].NextOffset != nil {
		t.Fatalf("withheld read JSON %v: %s, want chars %d, no content and no next_offset", err, stdout.String(),
			utf8.RuneCountInString(want))
	}
}
