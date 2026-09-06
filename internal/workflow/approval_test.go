package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/basant-kumar/rolemux/internal/reviewhost"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

type fakeExternalReviewHost struct {
	publishes int
	feedback  reviewhost.Feedback
}

func (f *fakeExternalReviewHost) Publish(_ context.Context, st task.State, record task.ApprovalRecord) (task.ExternalReview, error) {
	f.publishes++
	review := task.ExternalReview{
		Provider:                      "github",
		URL:                           "https://github.com/example/project/pull/7",
		Number:                        7,
		Repository:                    "example/project",
		Remote:                        "origin",
		BaseBranch:                    "rolemux-review/example-base",
		HeadBranch:                    "rolemux-review/example-candidate",
		BaseCommit:                    "base",
		HeadCommit:                    fmt.Sprintf("candidate-%d", f.publishes),
		PublishedCandidateFingerprint: record.SubjectFingerprint,
	}
	if record.ExternalReview != nil {
		review = *record.ExternalReview
		review.HeadCommit = fmt.Sprintf("candidate-%d", f.publishes)
		review.PublishedCandidateFingerprint = record.SubjectFingerprint
	}
	return review, nil
}

func (f *fakeExternalReviewHost) FetchFeedback(_ context.Context, review task.ExternalReview) (reviewhost.Feedback, error) {
	result := f.feedback
	if result.Review.Provider == "" {
		result.Review = review
	}
	return result, nil
}

func TestHumanPlanAndCodeBoundaries(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner:      {{Role: string(runner.RolePlanner), Status: "ready", Plan: "change app"}},
			runner.RolePlanReviewer: {{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}}},
			runner.RoleImplementer:  {{Role: string(runner.RoleImplementer), Status: "ready"}},
			runner.RoleCodeReviewer: {{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}}},
		},
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})

	started, err := service.StartPlan(context.Background(), "change app", "human-boundaries")
	if err != nil || started.State.Phase != task.PhasePlanned {
		t.Fatalf("start=%#v err=%v", started, err)
	}
	planGate, err := service.ReviewPlan(context.Background(), started.State.ID)
	if ExitCode(err) != ExitNeedsInput || planGate.Status != "approval_required" || planGate.State.Phase != task.PhaseAwaitingApproval || planGate.State.Approval == nil {
		t.Fatalf("plan gate=%#v err=%v", planGate, err)
	}
	control, err := service.Approval(started.State.ID)
	if err != nil || control.Status != "approval_required" || control.NextAction != "approval_respond" || control.ApprovalKind != "plan" || !control.RequiresExplicitHumanConfirmation || len(control.Choices) != 3 || control.Choices[0].Label != "Approve" || control.Choices[1].Value != "request_changes" || control.Choices[2].Label != "Discuss" || control.ArtifactPath == "" {
		t.Fatalf("plan control=%#v err=%v", control, err)
	}
	approvedPlan, err := service.RespondApproval(context.Background(), started.State.ID, control.ApprovalID, "approve", "")
	if err != nil || approvedPlan.State.Phase != task.PhasePlanApproved || approvedPlan.State.Approval == nil || approvedPlan.State.Approval.Status != task.ApprovalDecisionApprove {
		t.Fatalf("approved plan=%#v err=%v", approvedPlan, err)
	}

	implemented, err := service.Implement(context.Background(), started.State.ID, "app.go")
	if err != nil || implemented.State.Phase != task.PhaseImplementationReady {
		t.Fatalf("implemented=%#v err=%v", implemented, err)
	}
	codeGate, err := service.ReviewCode(context.Background(), started.State.ID)
	if ExitCode(err) != ExitNeedsInput || codeGate.Status != "approval_required" || codeGate.State.Phase != task.PhaseAwaitingApproval || codeGate.State.Approval == nil || codeGate.State.Approval.Kind != task.ApprovalKindCode {
		t.Fatalf("code gate=%#v err=%v", codeGate, err)
	}
	codeControl, err := service.Approval(started.State.ID)
	if err != nil || codeControl.ApprovalID != codeGate.State.Approval.GateID || codeControl.ApprovalKind != "code" || codeControl.Scope != "app.go" {
		t.Fatalf("code control=%#v err=%v", codeControl, err)
	}
	completed, err := service.RespondApproval(context.Background(), started.State.ID, codeControl.ApprovalID, task.ApprovalDecisionApprove, "")
	if err != nil || completed.State.Phase != task.PhaseApproved || completed.State.Approval == nil || completed.State.Approval.Status != task.ApprovalDecisionApprove {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
}

