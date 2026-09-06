package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

type usageStep struct {
	response runner.Response
	err      error
}

type usageAdapter struct {
	mu       sync.Mutex
	steps    []usageStep
	requests []runner.Request
	session  string
}

func (a *usageAdapter) Run(_ context.Context, req runner.Request, callbacks runner.Callbacks) (runner.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, req)
	if req.Resume {
		if req.SessionID == "" || req.SessionID != a.session {
			return runner.Response{}, errors.New("retry did not resume the saved session")
		}
	} else {
		if a.session != "" {
			return runner.Response{}, errors.New("fresh invocation replaced the saved session")
		}
		a.session = "usage-session"
		if callbacks.SessionStarted != nil {
			if err := callbacks.SessionStarted(a.session); err != nil {
				return runner.Response{}, err
			}
		}
	}
	if len(a.steps) == 0 {
		return runner.Response{}, errors.New("usage test adapter exhausted")
	}
	step := a.steps[0]
	a.steps = a.steps[1:]
	if step.response.SessionID == "" {
		step.response.SessionID = a.session
	}
	return step.response, step.err
}

func (*usageAdapter) ListModels(context.Context, runner.ModelListRequest) (runner.ModelPage, error) {
	return workflowTestModels(), nil
}
func (*usageAdapter) Version(context.Context) (string, error) { return "usage-test", nil }
func (*usageAdapter) Auth(context.Context) (runner.AuthStatus, error) {
	return runner.AuthStatus{Authenticated: true}, nil
}

func plannerUsageResponse(output int64, status runner.UsageStatus, cumulative bool) runner.Response {
	return runner.Response{
		Envelope: &runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: "usage plan"},
		Usage:    task.TokenUsage{OutputTokens: output, TotalTokens: output}, UsageStatus: status, UsageCumulative: cumulative,
	}
}

func TestInterruptedUsagePersistsAndSameSessionRetryAddsWithoutErasingUncertainty(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
	}{
		{name: "cancellation", cause: context.Canceled},
		{name: "timeout", cause: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := workflowRepo(t)
			adapter := &usageAdapter{steps: []usageStep{
				{response: runner.Response{
					Envelope: &runner.Envelope{Role: string(runner.RolePlanner), Status: "ready", Plan: "partial plan"},
					Usage: task.TokenUsage{
						Requests: 99, PromptBytes: 999, UnreportedRequests: 7, IncompleteRequests: 8,
						OutputTokens: 10, TotalTokens: 10,
					}, UsageStatus: runner.UsageIncomplete, UsageCumulative: true,
				}, err: test.cause},
				{response: plannerUsageResponse(15, runner.UsageComplete, true)},
			}}
			service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": adapter})
			result, err := service.StartPlan(context.Background(), "usage task", "usage-"+test.name)
			if err == nil || result.State.Phase != task.PhaseFailed {
				t.Fatalf("interrupted start result=%#v err=%v", result, err)
			}
			first, err := service.Status(result.State.ID)
			if err != nil {
				t.Fatal(err)
			}
			adapter.mu.Lock()
			firstPromptBytes := int64(len(adapter.requests[0].Prompt))
			adapter.mu.Unlock()
			usage := first.Usage[string(runner.RolePlanner)]
			if usage.Requests != 1 || usage.PromptBytes != firstPromptBytes || usage.IncompleteRequests != 1 || usage.UnreportedRequests != 0 || usage.OutputTokens != 10 {
				t.Fatalf("persisted interrupted usage=%#v, prompt_bytes=%d", usage, firstPromptBytes)
			}
			if first.ProviderUsageCumulative[string(runner.RolePlanner)].OutputTokens != 10 || first.Retry == nil || first.Retry.SessionID != "usage-session" {
				t.Fatalf("persisted retry/cumulative state=%#v", first)
			}

			// Reload the service around the persisted retry to verify that the
			// adapter resumes the exact session rather than replaying fresh.
			resumed := New(root, workflowConfig(), map[string]runner.Adapter{"codex": adapter})
			retried, retryErr := resumed.Retry(context.Background(), result.State.ID)
			if retryErr != nil || retried.State.Phase != task.PhasePlanned {
				t.Fatalf("retry result=%#v err=%v", retried, retryErr)
			}
			adapter.mu.Lock()
			if len(adapter.requests) != 2 || !adapter.requests[1].Resume || adapter.requests[1].SessionID != "usage-session" {
				t.Fatalf("requests=%#v", adapter.requests)
			}
			promptBytes := int64(len(adapter.requests[0].Prompt) + len(adapter.requests[1].Prompt))
			adapter.mu.Unlock()
			usage = retried.State.Usage[string(runner.RolePlanner)]
			if usage.Requests != 2 || usage.PromptBytes != promptBytes || usage.IncompleteRequests != 1 || usage.UnreportedRequests != 0 || usage.OutputTokens != 15 || usage.TotalTokens != 15 {
				t.Fatalf("retried usage=%#v, prompt_bytes=%d", usage, promptBytes)
			}
			if retried.State.ProviderUsageCumulative[string(runner.RolePlanner)].OutputTokens != 15 {
				t.Fatalf("retried cumulative=%#v", retried.State.ProviderUsageCumulative)
			}
		})
	}
}

