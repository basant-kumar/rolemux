package workflow

import (
	"reflect"
	"testing"

	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

func TestControlForMaterializedWorkUnitChild(t *testing.T) {
	cases := []struct {
		name      string
		state     task.State
		status    string
		action    string
		kind      string
		round     int
		max       int
		canReview bool
		question  string
		source    string
	}{
		{
			name:   "child is ready",
			state:  task.State{Phase: task.PhasePlanApproved, ParentTaskID: "parent", WorkUnitID: "T1"},
			status: "ready", action: "implement", max: MaxRounds,
		},
		{
			name:   "work graph parent remains approved",
			state:  task.State{Phase: task.PhasePlanApproved, WorkGraph: true},
			status: "approved", action: "advance", kind: "plan", max: MaxRounds,
		},
		{
			name:   "parent identifier alone is not a child",
			state:  task.State{Phase: task.PhasePlanApproved, ParentTaskID: "parent"},
			status: "approved", action: "advance", kind: "plan", max: MaxRounds,
		},
		{
			name:   "work unit identifier alone is not a child",
			state:  task.State{Phase: task.PhasePlanApproved, WorkUnitID: "T1"},
			status: "approved", action: "advance", kind: "plan", max: MaxRounds,
		},
		{
			name:   "integration stays higher priority",
			state:  task.State{Phase: task.PhasePlanApproved, ParentTaskID: "parent", WorkUnitID: "T1", IntegrationReview: true},
			status: "approved", action: "advance", kind: "integration", max: MaxRounds,
		},
		{
			name:   "durable approved outcome wins",
			state:  task.State{Phase: task.PhasePlanApproved, ParentTaskID: "parent", WorkUnitID: "T1", ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "approved"}},
			status: "approved", action: "advance", kind: "code", max: MaxRounds,
		},
		{
			name:   "durable fixed outcome wins",
			state:  task.State{Phase: task.PhasePlanApproved, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 1, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 3}, MaxRounds: 3, ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "fixed"}},
			status: "fixed", action: "code_review", kind: "code", round: 1, max: 3, canReview: true,
		},
		{
			name:   "implementation ready child",
			state:  task.State{Phase: task.PhaseImplementationReady, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 2, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 4}, MaxRounds: 4},
			status: "implementation_ready", action: "code_review", kind: "code", round: 2, max: 4, canReview: true,
		},
		{
			name:   "implementing child waits",
			state:  task.State{Phase: task.PhaseImplementing, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 1, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 4}, MaxRounds: 4},
			status: "in_flight", action: "wait", kind: "code", round: 1, max: 4,
		},
		{
			name:   "code reviewing child waits",
			state:  task.State{Phase: task.PhaseCodeReviewing, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 1, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 4}, MaxRounds: 4},
			status: "in_flight", action: "wait", kind: "code", round: 1, max: 4,
		},
		{
			name:   "approved child advances",
			state:  task.State{Phase: task.PhaseApproved, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 2, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 4}, MaxRounds: 4},
			status: "approved", action: "advance", kind: "code", round: 2, max: 4,
		},
		{
			name:   "review-needed child retries",
			state:  task.State{Phase: task.PhaseReviewNeeded, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 2, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 4}, MaxRounds: 4},
			status: "review_needed", action: "retry", kind: "code", round: 2, max: 4,
		},
		{
			name:   "in-flight implementer overrides ready phase",
			state:  task.State{Phase: task.PhasePlanApproved, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 1, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 4}, MaxRounds: 4, ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "approved"}, InFlight: &task.InFlight{Role: string(runner.RoleImplementer), Operation: "implement", Loop: "implement"}},
			status: "in_flight", action: "wait", kind: "code", round: 1, max: 4,
		},
		{
			name:   "implementer needs input",
			state:  task.State{Phase: task.PhaseNeedsInput, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 1, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 4}, MaxRounds: 4, PendingQuestion: "which API", PendingQuestionSource: string(runner.RoleImplementer), ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "approved"}},
			status: "needs_input", action: "implement_answer", kind: "code", round: 1, max: 4, question: "which API", source: string(runner.RoleImplementer),
		},
		{
			name:   "implementer retry remains code work",
			state:  task.State{Phase: task.PhaseFailed, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 2, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 4}, MaxRounds: 4, ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "failed"}, Retry: &task.RetryState{Role: string(runner.RoleImplementer), Operation: "implement", Loop: "implement", KnownSession: true, SessionID: "implementer-session"}},
			status: "failed", action: "retry", kind: "code", round: 2, max: 4,
		},
		{
			name:   "exhausted child stops",
			state:  task.State{Phase: task.PhaseFailed, ParentTaskID: "parent", WorkUnitID: "T1", CodeRound: 4, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 4}, MaxRounds: 4, ReviewProgress: &task.ReviewProgress{Kind: "code", Status: "exhausted"}},
			status: "exhausted", action: "stop", kind: "code", round: 4, max: 4,
		},
		{
			name:   "explicit unlimited child policy",
			state:  task.State{Phase: task.PhasePlanApproved, ParentTaskID: "parent", WorkUnitID: "T1", ReviewPolicy: &task.ReviewPolicy{MaxRounds: 0}},
			status: "ready", action: "implement", max: 0,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if test.state.Phase == task.PhasePlanApproved && !isImplementationReadyPlan(test.state) && test.status == "approved" {
				markPlanHumanApproved(&test.state)
			}
			control := ControlFor(test.state)
			if control.Status != test.status || control.NextAction != test.action || control.ReviewKind != test.kind || control.ReviewRound != test.round || control.MaxRounds != test.max || control.CanReview != test.canReview || control.Question != test.question || control.Source != test.source {
				t.Fatalf("control=%#v", control)
			}
		})
	}
}