func TestGitHubReviewFeedbackResumesImplementerAndReusesDraft(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{runner.RoleImplementer: "session-implementer"},
		responses: map[runner.Role][]runner.Envelope{
			runner.RoleImplementer: {
				{Role: string(runner.RoleImplementer), Status: "ready"},
			},
			runner.RoleCodeReviewer: {
				{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}},
				{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}},
			},
		},
	}
	fake.hook = func(req runner.Request) {
		if req.Role == runner.RoleImplementer {
			if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst githubFeedbackFixed = true\n"), 0o600); err != nil {
				t.Errorf("write feedback candidate: %v", err)
			}
		}
	}
	host := &fakeExternalReviewHost{}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	service.ReviewHost = host
	state := readyCodeState(t, service, "github-human-review", MaxRounds, false)
	if _, err := service.Store.Update(state.ID, func(current *task.State) error {
		current.ImplementerSessionID = "session-implementer"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	gate, err := service.ReviewCode(context.Background(), state.ID)
	if ExitCode(err) != ExitNeedsInput || gate.State.Approval == nil {
		t.Fatalf("review gate=%#v err=%v", gate, err)
	}
	published, review, err := service.PublishApprovalReview(context.Background(), state.ID)
	if err != nil || review == nil || host.publishes != 1 {
		t.Fatalf("publish=%#v review=%#v err=%v", published, review, err)
	}
	control := ControlFor(published.State)
	if control.ExternalReview == nil || control.ExternalReview.URL != review.URL || control.ReviewOutdated {
		t.Fatalf("published control=%#v", control)
	}

	host.feedback = reviewhost.Feedback{
		Text: "[inline comment by @reviewer on app.go:3]\nUse the compatible value.",
		Review: func() task.ExternalReview {
			updated := *review
			updated.LastReviewCommentID = 42
			return updated
		}(),
	}
	fixed, err := service.SyncApprovalReview(context.Background(), state.ID)
	if err != nil || fixed.Status != "fixed" || fixed.State.Phase != task.PhaseImplementationReady {
		t.Fatalf("sync=%#v err=%v", fixed, err)
	}
	if len(fixed.State.ApprovalHistory) != 1 || fixed.State.ApprovalHistory[0].HumanFeedback != host.feedback.Text || fixed.State.ApprovalHistory[0].ExternalReview == nil || fixed.State.ApprovalHistory[0].ExternalReview.LastReviewCommentID != 42 {
		t.Fatalf("feedback history=%#v", fixed.State.ApprovalHistory)
	}
	fake.mu.Lock()
	var implementation runner.Request
	for _, request := range fake.requests {
		if request.Role == runner.RoleImplementer {
			implementation = request
		}
	}
	fake.mu.Unlock()
	if !implementation.Resume || implementation.SessionID != "session-implementer" || !strings.Contains(implementation.Prompt, "Use the compatible value") {
		t.Fatalf("implementation request=%#v", implementation)
	}

	secondGate, err := service.ReviewCode(context.Background(), state.ID)
	if ExitCode(err) != ExitNeedsInput || secondGate.State.Approval == nil || secondGate.State.Approval.ExternalReview == nil {
		t.Fatalf("second gate=%#v err=%v", secondGate, err)
	}
	secondControl := ControlFor(secondGate.State)
	if !secondControl.ReviewOutdated || secondControl.ExternalReview.URL != review.URL {
		t.Fatalf("stale draft control=%#v", secondControl)
	}
	republished, secondReview, err := service.PublishApprovalReview(context.Background(), state.ID)
	if err != nil || secondReview == nil || host.publishes != 2 || secondReview.URL != review.URL || secondReview.PublishedCandidateFingerprint != republished.State.Approval.SubjectFingerprint || ControlFor(republished.State).ReviewOutdated {
		t.Fatalf("republish=%#v review=%#v err=%v", republished, secondReview, err)
	}
}

func TestApprovalFeedbackIsDurableAndIdempotent(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{
		sessions: map[runner.Role]string{},
		responses: map[runner.Role][]runner.Envelope{
			runner.RolePlanner: {
				{Role: string(runner.RolePlanner), Status: "ready", Plan: "plan v1"},
				{Role: string(runner.RolePlanner), Status: "ready", Plan: "plan v2"},
			},
			runner.RolePlanReviewer: {
				{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}},
				{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}},
			},
			runner.RoleImplementer: {
				{Role: string(runner.RoleImplementer), Status: "ready"},
				{Role: string(runner.RoleImplementer), Status: "ready"},
			},
			runner.RoleCodeReviewer: {
				{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}},
				{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}},
			},
		},
	}
	implementerTurns := 0
	fake.hook = func(req runner.Request) {
		if req.Role == runner.RoleImplementer {
			implementerTurns++
			if implementerTurns == 2 {
				if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst feedback = true\n"), 0o600); err != nil {
					t.Errorf("write feedback candidate: %v", err)
				}
			}
		}
	}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	started, err := service.StartPlan(context.Background(), "change app", "feedback-boundaries")
	if err != nil {
		t.Fatal(err)
	}
	firstPlan, err := service.ReviewPlan(context.Background(), started.State.ID)
	if ExitCode(err) != ExitNeedsInput || firstPlan.State.Approval == nil {
		t.Fatalf("first plan=%#v err=%v", firstPlan, err)
	}
	firstPlanGate := firstPlan.State.Approval.GateID
	feedbackResult, err := service.RespondApproval(context.Background(), started.State.ID, firstPlanGate, "request_changes", "Use the revised API")
	if err != nil || feedbackResult.Status != "revised" || feedbackResult.State.Plan != "plan v2" || len(feedbackResult.State.ApprovalHistory) != 1 || feedbackResult.State.ApprovalHistory[0].HumanFeedback != "Use the revised API" {
		t.Fatalf("plan feedback=%#v err=%v", feedbackResult, err)
	}
	replay, err := service.RespondApproval(context.Background(), started.State.ID, firstPlanGate, "request_changes", "Use the revised API")
	if err != nil || replay.Status != "revised" {
		t.Fatalf("plan replay=%#v err=%v", replay, err)
	}
	fake.mu.Lock()
	plannerCalls := 0
	for _, request := range fake.requests {
		if request.Role == runner.RolePlanner {
			plannerCalls++
		}
	}
	fake.mu.Unlock()
	if plannerCalls != 2 {
		t.Fatalf("planner calls after replay=%d", plannerCalls)
	}
	secondPlan, err := service.ReviewPlan(context.Background(), started.State.ID)
	if ExitCode(err) != ExitNeedsInput || secondPlan.State.Approval == nil || secondPlan.State.Approval.GateID == firstPlanGate {
		t.Fatalf("second plan gate=%#v err=%v", secondPlan, err)
	}
	planControl, _ := service.Approval(started.State.ID)
	if _, err := service.RespondApproval(context.Background(), started.State.ID, planControl.ApprovalID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Implement(context.Background(), started.State.ID, "app.go"); err != nil {
		t.Fatal(err)
	}
	codeGate, err := service.ReviewCode(context.Background(), started.State.ID)
	if ExitCode(err) != ExitNeedsInput || codeGate.State.Approval == nil {
		t.Fatalf("code gate=%#v err=%v", codeGate, err)
	}
	codeGateID := codeGate.State.Approval.GateID
	fixed, err := service.RespondApproval(context.Background(), started.State.ID, codeGateID, task.ApprovalDecisionRequestChanges, "Apply the compatibility fix")
	if err != nil || fixed.Status != "fixed" || fixed.State.Phase != task.PhaseImplementationReady {
		t.Fatalf("code feedback=%#v err=%v", fixed, err)
	}
	codeReplay, err := service.RespondApproval(context.Background(), started.State.ID, codeGateID, task.ApprovalDecisionRequestChanges, "Apply the compatibility fix")
	if err != nil || codeReplay.Status != "fixed" {
		t.Fatalf("code replay=%#v err=%v", codeReplay, err)
	}
	secondCode, err := service.ReviewCode(context.Background(), started.State.ID)
	if ExitCode(err) != ExitNeedsInput || secondCode.State.Approval == nil || secondCode.State.Approval.GateID == codeGateID {
		t.Fatalf("second code gate=%#v err=%v", secondCode, err)
	}
}

