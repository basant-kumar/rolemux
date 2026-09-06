package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	failures  map[runner.Role]int
	requests  []runner.Request
	sessions  map[runner.Role]string
	hook      func(runner.Request)
	listCalls int
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
	if f.failures != nil && f.failures[req.Role] > 0 {
		f.failures[req.Role]--
		return runner.Response{SessionID: session}, &runner.ProviderError{Code: "FIX_FAILED", Message: "fix failed", Retryable: true, KnownSession: true, SessionID: session}
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
	f.mu.Lock()
	f.listCalls++
	f.mu.Unlock()
	return workflowTestModels(), nil
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
		config.RolePlanner:     {Provider: "codex", Model: "planner-model", Effort: "max", Speed: "priority"},
		config.RoleImplementer: {Provider: "codex", Model: "implementer-model", Effort: "max"},
		config.RoleReviewer:    {Provider: "codex", Model: "reviewer-model", Effort: "xhigh"},
	}
	return cfg
}

func workflowTestModels() runner.ModelPage {
	models := []runner.ModelInfo{}
	for _, id := range []string{"planner-model", "implementer-model", "reviewer-model"} {
		models = append(models, runner.ModelInfo{
			ID: id, Availability: "available",
			Efforts:       []string{"max", "xhigh"},
			EffortOptions: []runner.ModelOption{{ID: "max"}, {ID: "xhigh"}},
			SpeedOptions:  []runner.ModelOption{{ID: "priority"}},
		})
	}
	return runner.ModelPage{Models: models}
}

func markPlanHumanApproved(st *task.State) {
	if st == nil {
		return
	}
	if st.Plan == "" {
		st.Plan = "approved test plan"
	}
	if st.PlanHash == "" {
		st.PlanHash = hash(st.Plan)
	}
	if st.ApprovedPlanHash == "" {
		st.ApprovedPlanHash = st.PlanHash
	}
	fingerprint := planReviewFingerprint(*st)
	st.ApprovalGateSchemaVersion = task.ApprovalGateSchemaVersion
	st.Approval = &task.ApprovalRecord{
		GateID:             "test-plan-approval-" + st.ID,
		Kind:               task.ApprovalKindPlan,
		Status:             task.ApprovalDecisionApprove,
		SubjectFingerprint: fingerprint,
	}
}

func approveIfRequired(ctx context.Context, service *Service, id string, result Result, err error) (Result, error) {
	if result.Status != "approval_required" || ExitCode(err) != ExitNeedsInput || result.State.Approval == nil {
		return result, err
	}
	if result.State.ID != "" {
		id = result.State.ID
	}
	return service.RespondApproval(ctx, id, result.State.Approval.GateID, task.ApprovalDecisionApprove, "")
}

func TestRuntimeSnapshotUsesCopilotGateway(t *testing.T) {
	snapshot := RuntimeSnapshot("copilot", config.Provider{GatewayURL: "https://gateway.example.invalid", BearerTokenEnv: "COPILOT_TOKEN"})
	if snapshot.Endpoint != "https://gateway.example.invalid" || snapshot.SDKSettings["base_url"] != snapshot.Endpoint || snapshot.SDKSettings["bearer_token_env"] != "COPILOT_TOKEN" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestWorkGraphStartsOnlyReadyIndependentUnits(t *testing.T) {
	root := workflowRepo(t)
	service := New(root, workflowConfig(), nil)
	units, err := task.NormalizeWorkUnits([]task.WorkUnit{
		{ID: "T1", Objective: "one", Scope: "one.go", ExecutionPacket: "one packet", AcceptanceCriteria: []string{"one"}, ValidationCommands: []string{"test one"}},
		{ID: "T2", Objective: "two", Scope: "two.go", ExecutionPacket: "two packet", AcceptanceCriteria: []string{"two"}, ValidationCommands: []string{"test two"}},
		{ID: "T3", Objective: "three", Scope: "three.go", DependsOn: []string{"T1", "T2"}, ExecutionPacket: "three packet", AcceptanceCriteria: []string{"three"}, ValidationCommands: []string{"test three"}},
	}, "plan")
	if err != nil {
		t.Fatal(err)
	}
	parent := task.State{ID: "dag", RepoRoot: root, Phase: task.PhasePlanApproved, Task: "dag", Plan: "plan", PlanHash: hash("plan"), ApprovedPlanHash: hash("plan"), WorkUnits: units, ProfilesSnapshot: map[string]task.ProfileSnapshot{}, RuntimeSnapshot: map[string]task.RuntimeSnapshot{}, MaxRounds: MaxRounds}
	markPlanHumanApproved(&parent)
	if err := service.Store.Create(parent); err != nil {
		t.Fatal(err)
	}
	graph, err := service.Graph("dag")
	if err != nil || strings.Join(graph.Ready, ",") != "T1,T2" {
		t.Fatalf("graph=%#v err=%v", graph, err)
	}
	if _, err := service.StartWork("dag", "T3"); err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("blocked T3 started: %v", err)
	}
	for _, id := range []string{"T1", "T2"} {
		result, startErr := service.StartWork("dag", id)
		wantScope := map[string]string{"T1": "one.go", "T2": "two.go"}[id]
		if startErr != nil || result.State.ParentTaskID != "dag" || result.State.PlannedScope != wantScope {
			t.Fatalf("start %s: %#v err=%v", id, result.State, startErr)
		}
		if _, updateErr := service.Store.Update(result.State.ID, func(st *task.State) error { st.Phase = task.PhaseApproved; return nil }); updateErr != nil {
			t.Fatal(updateErr)
		}
	}
	graph, err = service.Graph("dag")
	if err != nil || strings.Join(graph.Ready, ",") != "T3" {
		t.Fatalf("graph after dependencies=%#v err=%v", graph, err)
	}
}

