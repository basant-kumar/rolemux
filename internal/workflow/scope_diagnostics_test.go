package workflow

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

func TestScopeStateRelevant(t *testing.T) {
	resumable := &task.RetryState{Role: string(runner.RoleCodeReviewer), KnownSession: true, SessionID: "review-session"}
	cases := []struct {
		name  string
		state task.State
		want  bool
	}{
		{name: "approved", state: task.State{Phase: task.PhaseApproved}, want: false},
		{name: "failed without retry", state: task.State{Phase: task.PhaseFailed}, want: false},
		{name: "failed with unknown session", state: task.State{Phase: task.PhaseFailed, Retry: &task.RetryState{KnownSession: true}}, want: false},
		{name: "failed with resumable review", state: task.State{Phase: task.PhaseFailed, Retry: resumable}, want: true},
		{name: "exhausted", state: task.State{Phase: task.PhaseFailed, ReviewProgress: &task.ReviewProgress{Status: "exhausted"}}, want: false},
		{name: "implementation ready", state: task.State{Phase: task.PhaseImplementationReady}, want: true},
		{name: "implementing", state: task.State{Phase: task.PhaseImplementing}, want: true},
		{name: "code reviewing", state: task.State{Phase: task.PhaseCodeReviewing}, want: true},
		{name: "implementer needs input", state: task.State{Phase: task.PhaseNeedsInput, PendingQuestionSource: string(runner.RoleImplementer)}, want: true},
		{name: "planner needs input", state: task.State{Phase: task.PhaseNeedsInput, PendingQuestionSource: string(runner.RolePlanner)}, want: false},
		{name: "review needed with resumable review", state: task.State{Phase: task.PhaseReviewNeeded, Retry: resumable}, want: true},
		{name: "review needed without retry", state: task.State{Phase: task.PhaseReviewNeeded}, want: false},
		{name: "plan approved", state: task.State{Phase: task.PhasePlanApproved}, want: true},
		{name: "in-flight implementer overrides approved phase", state: task.State{Phase: task.PhaseApproved, InFlight: &task.InFlight{Role: string(runner.RoleImplementer)}}, want: true},
		{name: "in-flight reviewer overrides approved phase", state: task.State{Phase: task.PhaseApproved, InFlight: &task.InFlight{Role: string(runner.RoleCodeReviewer)}}, want: true},
		{name: "in-flight planner does not override approved phase", state: task.State{Phase: task.PhaseApproved, InFlight: &task.InFlight{Role: string(runner.RolePlanner)}}, want: false},
		{name: "in-flight plan reviewer does not override approved phase", state: task.State{Phase: task.PhaseApproved, InFlight: &task.InFlight{Role: string(runner.RolePlanReviewer)}}, want: false},
		{name: "failed planner retry is not scope-relevant", state: task.State{Phase: task.PhaseFailed, Retry: &task.RetryState{Role: string(runner.RolePlanner), KnownSession: true, SessionID: "planner-session"}}, want: false},
		{name: "review-needed plan reviewer retry is not scope-relevant", state: task.State{Phase: task.PhaseReviewNeeded, Retry: &task.RetryState{Role: string(runner.RolePlanReviewer), KnownSession: true, SessionID: "plan-reviewer-session"}}, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := scopeStateRelevant(test.state); got != test.want {
				t.Fatalf("scopeStateRelevant(%#v) = %t, want %t", test.state, got, test.want)
			}
		})
	}
}