func TestDiscussDoesNotMutateGateOrCallProvider(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
		runner.RolePlanner:      {{Role: string(runner.RolePlanner), Status: "ready", Plan: "reviewed plan"}},
		runner.RolePlanReviewer: {{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}}},
	}}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	if _, err := service.StartPlan(context.Background(), "change app", "discuss-gate"); err != nil {
		t.Fatal(err)
	}
	gate, err := service.ReviewPlan(context.Background(), "discuss-gate")
	if ExitCode(err) != ExitNeedsInput || gate.State.Approval == nil {
		t.Fatalf("gate=%#v err=%v", gate, err)
	}
	before, err := service.Store.Load("discuss-gate")
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(before)
	fake.mu.Lock()
	calls := len(fake.requests)
	fake.mu.Unlock()
	result, err := service.RespondApproval(context.Background(), before.ID, before.Approval.GateID, task.ApprovalDecisionDiscuss, "")
	if ExitCode(err) != ExitNeedsInput || result.Status != "approval_required" {
		t.Fatalf("discuss=%#v err=%v", result, err)
	}
	after, err := service.Store.Load(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("discuss mutated state\nbefore=%s\nafter=%s", beforeJSON, afterJSON)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != calls {
		t.Fatalf("discuss called provider: %d -> %d", calls, len(fake.requests))
	}
}

