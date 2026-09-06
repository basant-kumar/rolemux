package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/basant-kumar/rolemux/internal/task"
	"github.com/basant-kumar/rolemux/internal/workflow"
)

func TestWorkStartAndStatusExposeCompactControlJSON(t *testing.T) {
	for _, test := range []struct {
		name      string
		maxRounds int
	}{
		{name: "finite", maxRounds: 3},
		{name: "unlimited", maxRounds: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := cliRepo(t)
			parentID := "control-parent"
			unitID := "UNIT"
			parentPlan := "PARENT_PLAN_SENTINEL_DO_NOT_EMIT"
			executionPacket := "EXECUTION_PACKET_SENTINEL_DO_NOT_EMIT"
			promptSentinel := "CHILD_PROMPT_SENTINEL_DO_NOT_EMIT"
			manifestSentinel := "CHILD_MANIFEST_SENTINEL_DO_NOT_EMIT"

			units, err := task.NormalizeWorkUnits([]task.WorkUnit{{
				ID:                 unitID,
				Objective:          "implement the unit",
				Scope:              "unit.go",
				ExecutionPacket:    executionPacket,
				AcceptanceCriteria: []string{"the unit is implemented"},
				ValidationCommands: []string{"go test ./..."},
			}}, parentPlan)
			if err != nil {
				t.Fatal(err)
			}
			planHash := task.ScopeSpecHash(parentPlan)
			parent := task.State{
				ID:               parentID,
				RepoRoot:         root,
				Phase:            task.PhasePlanApproved,
				Task:             "parent objective",
				Plan:             parentPlan,
				PlanHash:         planHash,
				ApprovedPlanHash: planHash,
				WorkGraph:        true,
				WorkUnits:        units,
				ReviewPolicy:     &task.ReviewPolicy{MaxRounds: test.maxRounds},
			}
			markCLIPlanApproved(t, &parent)
			store := task.NewStore(root)
			if err := store.Create(parent); err != nil {
				t.Fatal(err)
			}

			code, output, stderr := runTestApp(t, root, "", "work", "start", parentID, unitID, "--json")
			if code != workflow.ExitOK || stderr != "" {
				t.Fatalf("work start code=%d stderr=%q output=%s", code, stderr, output)
			}
			payload := decodeJSONObjectExactlyOnce(t, output)
			result, child := assertSuccessEnvelope(t, payload, "work-start")
			childID := stringValue(t, child, "id")
			if childID != task.WorkTaskID(parentID, unitID) {
				t.Fatalf("child id=%q want %q", childID, task.WorkTaskID(parentID, unitID))
			}
			assertCompactChildControl(t, result, "ready", test.maxRounds)
			assertCompactWorkflowResultShape(t, result)
			assertChildSummary(t, child, childID, test.maxRounds, parentID, unitID)
			assertNoInternalPayload(t, output, parentPlan, executionPacket, promptSentinel, manifestSentinel)

			code, output, stderr = runTestApp(t, root, "", "status", childID, "--json")
			if code != workflow.ExitOK || stderr != "" {
				t.Fatalf("status code=%d stderr=%q output=%s", code, stderr, output)
			}
			payload = decodeJSONObjectExactlyOnce(t, output)
			result, child = assertSuccessEnvelope(t, payload, "status")
			assertCompactChildControl(t, result, "ready", test.maxRounds)
			assertCompactStatusResultShape(t, result)
			assertStatusSummary(t, result, childID, parentID, unitID)
			assertChildSummary(t, child, childID, test.maxRounds, parentID, unitID)
			assertNoInternalPayload(t, output, parentPlan, executionPacket, promptSentinel, manifestSentinel)

			if _, err := store.Update(childID, func(st *task.State) error {
				st.Prompt = promptSentinel
				st.ScopedBaseline = []task.FileEntry{{Path: manifestSentinel + "-baseline", Kind: "file"}}
				st.CandidateManifest = []task.FileEntry{{Path: manifestSentinel + "-candidate", Kind: "file"}}
				st.ChangeManifest = []task.FileEntry{{Path: manifestSentinel + "-change", Kind: "file"}}
				st.ReviewCheckpoint = []task.FileEntry{{Path: manifestSentinel + "-checkpoint", Kind: "file"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			code, output, stderr = runTestApp(t, root, "", "status", childID, "--json")
			if code != workflow.ExitOK || stderr != "" {
				t.Fatalf("status after update code=%d stderr=%q output=%s", code, stderr, output)
			}
			payload = decodeJSONObjectExactlyOnce(t, output)
			result, child = assertSuccessEnvelope(t, payload, "status")
			assertCompactChildControl(t, result, "ready", test.maxRounds)
			assertCompactStatusResultShape(t, result)
			assertStatusSummary(t, result, childID, parentID, unitID)
			assertChildSummary(t, child, childID, test.maxRounds, parentID, unitID)
			assertNoInternalPayload(t, output, parentPlan, executionPacket, promptSentinel, manifestSentinel)

			code, output, stderr = runTestApp(t, root, "", "work", "start", parentID, unitID, "--json")
			if code != workflow.ExitOK || stderr != "" {
				t.Fatalf("repeated work start code=%d stderr=%q output=%s", code, stderr, output)
			}
			payload = decodeJSONObjectExactlyOnce(t, output)
			result, child = assertSuccessEnvelope(t, payload, "work-start")
			assertCompactChildControl(t, result, task.PhasePlanApproved, test.maxRounds)
			assertCompactWorkflowResultShape(t, result)
			assertChildSummary(t, child, childID, test.maxRounds, parentID, unitID)
			assertNoInternalPayload(t, output, parentPlan, executionPacket, promptSentinel, manifestSentinel)
		})
	}
}

func TestCompactWorkflowResultPreservesDerivedControl(t *testing.T) {
	state := task.State{
		ID:           "child",
		Phase:        task.PhasePlanApproved,
		ParentTaskID: "parent",
		WorkUnitID:   "UNIT",
		ReviewPolicy: &task.ReviewPolicy{MaxRounds: 0},
	}
	derived := workflow.ControlFor(state)
	if derived.Status != "ready" || derived.NextAction != "implement" || derived.ReviewKind != "" {
		t.Fatalf("derived control=%#v", derived)
	}

	t.Run("empty status uses derived ready", func(t *testing.T) {
		got := compactWorkflowResult(workflow.Result{State: state}, nil)
		if !reflect.DeepEqual(got, derived) {
			t.Fatalf("control=%#v want %#v", got, derived)
		}
	})

	t.Run("explicit status overrides only status", func(t *testing.T) {
		want := derived
		want.Status = task.PhasePlanApproved
		got := compactWorkflowResult(workflow.Result{State: state, Status: task.PhasePlanApproved}, nil)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("control=%#v want %#v", got, want)
		}
	})

	t.Run("error takes precedence over explicit success", func(t *testing.T) {
		want := derived
		want.Status = "failed"
		got := compactWorkflowResult(workflow.Result{State: state, Status: "success"}, errors.New("provider failed"))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("control=%#v want %#v", got, want)
		}
	})
}

func decodeJSONObjectExactlyOnce(t *testing.T, data []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	payload := make(map[string]any)
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode JSON: %v (%s)", err, data)
	}
	if payload == nil {
		t.Fatalf("JSON object was null: %s", data)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("JSON did not contain exactly one object: second decode=%v (%s)", err, data)
	}
	return payload
}