func TestStartWorkChildControlIsReady(t *testing.T) {
	root := workflowRepo(t)
	service := New(root, workflowConfig(), nil)
	parentLimit := 3
	units, err := task.NormalizeWorkUnits([]task.WorkUnit{{
		ID:                 "T1",
		Objective:          "change app",
		Scope:              "app.go",
		ExecutionPacket:    "implement the app change",
		AcceptanceCriteria: []string{"works"},
		ValidationCommands: []string{"go test ./..."},
	}}, "approved plan")
	if err != nil {
		t.Fatal(err)
	}
	parent := task.State{
		ID:               "parent",
		RepoRoot:         root,
		Phase:            task.PhasePlanApproved,
		Task:             "change app",
		Plan:             "approved plan",
		PlanHash:         hash("approved plan"),
		ApprovedPlanHash: hash("approved plan"),
		WorkGraph:        true,
		WorkUnits:        units,
		MaxRounds:        parentLimit,
		ReviewPolicy:     &task.ReviewPolicy{MaxRounds: parentLimit},
	}
	markPlanHumanApproved(&parent)
	if err := service.Store.Create(parent); err != nil {
		t.Fatal(err)
	}

	created, err := service.StartWork(parent.ID, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != "ready" || created.State.ParentTaskID != parent.ID || created.State.WorkUnitID != "T1" || created.State.Phase != task.PhasePlanApproved {
		t.Fatalf("created=%#v", created)
	}
	if control := ControlFor(created.State); control.Status != "ready" || control.NextAction != "implement" || control.ReviewKind != "" || control.CanReview {
		t.Fatalf("created control=%#v", control)
	}

	persisted, err := service.Status(created.State.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != task.PhasePlanApproved || persisted.ParentTaskID != parent.ID || persisted.WorkUnitID != "T1" || persisted.ReviewPolicy == nil || persisted.ReviewPolicy.MaxRounds != parentLimit || persisted.MaxRounds != parentLimit {
		t.Fatalf("persisted child=%#v", persisted)
	}
	wantControl := Control{Status: "ready", ReviewRound: 0, MaxRounds: parentLimit, CanReview: false, NextAction: "implement"}
	if control := ControlFor(persisted); !reflect.DeepEqual(control, wantControl) {
		t.Fatalf("persisted control=%#v want=%#v", control, wantControl)
	}

	existing, err := service.StartWork(parent.ID, "T1")
	if err != nil || existing.Status != task.PhasePlanApproved {
		t.Fatalf("existing=%#v err=%v", existing, err)
	}
}