func TestCodeApprovalRejectsCandidateChangedAfterReview(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
		runner.RoleImplementer:  {{Role: string(runner.RoleImplementer), Status: "ready"}},
		runner.RoleCodeReviewer: {{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}}},
	}}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	state := readyCodeState(t, service, "stale-human-code", MaxRounds, false)
	gate, err := service.ReviewCode(context.Background(), state.ID)
	if ExitCode(err) != ExitNeedsInput || gate.State.Approval == nil {
		t.Fatalf("gate=%#v err=%v", gate, err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst changedAfterReview = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := service.RespondApproval(context.Background(), state.ID, gate.State.Approval.GateID, task.ApprovalDecisionApprove, "")
	if ExitCode(err) != ExitReviewNeeded || result.State.Phase != task.PhaseAwaitingApproval || result.State.Approval == nil || result.State.Approval.Status != "" {
		t.Fatalf("stale approval result=%#v err=%v", result, err)
	}
}

func TestCodeApprovalAcceptsReviewedCandidateCommittedUnchanged(t *testing.T) {
	root := workflowRepo(t)
	for _, args := range [][]string{{"commit", "-qm", "baseline"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst reviewed = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
		runner.RoleCodeReviewer: {{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}}},
	}}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	state := readyCodeState(t, service, "committed-human-code", MaxRounds, false)
	gate, err := service.ReviewCode(context.Background(), state.ID)
	if ExitCode(err) != ExitNeedsInput || gate.State.Approval == nil {
		t.Fatalf("gate=%#v err=%v", gate, err)
	}
	for _, args := range [][]string{{"add", "app.go"}, {"commit", "-qm", "reviewed candidate"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			t.Fatalf("git %v: %v (%s)", args, runErr, output)
		}
	}
	result, err := service.RespondApproval(context.Background(), state.ID, gate.State.Approval.GateID, task.ApprovalDecisionApprove, "")
	if err != nil || result.Status != "approved" || result.State.Phase != task.PhaseApproved {
		t.Fatalf("approval result=%#v err=%v", result, err)
	}
}

