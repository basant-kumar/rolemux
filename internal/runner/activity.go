package runner

import (
	"bytes"
	"encoding/json"
	"strings"
)

// EventsFromLine reduces provider-specific JSONL into non-secret lifecycle
// facts. Raw arguments, command output, and model prose are never forwarded.
func EventsFromLine(provider string, line []byte) []Event {
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(line)))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	typ := firstMapString(value, "type", "event")
	lower := strings.ToLower(typ)
	switch strings.ToLower(provider) {
	case "codex":
		return codexActivity(value, typ)
	case "claude":
		return claudeActivity(value, typ)
	case "antigravity":
		tool := strings.Contains(lower, "tool") && (strings.Contains(lower, "start") || strings.Contains(lower, "request"))
		turn := strings.Contains(lower, "model_call_start") || lower == "assistant" || lower == "result" || lower == "turn.completed"
		if !tool && !turn {
			return nil
		}
		return []Event{{Type: typ, Activity: true, AgentTurn: turn && !tool, ToolCall: tool, ToolName: firstMapString(value, "tool_name", "toolName", "name")}}
	default:
		return nil
	}
}

func codexActivity(value map[string]any, typ string) []Event {
	lower := strings.ToLower(typ)
	if lower == "turn.completed" || lower == "response.completed" {
		return []Event{{Type: typ, Activity: true, AgentTurn: true}}
	}
	if lower == "turn.started" || lower == "response.started" {
		return []Event{{Type: typ, Activity: true}}
	}
	if lower == "item.completed" {
		// Completion is a useful heartbeat for long reasoning and tool steps,
		// but starts remain the sole source of counter increments.
		return []Event{{Type: typ, Activity: true}}
	}
	if lower != "item.started" {
		return nil
	}
	item, _ := value["item"].(map[string]any)
	itemType := strings.ToLower(firstMapString(item, "type"))
	switch itemType {
	case "command_execution", "mcp_tool_call", "web_search", "file_change", "tool_call":
		name := firstMapString(item, "name", "tool_name", "toolName")
		if name == "" {
			name = itemType
		}
		// A tool result necessarily starts a later model step. Counting the
		// tool-start plus the terminal response is a conservative turn estimate
		// when Codex does not expose its internal model-call counter.
		return []Event{{Type: typ, Activity: true, AgentTurn: true, ToolCall: true, ToolName: name}}
	default:
		return nil
	}
}

func claudeActivity(value map[string]any, typ string) []Event {
	if strings.ToLower(typ) != "assistant" {
		return nil
	}
	events := []Event{{Type: typ, Activity: true, AgentTurn: true}}
	message, _ := value["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if strings.EqualFold(firstMapString(block, "type"), "tool_use") {
			events = append(events, Event{Type: "tool_use", Activity: true, ToolCall: true, ToolName: firstMapString(block, "name")})
		}
	}
	return events
}

func firstMapString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}
