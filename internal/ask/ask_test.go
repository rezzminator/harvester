package ask

import (
	"fmt"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/engine"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckModelFamilies(t *testing.T) {
	for _, tc := range []struct {
		id          engine.ID
		model, want string
	}{
		{engine.Claude, " HaIkU ", "haiku"}, {engine.Claude, "SONNET[1M]", "sonnet[1m]"}, {engine.Claude, "claude-future", "claude-future"}, {engine.Codex, "gpt-future", "gpt-future"},
		// An id of neither family goes to the engine, which judges it.
		{engine.Codex, "o4-mini", "o4-mini"}, {engine.Codex, "codex-mini-latest", "codex-mini-latest"}, {engine.Claude, "sonnet[1m][1m]", "sonnet[1m][1m]"},
		{engine.Claude, "gpt-future", ""}, {engine.Codex, "haiku", ""}, {engine.Codex, "claude-future", ""},
	} {
		got, err := CheckModel(tc.id, tc.model)
		if tc.want == "" {
			if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(tc.model)) {
				t.Fatalf("CheckModel(%s,%q)=%q,%v", tc.id, tc.model, got, err)
			}
		} else if err != nil || got != tc.want {
			t.Fatalf("CheckModel(%s,%q)=%q,%v want %q", tc.id, tc.model, got, err, tc.want)
		}
	}
}
func TestResolveInputAndEvidencePrompt(t *testing.T) {
	input, err := ResolveInput(AskInput{ContentFiles: []string{"prepared.md"}, SourceLabels: []string{"https://example.test/source"}, Prompt: "Find evidence"}, config.Config{Ask: config.AskConfig{Engine: engine.Claude, Prefs: map[engine.ID]config.EnginePrefs{engine.Claude: {Model: "Haiku", Effort: "low"}}}})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := BuildPrompt(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1. prepared.md — source: https://example.test/source\nTASK: Find evidence", "no network access, no other files.", `section titled exactly "EVIDENCE"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q: %s", want, prompt)
		}
	}
	if input.Model != "haiku" || input.Effort != "low" {
		t.Fatalf("input=%+v", input)
	}
	// An ask's deadline grows with its content files and their size, up to a ceiling; one the caller set stands.
	prepared := t.TempDir()
	for _, tc := range []struct {
		files   int
		bytes   int // each file's size
		timeout time.Duration
		want    time.Duration
	}{
		{1, 0, 0, 150 * time.Second}, {15, 0, 0, 570 * time.Second}, {16, 0, 0, 10 * time.Minute}, {17, 0, 0, 10 * time.Minute},
		{1, 1023, 0, 150 * time.Second}, {1, 1024, 0, 151 * time.Second}, {1, 130701, 0, 277 * time.Second},
		{2, 130701, 0, 434 * time.Second}, {4, 130701, 0, 10 * time.Minute},
		{1, 130701, 5 * time.Second, 5 * time.Second},
	} {
		files := make([]string, tc.files)
		for index := range files {
			files[index] = filepath.Join(prepared, fmt.Sprintf("prepared-%d-%d.md", tc.bytes, index+1))
			if err := os.WriteFile(files[index], make([]byte, tc.bytes), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		resolved, err := ResolveInput(AskInput{ContentFiles: files, Prompt: "Find evidence", Engine: engine.Claude, Timeout: tc.timeout}, config.Config{})
		if err != nil || resolved.Timeout != tc.want {
			t.Fatalf("ResolveInput(%d files of %d bytes, timeout %s).Timeout = %s, %v; want %s", tc.files, tc.bytes, tc.timeout, resolved.Timeout, err, tc.want)
		}
	}
	for _, input := range []AskInput{{}, {ContentFiles: []string{"file"}, Prompt: " "}, {ContentFiles: []string{" "}, Prompt: "Q"}, {ContentFiles: []string{"file"}, Prompt: "Q", SourceLabels: []string{"a", "b"}}} {
		if _, err := ResolveInput(input, config.Config{}); err == nil {
			t.Fatalf("accepted %+v", input)
		}
	}
}
