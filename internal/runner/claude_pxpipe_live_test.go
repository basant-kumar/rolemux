package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basant-kumar/rolemux/internal/task"
)

// This opt-in check spends a small real Claude turn. It verifies RoleMux's
// owned pxpipe lifecycle, not just the injectable process seams used by the
// default test suite.
func TestLiveClaudePrivatePXPipe(t *testing.T) {
	if os.Getenv("ROLEMUX_LIVE_CLAUDE_PXPIPE") != "1" {
		t.Skip("set ROLEMUX_LIVE_CLAUDE_PXPIPE=1 to run the live transport check")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "pxpipe-events.jsonl")
	t.Setenv("PXPIPE_LOG", logPath)
	adapter, err := NewClaude("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	page, err := adapter.ListModels(ctx, ModelListRequest{Refresh: true, Runtime: task.RuntimeSnapshot{ProviderType: "claude"}})
	if err != nil || len(page.Models) == 0 {
		t.Fatalf("discover Claude models: models=%d err=%v", len(page.Models), err)
	}
	model := page.Models[0].ID
	for _, candidate := range page.Models {
		if candidate.IsDefault {
			model = candidate.ID
			break
		}
	}
	var diagnostics []string
	response, err := adapter.Run(ctx, Request{
		Role: RolePlanner, RepoRoot: root, Model: model,
		Prompt:  "Return a ready plan for a no-op verification. Include exactly one work unit T1 scoped to README.md with a self-contained packet, one acceptance criterion, and one harmless validation command. Do not use tools or modify files.",
		Runtime: task.RuntimeSnapshot{ProviderType: "claude"},
	}, Callbacks{Diagnostic: func(message string) { diagnostics = append(diagnostics, message) }})
	if err != nil {
		var causes []string
		for cause := err; cause != nil; cause = errors.Unwrap(cause) {
			causes = append(causes, cause.Error())
		}
		events, _ := os.ReadFile(logPath)
		t.Fatalf("live Claude turn: %v; causes=%v; diagnostics=%v; events=%s", err, causes, diagnostics, events)
	}
	if response.SessionID == "" || response.Envelope == nil || len(response.Envelope.WorkUnits) != 1 {
		t.Fatalf("response=%#v", response)
	}
	if len(diagnostics) == 0 || !strings.Contains(strings.Join(diagnostics, "\n"), "pxpipe dashboard (this turn): http://127.0.0.1:") {
		t.Fatalf("dashboard diagnostic missing: %v", diagnostics)
	}
	events, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(events), `"status":200`) {
		t.Fatalf("pxpipe events missing successful request: err=%v events=%s", err, events)
	}
}
