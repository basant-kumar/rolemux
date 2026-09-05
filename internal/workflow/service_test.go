package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

type scriptedAdapter struct {
	mu        sync.Mutex
	responses map[runner.Role][]runner.Envelope
	requests  []runner.Request
	sessions  map[runner.Role]string
	hook      func(runner.Request)
}

func (f *scriptedAdapter) Run(_ context.Context, req runner.Request, callbacks runner.Callbacks) (runner.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	session := f.sessions[req.Role]
	if req.Resume {
		if session == "" || req.SessionID != session {
			return runner.Response{}, errors.New("resume did not use the saved role session")
		}
	} else {
		if session != "" {
			return runner.Response{}, errors.New("role unexpectedly started a second session")
		}
		session = "session-" + string(req.Role)
		f.sessions[req.Role] = session
		if callbacks.SessionStarted != nil {
			if err := callbacks.SessionStarted(session); err != nil {
				return runner.Response{}, err
			}
		}
	}
	queue := f.responses[req.Role]
	if len(queue) == 0 {
		return runner.Response{}, errors.New("script exhausted")
	}
	envelope := queue[0]
	f.responses[req.Role] = queue[1:]
	if f.hook != nil {
		f.hook(req)
	}
	return runner.Response{SessionID: session, Envelope: &envelope}, nil
}

func (f *scriptedAdapter) ListModels(context.Context, runner.ModelListRequest) (runner.ModelPage, error) {
	return runner.ModelPage{}, nil
}
func (f *scriptedAdapter) Version(context.Context) (string, error) { return "test", nil }
func (f *scriptedAdapter) Auth(context.Context) (runner.AuthStatus, error) {
	return runner.AuthStatus{Authenticated: true}, nil
}

func workflowRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "RoleMux Tests"}, {"config", "user.email", "tests@example.invalid"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", root, "add", "app.go")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, output)
	}
	return root
}

func workflowConfig() config.Config {
	cfg := config.Default()
	cfg.Profiles = map[string]config.Profile{
		config.RolePlanner:     {Provider: "codex", Model: "planner-model", Effort: "max"},
		config.RoleImplementer: {Provider: "codex", Model: "implementer-model", Effort: "max"},
		config.RoleReviewer:    {Provider: "codex", Model: "reviewer-model", Effort: "xhigh"},
	}
	return cfg
}