func TestConcurrentApprovalResponsesAcceptOnlyOneDecision(t *testing.T) {
	root := workflowRepo(t)
	fake := &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
		runner.RolePlanner: {
			{Role: string(runner.RolePlanner), Status: "ready", Plan: "reviewed plan"},
			{Role: string(runner.RolePlanner), Status: "ready", Plan: "revised plan"},
		},
		runner.RolePlanReviewer: {{Role: string(runner.RolePlanReviewer), Verdict: "approved", Findings: []task.Finding{}}},
	}}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": fake})
	if _, err := service.StartPlan(context.Background(), "change app", "concurrent-gate"); err != nil {
		t.Fatal(err)
	}
	gate, err := service.ReviewPlan(context.Background(), "concurrent-gate")
	if ExitCode(err) != ExitNeedsInput || gate.State.Approval == nil {
		t.Fatalf("gate=%#v err=%v", gate, err)
	}

	type response struct {
		result Result
		err    error
	}
	results := make(chan response, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, decision := range []task.ApprovalDecision{task.ApprovalDecisionApprove, task.ApprovalDecisionRequestChanges} {
		decision := decision
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			feedback := ""
			if decision == task.ApprovalDecisionRequestChanges {
				feedback = "revise it"
			}
			result, callErr := service.RespondApproval(context.Background(), gate.State.ID, gate.State.Approval.GateID, decision, feedback)
			results <- response{result: result, err: callErr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for response := range results {
		if response.err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful decisions=%d want 1", successes)
	}
	persisted, err := service.Store.Load(gate.State.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != task.PhasePlanApproved && persisted.Phase != task.PhasePlanned {
		t.Fatalf("unexpected winning state=%#v", persisted)
	}
}

func TestTrivialIntegrationMaterializesFinalGateFromChildEvidence(t *testing.T) {
	root := workflowRepo(t)
	adapter := &scriptedAdapter{sessions: map[runner.Role]string{}, responses: map[runner.Role][]runner.Envelope{
		runner.RoleImplementer: {
			{Role: string(runner.RoleImplementer), Status: "ready"},
			{Role: string(runner.RoleImplementer), Status: "ready"},
		},
		runner.RoleCodeReviewer: {{Role: string(runner.RoleCodeReviewer), Verdict: "approved", Findings: []task.Finding{}}},
	}}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": adapter})
	profiles, runtimes := reviewProfiles()
	units, err := task.NormalizeWorkUnits([]task.WorkUnit{{
		ID: "T1", Objective: "change app", Scope: "app.go", ExecutionPacket: "change app",
		AcceptanceCriteria: []string{"works"}, ValidationCommands: []string{"go test ./internal/workflow"},
	}}, "approved plan")
	if err != nil {
		t.Fatal(err)
	}
	parent := task.State{
		ID: "graph-parent", RepoRoot: root, Phase: task.PhasePlanApproved, Task: "change app", Plan: "approved plan",
		PlanHash: hash("approved plan"), ApprovedPlanHash: hash("approved plan"), WorkGraph: true, WorkUnits: units,
		Complexity: task.ComplexityTrivial, ProfilesSnapshot: profiles, RuntimeSnapshot: runtimes,
		ReviewPolicy: &task.ReviewPolicy{MaxRounds: 3}, MaxRounds: 3, PlanRound: 1,
		ReviewProgress: &task.ReviewProgress{Kind: "plan", Status: "approved"}, PlanReviewerSessionID: "plan-session",
	}
	parent.Approval = &task.ApprovalRecord{
		GateID: "plan-gate", Kind: task.ApprovalKindPlan, Status: task.ApprovalDecisionApprove,
		SubjectFingerprint: planReviewFingerprint(parent), ReviewerEvidence: reviewerEvidence(parent, parent.ID, string(runner.RolePlanReviewer), "approved", planReviewFingerprint(parent), "plan-session", 1),
	}
	if err := service.Store.Create(parent); err != nil {
		t.Fatal(err)
	}
	baseline, err := service.Observe("app.go")
	if err != nil {
		t.Fatal(err)
	}
	childID := task.WorkTaskID(parent.ID, "T1")
	baseline, err = service.Capture(baseline, "unit-baseline", childID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n\nconst child = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Observe("app.go")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = service.Capture(candidate, "unit-candidate", childID)
	if err != nil {
		t.Fatal(err)
	}
	child := task.State{
		ID: childID, RepoRoot: root, Phase: task.PhaseApproved, ParentTaskID: parent.ID, WorkUnitID: "T1",
		Scope: "app.go", ScopedBaseline: baseline, CandidateManifest: candidate, CandidateManifestHash: task.HashManifest(candidate),
		ApprovedManifestHash: task.HashManifest(candidate), CodeRound: 1, CodeReviewerSessionID: "code-session",
		ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "approved"}, ProfilesSnapshot: profiles, RuntimeSnapshot: runtimes,
		ReviewPolicy: &task.ReviewPolicy{MaxRounds: 3}, MaxRounds: 3,
	}
	if err := service.Store.Create(child); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReviewIntegration(context.Background(), parent.ID)
	if ExitCode(err) != ExitNeedsInput || result.Status != "approval_required" || result.State.Phase != task.PhaseAwaitingApproval || result.State.Approval == nil {
		t.Fatalf("integration gate=%#v err=%v", result, err)
	}
	if result.State.Approval.ReviewerEvidence == nil || result.State.Approval.ReviewerEvidence.SourceTask != task.IntegrationTaskID(parent.ID) || result.State.CodeRound != 1 {
		t.Fatalf("integration evidence=%#v", result.State)
	}
	if control, controlErr := service.Approval(parent.ID); controlErr != nil || control.ApprovalTaskID != task.IntegrationTaskID(parent.ID) || control.ApprovalKind != "code" {
		t.Fatalf("parent approval control=%#v err=%v", control, controlErr)
	}
	adapter.mu.Lock()
	if len(adapter.requests) != 0 {
		t.Fatalf("trivial integration called provider: %#v", adapter.requests)
	}
	adapter.mu.Unlock()

	fixNumber := 0
	adapter.hook = func(req runner.Request) {
		if req.Role != runner.RoleImplementer {
			return
		}
		fixNumber++
		if err := os.WriteFile(filepath.Join(root, "app.go"), []byte(fmt.Sprintf("package app\n\nconst integrationFix = %d\n", fixNumber)), 0o600); err != nil {
			t.Errorf("integration fix: %v", err)
		}
	}
	firstGate := result.State.Approval.GateID
	fixed, err := service.RespondApproval(context.Background(), parent.ID, firstGate, task.ApprovalDecisionRequestChanges, "adjust integration")
	if err != nil || fixed.Status != "fixed" || fixed.State.ImplementerSessionID == "" {
		t.Fatalf("first integration feedback=%#v err=%v", fixed, err)
	}
	firstSession := fixed.State.ImplementerSessionID
	secondGate, err := service.ReviewIntegration(context.Background(), parent.ID)
	if ExitCode(err) != ExitNeedsInput || secondGate.State.Approval == nil {
		t.Fatalf("second integration gate=%#v err=%v", secondGate, err)
	}
	fixedAgain, err := service.RespondApproval(context.Background(), parent.ID, secondGate.State.Approval.GateID, task.ApprovalDecisionRequestChanges, "adjust integration again")
	if err != nil || fixedAgain.Status != "fixed" || fixedAgain.State.ImplementerSessionID != firstSession {
		t.Fatalf("second integration feedback=%#v err=%v", fixedAgain, err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.requests) != 3 || adapter.requests[0].Role != runner.RoleImplementer || adapter.requests[0].Resume || adapter.requests[1].Role != runner.RoleCodeReviewer || adapter.requests[2].Role != runner.RoleImplementer || !adapter.requests[2].Resume || adapter.requests[2].SessionID != firstSession {
		t.Fatalf("integration feedback requests=%#v", adapter.requests)
	}
}
