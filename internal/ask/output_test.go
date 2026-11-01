package ask

import (
	"reflect"
	"testing"

	"github.com/rezzminator/harvester/internal/engine"
)

func TestOutputPreservesSessionUsageAndRefusesIncompleteAnswers(t *testing.T) {
	output := processOutput{Stdout: `{"result":"answer","usage":{"input_tokens":1},"modelUsage":{"haiku":{"inputTokens":40,"outputTokens":5,"cacheReadInputTokens":7,"cacheCreationInputTokens":8},"sonnet":{"inputTokens":20,"outputTokens":3}}}`}
	if err := parseOutput(&output, engine.Claude); err != nil {
		t.Fatal(err)
	}
	if output.Usage == nil || *output.Usage != (TokenUsage{Input: 60, Output: 8, CachedInput: 7, CacheCreation: 8}) {
		t.Fatalf("usage=%+v", output.Usage)
	}
	// A field that only labels the receipt (usage, cost) or a line that is no event never costs
	// the answer: the answer is kept, the unread part named in Warnings.
	for _, tc := range []struct {
		id       engine.ID
		raw      string
		usage    *TokenUsage
		warnings []string
	}{
		{engine.Claude, `{"result":"answer","usage":{"input_tokens":-1},"total_cost_usd":-3}`, nil,
			[]string{"parse Claude usage: input_tokens must be nonnegative; usage unknown"}},
		{engine.Claude, `{"result":"answer","usage":{"input_tokens":4},"modelUsage":{"haiku":{"inputTokens":-1}}}`, nil,
			[]string{"parse Claude modelUsage: modelUsage haiku: negative token count; usage unknown"}},
		{engine.Claude, `{"result":"answer","total_cost_usd":"bad"}`, nil, nil},
		{engine.Codex, "WARNING: proceeding\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"answer\"}}\n{\"note\":1}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":\"bad\"}}",
			nil, []string{
				"skip Codex JSONL line 1: invalid character 'W' looking for beginning of value",
				"skip Codex event 3: it has no type",
				"parse Codex usage: input_tokens: json: cannot unmarshal string into Go value of type int; usage unknown",
			}},
	} {
		output := processOutput{Stdout: tc.raw}
		if err := parseOutput(&output, tc.id); err != nil || output.Answer != "answer" ||
			!reflect.DeepEqual(output.Usage, tc.usage) || !reflect.DeepEqual(output.Warnings, tc.warnings) {
			t.Errorf("%s output %s: err=%v answer=%q usage=%+v warnings=%q; want answer, usage %+v, warnings %q",
				tc.id, tc.raw, err, output.Answer, output.Usage, output.Warnings, tc.usage, tc.warnings)
		}
	}
	for _, tc := range []struct {
		id  engine.ID
		raw string
	}{
		{engine.Claude, `{"result":"denied","is_error":true}`},
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
