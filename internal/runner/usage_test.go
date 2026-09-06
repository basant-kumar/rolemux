package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
)

func TestUsagePresenceRecognizesCountersAndExplicitZero(t *testing.T) {
	zero, present := UsageFromMapWithPresence(map[string]any{
		"usage": map[string]any{"input_tokens": json.Number("0"), "output_tokens": json.Number("0")},
	}, true)
	if !present || zero.InputTokens != 0 || zero.OutputTokens != 0 {
		t.Fatalf("zero usage=%#v, present=%v", zero, present)
	}
	if _, present := UsageFromMapWithPresence(map[string]any{"usage": map[string]any{}}, true); present {
		t.Fatal("empty usage object was reported")
	}
	if _, present := UsageFromMapWithPresence(map[string]any{"usage": map[string]any{"cost": 1}}, true); present {
		t.Fatal("unrecognized numeric field was reported")
	}
}

func TestUsagePresenceAcceptsCamelCaseDetailCounters(t *testing.T) {
	usage, present := UsageFromMapWithPresence(map[string]any{
		"usage": map[string]any{
			"inputTokensDetails":  map[string]any{"cachedTokens": json.Number("0")},
			"outputTokensDetails": map[string]any{"reasoningTokens": json.Number("0")},
		},
	}, true)
	if !present || usage.CachedInputTokens != 0 || usage.ReasoningTokens != 0 {
		t.Fatalf("camel-case detail usage=%#v, present=%v", usage, present)
	}

	usage, present = UsageFromMapWithPresence(map[string]any{
		"inputTokensDetails":  map[string]any{"cachedTokens": json.Number("7")},
		"outputTokensDetails": map[string]any{"reasoningTokens": json.Number("3")},
	}, true)
	if !present || usage.CachedInputTokens != 7 || usage.ReasoningTokens != 3 {
		t.Fatalf("camel-case detail values=%#v, present=%v", usage, present)
	}
}

func TestJSONUsageHelpersPreservePresenceAndCacheSemantics(t *testing.T) {
	document := []byte(`{"usage":{"input_tokens":10,"cache_read_input_tokens":40,"cache_creation_input_tokens":5,"output_tokens":8}}`)
	usage, present := usageFromJSONDocumentWithPresence(document, false)
	if !present || usage.InputTokens != 10 || usage.CachedInputTokens != 40 || usage.CacheWriteTokens != 5 || usage.OutputTokens != 8 || usage.TotalTokens != 63 {
		t.Fatalf("usage=%#v, present=%v", usage, present)
	}

	lines := []byte("{\"type\":\"event\"}\n{\"usage\":{\"input_tokens\":0}}\n")
	usage, present = usageFromJSONLinesWithPresence(lines, true)
	if !present || usage.InputTokens != 0 {
		t.Fatalf("lines usage=%#v, present=%v", usage, present)
	}
	if got := usageFromJSONLines([]byte("{\"usage\":{\"input_tokens\":0}}\n"), true); got.InputTokens != 0 {
		t.Fatalf("legacy helper changed zero usage: %#v", got)
	}
}

func TestClaudeUsageParsingRejectsTrailingData(t *testing.T) {
	if usage, present := usageFromJSONDocumentWithPresence([]byte(`{"usage":{"input_tokens":10,"output_tokens":2}} trailing`), true); present || usage != (TokenUsage{}) {
		t.Fatalf("trailing Claude usage was accepted: usage=%#v present=%v", usage, present)
	}
}

func TestCodexUsageParsingRejectsTrailingData(t *testing.T) {
	if usage, reported, terminal := codexUsageFromJSONLines([]byte(`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}} trailing` + "\n")); reported || terminal || usage != (TokenUsage{}) {
		t.Fatalf("trailing Codex usage was accepted: usage=%#v reported=%v terminal=%v", usage, reported, terminal)
	}
}

func TestCodexUsageStatusRequiresTerminalTurnUsage(t *testing.T) {
	partial, reported, terminal := codexUsageFromJSONLines([]byte(
		`{"type":"turn.started","usage":{"input_tokens":4}}` + "\n" +
			`{"type":"turn.completed","usage":{}}` + "\n"),
	)
	if !reported || terminal || partial.InputTokens != 4 {
		t.Fatalf("partial=%#v reported=%v terminal=%v", partial, reported, terminal)
	}

	complete, reported, terminal := codexUsageFromJSONLines([]byte(
		`{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0}}` + "\n"),
	)
	if !reported || !terminal || complete.InputTokens != 0 || complete.OutputTokens != 0 {
		t.Fatalf("complete=%#v reported=%v terminal=%v", complete, reported, terminal)
	}
}

