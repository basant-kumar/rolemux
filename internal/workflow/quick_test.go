package workflow

import (
	"testing"

	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

func TestStartQuickCreatesReadyTaskWithoutModelDiscovery(t *testing.T) {
	root := workflowRepo(t)
	adapter := &scriptedAdapter{}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": adapter})

	result, err := service.StartQuick("Fix the role label and its focused tests", "internal/cli/view.go,internal/cli/view_test.go", "quick-task")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ready" || result.State.Phase != task.PhasePlanApproved {
		t.Fatalf("result=%#v", result)
	}
	if !result.State.DirectImplementation || result.State.Complexity != task.ComplexityTrivial {
		t.Fatalf("quick metadata=%#v", result.State)
	}
	if result.State.PlannedScope != "internal/cli/view.go,internal/cli/view_test.go" {
		t.Fatalf("scope=%q", result.State.PlannedScope)
	}
	control := ControlFor(result.State)
	if control.Status != "ready" || control.NextAction != "implement" || control.ReviewKind != "" {
		t.Fatalf("control=%#v", control)
	}
	if adapter.listCalls != 0 || len(adapter.requests) != 0 {
		t.Fatalf("quick start called provider: list=%d runs=%d", adapter.listCalls, len(adapter.requests))
	}
	if len(result.State.ProfilesSnapshot) != 2 || result.State.ProfilesSnapshot[string(runner.RolePlanner)].Model != "" {
		t.Fatalf("profiles=%#v", result.State.ProfilesSnapshot)
	}
}

func TestStartQuickRequiresNarrowScope(t *testing.T) {
	service := New(workflowRepo(t), workflowConfig(), map[string]runner.Adapter{"codex": &scriptedAdapter{}})
	for _, scope := range []string{"", "**"} {
		if _, err := service.StartQuick("small task", scope, "quick-wide"); err == nil || ExitCode(err) != ExitUsage {
			t.Fatalf("scope %q err=%v", scope, err)
		}
	}
}