func assertSuccessEnvelope(t *testing.T, payload map[string]any, command string) (map[string]any, map[string]any) {
	t.Helper()
	for key := range payload {
		switch key {
		case "ok", "command", "result", "task", "advisories":
		default:
			t.Fatalf("unexpected top-level key %q in payload=%#v", key, payload)
		}
	}
	if ok, exists := payload["ok"].(bool); !exists || !ok {
		t.Fatalf("payload ok=%#v", payload["ok"])
	}
	if got, ok := payload["command"].(string); !ok || got != command {
		t.Fatalf("payload command=%#v want %q", payload["command"], command)
	}
	if _, exists := payload["error"]; exists {
		t.Fatalf("successful payload contains error: %#v", payload)
	}
	result, ok := payload["result"].(map[string]any)
	if !ok {
		t.Fatalf("payload result=%#v", payload["result"])
	}
	child, ok := payload["task"].(map[string]any)
	if !ok {
		t.Fatalf("payload task=%#v", payload["task"])
	}
	return result, child
}

func assertCompactChildControl(t *testing.T, result map[string]any, status string, maxRounds int) {
	t.Helper()
	if got := result["status"]; got != status {
		t.Fatalf("control status=%#v want %q", got, status)
	}
	assertJSONInt(t, result, "review_round", 0)
	assertJSONInt(t, result, "max_rounds", maxRounds)
	if got, ok := result["can_review"].(bool); !ok || got {
		t.Fatalf("control can_review=%#v", result["can_review"])
	}
	if got := result["next_action"]; got != "implement" {
		t.Fatalf("control next_action=%#v want implement", got)
	}
	if _, exists := result["review_kind"]; exists {
		t.Fatalf("materialized child leaked review_kind: %#v", result)
	}
}

