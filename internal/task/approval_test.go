package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyStateDoesNotInventApproval(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{"id":"legacy","repo_root":"/repo","phase":"planned"}`), &st); err != nil {
		t.Fatal(err)
	}
	if st.Approval != nil || st.ApprovalHistory != nil || st.ApprovalGateSchemaVersion != 0 {
		t.Fatalf("legacy state gained approval metadata: %#v", st)
	}

	created := time.Unix(10, 20).UTC()
	decided := time.Unix(30, 40).UTC()
	current := ApprovalRecord{
		GateID:             "gate-1",
		Kind:               ApprovalKindPlan,
		Status:             ApprovalDecisionApprove,
		ArtifactRef:        "/private/approval.json",
		ArtifactDigest:     "digest",
		SubjectFingerprint: "plan-fingerprint",
		Question:           "Proceed?",
		Choices:            []string{"approve", "request_changes"},
		Scope:              "internal/task/**",
		ChangedFiles:       []ApprovalChangedFile{{Path: "internal/task/task.go", Kind: "modified"}},
		ReviewerEvidence: &ReviewerEvidence{
			SourceTask:          "task-1",
			Verdict:             "approved",
			ReviewerRole:        "reviewer",
			ReviewerSessionID:   "session-1",
			AcceptedRound:       2,
			ReviewedFingerprint: "plan-fingerprint",
		},
		CreatedAt:            created,
		DecidedAt:            &decided,
		HumanFeedback:        "Looks good",
		FeedbackOperationRef: "feedback-1",
	}
	store := NewStoreAt(filepath.Join(t.TempDir(), "state"))
	want := State{
		ID:                        "approval",
		RepoRoot:                  "/repo",
		Phase:                     PhaseAwaitingApproval,
		Approval:                  &current,
		ApprovalHistory:           []ApprovalRecord{{GateID: "old-gate", Kind: ApprovalKindPlan, Status: ApprovalDecisionRequestChanges, HumanFeedback: "Fix scope"}},
		ApprovalGateSchemaVersion: ApprovalGateSchemaVersion,
	}
	if err := store.Create(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Approval == nil || got.Approval.GateID != current.GateID || got.Approval.Status != current.Status {
		t.Fatalf("current approval was not retained: %#v", got.Approval)
	}
	if got.Approval.ReviewerEvidence == nil || got.Approval.ReviewerEvidence.ReviewedFingerprint != "plan-fingerprint" {
		t.Fatalf("reviewer evidence was not retained: %#v", got.Approval.ReviewerEvidence)
	}
	if len(got.ApprovalHistory) != 1 || got.ApprovalHistory[0].HumanFeedback != "Fix scope" {
		t.Fatalf("approval history was not retained: %#v", got.ApprovalHistory)
	}
	if got.ApprovalGateSchemaVersion != ApprovalGateSchemaVersion {
		t.Fatalf("gate schema version was not retained: %d", got.ApprovalGateSchemaVersion)
	}
}

func TestApprovalArtifactIsPrivateImmutableAndRepairable(t *testing.T) {
	store := NewStoreAt(filepath.Join(t.TempDir(), "state"))
	contents := []byte("approval report\n")
	ref, err := store.WriteApprovalArtifact("task-1", "gate-1", contents)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(ref.Path) || ref.Digest == "" || ref.Size != int64(len(contents)) {
		t.Fatalf("invalid artifact ref: %#v", ref)
	}
	if info, err := os.Stat(ref.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(ref.Path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("artifact directory mode: %v %v", info, err)
	}
	if again, err := store.WriteApprovalArtifact("task-1", "gate-1", contents); err != nil || again != ref {
		t.Fatalf("identical artifact write was not idempotent: %#v %v", again, err)
	}
	read, err := store.ReadApprovalArtifact("task-1", "gate-1", ref.Digest)
	if err != nil || !bytes.Equal(read, contents) {
		t.Fatalf("artifact read: %q %v", read, err)
	}

	if err := os.WriteFile(ref.Path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteApprovalArtifact("task-1", "gate-1", contents); !errors.Is(err, ErrArtifactConflict) {
		t.Fatalf("conflicting artifact was accepted: %v", err)
	}
	if _, err := store.ReadApprovalArtifact("task-1", "gate-1", ref.Digest); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("corrupt artifact was accepted: %v", err)
	}

	if err := os.Remove(ref.Path); err != nil {
		t.Fatal(err)
	}
	repaired, err := store.ReadOrRepairApprovalArtifact("task-1", "gate-1", contents)
	if err != nil || repaired != ref {
		t.Fatalf("artifact repair: %#v %v", repaired, err)
	}
}

func TestApprovalReportsAreDeterministicAndNarrow(t *testing.T) {
	st := State{
		ID:         "task-1",
		Plan:       "Update the task store",
		Complexity: ComplexitySmall,
		WorkUnits:  []WorkUnit{{ID: "T1", Objective: "store records", Scope: "internal/task", DependsOn: []string{"T0"}}},
		Scope:      "internal/task/**",
		ScopedBaseline: []FileEntry{
			{Path: "internal/task/old.go", Kind: "file"},
			{Path: "internal/task/task.go", Kind: "file", Worktree: ContentState{Hash: "before"}},
		},
		CandidateManifest: []FileEntry{
			{Path: "internal/task/new.go", Kind: "file"},
			{Path: "internal/task/task.go", Kind: "file", Worktree: ContentState{Hash: "after"}},
		},
		CandidateManifestHash: "candidate-fingerprint",
		ChangeManifest: []FileEntry{
			{Path: "internal/task/old.go", Kind: "file"},
			{Path: "internal/task/new.go", Kind: "file"},
			{Path: "internal/task/task.go", Kind: "file"},
		},
		ImplementerSessionID: "session-transcript-is-not-report-data",
		Findings:             []Finding{{Severity: "warning", Message: "check compatibility"}},
		Advisories:           []Diagnostic{{Code: "D1", Severity: "info", Message: "saved diagnostic"}},
		Diagnostics:          []string{"saved diagnostic"},
	}
	record := ApprovalRecord{
		GateID:             "gate-1",
		Kind:               ApprovalKindPlan,
		SubjectFingerprint: "plan-fingerprint",
		ReviewerEvidence:   &ReviewerEvidence{SourceTask: "task-1", Verdict: "approved", AcceptedRound: 1},
	}
	first, err := BuildPlanApprovalArtifact(st, record)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildPlanApprovalArtifact(st, record)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("plan report was not deterministic: %v", err)
	}
	var plan map[string]json.RawMessage
	if err := json.Unmarshal(first, &plan); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"plan", "complexity", "work_units", "reviewer_evidence"} {
		if _, ok := plan[key]; !ok {
			t.Fatalf("plan report omitted %q: %s", key, first)
		}
	}
	if _, ok := plan["implementer_session_id"]; ok {
		t.Fatal("plan report copied session state")
	}

	record.Kind = ApprovalKindCode
	code, err := BuildCodeApprovalArtifact(st, record)
	if err != nil {
		t.Fatal(err)
	}
	var codeMap map[string]json.RawMessage
	if err := json.Unmarshal(code, &codeMap); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"scope", "changed_paths", "changed_files", "candidate_fingerprint", "findings", "advisories", "diagnostics"} {
		if _, ok := codeMap[key]; !ok {
			t.Fatalf("code report omitted %q: %s", key, code)
		}
	}
	var codeReport CodeApprovalArtifact
	if err := json.Unmarshal(code, &codeReport); err != nil {
		t.Fatal(err)
	}
	changeKinds := map[string]string{}
	for _, file := range codeReport.ChangedFiles {
		changeKinds[file.Path] = file.ChangeKind
	}
	for path, want := range map[string]string{
		"internal/task/new.go":  "added",
		"internal/task/old.go":  "deleted",
		"internal/task/task.go": "modified",
	} {
		if changeKinds[path] != want {
			t.Fatalf("change kind for %s: got %q want %q", path, changeKinds[path], want)
		}
	}
	if _, ok := codeMap["implementer_session_id"]; ok {
		t.Fatal("code report copied session state")
	}
}

func TestPrivateArtifactAndPlanPathsRejectSymlinkDirectories(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	store := NewStoreAt(stateRoot)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stateRoot, "approvals")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteApprovalArtifact("task-1", "gate-1", []byte("report")); !errors.Is(err, ErrPrivatePath) {
		t.Fatalf("artifact symlink was followed: %v", err)
	}

	root := testRepo(t)
	planPath, err := PlanPath(root, "symlink-plan")
	if err != nil {
		t.Fatal(err)
	}
	privateRoot := filepath.Dir(filepath.Dir(planPath))
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(privateRoot, "plans")); err != nil {
		t.Fatal(err)
	}
	if err := WritePlan(root, "symlink-plan", "plan"); !errors.Is(err, ErrPrivatePath) {
		t.Fatalf("plan symlink was followed: %v", err)
	}
}

func TestReadApprovalArtifactRefRejectsIntermediateSymlink(t *testing.T) {
	store := NewStoreAt(filepath.Join(t.TempDir(), "state"))
	contents := []byte("reference bytes")
	ref, err := store.WriteApprovalArtifact("task-1", "gate-1", contents)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, ref.Digest), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(store.Dir, "approvals")
	if err := os.Symlink(outside, filepath.Join(artifactRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	forged := ref
	forged.Path = filepath.Join(artifactRoot, "escape", ref.Digest)
	if _, err := store.ReadApprovalArtifactRef(forged); !errors.Is(err, ErrPrivatePath) {
		t.Fatalf("reference read followed intermediate symlink: %v", err)
	}
}
