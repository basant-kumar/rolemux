package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/basant-kumar/rolemux/internal/task"
	"github.com/basant-kumar/rolemux/internal/workflow"
)

func pendingCodeApproval(t *testing.T, root, id string) task.State {
	t.Helper()
	if err := os.WriteFile(root+"/app.go", []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := task.NewWorktree(root).ManifestForScope("app.go")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := task.HashManifest(manifest)
	return task.State{
		ID: id, RepoRoot: root, Phase: task.PhaseAwaitingApproval, Scope: "app.go",
		CandidateManifest: manifest, CandidateManifestHash: fingerprint, ChangeManifest: manifest,
		CodeRound: 1, CodeReviewerSessionID: "review-session",
		ReviewProgress:            &task.ReviewProgress{Kind: "code", Status: "approved"},
		ApprovalGateSchemaVersion: task.ApprovalGateSchemaVersion,
		Approval: &task.ApprovalRecord{
			GateID: "gate-code-1", Kind: task.ApprovalKindCode,
			SubjectFingerprint: fingerprint, Question: "Approve reviewed code?",
			Choices: []string{"approve", "request_changes", "discuss"}, Scope: "app.go",
			ChangedFiles: []task.ApprovalChangedFile{{Path: "app.go", ChangeKind: "modified"}},
		},
	}
}

func TestApprovalShowAndApproveAreProviderFree(t *testing.T) {
	root := cliRepo(t)
	state := pendingCodeApproval(t, root, "approval-cli")
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}
	// Approval inspection and approval must not depend on valid configuration
	// or an available provider.
	if err := os.WriteFile(root+"/.rolemux.toml", []byte("invalid = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := runTestApp(t, root, "", "approval", "show", state.ID, "--json")
	if code != workflow.ExitNeedsInput || stderr != "" {
		t.Fatalf("show code=%d stderr=%q output=%s", code, stderr, output)
	}
	payload := decodeSingleObject(t, output)
	result := payload["result"].(map[string]any)
	if payload["ok"] != false || payload["error"].(map[string]any)["code"] != workflow.ApprovalRequiredCode || result["status"] != "approval_required" || result["approval_id"] != "gate-code-1" || result["next_action"] != "approval_respond" {
		t.Fatalf("show payload=%#v", payload)
	}
	if result["artifact_path"] == "" || result["scope"] != "app.go" || len(result["choices"].([]any)) != 3 || len(result["changed_files"].([]any)) != 1 {
		t.Fatalf("show control=%#v", result)
	}
	code, output, stderr = runTestApp(t, root, "", "status", state.ID, "--full", "--json")
	if code != workflow.ExitOK || stderr != "" {
		t.Fatalf("full status code=%d stderr=%q output=%s", code, stderr, output)
	}
	full := decodeSingleObject(t, output)["result"].(map[string]any)
	if full["id"] != state.ID || full["control"].(map[string]any)["approval_id"] != "gate-code-1" {
		t.Fatalf("full status=%#v", full)
	}

	code, output, stderr = runTestApp(t, root, "", "approval", "respond", state.ID, "--gate", "gate-code-1", "--decision", "approve", "--json")
	if code != workflow.ExitNeedsInput || stderr != "" {
		t.Fatalf("unconfirmed approval code=%d stderr=%q output=%s", code, stderr, output)
	}
	unconfirmed := decodeSingleObject(t, output)
	unconfirmedResult := unconfirmed["result"].(map[string]any)
	if unconfirmed["ok"] != false || unconfirmedResult["status"] != "approval_required" || unconfirmedResult["requires_explicit_human_confirmation"] != true || unconfirmedResult["human_confirmation_flag"] != "--human-confirmed" {
		t.Fatalf("unconfirmed payload=%#v", unconfirmed)
	}
	if !strings.Contains(unconfirmed["error"].(map[string]any)["message"].(string), "host agent must rerun") {
		t.Fatalf("unconfirmed error is not actionable: %#v", unconfirmed["error"])
	}

	code, output, stderr = runTestApp(t, root, "", "approval", "respond", state.ID, "--gate", "gate-code-1", "--decision", "approve", "--human-confirmed", "--json")
	if code != workflow.ExitOK || stderr != "" {
		t.Fatalf("approve code=%d stderr=%q output=%s", code, stderr, output)
	}
	payload = decodeSingleObject(t, output)
	result = payload["result"].(map[string]any)
	if payload["ok"] != true || result["status"] != "approved" || result["next_action"] != "advance" {
		t.Fatalf("approve payload=%#v", payload)
	}
	persisted, err := task.NewStore(root).Load(state.ID)
	if err != nil || persisted.Phase != task.PhaseApproved || persisted.Approval == nil || persisted.Approval.Status != task.ApprovalDecisionApprove {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}

	// An identical delayed response is idempotent; a conflicting response is not.
	code, _, _ = runTestApp(t, root, "", "approval", "respond", state.ID, "--gate", "gate-code-1", "--decision", "approve", "--human-confirmed", "--json")
	if code != workflow.ExitOK {
		t.Fatalf("duplicate approve code=%d", code)
	}
	code, output, _ = runTestApp(t, root, "", "approval", "respond", state.ID, "--gate", "gate-code-1", "--decision", "discuss", "--human-confirmed", "--json")
	if code != workflow.ExitAction || decodeSingleObject(t, output)["error"].(map[string]any)["code"] != workflow.ApprovalConflictCode {
		t.Fatalf("conflicting response code=%d output=%s", code, output)
	}
}

func TestApprovalDiscussIsReadOnlyAndHumanOutputIsActionable(t *testing.T) {
	root := cliRepo(t)
	state := pendingCodeApproval(t, root, "approval-human")
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := runTestApp(t, root, "", "approval", "show", state.ID)
	if code != workflow.ExitNeedsInput || stderr != "" {
		t.Fatalf("human show code=%d stderr=%q output=%s", code, stderr, output)
	}
	text := string(output)
	for _, required := range []string{"review verdict: Approved", "next step: Human code approval", "review artifact:", "review locally (status):", "review locally (tracked diff):", "review on GitHub: rolemux approval publish", "Approve (approve)", "Request changes (request_changes)", "Discuss (discuss)", "hard stop:", "--gate gate-code-1", "--decision request_changes --feedback", "--human-confirmed"} {
		if !strings.Contains(text, required) {
			t.Fatalf("human output missing %q: %s", required, text)
		}
	}
	before, err := task.NewStore(root).Load(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, output, _ = runTestApp(t, root, "", "approval", "respond", state.ID, "--gate", "gate-code-1", "--decision", "discuss", "--human-confirmed", "--json")
	if code != workflow.ExitNeedsInput {
		t.Fatalf("discuss code=%d output=%s", code, output)
	}
	after, err := task.NewStore(root).Load(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		beforeJSON, _ := json.Marshal(before)
		afterJSON, _ := json.Marshal(after)
		t.Fatalf("discuss mutated state\nbefore=%s\nafter=%s", beforeJSON, afterJSON)
	}
}

func TestApprovalShowExposesAttachedGitHubDraftAndStaleness(t *testing.T) {
	root := cliRepo(t)
	state := pendingCodeApproval(t, root, "approval-github")
	state.Approval.ExternalReview = &task.ExternalReview{
		Provider:                      "github",
		URL:                           "https://github.com/example/project/pull/7",
		Number:                        7,
		Repository:                    "example/project",
		PublishedCandidateFingerprint: "older-candidate",
	}
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := runTestApp(t, root, "", "approval", "show", state.ID, "--json")
	if code != workflow.ExitNeedsInput || stderr != "" {
		t.Fatalf("show code=%d stderr=%q output=%s", code, stderr, output)
	}
	result := decodeSingleObject(t, output)["result"].(map[string]any)
	review := result["external_review"].(map[string]any)
	if review["url"] != state.Approval.ExternalReview.URL || result["review_outdated"] != true {
		t.Fatalf("control=%#v", result)
	}

	code, output, stderr = runTestApp(t, root, "", "approval", "show", state.ID)
	if code != workflow.ExitNeedsInput || stderr != "" {
		t.Fatalf("human show code=%d stderr=%q output=%s", code, stderr, output)
	}
	for _, want := range []string{state.Approval.ExternalReview.URL, "update GitHub draft:", "import PR comments as requested changes:"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("human output missing %q: %s", want, output)
		}
	}
}

func TestApprovalParentResolvesIntegrationGateAndValidatesUsage(t *testing.T) {
	root := cliRepo(t)
	parent := task.State{ID: "approval-parent", RepoRoot: root, Phase: task.PhasePlanApproved, Plan: "plan", PlanHash: task.ScopeSpecHash("plan"), ApprovedPlanHash: task.ScopeSpecHash("plan"), WorkGraph: true}
	markCLIPlanApproved(t, &parent)
	store := task.NewStore(root)
	if err := store.Create(parent); err != nil {
		t.Fatal(err)
	}
	integration := pendingCodeApproval(t, root, task.IntegrationTaskID(parent.ID))
	integration.ParentTaskID = parent.ID
	integration.IntegrationReview = true
	integration.ReviewProgress.Kind = "integration"
	if err := store.Create(integration); err != nil {
		t.Fatal(err)
	}

	code, output, _ := runTestApp(t, root, "", "approval", "show", parent.ID, "--json")
	if code != workflow.ExitNeedsInput {
		t.Fatalf("parent show code=%d output=%s", code, output)
	}
	result := decodeSingleObject(t, output)["result"].(map[string]any)
	if result["approval_task_id"] != integration.ID || result["review_kind"] != "integration" {
		t.Fatalf("parent control=%#v", result)
	}

	for _, args := range [][]string{
		{"approval", "respond", parent.ID, "--gate", "gate-code-1", "--decision", "request_changes", "--human-confirmed", "--json"},
		{"approval", "respond", parent.ID, "--gate", "gate-code-1", "--decision", "approve", "--feedback", "unused", "--human-confirmed", "--json"},
		{"approval", "respond", parent.ID, "--gate", "gate-code-1", "--decision", "invalid", "--human-confirmed", "--json"},
	} {
		code, output, _ = runTestApp(t, root, "", args...)
		if code != workflow.ExitUsage || decodeSingleObject(t, output)["error"].(map[string]any)["code"] != "USAGE" {
			t.Fatalf("args=%v code=%d output=%s", args, code, output)
		}
	}

	none := task.State{ID: "no-approval", RepoRoot: root, Phase: task.PhasePlanned}
	if err := store.Create(none); err != nil {
		t.Fatal(err)
	}
	code, output, _ = runTestApp(t, root, "", "approval", "show", none.ID, "--json")
	if code != workflow.ExitUsage || decodeSingleObject(t, output)["error"].(map[string]any)["code"] != "NO_PENDING_APPROVAL" {
		t.Fatalf("no pending code=%d output=%s", code, output)
	}
}

func TestPlanApprovalUnlocksExecutionWithoutProviderSetup(t *testing.T) {
	root := cliRepo(t)
	state := task.State{
		ID: "plan-approval-cli", RepoRoot: root, Phase: task.PhaseAwaitingApproval,
		Plan: "approved plan", PlanHash: task.ScopeSpecHash("approved plan"), WorkGraph: true,
		WorkUnits: []task.WorkUnit{{ID: "T1", Objective: "change app", Scope: "app.go", ExecutionPacket: "change app", AcceptanceCriteria: []string{"works"}, ValidationCommands: []string{"go test ./..."}}},
	}
	markCLIPlanApproved(t, &state)
	state.Phase = task.PhaseAwaitingApproval
	state.ApprovedPlanHash = ""
	state.Approval.Status = ""
	state.Approval.GateID = "gate-plan-1"
	state.Approval.Kind = task.ApprovalKindPlan
	state.Approval.Question = "Approve reviewed plan?"
	state.Approval.Choices = []string{"approve", "request_changes", "discuss"}
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/.rolemux.toml", []byte("invalid = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := runTestApp(t, root, "", "approval", "respond", state.ID, "--gate", "gate-plan-1", "--decision", "approve", "--human-confirmed", "--json")
	if code != workflow.ExitOK || stderr != "" {
		t.Fatalf("plan approve code=%d stderr=%q output=%s", code, stderr, output)
	}
	result := decodeSingleObject(t, output)["result"].(map[string]any)
	if result["status"] != "approved" || result["review_kind"] != "plan" || result["next_action"] != "advance" {
		t.Fatalf("plan approve result=%#v", result)
	}
	persisted, err := task.NewStore(root).Load(state.ID)
	if err != nil || persisted.Phase != task.PhasePlanApproved || persisted.ApprovedPlanHash != persisted.PlanHash {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
}

func TestHelpListsApprovalCommands(t *testing.T) {
	root := cliRepo(t)
	code, output, stderr := runTestApp(t, root, "", "help")
	if code != workflow.ExitOK || stderr != "" {
		t.Fatalf("help code=%d stderr=%q", code, stderr)
	}
	for _, line := range []string{"rolemux approval show TASK-ID", "rolemux approval publish TASK-ID", "rolemux approval sync TASK-ID", "rolemux approval respond TASK-ID --gate GATE-ID", "--decision approve|request_changes|discuss", "--human-confirmed"} {
		if !bytes.Contains(output, []byte(line)) {
			t.Fatalf("help missing %q: %s", line, output)
		}
	}
}