func assertCompactWorkflowResultShape(t *testing.T, result map[string]any) {
	t.Helper()
	for key := range result {
		switch key {
		case "status", "review_round", "max_rounds", "can_review", "next_action":
		default:
			t.Fatalf("unexpected workflow result key %q in result=%#v", key, result)
		}
	}
}

func assertChildSummary(t *testing.T, child map[string]any, childID string, maxRounds int, parentID, unitID string) {
	t.Helper()
	if got := stringValue(t, child, "id"); got != childID {
		t.Fatalf("task id=%q want %q", got, childID)
	}
	if got := stringValue(t, child, "phase"); got != task.PhasePlanApproved {
		t.Fatalf("task phase=%q want %q", got, task.PhasePlanApproved)
	}
	assertJSONInt(t, child, "max_rounds", maxRounds)
	if got := stringValue(t, child, "parent_task_id"); got != parentID {
		t.Fatalf("task parent_task_id=%q want %q", got, parentID)
	}
	if got := stringValue(t, child, "work_unit_id"); got != unitID {
		t.Fatalf("task work_unit_id=%q want %q", got, unitID)
	}
}

func assertStatusSummary(t *testing.T, result map[string]any, childID, parentID, unitID string) {
	t.Helper()
	if got := stringValue(t, result, "id"); got != childID {
		t.Fatalf("status id=%q want %q", got, childID)
	}
	if got := stringValue(t, result, "phase"); got != task.PhasePlanApproved {
		t.Fatalf("status phase=%q want %q", got, task.PhasePlanApproved)
	}
	if got := stringValue(t, result, "parent_task_id"); got != parentID {
		t.Fatalf("status parent_task_id=%q want %q", got, parentID)
	}
	if got := stringValue(t, result, "work_unit_id"); got != unitID {
		t.Fatalf("status work_unit_id=%q want %q", got, unitID)
	}
	if updatedAt, ok := result["updated_at"].(string); !ok || strings.TrimSpace(updatedAt) == "" {
		t.Fatalf("status updated_at=%#v", result["updated_at"])
	}
}

func assertCompactStatusResultShape(t *testing.T, result map[string]any) {
	t.Helper()
	allowed := map[string]struct{}{
		"status": {}, "review_kind": {}, "review_round": {}, "max_rounds": {}, "can_review": {},
		"next_action": {}, "question": {}, "source": {}, "id": {}, "phase": {},
		"plan_round": {}, "code_round": {}, "scope": {}, "pending_question": {},
		"pending_question_source": {}, "findings": {}, "profiles": {}, "usage": {},
		"in_flight": {}, "retry": {}, "updated_at": {}, "parent_task_id": {},
		"work_unit_id": {}, "integration_review": {}, "work_graph": {},
		"approval_id": {}, "approval_task_id": {}, "approval_kind": {},
		"choices": {}, "artifact_path": {}, "changed_files": {}, "events": {},
		"budget_issue": {}, "progress": {},
	}
	for key := range result {
		if _, ok := allowed[key]; !ok {
			t.Fatalf("unexpected compact status result key %q in result=%#v", key, result)
		}
	}
}

func stringValue(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	got, ok := object[key].(string)
	if !ok {
		t.Fatalf("%s=%#v is not a string", key, object[key])
	}
	return got
}

func assertJSONInt(t *testing.T, object map[string]any, key string, want int) {
	t.Helper()
	got, ok := object[key].(float64)
	if !ok || int(got) != want || got != float64(want) {
		t.Fatalf("%s=%#v want %d", key, object[key], want)
	}
}

func assertNoInternalPayload(t *testing.T, data []byte, sentinels ...string) {
	t.Helper()
	text := string(data)
	for _, key := range []string{
		"repo_root", "prompt", "plan", "plan_hash", "approved_plan_hash", "approved_manifest_hash",
		"planned_scope", "work_units", "scope_spec_hash", "scoped_baseline_manifest",
		"scoped_baseline_manifest_hash", "candidate_manifest", "candidate_manifest_hash",
		"change_manifest", "review_checkpoint_manifest", "review_checkpoint_manifest_hash",
		"review_checkpoint_findings", "profiles_snapshot", "runtime_snapshot", "review_policy",
		"review_progress", "pending_answer", "prompt_inputs", "return_phase", "interrupted_loop",
		"provider_usage_cumulative", "diagnostics", "budgets_snapshot", "retry", "in_flight",
	} {
		if strings.Contains(text, `"`+key+`":`) {
			t.Fatalf("compact payload leaked internal key %q: %s", key, data)
		}
	}
	for _, sentinel := range sentinels {
		if strings.Contains(text, sentinel) {
			t.Fatalf("compact payload leaked sentinel %q: %s", sentinel, data)
		}
	}
}