func TestCodexRunRetainsTerminalUsageOnProcessAndEnvelopeErrors(t *testing.T) {
	output := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-1"}`,
		`{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":2}}`,
		`{"type":"item.completed","item":{"text":"{\"role\":\"planner\",\"status\":\"ready\",\"plan\":\"ok\",\"question\":\"\"}"}}`,
	}, "\n")
	codex := &Codex{Path: "/bin/codex", Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
		return ProcessResult{Stdout: []byte(output), ProcessStarted: true}, errors.New("interrupted after turn")
	}}
	response, err := codex.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: t.TempDir(), Model: "model", Resume: true, SessionID: "thread-1"}, Callbacks{})
	if err == nil || response.UsageStatus != UsageStatusComplete || response.Usage.OutputTokens != 2 {
		t.Fatalf("response=%#v err=%v", response, err)
	}

	badEnvelope := strings.Replace(output, `\"status\":\"ready\"`, `\"status\":\"needs_input\"`, 1)
	codex.Process = func(context.Context, ProcessSpec) (ProcessResult, error) {
		return ProcessResult{Stdout: []byte(badEnvelope), ProcessStarted: true}, nil
	}
	response, err = codex.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: t.TempDir(), Model: "model", Resume: true, SessionID: "thread-1"}, Callbacks{})
	if err == nil || response.UsageStatus != UsageStatusComplete || response.Usage.OutputTokens != 2 {
		t.Fatalf("invalid envelope response=%#v err=%v", response, err)
	}
}

func TestClaudeRunReportsTerminalUsageEvenWhenEnvelopeValidationFails(t *testing.T) {
	output := `{"session_id":"session-1","usage":{"input_tokens":0,"output_tokens":3},"structured_output":{"role":"planner","status":"ready","plan":"","question":""}}`
	claude := &Claude{Path: "/bin/claude", Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
		return ProcessResult{Stdout: []byte(output), ProcessStarted: true}, nil
	}}
	response, err := claude.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: t.TempDir(), Model: "model", Resume: true, SessionID: "session-1"}, Callbacks{})
	if err == nil || response.UsageStatus != UsageStatusComplete || response.Usage.InputTokens != 0 || response.Usage.OutputTokens != 3 {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestAntigravityUsageIsLatestAndSurvivesLaterParseFailure(t *testing.T) {
	data := strings.Join([]string{
		`{"event":"result","conversation_id":"conversation-1","structured_output":{"role":"planner","status":"ready","plan":"ok","question":""},"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}`,
		`{"event":"progress","usage":{}}`,
		`not-json`,
	}, "\n")
	response, err := parseAntigravityOutput([]byte(data), RolePlanner, Callbacks{})
	if err == nil || response.UsageStatus != UsageStatusComplete || !response.UsageCumulative || response.Usage.InputTokens != 12 || response.Usage.OutputTokens != 4 {
		t.Fatalf("response=%#v err=%v", response, err)
	}

	partial, err := parseAntigravityOutput([]byte(`{"event":"progress","usage":{"input_tokens":5}}`+"\n"+`not-json`), RolePlanner, Callbacks{})
	if err == nil || partial.UsageStatus != UsageStatusIncomplete || partial.Usage.InputTokens != 5 {
		t.Fatalf("partial=%#v err=%v", partial, err)
	}
}

func TestCopilotUsageAccumulatorRecognizesPointerZero(t *testing.T) {
	zero := int64(0)
	two := int64(2)
	var accumulator copilotUsageAccumulator
	accumulator.Add(&copilot.AssistantUsageData{InputTokens: &zero})
	accumulator.Add(&copilot.AssistantUsageData{})
	accumulator.Add(&copilot.AssistantUsageData{OutputTokens: &two})
	usage, reported := accumulator.Snapshot()
	if !reported || usage.InputTokens != 0 || usage.OutputTokens != 2 || usage.TotalTokens != 2 {
		t.Fatalf("usage=%#v reported=%v", usage, reported)
	}
}
