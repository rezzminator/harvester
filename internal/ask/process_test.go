package ask

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/engine"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/testjail"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProcessLookupArgumentsAndUsage(t *testing.T) {
	for _, id := range []engine.ID{engine.Claude, engine.Codex} {
		t.Run(string(id), func(t *testing.T) {
			root := t.TempDir()
			name := engine.MustLookup(id).Binary
			binary := filepath.Join(root, name)
			capture := filepath.Join(root, "captured")
			reply := `printf '%s\n' '{"result":"answer","usage":{"input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":4,"output_tokens":2}}'`
			model := "haiku"
			wantArgs := "-p|--model|haiku|--effort|high|--output-format|json|"
			if id == engine.Codex {
				model = "gpt-fixture"
				wantArgs = `exec|--model|gpt-fixture|-c|model_reasoning_effort="high"|--json|--ephemeral|--skip-git-repo-check|--color|never|`
				reply = `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":3,"cache_creation_input_tokens":4,"output_tokens":2}}'`
			}
			script := "#!/bin/sh\nprintf '%s|' \"$@\" > \"$ASK_CAPTURE\"\nprintf '\\n' >> \"$ASK_CAPTURE\"\nprintf 'identity=%s,%s,%s\\n' \"${CLAUDECODE-}\" \"${CLAUDE_CODE_SESSION_ID-}\" \"${CODEX_THREAD_ID-}\" >> \"$ASK_CAPTURE\"\ncat >> \"$ASK_CAPTURE\"\n" + reply + "\n"
			if err := testjail.WriteExecutable(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("ASK_CAPTURE", capture)
			t.Setenv("CLAUDECODE", "1")
			t.Setenv("CLAUDE_CODE_SESSION_ID", "parent")
			t.Setenv("CODEX_THREAD_ID", "parent")
			var runner Engine
			var err error
			if id == engine.Claude {
				runner, err = ResolveClaude(config.Config{})
			} else {
				runner, err = ResolveCodex(config.Config{})
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(context.Background(), AskInput{ContentFiles: []string{"source.md"}, SourceLabels: []string{"source"}, Prompt: "Question", Engine: id, Model: model, Effort: "high"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Answer != "answer" || result.Usage == nil || *result.Usage != (TokenUsage{Input: 10, CachedInput: 3, CacheCreation: 4, Output: 2}) {
				t.Fatalf("result=%+v", result)
			}
			got, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{wantArgs, "identity=,,\n", "TASK: Question", "EVIDENCE"} {
				if !strings.Contains(string(got), want) {
					t.Fatalf("capture lacks %q: %s", want, got)
				}
			}
		})
	}
}
func TestProcessReportsMissingCrashTimeoutAndEmpty(t *testing.T) {
	if _, err := ResolveClaude(config.Config{Claude: config.Claude{Binary: "/missing/claude"}}); err == nil {
		t.Fatal("missing binary accepted")
	} else {
		var missing *BinaryMissingError
		if !errors.As(err, &missing) {
			t.Fatalf("missing binary error=%v", err)
		}
	}
	for _, tc := range []struct {
		name, body, want string
		timeout          time.Duration
	}{
		{"crash", "cat >/dev/null\nprintf 'fixture failure' >&2\nexit 7\n", "fixture failure", time.Second},
		{"empty", "cat >/dev/null\nprintf '%s' '{\"result\":\"\"}'\n", "no answer", time.Second},
		{"timeout", "cat >/dev/null\nsleep 30 &\nwait\n", "timed out", 25 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "fixture")
			if err := testjail.WriteExecutable(binary, []byte("#!/bin/sh\n"+tc.body), 0700); err != nil {
				t.Fatal(err)
			}
			runner, err := ResolveClaude(config.Config{Claude: config.Claude{Binary: binary}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.Run(context.Background(), AskInput{ContentFiles: []string{"source.md"}, SourceLabels: []string{"source"}, Prompt: "Question", Timeout: tc.timeout})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want=%s", err, tc.want)
			}
			if tc.name == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout lost cause: %v", err)
			}
		})
	}
}

// TestProcessKeepsTheAnswerWhenStderrOverflows: stderr is diagnostics; its
// overflow is logged and the answer on stdout stands.
func TestProcessKeepsTheAnswerWhenStderrOverflows(t *testing.T) {
	ctx, recorder := obs.Test(t)
	binary := filepath.Join(t.TempDir(), "fixture")
	body := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nhead -c %d /dev/zero >&2\nprintf '%%s' '{\"result\":\"answer\"}'\n", maxCapturedOutput+1)
	if err := testjail.WriteExecutable(binary, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	runner, err := ResolveClaude(config.Config{Claude: config.Claude{Binary: binary}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(ctx, AskInput{ContentFiles: []string{"source.md"}, SourceLabels: []string{"source"}, Prompt: "Question"})
	if err != nil || result.Answer != "answer" {
		t.Fatalf("result=%+v err=%v; want the stdout answer kept", result, err)
	}
	want := "claude stderr exceeded the 8388608-byte capture limit; the rest was not kept"
	var got []string
	for _, record := range recorder.Records() {
		if record.Message == "ask.stderr.truncated" {
			got = append(got, fmt.Sprint(record.Fields["reason"]))
		}
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("stderr overflow log = %q, want [%q] (log %s)", got, want, recorder.Raw())
	}
}

func TestBoundedBufferRejectsOversizedEngineOutput(t *testing.T) {
	var buffer boundedBuffer
	body := []byte(strings.Repeat("x", maxCapturedOutput+1))
	n, err := buffer.Write(body)
	if err != nil || n != len(body) || buffer.Len() != maxCapturedOutput || !buffer.truncated {
		t.Fatalf("n=%d len=%d truncated=%v err=%v", n, buffer.Len(), buffer.truncated, err)
	}
	// io.Copy is how exec fills the buffer from a pipe: it must meet the same cap.
	var copied boundedBuffer
	written, err := io.Copy(&copied, bytes.NewReader(body))
	if err != nil || written != int64(len(body)) || copied.Len() != maxCapturedOutput || !copied.truncated {
		t.Fatalf("io.Copy: written=%d len=%d truncated=%v err=%v", written, copied.Len(), copied.truncated, err)
	}
}
