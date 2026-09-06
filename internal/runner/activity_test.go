package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderActivityNormalizationCountsOnlyStarts(t *testing.T) {
	codex := EventsFromLine("codex", []byte(`{"type":"item.started","item":{"type":"command_execution"}}`))
	if len(codex) != 1 || !codex[0].ToolCall || !codex[0].AgentTurn {
		t.Fatalf("codex events=%#v", codex)
	}
	if duplicate := EventsFromLine("codex", []byte(`{"type":"item.completed","item":{"type":"command_execution"}}`)); len(duplicate) != 0 {
		t.Fatalf("completed tool double-counted: %#v", duplicate)
	}
	claude := EventsFromLine("claude", []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"},{"type":"tool_use","name":"Grep"}]}}`))
	if len(claude) != 3 || !claude[0].AgentTurn || claude[1].ToolName != "Read" || claude[2].ToolName != "Grep" {
		t.Fatalf("claude events=%#v", claude)
	}
}

func TestClaudeStreamResultAndReportedTurns(t *testing.T) {
	data := []byte("{\"type\":\"system\",\"session_id\":\"s1\"}\n" +
		"{\"type\":\"result\",\"session_id\":\"s1\",\"num_turns\":7,\"structured_output\":{\"role\":\"implementer\",\"status\":\"ready\"}}\n")
	session, nested, _, _, err := parseClaudeResult(data, "s1", RoleImplementer)
	if err != nil || session != "s1" || !strings.Contains(string(nested), `"ready"`) || claudeReportedTurns(data) != 7 {
		t.Fatalf("session=%q nested=%s turns=%d err=%v", session, nested, claudeReportedTurns(data), err)
	}
	args, err := BuildClaudeArgs(Request{Role: RoleImplementer, RepoRoot: "/repo", Model: "claude-fable-5", SessionID: "s1"})
	if err != nil || !strings.Contains(strings.Join(args, " "), "--output-format stream-json --verbose") {
		t.Fatalf("args=%v err=%v", args, err)
	}
}

func TestPXPipeModeDiagnosticDistinguishesImageAndPassThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := "{\"status\":200,\"model\":\"gpt-luna\",\"compressed\":false,\"input\":{\"mode\":\"text\"}}\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var messages []string
	reportPXPipeMode(func(message string) { messages = append(messages, message) }, path, 0)
	if len(messages) != 1 || !strings.Contains(messages[0], "model=gpt-luna mode=text compressed=false") || !strings.Contains(messages[0], "pass-through") {
		t.Fatalf("messages=%#v", messages)
	}
	offset := int64(len(data))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"status\":200,\"model\":\"gpt-sol\",\"compressed\":true,\"saved_pct\":42.5,\"input\":{\"mode\":\"image\"}}\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	messages = nil
	reportPXPipeMode(func(message string) { messages = append(messages, message) }, path, offset)
	if len(messages) != 1 || !strings.Contains(messages[0], "model=gpt-sol mode=image compressed=true saved=42.5%") || strings.Contains(messages[0], "pass-through") {
		t.Fatalf("offset messages=%#v", messages)
	}
}
