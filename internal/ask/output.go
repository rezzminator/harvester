package ask

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rezzminator/harvester/internal/engine"
)

type claudeEnvelope struct {
	Result     json.RawMessage `json:"result"`
	Usage      json.RawMessage `json:"usage"`
	ModelUsage json.RawMessage `json:"modelUsage"`
	IsError    bool            `json:"is_error"`
}

// parseModelUsage sums Claude's per-model `modelUsage` totals — the whole session, every API
// turn. The envelope's `usage` block is the LAST turn only: a seat that took a second turn (a
// structured-output retry, a tool call) reported ~2k input tokens where the session spent 60k. Nil
// when the block is absent or empty.
func parseModelUsage(raw json.RawMessage) (*TokenUsage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var models map[string]struct {
		Input         int `json:"inputTokens"`
		Output        int `json:"outputTokens"`
		CacheRead     int `json:"cacheReadInputTokens"`
		CacheCreation int `json:"cacheCreationInputTokens"`
	}
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, nil
	}
	usage := &TokenUsage{}
	for name, m := range models {
		if m.Input < 0 || m.Output < 0 || m.CacheRead < 0 || m.CacheCreation < 0 {
			return nil, fmt.Errorf("modelUsage %s: negative token count", name)
		}
		usage.Input += m.Input
		usage.Output += m.Output
		usage.CachedInput += m.CacheRead
		usage.CacheCreation += m.CacheCreation
	}
	return usage, nil
}

func parseOutput(result *processOutput, id engine.ID) error {
	switch id {
	case engine.Claude:
		var envelope claudeEnvelope
		if err := json.Unmarshal([]byte(result.Stdout), &envelope); err != nil {
			return fmt.Errorf("parse Claude JSON envelope: %w", err)
		}
		result.IsError = envelope.IsError
		if len(envelope.Result) > 0 && string(envelope.Result) != "null" {
			if err := json.Unmarshal(envelope.Result, &result.Answer); err != nil {
				return fmt.Errorf("parse Claude result: %w", err)
			}
		}
		// Token counts only label the receipt: one that does not parse leaves Usage unknown, never
		// costs the answer.
		var err error
		if result.Usage, err = parseTokenUsage(envelope.Usage); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("parse Claude usage: %v; usage unknown", err))
		}
		if whole, err := parseModelUsage(envelope.ModelUsage); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("parse Claude modelUsage: %v; usage unknown", err))
			result.Usage = nil
		} else if whole != nil {
			result.Usage = whole // every turn, not the last one
		}
		if result.IsError {
			return fmt.Errorf("headless envelope reported an error for Claude")
		}
	case engine.Codex:
		if err := parseCodexJSONL(result); err != nil {
			return err
		}
	default:
		return fmt.Errorf("engine %s does not support normalized output", id)
	}
	if strings.TrimSpace(result.Answer) == "" {
		return fmt.Errorf("%s headless output contains no answer", id)
	}
	return nil
}

func parseCodexJSONL(result *processOutput) error {
	var terminal bool
	for lineNo, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Type  string          `json:"type"`
			Item  json.RawMessage `json:"item"`
			Usage json.RawMessage `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		// A line that is no event (a warning, a banner) or an event with no type costs that line:
		// the terminal event and the answer may still follow.
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("skip Codex JSONL line %d: %v", lineNo+1, err))
			continue
		}
		if event.Type == "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("skip Codex event %d: it has no type", lineNo+1))
			continue
		}
		switch event.Type {
		case "error":
			// Codex uses error events for retries as well as failures. Only a
			// subsequent terminal success proves that the engine recovered.
			if terminal {
				result.Answer = ""
			}
			terminal = false
		case "turn.failed", "thread.failed":
			return fmt.Errorf("headless event %s from Codex: %s", event.Type, event.Error)
		case "turn.started":
			terminal = false
			result.Answer = ""
		case "turn.completed":
			if len(event.Error) != 0 && string(event.Error) != "null" {
				return fmt.Errorf("turn.completed event from Codex contains an error")
			}
			terminal = true
			var err error
			if result.Usage, err = parseTokenUsage(event.Usage); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("parse Codex usage: %v; usage unknown", err))
			}
		case "item.completed":
			var item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			// An item that does not decode costs that item, like a line that is no event.
			if err := json.Unmarshal(event.Item, &item); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("skip Codex item in event %d: %v", lineNo+1, err))
				continue
			}
			if item.Type == "agent_message" {
				result.Answer = item.Text
			}
		}
	}
	if !terminal {
		return fmt.Errorf("missing terminal success event in headless output from Codex")
	}
	if strings.TrimSpace(result.Answer) == "" {
		return fmt.Errorf("headless output from Codex contains no completed answer")
	}
	return nil
}

func parseTokenUsage(raw json.RawMessage) (*TokenUsage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	usage := &TokenUsage{}
	known := false
	for _, field := range []struct {
		names  []string
		target *int
	}{
		{[]string{"input_tokens", "prompt_tokens"}, &usage.Input},
		{[]string{"cached_input_tokens", "cache_read_input_tokens", "cached_tokens"}, &usage.CachedInput},
		{[]string{"cache_creation_input_tokens", "cache_write_input_tokens"}, &usage.CacheCreation},
		{[]string{"output_tokens", "completion_tokens"}, &usage.Output},
	} {
		for _, name := range field.names {
			value, present := values[name]
			if !present || string(value) == "null" {
				continue
			}
			if err := json.Unmarshal(value, field.target); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if *field.target < 0 {
				return nil, fmt.Errorf("%s must be nonnegative", name)
			}
			known = true
			break
		}
	}
	if !known {
		return nil, nil
	}
	return usage, nil
}