func TestMissingCumulativeUsageDoesNotResetPreviousSnapshot(t *testing.T) {
	root := workflowRepo(t)
	adapter := &usageAdapter{steps: []usageStep{
		{response: plannerUsageResponse(10, runner.UsageComplete, true)},
		{response: runner.Response{UsageStatus: runner.UsageUnreported, UsageCumulative: true}, err: context.DeadlineExceeded},
	}}
	service := New(root, workflowConfig(), map[string]runner.Adapter{"codex": adapter})
	state := task.State{
		ID: "missing-cumulative", RepoRoot: root, Phase: task.PhasePlanned,
		ProfilesSnapshot: map[string]task.ProfileSnapshot{
			string(runner.RolePlanner): {Provider: "codex", Model: "planner-model"},
		},
		InFlight: &task.InFlight{Token: "first", Operation: "plan_start", Role: string(runner.RolePlanner), Prompt: "prompt"},
	}
	if err := service.Store.Create(state); err != nil {
		t.Fatal(err)
	}
	if _, err := service.call(context.Background(), state.ID, "first", runner.RolePlanner); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := service.Store.Update(state.ID, func(st *task.State) error {
		st.InFlight.Token = "second"
		st.InFlight.KnownSession = true
		st.InFlight.SessionID = "usage-session"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.call(context.Background(), state.ID, "second", runner.RolePlanner); err == nil {
		t.Fatal("missing cumulative report did not fail the invocation")
	}
	got, err := service.Status(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	usage := got.Usage[string(runner.RolePlanner)]
	if usage.Requests != 2 || usage.OutputTokens != 10 || usage.UnreportedRequests != 1 || got.ProviderUsageCumulative[string(runner.RolePlanner)].OutputTokens != 10 {
		t.Fatalf("usage=%#v cumulative=%#v", usage, got.ProviderUsageCumulative)
	}
}

func TestLegacyUsageClassificationIgnoresHostAndAggregateCounters(t *testing.T) {
	base := task.TokenUsage{Requests: 99, PromptBytes: 99, UnreportedRequests: 9, IncompleteRequests: 8}
	if status, reported := classifyUsage(runner.Response{Usage: base}, nil); status != runner.UsageUnreported || reported {
		t.Fatalf("absent legacy usage status=%q reported=%v", status, reported)
	}
	base.OutputTokens = 3
	if status, reported := classifyUsage(runner.Response{Usage: base}, nil); status != runner.UsageComplete || !reported {
		t.Fatalf("reported legacy usage status=%q reported=%v", status, reported)
	}
	if status, reported := classifyUsage(runner.Response{Usage: base}, context.Canceled); status != runner.UsageIncomplete || !reported {
		t.Fatalf("interrupted legacy usage status=%q reported=%v", status, reported)
	}
}
