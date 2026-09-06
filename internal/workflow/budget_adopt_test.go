package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

type budgetAdapter struct {
	session string
	calls   int
}

type outputLimitAdapter struct{ budgetAdapter }

func (a *outputLimitAdapter) Run(_ context.Context, _ runner.Request, callbacks runner.Callbacks) (runner.Response, error) {
	a.calls++
	a.session = "output-session"
	if err := callbacks.SessionStarted(a.session); err != nil {
		return runner.Response{}, err
	}
	return runner.Response{SessionID: a.session}, &runner.ProviderError{Code: "OUTPUT_LIMIT", Message: "output limit", Retryable: true, KnownSession: true, SessionID: a.session, Cause: runner.ErrOutputLimit}
}

type turnLimitAdapter struct{ budgetAdapter }

func (a *turnLimitAdapter) Run(_ context.Context, _ runner.Request, callbacks runner.Callbacks) (runner.Response, error) {
	a.calls++
	a.session = "turn-session"
	if err := callbacks.SessionStarted(a.session); err != nil {
		return runner.Response{}, err
	}
	for range 2 {
		if err := callbacks.Event(runner.Event{Type: "model", AgentTurn: true}); err != nil {
			return runner.Response{SessionID: a.session}, err
		}
	}
	return runner.Response{SessionID: a.session}, errors.New("model turn limit was not enforced")
}