func TestIntegrationReviewUsesFreshFixerThenResumesReviewer(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
		runner.RoleCodeReviewer: {
			{Role: string(runner.RoleCodeReviewer), Verdict: "changes_requested", Findings: []task.Finding{{Path: "app.go", Message: "align cross-unit contract"}}},
			{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}},
		},
		runner.RoleImplementer: {{Role: string(runner.RoleImplementer), Status: "ready"}},
	}}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	fake.hook = func(req runner.Request) {
		if req.Role == runner.RoleImplementer {
			if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst integrated = true\nconst fixed = true\n"), 0o600); err != nil {
				t.Errorf("mutate integration candidate: %v", err)
			}
		}
	}
	profiles, runtimes, err := service.snapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	units, err := task.NormalizeWorkUnits([]task.WorkUnit{{ID: "T1", Objective: "change app", Scope: "app.go", ExecutionPacket: "change app", AcceptanceCriteria: []string{"works"}, ValidationCommands: []string{"go test ./..."}}}, "plan")
	if err != nil {
		t.Fatal(err)
	}
	parent := task.State{ID: "integration-parent", RepoRoot: root, Phase: task.PhasePlanApproved, Task: "change app", Plan: "approved plan", PlanHash: hash("approved plan"), ApprovedPlanHash: hash("approved plan"), WorkUnits: units, ProfilesSnapshot: profiles, RuntimeSnapshot: runtimes, MaxRounds: MaxRounds}
	markPlanHumanApproved(&parent)
	if err := service.Store.Create(parent); err != nil {
		t.Fatal(err)
	}
	baseline, err := service.Observe("app.go")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err = service.Capture(baseline, "unit-baseline", task.WorkTaskID(parent.ID, "T1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst integrated = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child := task.State{ID: task.WorkTaskID(parent.ID, "T1"), RepoRoot: root, Phase: task.PhaseApproved, ParentTaskID: parent.ID, WorkUnitID: "T1", Scope: "app.go", ScopedBaseline: baseline, ProfilesSnapshot: profiles, RuntimeSnapshot: runtimes, MaxRounds: MaxRounds}
	if err := service.Store.Create(child); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReviewIntegration(context.Background(), parent.ID)
	if err != nil || result.Status != "fixed" || result.State.Phase != task.PhaseImplementationReady || result.State.CodeRound != 1 || !result.State.IntegrationReview || result.State.ParentTaskID != parent.ID {
		t.Fatalf("integration=%#v err=%v", result.State, err)
	}
	result, err = service.ReviewIntegration(context.Background(), parent.ID)
	result, err = approveIfRequired(context.Background(), service, parent.ID, result, err)
	if err != nil || result.Status != "approved" || result.State.Phase != task.PhaseApproved || result.State.CodeRound != 2 {
		t.Fatalf("integration approval=%#v err=%v", result.State, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 3 || fake.requests[0].Role != runner.RoleCodeReviewer || fake.requests[1].Role != runner.RoleImplementer || fake.requests[1].Resume || fake.requests[2].Role != runner.RoleCodeReviewer || !fake.requests[2].Resume {
		t.Fatalf("integration requests=%#v", fake.requests)
	}
	if !strings.Contains(fake.requests[0].Prompt, "Deep integration-review boundary") || !strings.Contains(fake.requests[1].Prompt, "fresh integration-fix session") {
		t.Fatalf("integration prompts reviewer=%q implementer=%q", fake.requests[0].Prompt, fake.requests[1].Prompt)
	}
}

func TestCumulativeUsageDeltaHandlesGrowthAndProcessReset(t *testing.T) {
	if got := tokenDelta(140, 100); got != 40 {
		t.Fatalf("growth delta=%d", got)
	}
	if got := tokenDelta(30, 100); got != 30 {
		t.Fatalf("reset delta=%d", got)
	}
	if got := tokenDelta(100, 100); got != 0 {
		t.Fatalf("unchanged delta=%d", got)
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
	implementerCalls := 0
	fake.hook = func(req runner.Request) {
		if req.Role == runner.RoleImplementer {
			implementerCalls++
			if implementerCalls == 2 {
				if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst review_fixed = true\n"), 0o600); err != nil {
					t.Errorf("mutate code-review candidate: %v", err)
				}
			}
		}
	}
	service.Capabilities = func(provider string, role runner.Role, taskText string) CapabilityContext {
		if provider != "codex" || taskText != "change app" {
			t.Fatalf("capability inputs provider=%q role=%q task=%q", provider, role, taskText)
		}
		return CapabilityContext{Note: "CAPABILITY-NOTE-" + string(role), SkillDirectories: []string{"/skills/" + string(role)}}
	}
	planned, err := service.StartPlan(context.Background(), "change app", "loop")
	if err != nil || planned.State.Phase != task.PhasePlanned {
		t.Fatalf("start: phase=%s err=%v", planned.State.Phase, err)
	}
	revisedPlan, err := service.ReviewPlan(context.Background(), "loop")
	if err != nil || revisedPlan.Status != "revised" || revisedPlan.State.Phase != task.PhasePlanned || revisedPlan.State.Plan != "plan v2" || revisedPlan.State.PlanRound != 1 {
		t.Fatalf("plan revision: %#v err=%v", revisedPlan.State, err)
	}
	approvedPlan, err := service.ReviewPlan(context.Background(), "loop")
	approvedPlan, err = approveIfRequired(context.Background(), service, "loop", approvedPlan, err)
	if err != nil || approvedPlan.State.Phase != task.PhasePlanApproved || approvedPlan.State.Plan != "plan v2" || approvedPlan.State.PlanRound != 2 {
		t.Fatalf("plan approval: %#v err=%v", approvedPlan.State, err)
	}
	implemented, err := service.Implement(context.Background(), "loop", "app.go")
	if err != nil || implemented.State.Phase != task.PhaseImplementationReady {
		t.Fatalf("implement: phase=%s err=%v", implemented.State.Phase, err)
	}
	fixedCode, err := service.ReviewCode(context.Background(), "loop")
	if err != nil || fixedCode.Status != "fixed" || fixedCode.State.Phase != task.PhaseImplementationReady || fixedCode.State.CodeRound != 1 {
		t.Fatalf("code fix: %#v err=%v", fixedCode.State, err)
	}
	approvedCode, err := service.ReviewCode(context.Background(), "loop")
	approvedCode, err = approveIfRequired(context.Background(), service, "loop", approvedCode, err)
	if err != nil || approvedCode.State.Phase != task.PhaseApproved || approvedCode.State.CodeRound != 2 {
		t.Fatalf("code approval: %#v err=%v", approvedCode.State, err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	counts := map[runner.Role]int{}
	promptBytes := map[runner.Role]int64{}
	for _, request := range fake.requests {
		counts[request.Role]++
		promptBytes[request.Role] += int64(len(request.Prompt))
		if request.Role == runner.RolePlanner && request.Speed != "priority" {
			t.Fatalf("planner speed=%q", request.Speed)
		}
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
		capabilityNote := "CAPABILITY-NOTE-" + string(request.Role)
		if counts[request.Role] == 1 && !strings.Contains(request.Prompt, capabilityNote) {
			t.Fatalf("initial %s prompt omitted capability note", request.Role)
		}
		if counts[request.Role] > 1 && strings.Contains(request.Prompt, capabilityNote) {
			t.Fatalf("resumed %s prompt repeated capability note", request.Role)
		}
		if !reflect.DeepEqual(request.SkillDirectories, []string{"/skills/" + string(request.Role)}) {
			t.Fatalf("%s skill directories = %#v", request.Role, request.SkillDirectories)
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
		if usage.Requests != 2 || usage.PromptBytes != promptBytes[role] {
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
	planResult, err := service.ReviewPlan(context.Background(), "implement-question")
	if _, err = approveIfRequired(context.Background(), service, "implement-question", planResult, err); err != nil {
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
			plannerResponses = append(plannerResponses, runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: fmt.Sprintf("revised plan %d", i)})
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
	for round := 1; round <= MaxRounds; round++ {
		result, err := service.ReviewPlan(context.Background(), "exhaust")
		if round < MaxRounds {
			if err != nil || result.Status != "revised" || result.State.Phase != task.PhasePlanned || result.State.PlanRound != round {
				t.Fatalf("round %d result=%#v err=%v", round, result, err)
			}
			continue
		}
		if ExitCode(err) != ExitExhausted || result.State.Phase != task.PhaseFailed || result.State.PlanRound != MaxRounds || result.Status != "exhausted" {
			t.Fatalf("result=%#v err=%v exit=%d", result, err, ExitCode(err))
		}
	}
	fake.mu.Lock()
	calls := len(fake.requests)
	fake.mu.Unlock()
	result, err := service.ReviewPlan(context.Background(), "exhaust")
	if ExitCode(err) != ExitExhausted || result.Status != "exhausted" {
		t.Fatalf("repeated review result=%#v err=%v", result, err)
	}
	if _, err = service.Retry(context.Background(), "exhaust"); ExitCode(err) != ExitExhausted {
		t.Fatalf("repeated retry err=%v exit=%d", err, ExitCode(err))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != calls {
		t.Fatalf("exhausted task made provider call: before=%d after=%d", calls, len(fake.requests))
	}
}

type blockingAdapter struct {
	entered chan struct{}
	release chan struct{}
}

type failOnceAdapter struct {
	requests []runner.Request
}

type interruptOnceAdapter struct {
	requests []runner.Request
}

func (adapter *interruptOnceAdapter) Run(ctx context.Context, req runner.Request, callbacks runner.Callbacks) (runner.Response, error) {
	adapter.requests = append(adapter.requests, req)
	if len(adapter.requests) == 1 {
		if err := callbacks.SessionStarted("interrupt-session"); err != nil {
			return runner.Response{}, err
		}
		<-ctx.Done()
		return runner.Response{SessionID: "interrupt-session"}, &runner.ProviderError{Code: "INTERRUPTED", Message: "provider interrupted", Retryable: true, KnownSession: true, SessionID: "interrupt-session", Cause: ctx.Err()}
	}
	return runner.Response{SessionID: "interrupt-session", Envelope: &runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: "recovered"}}, nil
}

func (adapter *interruptOnceAdapter) ListModels(context.Context, runner.ModelListRequest) (runner.ModelPage, error) {
	return workflowTestModels(), nil
}
func (adapter *interruptOnceAdapter) Version(context.Context) (string, error) { return "test", nil }
func (adapter *interruptOnceAdapter) Auth(context.Context) (runner.AuthStatus, error) {
	return runner.AuthStatus{Authenticated: true}, nil
}

func TestCanceledProviderTurnBecomesSameSessionRetry(t *testing.T) {
	root := workflowRepo(t)
	adapter := &interruptOnceAdapter{}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": adapter})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := service.StartPlan(ctx, "recover interrupt", "interrupt-retry")
	if ExitCode(err) != ExitAction {
		t.Fatalf("cancel error=%v exit=%d", err, ExitCode(err))
	}
	state, err := service.Status("interrupt-retry")
	if err != nil || state.InFlight != nil || state.Retry == nil || !state.Retry.KnownSession || state.Retry.SessionID != "interrupt-session" {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	result, err := service.Retry(context.Background(), "interrupt-retry")
	if err != nil || result.State.Plan != "recovered" || len(adapter.requests) != 2 || !adapter.requests[1].Resume || adapter.requests[1].SessionID != "interrupt-session" {
		t.Fatalf("result=%#v requests=%#v err=%v", result, adapter.requests, err)
	}
}

func abandonedPlannerState(root, id, session string, ownerPID int, startedAt time.Time) task.State {
	return task.State{
		ID: id, RepoRoot: root, Phase: task.PhasePlanned, Task: "recover abandoned turn", MaxRounds: MaxRounds,
		ProfilesSnapshot: map[string]task.ProfileSnapshot{
			string(runner.RolePlanner): {Provider: "codex", Model: "planner-model", Effort: "max"},
		},
		PlannerSessionID: session,
		InFlight: &task.InFlight{
			Token: "abandoned-token", Operation: "plan_start", Role: string(runner.RolePlanner), OwnerPID: ownerPID,
			StartedAt: startedAt, KnownSession: session != "", SessionID: session, PreviousPhase: task.PhasePlanned,
			Prompt: "continue the interrupted plan", Loop: "plan_initial",
		},
	}
}

func TestRetryRecoversAbandonedOwnerInSameSession(t *testing.T) {
	root := workflowRepo(t)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{runner.RolePlanner: "durable-session"},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner: {{Role: string(runner.RolePlanner), Status: "ready", Plan: "recovered abandoned plan"}},
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	service.Now = func() time.Time { return now }
	service.ProcessID = func() int { return 9002 }
	service.ProcessAlive = func(pid int) bool { return pid != 9001 }
	if err := service.Store.Create(abandonedPlannerState(root, "abandoned", "durable-session", 9001, now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}

	result, err := service.Retry(context.Background(), "abandoned")
	if err != nil || result.State.Plan != "recovered abandoned plan" || len(fake.requests) != 1 {
		t.Fatalf("result=%#v requests=%#v err=%v", result, fake.requests, err)
	}
	request := fake.requests[0]
	if !request.Resume || request.SessionID != "durable-session" || request.Prompt != "continue the interrupted plan" {
		t.Fatalf("abandoned retry changed provider continuity: %#v", request)
	}
}

func TestRetryDoesNotStealLiveOwnedOperation(t *testing.T) {
	root := workflowRepo(t)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	service := New(root, workflowConfig(), nil)
	service.Now = func() time.Time { return now }
	service.ProcessAlive = func(int) bool { return true }
	if err := service.Store.Create(abandonedPlannerState(root, "live-owner", "durable-session", 9001, now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}

	_, err := service.Retry(context.Background(), "live-owner")
	if ExitCode(err) != ExitInFlight {
		t.Fatalf("live owner error=%v exit=%d", err, ExitCode(err))
	}
	state, loadErr := service.Status("live-owner")
	if loadErr != nil || state.InFlight == nil || state.Retry != nil {
		t.Fatalf("live state=%#v err=%v", state, loadErr)
	}
}

func TestRetryRecoversExpiredLegacyOperationWithoutOwnerPID(t *testing.T) {
	root := workflowRepo(t)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{runner.RolePlanner: "legacy-session"},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner: {{Role: string(runner.RolePlanner), Status: "ready", Plan: "legacy recovered"}},
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	service.Now = func() time.Time { return now }
	legacy := abandonedPlannerState(root, "legacy-owner", "legacy-session", 0, now.Add(-20*time.Minute))
	if err := service.Store.Create(legacy); err != nil {
		t.Fatal(err)
	}

	result, err := service.Retry(context.Background(), "legacy-owner")
	if err != nil || result.State.Plan != "legacy recovered" || len(fake.requests) != 1 || !fake.requests[0].Resume {
		t.Fatalf("legacy result=%#v requests=%#v err=%v", result, fake.requests, err)
	}
}

func TestRetryRefusesFreshSessionForAbandonedOperation(t *testing.T) {
	root := workflowRepo(t)
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	service := New(root, workflowConfig(), nil)
	service.Now = func() time.Time { return now }
	service.ProcessAlive = func(int) bool { return false }
	if err := service.Store.Create(abandonedPlannerState(root, "no-session", "", 9001, now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}

	_, err := service.Retry(context.Background(), "no-session")
	var workflowErr *Error
	if !errors.As(err, &workflowErr) || workflowErr.Code != "INTERRUPTED_UNRECOVERABLE" || workflowErr.Retryable {
		t.Fatalf("unrecoverable error=%#v", err)
	}
	state, loadErr := service.Status("no-session")
	if loadErr != nil || state.Phase != task.PhaseFailed || state.InFlight != nil || state.Retry != nil {
		t.Fatalf("unrecoverable state=%#v err=%v", state, loadErr)
	}
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
	return workflowTestModels(), nil
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
	return workflowTestModels(), nil
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
	planResult, err := service.ReviewPlan(context.Background(), "barrier")
	if _, err = approveIfRequired(context.Background(), service, "barrier", planResult, err); err != nil {
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
	result, err = approveIfRequired(context.Background(), service, "barrier", result, err)
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

func TestCodeReviewRefreshesCandidateChangedBeforeReviewStarts(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner:      {{Role: string(runner.RolePlanner), Status: "ready", Plan: "plan"}},
			runner.RolePlanReviewer: {{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}}},
			runner.RoleImplementer:  {{Role: string(runner.RoleImplementer), Status: "ready"}},
			runner.RoleCodeReviewer: {{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}}},
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	if _, err := service.StartPlan(context.Background(), "change app", "pre-review-refresh"); err != nil {
		t.Fatal(err)
	}
	planResult, err := service.ReviewPlan(context.Background(), "pre-review-refresh")
	if _, err = approveIfRequired(context.Background(), service, "pre-review-refresh", planResult, err); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Implement(context.Background(), "pre-review-refresh", "app.go"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n// changed before review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReviewCode(context.Background(), "pre-review-refresh")
	result, err = approveIfRequired(context.Background(), service, "pre-review-refresh", result, err)
	if err != nil || result.State.Phase != task.PhaseApproved {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var prompt string
	for _, request := range fake.requests {
		if request.Role == runner.RoleCodeReviewer {
			prompt = request.Prompt
		}
	}
	if !strings.Contains(prompt, `"changed"`) || !strings.Contains(prompt, `"path":"app.go"`) {
		t.Fatalf("review prompt used stale candidate: %s", prompt)
	}
	manifest, err := service.Observe("app.go")
	if err != nil || result.State.ApprovedManifestHash != task.HashManifest(manifest) {
		t.Fatalf("approval did not bind refreshed candidate: err=%v", err)
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

func TestCodeReviewerRetryWithUnchangedCandidateContinuesWithoutRepeatingEvidence(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{runner.RoleCodeReviewer: "durable-reviewer"},
		responses: map[runner.Role][]runner.Envelope{
			runner.RoleCodeReviewer: {{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}}},
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	candidate, err := service.Observe("app.go")
	if err != nil {
		t.Fatal(err)
	}
	state := task.State{
		ID: "review-retry", RepoRoot: root, Phase: task.PhaseFailed, Task: "change app", Plan: "full approved plan",
		Scope: "app.go", CandidateManifest: candidate, CandidateManifestHash: task.HashManifest(candidate), MaxRounds: MaxRounds,
		CodeReviewerSessionID: "durable-reviewer",
		ProfilesSnapshot: map[string]task.ProfileSnapshot{
			string(runner.RoleCodeReviewer): {Provider: "codex", Model: "reviewer-model", Effort: "xhigh"},
		},
		Retry: &task.RetryState{
			Operation: "code_review", Role: string(runner.RoleCodeReviewer), PreviousPhase: task.PhaseImplementationReady,
			Prompt: "the original large review prompt", KnownSession: true, SessionID: "durable-reviewer",
			SnapshotManifest: candidate, Scope: "app.go", Loop: "code_review",
		},
	}
	if err := service.Store.Create(state); err != nil {
		t.Fatal(err)
	}
	result, err := service.Retry(context.Background(), state.ID)
	result, err = approveIfRequired(context.Background(), service, state.ID, result, err)
	if err != nil || result.State.Phase != task.PhaseApproved {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests=%#v", fake.requests)
	}
	prompt := fake.requests[0].Prompt
	if !strings.Contains(prompt, "candidate is unchanged") || !strings.Contains(prompt, "do not reread files") {
		t.Fatalf("retry prompt did not continue existing review: %q", prompt)
	}
	if strings.Contains(prompt, state.Plan) || strings.Contains(prompt, "original large review prompt") {
		t.Fatalf("retry repeated unchanged evidence: %q", prompt)
	}
}

func TestCodeReviewRevisionPromptUsesOnlyFixCheckpointDelta(t *testing.T) {
	entry := func(hash string) task.FileEntry {
		return task.FileEntry{Path: "app.go", Kind: "file", Worktree: task.ContentState{Present: true, Hash: hash}}
	}
	state := task.State{
		Task: "large unchanged task", Plan: "large unchanged plan", Scope: "app.go",
		ScopedBaseline:           []task.FileEntry{entry("baseline")},
		CandidateManifest:        []task.FileEntry{entry("fixed")},
		ReviewCheckpoint:         []task.FileEntry{entry("reviewed")},
		ReviewCheckpointHash:     task.HashManifest([]task.FileEntry{entry("reviewed")}),
		ReviewCheckpointFindings: []task.Finding{{Path: "app.go", Line: 7, Message: "handle the edge case"}},
	}
	prompt := codeReviewPrompt(state, true)
	for _, required := range []string{"Fix delta since the previous completed review", "handle the edge case", "do not restart the original review"} {
		if !strings.Contains(strings.ToLower(prompt), strings.ToLower(required)) {
			t.Fatalf("revision prompt omitted %q: %s", required, prompt)
		}
	}
	if strings.Contains(prompt, state.Task) || strings.Contains(prompt, state.Plan) || strings.Contains(prompt, "baseline") {
		t.Fatalf("revision prompt repeated full task evidence: %s", prompt)
	}
}

func TestPlannerAndImplementerPromptsAssignResearchToPlanner(t *testing.T) {
	planning := plannerPrompt("change app", nil)
	review := planReviewPrompt("change app", "plan", task.ComplexitySmall, []task.WorkUnit{{ID: "W1", Scope: "app.go,app_test.go"}}, true)
	implementation := implementPrompt("change app", "packet", "app.go", nil)
	for _, required := range []string{"primary research and architecture brain", "execution packet", "direct blast radius", "validation commands"} {
		if !strings.Contains(planning, required) {
			t.Fatalf("planner prompt omitted %q", required)
		}
	}
	for _, required := range []string{"authoritative", "at most three", "repository-wide searches", "needs_input"} {
		if !strings.Contains(implementation, required) {
			t.Fatalf("implementer prompt omitted %q", required)
		}
	}
	for _, required := range []string{"comma-separated", "commas between entries are valid"} {
		if !strings.Contains(review, required) {
			t.Fatalf("plan review prompt omitted canonical scope rule %q", required)
		}
	}
}

func TestDelegatedRolePromptsBoundAgentLatency(t *testing.T) {
	implementation := implementPrompt("change app", "packet", "app.go", nil)
	planReview := planReviewPrompt("change app", "plan", task.ComplexitySmall, []task.WorkUnit{{ID: "W1", Scope: "app.go"}}, false)
	review := codeReviewPrompt(task.State{Task: "change app", Plan: "plan", Scope: "app.go"}, false)
	for _, required := range []string{"at most three", "batched pre-edit", "git status/diff/log", "repository-wide searches", "repository-wide surveys", "post-green survey", "cohesive edits", "narrow validation", "full repository suite", "30 seconds", "one-second polling", "stop immediately", "focused validation passes"} {
		if !strings.Contains(implementation, required) {
			t.Fatalf("implementer prompt omitted %q: %s", required, implementation)
		}
	}
	for _, required := range []string{"supplied delta and evidence", "changed files", "direct blast radius", "no git commands", "full suite", "validated verdict promptly"} {
		if !strings.Contains(review, required) {
			t.Fatalf("review prompt omitted %q: %s", required, review)
		}
	}
	for _, required := range []string{"supplied task, plan, and work graph", "without redoing repository research"} {
		if !strings.Contains(planReview, required) {
			t.Fatalf("plan-review prompt omitted %q: %s", required, planReview)
		}
	}
}

func reviewLimitConfig(limit int) config.Config {
	cfg := workflowConfig()
	cfg.ReviewMaxRounds = &limit
	return cfg
}

func reviewProfiles() (map[string]task.ProfileSnapshot, map[string]task.RuntimeSnapshot) {
	profiles := map[string]task.ProfileSnapshot{}
	runtimes := map[string]task.RuntimeSnapshot{}
	for _, role := range []runner.Role{runner.RolePlanner, runner.RolePlanReviewer, runner.RoleImplementer, runner.RoleCodeReviewer} {
		profiles[string(role)] = task.ProfileSnapshot{Provider: "codex", Model: string(role)}
		runtimes[string(role)] = task.RuntimeSnapshot{}
	}
	return profiles, runtimes
}

func readyCodeState(t *testing.T, service *Service, id string, limit int, integration bool) task.State {
	t.Helper()
	manifest, err := service.Observe("app.go")
	if err != nil {
		t.Fatal(err)
	}
	profiles, runtimes := reviewProfiles()
	plan := "approved plan"
	state := task.State{
		ID: id, RepoRoot: service.RepoRoot, Phase: task.PhaseImplementationReady, Task: "change app",
		Plan: plan, PlanHash: hash(plan), ApprovedPlanHash: hash(plan), IntegrationReview: integration,
		Scope: "app.go", ScopedBaseline: cloneManifest(manifest), ScopedBaselineHash: task.HashManifest(manifest),
		CandidateManifest: cloneManifest(manifest), CandidateManifestHash: task.HashManifest(manifest),
		ProfilesSnapshot: profiles, RuntimeSnapshot: runtimes, MaxRounds: limit,
		ReviewPolicy: &task.ReviewPolicy{MaxRounds: limit},
	}
	if err := service.Store.Create(state); err != nil {
		t.Fatal(err)
	}
	return state
}

func reviewFixture(t *testing.T, kind string, limit int, fake *scriptedAdapter) (*Service, string) {
	t.Helper()
	root := workflowRepo(t)
	service := New(root, reviewLimitConfig(limit), map[string]runner.Adapter{"codex": fake})
	switch kind {
	case "plan":
		result, err := service.StartPlan(context.Background(), "review policy matrix", "matrix-plan")
		if err != nil {
			t.Fatalf("start plan: %v", err)
		}
		return service, result.State.ID
	case "code":
		state := readyCodeState(t, service, "matrix-code", limit, false)
		return service, state.ID
	case "integration":
		parentID := "matrix-integration-parent"
		readyCodeState(t, service, task.IntegrationTaskID(parentID), limit, true)
		return service, parentID
	default:
		t.Fatalf("unknown review kind %q", kind)
		return nil, ""
	}
}

func reviewFixtureCall(ctx context.Context, service *Service, kind, id string) (Result, error) {
	var result Result
	var err error
	switch kind {
	case "plan":
		result, err = service.ReviewPlan(ctx, id)
	case "code":
		result, err = service.ReviewCode(ctx, id)
	case "integration":
		result, err = service.ReviewIntegration(ctx, id)
	default:
		panic("unknown review kind " + kind)
	}
	return approveIfRequired(ctx, service, id, result, err)
}

func reviewFixtureTaskID(kind string, result Result, commandID string) string {
	if kind == "integration" {
		return result.State.ID
	}
	return commandID
}

func reviewFixtureRound(st task.State, kind string) int {
	return reviewRound(st, kind)
}

func TestReviewPolicyMatrixAcrossKindsAndReload(t *testing.T) {
	for _, kind := range []string{"plan", "code", "integration"} {
		for _, limit := range []int{0, 1, 2, 5} {
			t.Run(fmt.Sprintf("%s-limit-%d", kind, limit), func(t *testing.T) {
				rejectionRounds := limit
				if rejectionRounds == 0 {
					rejectionRounds = 6
				}
				fixRounds := rejectionRounds
				if limit > 0 {
					fixRounds--
				}

				reviewerRole := runner.RoleCodeReviewer
				if kind == "plan" {
					reviewerRole = runner.RolePlanReviewer
				}
				reviewerResponses := make([]runner.Envelope, 0, rejectionRounds+1)
				for round := 1; round <= rejectionRounds; round++ {
					reviewerResponses = append(reviewerResponses, runner.Envelope{
						Role: string(reviewerRole), Verdict: "changes_requested",
						Findings: []task.Finding{{Path: "app.go", Message: fmt.Sprintf("%s finding %d", kind, round)}},
					})
				}
				if limit == 0 {
					reviewerResponses = append(reviewerResponses, runner.Envelope{Role: string(reviewerRole), Verdict: "approved", Findings: []task.Finding{}})
				}
				fake := &scriptedAdapter{
					sessions:  map[runner.Role]string{},
					responses: map[runner.Role][]runner.Envelope{reviewerRole: reviewerResponses},
				}
				if kind == "plan" {
					plannerResponses := []runner.Envelope{{Role: string(runner.RolePlanner), Status: "ready", Plan: "matrix plan 0"}}
					for round := 1; round <= fixRounds; round++ {
						plannerResponses = append(plannerResponses, runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: fmt.Sprintf("matrix plan %d", round)})
					}
					fake.responses[runner.RolePlanner] = plannerResponses
				} else {
					implementerResponses := make([]runner.Envelope, fixRounds)
					for i := range implementerResponses {
						implementerResponses[i] = runner.Envelope{Role: string(runner.RoleImplementer), Status: "ready"}
					}
					fake.responses[runner.RoleImplementer] = implementerResponses
				}

				service, commandID := reviewFixture(t, kind, limit, fake)
				fixes := 0
				if kind != "plan" {
					fake.hook = func(req runner.Request) {
						if req.Role != runner.RoleImplementer {
							return
						}
						fixes++
						if err := os.WriteFile(filepath.Join(service.RepoRoot, "app.go"), []byte(fmt.Sprintf("package app\n\nconst matrix_fix = %d\n", fixes)), 0o600); err != nil {
							t.Errorf("write %s fix %d: %v", kind, fixes, err)
						}
					}
				}

				for round := 1; round <= rejectionRounds; round++ {
					result, err := reviewFixtureCall(context.Background(), service, kind, commandID)
					if limit > 0 && round == limit {
						if ExitCode(err) != ExitExhausted || result.Status != "exhausted" || result.State.Phase != task.PhaseFailed || reviewFixtureRound(result.State, kind) != limit {
							t.Fatalf("final %s review: result=%#v err=%v exit=%d", kind, result, err, ExitCode(err))
						}
						callsBefore := len(fake.requests)
						repeated, repeatedErr := reviewFixtureCall(context.Background(), service, kind, commandID)
						if ExitCode(repeatedErr) != ExitExhausted || repeated.Status != "exhausted" || len(fake.requests) != callsBefore {
							t.Fatalf("repeated exhausted %s review: result=%#v err=%v requests=%#v", kind, repeated, repeatedErr, fake.requests)
						}
						break
					}
					if err != nil || result.Status != map[string]string{"plan": "revised", "code": "fixed", "integration": "fixed"}[kind] || result.State.Phase != task.PhasePlanned && kind == "plan" || result.State.Phase != task.PhaseImplementationReady && kind != "plan" || reviewFixtureRound(result.State, kind) != round {
						t.Fatalf("round %d %s review: result=%#v err=%v exit=%d", round, kind, result, err, ExitCode(err))
					}
					service = New(service.RepoRoot, reviewLimitConfig(limit), map[string]runner.Adapter{"codex": fake})
				}

				if limit == 0 {
					service = New(service.RepoRoot, reviewLimitConfig(limit), map[string]runner.Adapter{"codex": fake})
					result, err := reviewFixtureCall(context.Background(), service, kind, commandID)
					gotRound := reviewFixtureRound(result.State, kind)
					wantPhase := task.PhaseApproved
					if kind == "plan" {
						wantPhase = task.PhasePlanApproved
					}
					if err != nil || result.Status != "approved" || result.State.Phase != wantPhase || gotRound != rejectionRounds+1 {
						t.Fatalf("unlimited approval %s: result=%#v err=%v exit=%d round=%d want=%d", kind, result, err, ExitCode(err), gotRound, rejectionRounds+1)
					}
				}

				fake.mu.Lock()
				requests := append([]runner.Request(nil), fake.requests...)
				fake.mu.Unlock()
				reviewerCalls, workerCalls := 0, 0
				for _, request := range requests {
					if request.Role == reviewerRole {
						reviewerCalls++
					}
					if request.Role == runner.RolePlanner || request.Role == runner.RoleImplementer {
						workerCalls++
					}
				}
				wantReviewerCalls := rejectionRounds
				if limit == 0 {
					wantReviewerCalls++
				}
				wantWorkerCalls := fixRounds
				if kind == "plan" {
					wantWorkerCalls++
				}
				if reviewerCalls != wantReviewerCalls || workerCalls != wantWorkerCalls {
					t.Fatalf("%s limit %d calls: reviewer=%d/%d worker=%d/%d requests=%#v", kind, limit, reviewerCalls, wantReviewerCalls, workerCalls, wantWorkerCalls, requests)
				}
			})
		}
	}
}

func TestFinalRoundApprovalAtConfiguredCeilingAcrossKindsAndReload(t *testing.T) {
	for _, kind := range []string{"plan", "code", "integration"} {
		for _, limit := range []int{1, 2, 5} {
			t.Run(fmt.Sprintf("%s-limit-%d", kind, limit), func(t *testing.T) {
				reviewerRole := runner.RoleCodeReviewer
				if kind == "plan" {
					reviewerRole = runner.RolePlanReviewer
				}
				reviewerResponses := make([]runner.Envelope, 0, limit)
				for round := 1; round <= limit; round++ {
					if round == limit {
						reviewerResponses = append(reviewerResponses, runner.Envelope{Role: string(reviewerRole), Verdict: "approved", Findings: []task.Finding{}})
						continue
					}
					reviewerResponses = append(reviewerResponses, runner.Envelope{
						Role: string(reviewerRole), Verdict: "changes_requested",
						Findings: []task.Finding{{Path: "app.go", Message: fmt.Sprintf("%s ceiling finding %d", kind, round)}},
					})
				}
				fake := &scriptedAdapter{
					sessions:  map[runner.Role]string{},
					responses: map[runner.Role][]runner.Envelope{reviewerRole: reviewerResponses},
				}
				fixRounds := limit - 1
				if kind == "plan" {
					plannerResponses := []runner.Envelope{{Role: string(runner.RolePlanner), Status: "ready", Plan: "ceiling plan 0"}}
					for round := 1; round <= fixRounds; round++ {
						plannerResponses = append(plannerResponses, runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: fmt.Sprintf("ceiling plan %d", round)})
					}
					fake.responses[runner.RolePlanner] = plannerResponses
				} else {
					implementerResponses := make([]runner.Envelope, fixRounds)
					for i := range implementerResponses {
						implementerResponses[i] = runner.Envelope{Role: string(runner.RoleImplementer), Status: "ready"}
					}
					fake.responses[runner.RoleImplementer] = implementerResponses
				}

				service, commandID := reviewFixture(t, kind, limit, fake)
				fixes := 0
				if kind != "plan" {
					fake.hook = func(req runner.Request) {
						if req.Role != runner.RoleImplementer {
							return
						}
						fixes++
						if err := os.WriteFile(filepath.Join(service.RepoRoot, "app.go"), []byte(fmt.Sprintf("package app\n\nconst ceiling_fix = %d\n", fixes)), 0o600); err != nil {
							t.Errorf("write %s fix %d: %v", kind, fixes, err)
						}
					}
				}

				for round := 1; round <= limit; round++ {
					service = New(service.RepoRoot, reviewLimitConfig(limit), map[string]runner.Adapter{"codex": fake})
					result, err := reviewFixtureCall(context.Background(), service, kind, commandID)
					if round < limit {
						wantStatus := "fixed"
						wantPhase := task.PhaseImplementationReady
						if kind == "plan" {
							wantStatus = "revised"
							wantPhase = task.PhasePlanned
						}
						if err != nil || result.Status != wantStatus || result.State.Phase != wantPhase || reviewFixtureRound(result.State, kind) != round {
							t.Fatalf("round %d %s review: result=%#v err=%v exit=%d", round, kind, result, err, ExitCode(err))
						}
						continue
					}
					wantPhase := task.PhaseApproved
					if kind == "plan" {
						wantPhase = task.PhasePlanApproved
					}
					if err != nil || result.Status != "approved" || result.State.Phase != wantPhase || reviewFixtureRound(result.State, kind) != limit {
						t.Fatalf("ceiling approval %s: result=%#v err=%v exit=%d", kind, result, err, ExitCode(err))
					}
				}

				fake.mu.Lock()
				requests := append([]runner.Request(nil), fake.requests...)
				fake.mu.Unlock()
				reviewerCalls, workerCalls := 0, 0
				for _, request := range requests {
					if request.Role == reviewerRole {
						reviewerCalls++
					}
					if request.Role == runner.RolePlanner || request.Role == runner.RoleImplementer {
						workerCalls++
					}
				}
				wantWorkerCalls := fixRounds
				if kind == "plan" {
					wantWorkerCalls++
				}
				if reviewerCalls != limit || workerCalls != wantWorkerCalls {
					t.Fatalf("%s limit %d calls: reviewer=%d/%d worker=%d/%d requests=%#v", kind, limit, reviewerCalls, limit, workerCalls, wantWorkerCalls, requests)
				}
			})
		}
	}
}

func TestReviewFailuresAndInvalidEnvelopesDoNotConsumeRounds(t *testing.T) {
	for _, kind := range []string{"plan", "code", "integration"} {
		for _, failure := range []string{"invalid_envelope", "provider_failure"} {
			t.Run(kind+"-"+failure, func(t *testing.T) {
				reviewerRole := runner.RoleCodeReviewer
				if kind == "plan" {
					reviewerRole = runner.RolePlanReviewer
				}
				fake := &scriptedAdapter{
					sessions: map[runner.Role]string{},
					responses: map[runner.Role][]runner.Envelope{reviewerRole: {
						{Role: string(reviewerRole), Verdict: "approved", Findings: []task.Finding{}},
					}},
				}
				if failure == "invalid_envelope" {
					fake.responses[reviewerRole] = append([]runner.Envelope{{Role: string(reviewerRole), Verdict: "approved", Findings: nil}}, fake.responses[reviewerRole]...)
				}
				if kind == "plan" {
					fake.responses[runner.RolePlanner] = []runner.Envelope{{Role: string(runner.RolePlanner), Status: "ready", Plan: "failure matrix plan"}}
				}
				if failure == "provider_failure" {
					fake.failures = map[runner.Role]int{reviewerRole: 1}
				}

				service, commandID := reviewFixture(t, kind, 2, fake)
				result, err := reviewFixtureCall(context.Background(), service, kind, commandID)
				if err == nil || ExitCode(err) != ExitAction || result.State.Phase != task.PhaseFailed || reviewFixtureRound(result.State, kind) != 0 {
					t.Fatalf("initial %s %s failure: result=%#v err=%v exit=%d", kind, failure, result, err, ExitCode(err))
				}
				if result.State.ReviewProgress == nil || result.State.ReviewProgress.Status != "failed" || result.State.Usage[string(reviewerRole)].Requests != 1 || result.State.Retry == nil {
					t.Fatalf("durable %s %s failure state=%#v", kind, failure, result.State)
				}

				service = New(service.RepoRoot, reviewLimitConfig(2), map[string]runner.Adapter{"codex": fake})
				taskID := reviewFixtureTaskID(kind, result, commandID)
				retried, retryErr := service.Retry(context.Background(), taskID)
				retried, retryErr = approveIfRequired(context.Background(), service, taskID, retried, retryErr)
				wantPhase := task.PhaseApproved
				if kind == "plan" {
					wantPhase = task.PhasePlanApproved
				}
				if retryErr != nil || retried.Status != "approved" || retried.State.Phase != wantPhase || reviewFixtureRound(retried.State, kind) != 1 {
					t.Fatalf("retry %s %s: result=%#v err=%v exit=%d", kind, failure, retried, retryErr, ExitCode(retryErr))
				}
				if retried.State.Usage[string(reviewerRole)].Requests != 2 {
					t.Fatalf("retry usage %s %s=%#v", kind, failure, retried.State.Usage)
				}
			})
		}
	}
}

func TestReviewLimitsAndExplicitBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		limit      int
		verdict    string
		wantStatus string
		wantPhase  string
		wantErr    int
	}{
		{name: "rejection at one exhausts", limit: 1, verdict: "changes_requested", wantStatus: "exhausted", wantPhase: task.PhaseFailed, wantErr: ExitExhausted},
		{name: "approval at one succeeds", limit: 1, verdict: "approved", wantStatus: "approved", wantPhase: task.PhaseApproved, wantErr: ExitOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := workflowRepo(t)
			fake := &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
				runner.RoleCodeReviewer: {{Role: string(runner.RoleCodeReviewer), Verdict: test.verdict, Findings: func() []task.Finding {
					if test.verdict == "approved" {
						return []task.Finding{}
					}
					return []task.Finding{{Path: "app.go", Message: "fix"}}
				}()}},
			}}
			service := New(root, reviewLimitConfig(test.limit), map[string]runner.Adapter{"codex": fake})
			state := readyCodeState(t, service, "limit-task", test.limit, false)
			result, err := service.ReviewCode(context.Background(), state.ID)
			result, err = approveIfRequired(context.Background(), service, state.ID, result, err)
			if ExitCode(err) != test.wantErr || result.Status != test.wantStatus || result.State.Phase != test.wantPhase {
				t.Fatalf("result=%#v err=%v exit=%d", result, err, ExitCode(err))
			}
			if result.State.CodeRound != 1 {
				t.Fatalf("accepted rounds=%d", result.State.CodeRound)
			}
			fake.mu.Lock()
			calls := len(fake.requests)
			fake.mu.Unlock()
			if test.wantErr == ExitExhausted {
				if _, retryErr := service.ReviewCode(context.Background(), state.ID); ExitCode(retryErr) != ExitExhausted {
					t.Fatalf("repeated review err=%v", retryErr)
				}
				if _, retryErr := service.Retry(context.Background(), state.ID); ExitCode(retryErr) != ExitExhausted {
					t.Fatalf("repeated retry err=%v", retryErr)
				}
				fake.mu.Lock()
				defer fake.mu.Unlock()
				if len(fake.requests) != calls {
					t.Fatalf("exhausted task made provider call: %d -> %d", calls, len(fake.requests))
				}
			}
		})
	}
}

func TestPlanRevisionNoProgressAfterAnswer(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
		runner.RolePlanner: {
			{Role: string(runner.RolePlanner), Status: "ready", Plan: "same plan"},
			{Role: string(runner.RolePlanner), Status: "needs_input", Question: "Need one detail"},
			{Role: string(runner.RolePlanner), Status: "ready", Plan: "same plan"},
		},
		runner.RolePlanReviewer: {{Role: string(runner.RolePlanReviewer), Verdict: "changes_requested", Findings: []task.Finding{{Message: "revise"}}}},
	}}
	service := New(root, reviewLimitConfig(5), map[string]runner.Adapter{"codex": fake})
	if _, err := service.StartPlan(context.Background(), "same plan", "answer-no-progress"); err != nil {
		t.Fatal(err)
	}
	if result, err := service.ReviewPlan(context.Background(), "answer-no-progress"); ExitCode(err) != ExitNeedsInput || result.State.ReviewProgress == nil || result.State.ReviewProgress.Status != "needs_input" {
		t.Fatalf("revision question result=%#v err=%v", result, err)
	}
	result, err := service.AnswerPlan(context.Background(), "answer-no-progress", "use the existing detail")
	if ExitCode(err) != ExitAction || result.Status != "no_progress" || result.State.Phase != task.PhasePlanned || result.State.InFlight != nil || result.State.Retry != nil {
		t.Fatalf("answer no-progress result=%#v err=%v exit=%d", result, err, ExitCode(err))
	}
	if result.State.ReviewProgress == nil || result.State.ReviewProgress.Status != "no_progress" {
		t.Fatalf("progress=%#v", result.State.ReviewProgress)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 4 {
		t.Fatalf("requests=%#v", fake.requests)
	}
}

func TestCodeFixNoProgressAfterAnswerAndRetry(t *testing.T) {
	tests := []struct {
		name     string
		question bool
		failFix  bool
	}{
		{name: "answer", question: true},
		{name: "retry", failFix: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := workflowRepo(t)
			implementerResponses := []runner.Envelope{{Role: string(runner.RoleImplementer), Status: "ready"}}
			if test.question {
				implementerResponses[0] = runner.Envelope{Role: string(runner.RoleImplementer), Status: "needs_input", Question: "which compatibility mode?"}
				implementerResponses = append(implementerResponses, runner.Envelope{Role: string(runner.RoleImplementer), Status: "ready"})
			}
			fake := &scriptedAdapter{
				sessions: map[runner.Role]string{}, failures: map[runner.Role]int{},
				responses: map[runner.Role][]runner.Envelope{
					runner.RoleCodeReviewer: {{Role: string(runner.RoleCodeReviewer), Verdict: "changes_requested", Findings: []task.Finding{{Path: "app.go", Message: "fix"}}}},
					runner.RoleImplementer:  implementerResponses,
				},
			}
			if test.failFix {
				fake.failures[runner.RoleImplementer] = 1
			}
			service := New(root, reviewLimitConfig(5), map[string]runner.Adapter{"codex": fake})
			state := readyCodeState(t, service, "fix-no-progress-"+test.name, 5, false)
			result, err := service.ReviewCode(context.Background(), state.ID)
			if test.question {
				if ExitCode(err) != ExitNeedsInput || result.Status != "needs_input" {
					t.Fatalf("question result=%#v err=%v", result, err)
				}
				result, err = service.AnswerImplement(context.Background(), state.ID, "preserve it")
			} else {
				if ExitCode(err) != ExitAction || result.State.Retry == nil {
					t.Fatalf("fix failure result=%#v err=%v", result, err)
				}
				result, err = service.Retry(context.Background(), state.ID)
			}
			if ExitCode(err) != ExitAction || result.Status != "no_progress" || result.State.Phase != task.PhaseImplementationReady || result.State.CodeRound != 1 || result.State.InFlight != nil || result.State.Retry != nil {
				t.Fatalf("no-progress result=%#v err=%v exit=%d", result, err, ExitCode(err))
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.requests) != 3 || fake.requests[2].Role != runner.RoleImplementer || !fake.requests[2].Resume {
				t.Fatalf("requests=%#v", fake.requests)
			}
		})
	}
}

func TestUnlimitedCodeReviewAllowsMoreThanFiveChangingRounds(t *testing.T) {
	root := workflowRepo(t)
	reviewerResponses := make([]runner.Envelope, 0, 7)
	for round := 1; round <= 6; round++ {
		reviewerResponses = append(reviewerResponses, runner.Envelope{Role: string(runner.RoleCodeReviewer), Verdict: "changes_requested", Findings: []task.Finding{{Path: "app.go", Message: fmt.Sprintf("fix %d", round)}}})
	}
	reviewerResponses = append(reviewerResponses, runner.Envelope{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}})
	implementerResponses := make([]runner.Envelope, 6)
	for i := range implementerResponses {
		implementerResponses[i] = runner.Envelope{Role: string(runner.RoleImplementer), Status: "ready"}
	}
	fake := &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
		runner.RoleCodeReviewer: reviewerResponses,
		runner.RoleImplementer:  implementerResponses,
	}}
	fixes := 0
	fake.hook = func(req runner.Request) {
		if req.Role == runner.RoleImplementer {
			fixes++
			if err := os.WriteFile(filepath.Join(root, "app.go"), []byte(fmt.Sprintf("package app\n\nconst fix = %d\n", fixes)), 0o600); err != nil {
				t.Errorf("mutate round %d: %v", fixes, err)
			}
		}
	}
	service := New(root, reviewLimitConfig(0), map[string]runner.Adapter{"codex": fake})
	state := readyCodeState(t, service, "unlimited", 0, false)
	for round := 1; round <= 6; round++ {
		result, err := service.ReviewCode(context.Background(), state.ID)
		if err != nil || result.Status != "fixed" || result.State.CodeRound != round {
			t.Fatalf("round %d result=%#v err=%v", round, result, err)
		}
	}
	result, err := service.ReviewCode(context.Background(), state.ID)
	result, err = approveIfRequired(context.Background(), service, state.ID, result, err)
	if err != nil || result.Status != "approved" || result.State.Phase != task.PhaseApproved || result.State.CodeRound != 7 {
		t.Fatalf("approval result=%#v err=%v", result, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 13 {
		t.Fatalf("requests=%d want 13: %#v", len(fake.requests), fake.requests)
	}
}

func TestChildReviewPolicyUsesParentSnapshotAfterConfigChange(t *testing.T) {
	root := workflowRepo(t)
	limit := 2
	parentProfiles, parentRuntimes := reviewProfiles()
	units, err := task.NormalizeWorkUnits([]task.WorkUnit{{ID: "T1", Objective: "change app", Scope: "app.go", ExecutionPacket: "change app", AcceptanceCriteria: []string{"works"}, ValidationCommands: []string{"go test ./..."}}}, "plan")
	if err != nil {
		t.Fatal(err)
	}
	parent := task.State{ID: "policy-parent", RepoRoot: root, Phase: task.PhasePlanApproved, Task: "change app", Plan: "plan", PlanHash: hash("plan"), ApprovedPlanHash: hash("plan"), WorkGraph: true, WorkUnits: units, ProfilesSnapshot: parentProfiles, RuntimeSnapshot: parentRuntimes, MaxRounds: limit, ReviewPolicy: &task.ReviewPolicy{MaxRounds: limit}}
	markPlanHumanApproved(&parent)
	service := New(root, reviewLimitConfig(1), map[string]runner.Adapter{"codex": &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
		runner.RoleCodeReviewer: {{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}}},
	}}})
	if err := service.Store.Create(parent); err != nil {
		t.Fatal(err)
	}
	child, err := service.StartWork(parent.ID, "T1")
	if err != nil || child.State.MaxRounds != limit || child.State.ReviewPolicy == nil || child.State.ReviewPolicy.MaxRounds != limit {
		t.Fatalf("child policy=%#v err=%v", child.State.ReviewPolicy, err)
	}
	if _, err := service.Store.Update(child.State.ID, func(st *task.State) error { st.Phase = task.PhaseApproved; return nil }); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReviewIntegration(context.Background(), parent.ID)
	result, err = approveIfRequired(context.Background(), service, parent.ID, result, err)
	if err != nil || result.State.MaxRounds != limit || result.State.ReviewPolicy == nil || result.State.ReviewPolicy.MaxRounds != limit || result.State.Phase != task.PhaseApproved {
		t.Fatalf("integration policy=%#v err=%v", result.State.ReviewPolicy, err)
	}
}

func TestControlForDurableOutcomes(t *testing.T) {
	zero := 0
	planRetry := &task.RetryState{Role: string(runner.RolePlanner), Loop: "plan_review", KnownSession: true, SessionID: "planner-session"}
	cases := []struct {
		name      string
		state     task.State
		status    string
		action    string
		kind      string
		round     int
		max       int
		canReview bool
	}{
		{name: "planned", state: task.State{Phase: task.PhasePlanned, ReviewPolicy: &task.ReviewPolicy{MaxRounds: zero}}, status: "planned", action: "plan_review", kind: "plan", max: 0, canReview: true},
		{name: "revised", state: task.State{Phase: task.PhasePlanned, PlanRound: 1, ReviewProgress: &task.ReviewProgress{Kind: "plan", Status: "revised"}, MaxRounds: 2}, status: "revised", action: "plan_review", kind: "plan", round: 1, max: 2, canReview: true},
		{name: "fixed", state: task.State{Phase: task.PhaseImplementationReady, CodeRound: 1, ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "fixed"}, MaxRounds: 2}, status: "fixed", action: "code_review", kind: "code", round: 1, max: 2, canReview: true},
		{name: "integration fixed", state: task.State{Phase: task.PhaseImplementationReady, CodeRound: 1, ReviewProgress: &task.ReviewProgress{Kind: "integration", Status: "fixed"}, MaxRounds: 2}, status: "fixed", action: "work_integrate", kind: "integration", round: 1, max: 2, canReview: true},
		{name: "no progress", state: task.State{Phase: task.PhaseImplementationReady, ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "no_progress"}, MaxRounds: 2}, status: "no_progress", action: "inspect", kind: "code", max: 2, canReview: true},
		{name: "approved", state: task.State{Phase: task.PhaseApproved, ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "approved"}, MaxRounds: 2}, status: "approved", action: "advance", kind: "code", max: 2},
		{name: "exhausted", state: task.State{Phase: task.PhaseFailed, ReviewProgress: &task.ReviewProgress{Kind: "plan", Status: "exhausted"}, MaxRounds: 2, PlanRound: 2}, status: "exhausted", action: "stop", kind: "plan", round: 2, max: 2},
		{name: "question", state: task.State{Phase: task.PhaseNeedsInput, ReviewProgress: &task.ReviewProgress{Kind: "plan", Status: "needs_input"}, PendingQuestion: "which API", PendingQuestionSource: string(runner.RolePlanner), MaxRounds: 2}, status: "needs_input", action: "plan_answer", kind: "plan", max: 2},
		{name: "review needed", state: task.State{Phase: task.PhaseReviewNeeded, ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "review_needed"}, MaxRounds: 2}, status: "review_needed", action: "retry", kind: "code", max: 2},
		{name: "resumable failure", state: task.State{Phase: task.PhaseFailed, ReviewProgress: &task.ReviewProgress{Kind: "plan", Status: "failed"}, Retry: planRetry, MaxRounds: 2}, status: "failed", action: "retry", kind: "plan", max: 2},
		{name: "in flight wins", state: task.State{Phase: task.PhasePlanned, ReviewProgress: &task.ReviewProgress{Kind: "plan", Status: "approved"}, InFlight: &task.InFlight{Role: string(runner.RolePlanReviewer)}, MaxRounds: 2}, status: "in_flight", action: "wait", kind: "plan", max: 2},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			control := ControlFor(test.state)
			if control.Status != test.status || control.NextAction != test.action || control.ReviewKind != test.kind || control.ReviewRound != test.round || control.MaxRounds != test.max || control.CanReview != test.canReview {
				t.Fatalf("control=%#v", control)
			}
			if test.max == 0 {
				encoded, err := json.Marshal(control)
				if err != nil || !strings.Contains(string(encoded), `"max_rounds":0`) {
					t.Fatalf("zero max rounds was omitted: %s err=%v", encoded, err)
				}
			}
		})
	}
}