func TestRuntimeSnapshotUsesCopilotGateway(t *testing.T) {
	snapshot := RuntimeSnapshot("copilot", config.Provider{GatewayURL: "https://gateway.example.invalid", BearerTokenEnv: "COPILOT_TOKEN"})
	if snapshot.Endpoint != "https://gateway.example.invalid" || snapshot.SDKSettings["base_url"] != snapshot.Endpoint || snapshot.SDKSettings["bearer_token_env"] != "COPILOT_TOKEN" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestAutomaticReviewLoopsResumeEveryRoleSession(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner: {
				{Role: string(runner.RolePlanner), Status: "ready", Plan: "plan v1"},
				{Role: string(runner.RolePlanner), Status: "ready", Plan: "plan v2"},
			},
			runner.RolePlanReviewer: {
				{Role: string(runner.RolePlanReviewer), Verdict: "changes_requested", Findings: []task.Finding{{Message: "add a test"}}},
				{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}},
			},
			runner.RoleImplementer: {
				{Role: string(runner.RoleImplementer), Status: "ready"},
				{Role: string(runner.RoleImplementer), Status: "ready"},
			},
			runner.RoleCodeReviewer: {
				{Role: string(runner.RoleCodeReviewer), Verdict: "changes_requested", Findings: []task.Finding{{Path: "app.go", Message: "tighten it"}}},
				{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}},
			},
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	planned, err := service.StartPlan(context.Background(), "change app", "loop")
	if err != nil || planned.State.Phase != task.PhasePlanned {
		t.Fatalf("start: phase=%s err=%v", planned.State.Phase, err)
	}
	approvedPlan, err := service.ReviewPlan(context.Background(), "loop")
	if err != nil || approvedPlan.State.Phase != task.PhasePlanApproved || approvedPlan.State.Plan != "plan v2" || approvedPlan.State.PlanRound != 2 {
		t.Fatalf("plan loop: %#v err=%v", approvedPlan.State, err)
	}
	implemented, err := service.Implement(context.Background(), "loop", "app.go")
	if err != nil || implemented.State.Phase != task.PhaseImplementationReady {
		t.Fatalf("implement: phase=%s err=%v", implemented.State.Phase, err)
	}
	approvedCode, err := service.ReviewCode(context.Background(), "loop")
	if err != nil || approvedCode.State.Phase != task.PhaseApproved || approvedCode.State.CodeRound != 2 {
		t.Fatalf("code loop: %#v err=%v", approvedCode.State, err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	counts := map[runner.Role]int{}
	for _, request := range fake.requests {
		counts[request.Role]++
		if request.Role == runner.RoleImplementer && request.Sandbox != "workspace-write" {
			t.Fatalf("implementer sandbox=%q", request.Sandbox)
		}
		if request.Role != runner.RoleImplementer && request.Sandbox != "read-only" {
			t.Fatalf("read-only role %s sandbox=%q", request.Role, request.Sandbox)
		}
		if counts[request.Role] > 1 && (!request.Resume || request.SessionID != fake.sessions[request.Role]) {
			t.Fatalf("role %s did not resume its exact session: %#v", request.Role, request)
		}
		if counts[request.Role] == 1 && !strings.Contains(request.Prompt, tokenDiscipline) {
			t.Fatalf("initial %s prompt omitted token discipline", request.Role)
		}
		if counts[request.Role] > 1 && strings.Contains(request.Prompt, tokenDiscipline) {
			t.Fatalf("resumed %s prompt repeated initial guidance", request.Role)
		}
		if counts[request.Role] > 1 && request.Role == runner.RolePlanReviewer && strings.Contains(request.Prompt, "Task:\nchange app") {
			t.Fatal("resumed plan review repeated the unchanged task")
		}
		if counts[request.Role] > 1 && request.Role == runner.RoleCodeReviewer && strings.Contains(request.Prompt, "Approved plan:") {
			t.Fatal("resumed code review repeated the unchanged plan")
		}
	}
	for _, role := range []runner.Role{runner.RolePlanner, runner.RolePlanReviewer, runner.RoleImplementer, runner.RoleCodeReviewer} {
		if counts[role] != 2 {
			t.Fatalf("role %s calls=%d, want 2", role, counts[role])
		}
		usage := approvedCode.State.Usage[string(role)]
		if usage.Requests != 2 || usage.PromptBytes == 0 {
			t.Fatalf("role %s usage=%#v", role, usage)
		}
	}
}

func TestPlannerQuestionReturnsExitThreeAndResumesSameSession(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner: {
				{Role: string(runner.RolePlanner), Status: "needs_input", Question: "Which API?"},
				{Role: string(runner.RolePlanner), Status: "ready", Plan: "use v2"},
			},
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	result, err := service.StartPlan(context.Background(), "choose API", "question")
	if ExitCode(err) != ExitNeedsInput || result.State.PendingQuestion != "Which API?" {
		t.Fatalf("needs input result=%#v err=%v exit=%d", result, err, ExitCode(err))
	}
	result, err = service.AnswerPlan(context.Background(), "question", "v2")
	if err != nil || result.State.Phase != task.PhasePlanned || result.State.Plan != "use v2" {
		t.Fatalf("answer result=%#v err=%v", result, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 2 || !fake.requests[1].Resume || fake.requests[1].SessionID != "session-planner" {
		t.Fatalf("planner session was not resumed: %#v", fake.requests)
	}
}

func TestImplementerQuestionReturnsExitThreeAndResumesSameSession(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner:      {{Role: string(runner.RolePlanner), Status: "ready", Plan: "change app"}},
			runner.RolePlanReviewer: {{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}}},
			runner.RoleImplementer: {
				{Role: string(runner.RoleImplementer), Status: "needs_input", Question: "Keep compatibility?"},
				{Role: string(runner.RoleImplementer), Status: "ready"},
			},
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	if _, err := service.StartPlan(context.Background(), "change app", "implement-question"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReviewPlan(context.Background(), "implement-question"); err != nil {
		t.Fatal(err)
	}
	result, err := service.Implement(context.Background(), "implement-question", "app.go")
	if ExitCode(err) != ExitNeedsInput || result.State.PendingQuestion != "Keep compatibility?" {
		t.Fatalf("needs input result=%#v err=%v exit=%d", result, err, ExitCode(err))
	}
	result, err = service.AnswerImplement(context.Background(), "implement-question", "yes")
	if err != nil || result.State.Phase != task.PhaseImplementationReady {
		t.Fatalf("answer result=%#v err=%v", result, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var requests []runner.Request
	for _, request := range fake.requests {
		if request.Role == runner.RoleImplementer {
			requests = append(requests, request)
		}
	}
	if len(requests) != 2 || !requests[1].Resume || requests[1].SessionID != "session-implementer" || !strings.Contains(requests[1].Prompt, "yes") {
		t.Fatalf("implementer session was not resumed: %#v", requests)
	}
}

func TestPlanReviewStopsAfterFiveAcceptedRounds(t *testing.T) {
	root := workflowRepo(t)
	plannerResponses := []runner.Envelope{{Role: string(runner.RolePlanner), Status: "ready", Plan: "plan 0"}}
	reviewerResponses := []runner.Envelope{}
	for i := 1; i <= MaxRounds; i++ {
		reviewerResponses = append(reviewerResponses, runner.Envelope{Role: string(runner.RolePlanReviewer), Verdict: "changes_requested", Findings: []task.Finding{{Message: "revision required"}}})
		if i < MaxRounds {
			plannerResponses = append(plannerResponses, runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: "revised plan"})
		}
	}
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner:      plannerResponses,
			runner.RolePlanReviewer: reviewerResponses,
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	if _, err := service.StartPlan(context.Background(), "never approved", "exhaust"); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReviewPlan(context.Background(), "exhaust")
	if ExitCode(err) != ExitExhausted || result.State.Phase != task.PhaseFailed || result.State.PlanRound != MaxRounds {
		t.Fatalf("result=%#v err=%v exit=%d", result, err, ExitCode(err))
	}
}

type blockingAdapter struct {
	entered chan struct{}
	release chan struct{}
}

type failOnceAdapter struct {
	requests []runner.Request
}

func (f *failOnceAdapter) Run(_ context.Context, req runner.Request, callbacks runner.Callbacks) (runner.Response, error) {
	f.requests = append(f.requests, req)
	if len(f.requests) == 1 {
		if err := callbacks.SessionStarted("durable-planner"); err != nil {
			return runner.Response{}, err
		}
		return runner.Response{SessionID: "durable-planner"}, &runner.ProviderError{Code: "RATE_LIMITED", Message: "try later", Retryable: true, KnownSession: true, SessionID: "durable-planner"}
	}
	envelope := runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: "recovered plan"}
	return runner.Response{SessionID: "durable-planner", Envelope: &envelope}, nil
}
func (f *failOnceAdapter) ListModels(context.Context, runner.ModelListRequest) (runner.ModelPage, error) {
	return runner.ModelPage{}, nil
}
func (f *failOnceAdapter) Version(context.Context) (string, error) { return "test", nil }
func (f *failOnceAdapter) Auth(context.Context) (runner.AuthStatus, error) {
	return runner.AuthStatus{Authenticated: true}, nil
}

func (f *blockingAdapter) Run(_ context.Context, req runner.Request, callbacks runner.Callbacks) (runner.Response, error) {
	session := "session-" + req.Model
	if callbacks.SessionStarted != nil {
		if err := callbacks.SessionStarted(session); err != nil {
			return runner.Response{}, err
		}
	}
	f.entered <- struct{}{}
	<-f.release
	envelope := runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: "plan"}
	return runner.Response{SessionID: session, Envelope: &envelope}, nil
}
func (f *blockingAdapter) ListModels(context.Context, runner.ModelListRequest) (runner.ModelPage, error) {
	return runner.ModelPage{}, nil
}
func (f *blockingAdapter) Version(context.Context) (string, error) { return "test", nil }
func (f *blockingAdapter) Auth(context.Context) (runner.AuthStatus, error) {
	return runner.AuthStatus{Authenticated: true}, nil
}

func TestDifferentTaskIDsCanRunProviderCallsConcurrently(t *testing.T) {
	root := workflowRepo(t)
	fake := &blockingAdapter{entered: make(chan struct{}, 2), release: make(chan struct{})}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	done := make(chan error, 2)
	for _, id := range []string{"parallel-a", "parallel-b"} {
		id := id
		go func() {
			_, err := service.StartPlan(context.Background(), id, id)
			done <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-fake.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("different task IDs were serialized")
		}
	}
	close(fake.release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestSameTaskRejectsAnotherPhaseWhileProviderIsRunning(t *testing.T) {
	root := workflowRepo(t)
	fake := &blockingAdapter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	done := make(chan error, 1)
	go func() {
		_, err := service.StartPlan(context.Background(), "change app", "one-at-a-time")
		done <- err
	}()
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider call did not start")
	}
	if _, err := service.ReviewPlan(context.Background(), "one-at-a-time"); ExitCode(err) != ExitInFlight {
		t.Fatalf("same-task review error=%v exit=%d", err, ExitCode(err))
	}
	close(fake.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestScopedMutationDuringReviewConsumesNoRoundAndRetryResumesReviewer(t *testing.T) {
	root := workflowRepo(t)
	mutations := 0
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner:      {{Role: string(runner.RolePlanner), Status: "ready", Plan: "plan"}},
			runner.RolePlanReviewer: {{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}}},
			runner.RoleImplementer:  {{Role: string(runner.RoleImplementer), Status: "ready"}},
			runner.RoleCodeReviewer: {
				{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}},
				{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}},
			},
		},
	}
	fake.hook = func(req runner.Request) {
		if req.Role == runner.RoleCodeReviewer && mutations == 0 {
			mutations++
			if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n// concurrent change\n"), 0o600); err != nil {
				t.Errorf("mutate: %v", err)
			}
		}
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	if _, err := service.StartPlan(context.Background(), "change app", "barrier"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReviewPlan(context.Background(), "barrier"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Implement(context.Background(), "barrier", "app.go"); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReviewCode(context.Background(), "barrier")
	if ExitCode(err) != ExitReviewNeeded || result.State.CodeRound != 0 || result.State.Retry == nil {
		t.Fatalf("barrier result=%#v err=%v exit=%d", result, err, ExitCode(err))
	}
	result, err = service.Retry(context.Background(), "barrier")
	if err != nil || result.State.Phase != task.PhaseApproved || result.State.CodeRound != 1 {
		t.Fatalf("retry result=%#v err=%v", result, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var reviews []runner.Request
	for _, request := range fake.requests {
		if request.Role == runner.RoleCodeReviewer {
			reviews = append(reviews, request)
		}
	}
	if len(reviews) != 2 || !reviews[1].Resume || reviews[1].SessionID != "session-code_reviewer" || !strings.Contains(reviews[1].Prompt, "changed while your previous review") {
		t.Fatalf("review retry did not resume safely: %#v", reviews)
	}
}

func TestKnownSessionProviderFailureRetriesExactTurn(t *testing.T) {
	root := workflowRepo(t)
	fake := &failOnceAdapter{}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	_, err := service.StartPlan(context.Background(), "recover", "retryable")
	if ExitCode(err) != ExitAction {
		t.Fatalf("start error=%v exit=%d", err, ExitCode(err))
	}
	state, err := service.Status("retryable")
	if err != nil || state.Retry == nil || !state.Retry.KnownSession || state.Retry.SessionID != "durable-planner" {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	result, err := service.Retry(context.Background(), "retryable")
	if err != nil || result.State.Plan != "recovered plan" || len(fake.requests) != 2 {
		t.Fatalf("result=%#v requests=%#v err=%v", result, fake.requests, err)
	}
	if !fake.requests[1].Resume || fake.requests[1].SessionID != "durable-planner" || fake.requests[1].Prompt != fake.requests[0].Prompt {
		t.Fatalf("retry changed the provider turn: %#v", fake.requests)
	}
}
