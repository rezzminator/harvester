package ask

import (
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/engine"
	"strings"
	"testing"
)

func TestCheckModelFamilies(t *testing.T) {
	for _, tc := range []struct {
		id          engine.ID
		model, want string
	}{
		{engine.Claude, " HaIkU ", "haiku"}, {engine.Claude, "SONNET[1M]", "sonnet[1m]"}, {engine.Claude, "claude-future", "claude-future"}, {engine.Codex, "gpt-future", "gpt-future"},
		{engine.Claude, "gpt-future", ""}, {engine.Codex, "haiku", ""}, {engine.Codex, "codex-fixture", ""}, {engine.Claude, "sonnet[1m][1m]", ""},
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
	for _, input := range []AskInput{{}, {ContentFiles: []string{"file"}, Prompt: " "}, {ContentFiles: []string{" "}, Prompt: "Q"}, {ContentFiles: []string{"file"}, Prompt: "Q", SourceLabels: []string{"a", "b"}}} {
		if _, err := ResolveInput(input, config.Config{}); err == nil {
			t.Fatalf("accepted %+v", input)
		}
	}
}