func TestScopeAdvisoriesIgnoreTerminalOverlaps(t *testing.T) {
	root := workflowRepo(t)
	service := New(root, workflowConfig(), nil)
	states := []task.State{
		{ID: "active", Scope: "app.go", Phase: task.PhaseImplementationReady},
		{ID: "approved", Scope: "app.go", Phase: task.PhaseApproved},
		{ID: "failed", Scope: "app.go", Phase: task.PhaseFailed},
		{ID: "failed-unknown", Scope: "app.go", Phase: task.PhaseFailed, Retry: &task.RetryState{KnownSession: true}},
		{ID: "failed-resumable", Scope: "app.go", Phase: task.PhaseFailed, Retry: &task.RetryState{Role: string(runner.RoleImplementer), KnownSession: true, SessionID: "implementer-session"}},
		{ID: "exhausted", Scope: "app.go", Phase: task.PhaseFailed, ReviewProgress: &task.ReviewProgress{Status: "exhausted"}},
		{ID: "implementation-ready", Scope: "app.go", Phase: task.PhaseImplementationReady},
		{ID: "implementing", Scope: "app.go", Phase: task.PhaseImplementing},
		{ID: "code-reviewing", Scope: "app.go", Phase: task.PhaseCodeReviewing},
		{ID: "needs-input-implementer", Scope: "app.go", Phase: task.PhaseNeedsInput, PendingQuestionSource: string(runner.RoleImplementer)},
		{ID: "needs-input-planner", Scope: "app.go", Phase: task.PhaseNeedsInput, PendingQuestionSource: string(runner.RolePlanner)},
		{ID: "review-needed", Scope: "app.go", Phase: task.PhaseReviewNeeded, Retry: &task.RetryState{Role: string(runner.RoleCodeReviewer), KnownSession: true, SessionID: "review-session"}},
		{ID: "review-needed-no-retry", Scope: "app.go", Phase: task.PhaseReviewNeeded},
		{ID: "plan-approved", Scope: "app.go", Phase: task.PhasePlanApproved},
		{ID: "inflight-approved", Scope: "app.go", Phase: task.PhaseApproved, InFlight: &task.InFlight{Role: string(runner.RoleImplementer)}},
		{ID: "inflight-review-approved", Scope: "app.go", Phase: task.PhaseApproved, InFlight: &task.InFlight{Role: string(runner.RoleCodeReviewer)}},
		{ID: "empty-scope", Phase: task.PhaseImplementationReady},
	}
	for _, state := range states {
		if err := service.Store.Create(state); err != nil {
			t.Fatalf("create %s: %v", state.ID, err)
		}
	}

	diagnostics := service.scopeAdvisories("active", "app.go", []task.FileEntry{{Path: "app.go", Kind: "file"}})
	got := map[string]bool{}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code != "SCOPE_OVERLAP" {
			t.Fatalf("unexpected diagnostic: %#v", diagnostic)
		}
		got[diagnostic.TaskID] = true
	}
	want := map[string]bool{
		"code-reviewing":           true,
		"failed-resumable":         true,
		"implementation-ready":     true,
		"implementing":             true,
		"inflight-approved":        true,
		"inflight-review-approved": true,
		"needs-input-implementer":  true,
		"plan-approved":            true,
		"review-needed":            true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlap task IDs = %#v, want %#v", got, want)
	}
}

func TestImplementerOutsideScopeIgnoresStructuralDirectories(t *testing.T) {
	root := workflowRepo(t)
	pkgDir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "app.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner:      {{Role: string(runner.RolePlanner), Status: "ready", Plan: "implement the package"}},
			runner.RolePlanReviewer: {{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}}},
			runner.RoleImplementer:  {{Role: string(runner.RoleImplementer), Status: "ready"}},
		},
		hook: func(req runner.Request) {
			if req.Role != runner.RoleImplementer {
				return
			}
			if err := os.WriteFile(filepath.Join(pkgDir, "app.go"), []byte("package app\n\nconst changed = true\n"), 0o600); err != nil {
				t.Errorf("modify scoped file: %v", err)
			}
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	if _, err := service.StartPlan(context.Background(), "implement the package", "scope-directory-noise"); err != nil {
		t.Fatalf("start plan: %v", err)
	}
	planResult, err := service.ReviewPlan(context.Background(), "scope-directory-noise")
	if _, err = approveIfRequired(context.Background(), service, "scope-directory-noise", planResult, err); err != nil {
		t.Fatalf("review plan: %v", err)
	}
	result, err := service.Implement(context.Background(), "scope-directory-noise", "pkg/app.go")
	if err != nil {
		t.Fatalf("implement: %v", err)
	}
	for _, diagnostic := range result.State.Advisories {
		if diagnostic.Code == "OUT_OF_SCOPE_CHANGE" {
			t.Fatalf("structural directory produced an outside-scope warning: %#v", diagnostic)
		}
	}
}

func TestOutsideScopePreservesMeaningfulEntryTransitions(t *testing.T) {
	before := []task.FileEntry{
		{Path: "dir-noise", Kind: "directory", Worktree: task.ContentState{Present: true, Hash: "old"}},
		{Path: "file-to-directory", Kind: "file", Worktree: task.ContentState{Present: true, Hash: "old"}},
		{Path: "directory-to-file", Kind: "directory", Worktree: task.ContentState{Present: true, Hash: "old"}},
		{Path: "indexed-dir", Kind: "directory", Index: task.IndexState{Present: true, Mode: "160000", Blob: "old"}},
		{Path: "removed-symlink", Kind: "symlink", Worktree: task.ContentState{Present: true, Hash: "old"}},
		{Path: "removed-deleted", Kind: "deleted", Index: task.IndexState{Present: true, Blob: "old"}},
	}
	after := []task.FileEntry{
		{Path: "dir-noise", Kind: "directory", Worktree: task.ContentState{Present: true, Hash: "new"}},
		{Path: "file-to-directory", Kind: "directory", Worktree: task.ContentState{Present: true, Hash: "new"}},
		{Path: "directory-to-file", Kind: "file", Worktree: task.ContentState{Present: true, Hash: "new"}},
		{Path: "indexed-dir", Kind: "directory", Index: task.IndexState{Present: true, Mode: "160000", Blob: "new"}},
		{Path: "new-unknown", Kind: "unknown", Worktree: task.ContentState{Present: true, Hash: "new"}},
	}
	want := []string{"directory-to-file", "file-to-directory", "indexed-dir", "new-unknown", "removed-deleted", "removed-symlink"}
	if got := outsideScope(before, after, "inside/**"); !reflect.DeepEqual(got, want) {
		t.Fatalf("outsideScope = %#v, want %#v", got, want)
	}
}