func (b *budgetAdapter) Run(_ context.Context, req runner.Request, callbacks runner.Callbacks) (runner.Response, error) {
	b.calls++
	if b.session == "" {
		b.session = "budget-session"
		if err := callbacks.SessionStarted(b.session); err != nil {
			return runner.Response{}, err
		}
	} else if !req.Resume || req.SessionID != b.session {
		return runner.Response{}, errors.New("budget retry did not resume")
	}
	for _, name := range []string{"read", "search"} {
		if err := callbacks.Event(runner.Event{Type: "tool", ToolCall: true, ToolName: name}); err != nil {
			return runner.Response{SessionID: b.session}, err
		}
	}
	envelope := runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: "bounded plan", Complexity: task.ComplexityTrivial, WorkUnits: []task.WorkUnit{{ID: "T1", Objective: "bounded", Scope: "app.go", ContextGroup: "T1", ContextFiles: []string{"app.go"}, AffectedSymbols: []string{"app"}, EstimatedMinutes: 2, ExecutionPacket: "change app", AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test app"}}}}
	return runner.Response{SessionID: b.session, Envelope: &envelope}, nil
}

func (b *budgetAdapter) ListModels(context.Context, runner.ModelListRequest) (runner.ModelPage, error) {
	return workflowTestModels(), nil
}
func (b *budgetAdapter) Version(context.Context) (string, error) { return "test", nil }
func (b *budgetAdapter) Auth(context.Context) (runner.AuthStatus, error) {
	return runner.AuthStatus{Authenticated: true}, nil
}

func TestToolBudgetExhaustionIsDurableExtendableAndResumable(t *testing.T) {
	root := workflowRepo(t)
	cfg := workflowConfig()
	budget := cfg.Budgets[config.RolePlanner]
	budget.MaxToolCalls = 1
	cfg.Budgets[config.RolePlanner] = budget
	adapter := &budgetAdapter{}
	service := New(root, cfg, map[string]runner.Adapter{"codex": adapter})
	result, err := service.StartPlan(context.Background(), "bounded work", "budgeted")
	if err == nil || result.Status != "budget_exhausted" || result.State.BudgetIssue == nil || result.State.BudgetIssue.Kind != "tool_calls" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.State.Retry == nil || !result.State.Retry.KnownSession || result.State.Usage[string(runner.RolePlanner)].ToolCalls != 2 {
		t.Fatalf("recovery state=%#v usage=%#v", result.State.Retry, result.State.Usage)
	}
	if result.State.Progress == nil || result.State.Progress.Active || result.State.Progress.ToolCalls != 2 {
		t.Fatalf("progress=%#v", result.State.Progress)
	}
	if control := ControlFor(result.State); control.NextAction != "budget_extend" {
		t.Fatalf("control=%#v", control)
	}
	blocked, blockedErr := service.Retry(context.Background(), "budgeted")
	if ExitCode(blockedErr) != ExitAction || blocked.Status != "budget_exhausted" || adapter.calls != 1 {
		t.Fatalf("retry bypassed required budget extension: result=%#v err=%v calls=%d", blocked, blockedErr, adapter.calls)
	}
	if _, mismatchErr := service.ExtendBudget("budgeted", config.RolePlanner, 1, 0, 0, 0); mismatchErr == nil {
		t.Fatal("wrong exhausted limit extension was accepted")
	}
	if _, oversizedErr := service.ExtendBudget("budgeted", config.RolePlanner, 0, 3, 0, 0); oversizedErr == nil {
		t.Fatal("oversized exhausted limit extension was accepted")
	}
	if _, unrelatedErr := service.ExtendBudget("budgeted", config.RolePlanner, 1, 1, 0, 0); unrelatedErr == nil {
		t.Fatal("unrelated budget extension was accepted")
	}
	result, err = service.ExtendBudget("budgeted", config.RolePlanner, 0, 1, 0, 0)
	if err != nil || result.Status != "budget_extended" || result.State.BudgetIssue != nil {
		t.Fatalf("extend=%#v err=%v", result, err)
	}
	result, err = service.Retry(context.Background(), "budgeted")
	if err != nil || result.Status != "planned" || result.State.PlannerSessionID != adapter.session || adapter.calls != 2 {
		t.Fatalf("retry=%#v err=%v calls=%d", result, err, adapter.calls)
	}
}

func TestOutputBudgetExhaustionCanExtendTheExactLimit(t *testing.T) {
	root := workflowRepo(t)
	cfg := workflowConfig()
	adapter := &outputLimitAdapter{}
	service := New(root, cfg, map[string]runner.Adapter{"codex": adapter})
	result, err := service.StartPlan(context.Background(), "bounded output", "output-budget")
	if err == nil || result.State.BudgetIssue == nil || result.State.BudgetIssue.Kind != "output_bytes" || result.State.Retry == nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	before := result.State.BudgetsSnapshot[config.RolePlanner].MaxOutputBytes
	result, err = service.ExtendBudget(result.State.ID, config.RolePlanner, 0, 0, 0, 1<<20)
	if err != nil || result.State.BudgetsSnapshot[config.RolePlanner].MaxOutputBytes != before+1<<20 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestModelTurnBudgetUsesProviderActivityNotInvocationCount(t *testing.T) {
	root := workflowRepo(t)
	cfg := workflowConfig()
	budget := cfg.Budgets[config.RolePlanner]
	budget.MaxTurns = 1
	cfg.Budgets[config.RolePlanner] = budget
	service := New(root, cfg, map[string]runner.Adapter{"codex": &turnLimitAdapter{}})
	result, err := service.StartPlan(context.Background(), "bounded turns", "turn-budget")
	if err == nil || result.State.BudgetIssue == nil || result.State.BudgetIssue.Kind != "model_turns" || result.State.BudgetIssue.Observed != 2 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	usage := result.State.Usage[config.RolePlanner]
	if usage.Requests != 1 || usage.AgentTurns != 2 {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestAdoptCapturesHostChangesAtInterruptedBoundary(t *testing.T) {
	root := workflowRepo(t)
	service := New(root, config.Default(), nil)
	baseline, err := service.Observe("app.go")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err = service.Capture(baseline, "baseline", "adopt")
	if err != nil {
		t.Fatal(err)
	}
	state := task.State{ID: "adopt", RepoRoot: root, Phase: task.PhaseFailed, Task: "change app", Plan: "host packet", PlanHash: hash("host packet"), ApprovedPlanHash: hash("host packet"), Scope: "app.go", ScopedBaseline: baseline, ScopedBaselineHash: task.HashManifest(baseline), Retry: &task.RetryState{Role: string(runner.RoleImplementer), KnownSession: true, SessionID: "lost-provider"}}
	if err := service.Store.Create(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst adopted = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := service.Adopt("adopt", "orchestrator completed the narrow fallback")
	if err != nil || result.State.Phase != task.PhaseImplementationReady || result.State.Retry != nil || len(result.State.ChangeManifest) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(result.State.Events) != 1 || result.State.Events[0].Type != "host_changes_adopted" {
		t.Fatalf("events=%#v", result.State.Events)
	}
}

func TestAdoptAcceptsAnEstablishedEmptyBaselineForANewFile(t *testing.T) {
	root := workflowRepo(t)
	service := New(root, config.Default(), nil)
	baseline, err := service.Observe("new.go")
	if err != nil {
		t.Fatal(err)
	}
	state := task.State{ID: "adopt-new", RepoRoot: root, Phase: task.PhaseFailed, Scope: "new.go", ScopedBaseline: baseline, ScopedBaselineHash: task.HashManifest(baseline), Plan: "add new.go", PlanHash: hash("add new.go"), ApprovedPlanHash: hash("add new.go")}
	if err := service.Store.Create(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := service.Adopt(state.ID, "host added the scoped file")
	if err != nil || result.State.Phase != task.PhaseImplementationReady || len(result.State.ChangeManifest) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestDependencyOrderedContextGroupReusesImplementerSession(t *testing.T) {
	root := workflowRepo(t)
	service := New(root, config.Default(), nil)
	units, err := task.NormalizeWorkUnits([]task.WorkUnit{
		{ID: "T1", Objective: "first", Scope: "one.go", ContextGroup: "lane", ContextFiles: []string{"app.go"}, EstimatedMinutes: 2, ExecutionPacket: "first", AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test"}},
		{ID: "T2", Objective: "second", Scope: "two.go", DependsOn: []string{"T1"}, ContextGroup: "lane", ContextFiles: []string{"app.go"}, EstimatedMinutes: 2, ExecutionPacket: "second", AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test"}},
	}, "plan")
	if err != nil {
		t.Fatal(err)
	}
	parent := task.State{ID: "context-parent", RepoRoot: root, Phase: task.PhasePlanApproved, Task: "two steps", Plan: "plan", PlanHash: hash("plan"), ApprovedPlanHash: hash("plan"), WorkUnits: units}
	markPlanHumanApproved(&parent)
	if err := service.Store.Create(parent); err != nil {
		t.Fatal(err)
	}
	first := task.State{ID: task.WorkTaskID(parent.ID, "T1"), RepoRoot: root, ParentTaskID: parent.ID, WorkUnitID: "T1", Phase: task.PhaseApproved, ImplementerSessionID: "shared-session"}
	if err := service.Store.Create(first); err != nil {
		t.Fatal(err)
	}
	result, err := service.StartWork(parent.ID, "T2")
	if err != nil || result.State.ImplementerSessionID != "shared-session" {
		t.Fatalf("result=%#v err=%v", result.State, err)
	}
}
