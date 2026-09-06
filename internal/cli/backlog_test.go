package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
	"github.com/basant-kumar/rolemux/internal/workflow"
)

func TestPlanGraphJSONIsCompactUnlessFull(t *testing.T) {
	root := cliRepo(t)
	packet := "PACKET_SENTINEL_SHOULD_BE_FULL_ONLY"
	units, err := task.NormalizeWorkUnits([]task.WorkUnit{{ID: "T1", Objective: "change", Scope: "app.go", ContextGroup: "lane", ContextFiles: []string{"app.go"}, AffectedSymbols: []string{"Run"}, EstimatedMinutes: 3, ExecutionPacket: packet, AcceptanceCriteria: []string{"ACCEPTANCE_SENTINEL"}, ValidationCommands: []string{"VALIDATION_SENTINEL"}}}, "plan")
	if err != nil {
		t.Fatal(err)
	}
	state := task.State{ID: "compact-graph", RepoRoot: root, Phase: task.PhasePlanApproved, Plan: "plan", PlanHash: task.ScopeSpecHash("plan"), ApprovedPlanHash: task.ScopeSpecHash("plan"), WorkGraph: true, WorkUnits: units, Complexity: task.ComplexityTrivial}
	markCLIPlanApproved(t, &state)
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}
	code, output, stderr := runTestApp(t, root, "", "plan", "graph", state.ID, "--json")
	if code != 0 || stderr != "" || strings.Contains(string(output), packet) || strings.Contains(string(output), "ACCEPTANCE_SENTINEL") || strings.Contains(string(output), "context_files") {
		t.Fatalf("compact code=%d stderr=%q output=%s", code, stderr, output)
	}
	if !strings.Contains(string(output), `"critical_path_minutes":3`) || !strings.Contains(string(output), `"context_group":"lane"`) {
		t.Fatalf("compact scheduling data missing: %s", output)
	}
	code, output, stderr = runTestApp(t, root, "", "plan", "graph", state.ID, "--full", "--json")
	if code != 0 || stderr != "" || !strings.Contains(string(output), packet) || !strings.Contains(string(output), "context_files") {
		t.Fatalf("full code=%d stderr=%q output=%s", code, stderr, output)
	}
}

func TestBudgetShowAndExtendCommandsAreProviderFree(t *testing.T) {
	root := cliRepo(t)
	state := task.State{ID: "budget-cli", RepoRoot: root, Phase: task.PhaseFailed, BudgetsSnapshot: map[string]task.RoleBudget{config.RolePlanner: {MaxTurns: 1, MaxToolCalls: 2, TimeoutSeconds: 30}}, BudgetIssue: &task.BudgetIssue{Role: config.RolePlanner, Kind: "model_turns", Limit: 1, Observed: 1}, Retry: &task.RetryState{Role: string(runner.RolePlanner), KnownSession: true, SessionID: "session"}}
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}
	code, output, stderr := runTestApp(t, root, "", "budget", "show", state.ID, "--json")
	if code != 0 || stderr != "" || !strings.Contains(string(output), `"max_turns":1`) {
		t.Fatalf("show code=%d stderr=%q output=%s", code, stderr, output)
	}
	code, output, stderr = runTestApp(t, root, "", "budget", "extend", state.ID, "--role", "planner", "--turns", "1", "--json")
	if code != 0 || stderr != "" || !strings.Contains(string(output), `"status":"budget_extended"`) || !strings.Contains(string(output), `"next_action":"retry"`) {
		t.Fatalf("extend code=%d stderr=%q output=%s", code, stderr, output)
	}
}

func TestWorkAdoptCommandCapturesHostFallback(t *testing.T) {
	root := cliRepo(t)
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := workflow.New(root, config.Default(), nil)
	baseline, err := service.Observe("app.go")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err = service.Capture(baseline, "baseline", "adopt-cli")
	if err != nil {
		t.Fatal(err)
	}
	state := task.State{ID: "adopt-cli", RepoRoot: root, Phase: task.PhaseFailed, Scope: "app.go", ScopedBaseline: baseline, ScopedBaselineHash: task.HashManifest(baseline), Plan: "packet", PlanHash: task.ScopeSpecHash("packet"), ApprovedPlanHash: task.ScopeSpecHash("packet")}
	if err := service.Store.Create(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst fixed = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output, stderr := runTestApp(t, root, "", "work", "adopt", state.ID, "--note", "host fixed the scoped change", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q output=%s", code, stderr, output)
	}
	var payload map[string]any
	if err := json.Unmarshal(output, &payload); err != nil || !strings.Contains(string(output), `"status":"implementation_ready"`) || !strings.Contains(string(output), "host_changes_adopted") {
		t.Fatalf("payload=%#v err=%v output=%s", payload, err, output)
	}
}

func TestListJSONIsATokenEfficientIndex(t *testing.T) {
	root := cliRepo(t)
	state := task.State{
		ID: "compact-list", RepoRoot: root, Phase: task.PhaseFailed, Task: "TASK_SENTINEL",
		Plan: "PLAN_SENTINEL", Findings: []task.Finding{{Message: "FINDING_SENTINEL"}},
		ProfilesSnapshot: map[string]task.ProfileSnapshot{config.RolePlanner: {Provider: "codex", Model: "MODEL_SENTINEL"}},
		Usage:            map[string]task.TokenUsage{config.RolePlanner: {InputTokens: 999999}},
	}
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}
	code, output, stderr := runTestApp(t, root, "", "list", "--json")
	if code != 0 || stderr != "" || !strings.Contains(string(output), `"id":"compact-list"`) {
		t.Fatalf("code=%d stderr=%q output=%s", code, stderr, output)
	}
	for _, sentinel := range []string{"TASK_SENTINEL", "PLAN_SENTINEL", "FINDING_SENTINEL", "MODEL_SENTINEL", "999999", `"usage"`, `"profiles"`, `"findings"`} {
		if strings.Contains(string(output), sentinel) {
			t.Fatalf("list leaked %q: %s", sentinel, output)
		}
	}
}
