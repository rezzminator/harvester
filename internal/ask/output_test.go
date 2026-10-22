package ask

import (
	"github.com/rezzminator/harvester/internal/engine"
	"testing"
)

func TestOutputPreservesSessionUsageAndRefusesIncompleteAnswers(t *testing.T) {
	output := processOutput{Stdout: `{"result":"answer","usage":{"input_tokens":1},"modelUsage":{"haiku":{"inputTokens":40,"outputTokens":5,"cacheReadInputTokens":7,"cacheCreationInputTokens":8},"sonnet":{"inputTokens":20,"outputTokens":3}}}`}
	if err := parseOutput(&output, engine.Claude); err != nil {
		t.Fatal(err)
	}
	if output.Usage == nil || *output.Usage != (TokenUsage{Input: 60, Output: 8, CachedInput: 7, CacheCreation: 8}) {
		t.Fatalf("usage=%+v", output.Usage)
	}
	for _, tc := range []struct {
		id  engine.ID
		raw string
	}{
		{engine.Claude, `{"result":"denied","is_error":true}`},
		{engine.Claude, `{"result":"answer","usage":{"input_tokens":-1}}`},
		{engine.Claude, `{"result":"answer","modelUsage":{"haiku":{"inputTokens":-1}}}`},
		{engine.Codex, `{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}`},
		{engine.Codex, "{\"type\":\"turn.completed\"}\n{\"type\":\"error\",\"message\":\"failed\"}"},
		{engine.Codex, `{"type":"turn.failed","error":{"message":"failed"}}`},
		{engine.Codex, `{"type":"turn.completed","usage":{"input_tokens":"bad"}}`},
		{engine.Codex, `{"unknown":"event"}`},
	} {
		output := processOutput{Stdout: tc.raw}
		if err := parseOutput(&output, tc.id); err == nil {
			t.Fatalf("accepted %s output %s", tc.id, tc.raw)
		}
	}
}
