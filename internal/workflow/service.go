package workflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/basant-kumar/rolemux/internal/catalog"
	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

// MaxRounds is retained for embedders that used the old workflow package
// constant. New tasks resolve their limit from config and persist it in the
// task policy snapshot.
const MaxRounds = config.DefaultReviewMaxRounds

const abandonedOperationGrace = 5 * time.Second

type Result struct {
	State  task.State
	Status string
}

// Control is the durable host-action view of a task. It intentionally carries
// only workflow coordination data; provider prompts, findings, and usage stay
// in task.State.
type Control struct {
	Status         string                     `json:"status"`
	ReviewKind     string                     `json:"review_kind,omitempty"`
	ReviewRound    int                        `json:"review_round"`
	MaxRounds      int                        `json:"max_rounds"`
	CanReview      bool                       `json:"can_review"`
	NextAction     string                     `json:"next_action"`
	ApprovalID     string                     `json:"approval_id,omitempty"`
	ApprovalTaskID string                     `json:"approval_task_id,omitempty"`
	ApprovalKind   string                     `json:"approval_kind,omitempty"`
	Question       string                     `json:"question,omitempty"`
	Choices        []ApprovalChoice           `json:"choices,omitempty"`
	ArtifactPath   string                     `json:"artifact_path,omitempty"`
	Scope          string                     `json:"scope,omitempty"`
	ChangedFiles   []task.ApprovalChangedFile `json:"changed_files,omitempty"`
	Source         string                     `json:"source,omitempty"`
}

type ApprovalChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type ControlChoice = ApprovalChoice

// ControlFor derives the next safe host action from durable state. In-flight
// and question state are checked before completed review metadata so a stale
// outcome can never mask a live recovery action.
func ControlFor(st task.State) Control {
	kind := reviewKind(st)
	control := Control{
		ReviewKind:  kind,
		ReviewRound: reviewRound(st, kind),
		MaxRounds:   maxRounds(&st),
		NextAction:  "inspect",
	}
	if st.IntegrationReview && st.ParentTaskID != "" {
		control.ApprovalTaskID = task.IntegrationTaskID(st.ParentTaskID)
	}
	if st.InFlight != nil {
		control.Status = "in_flight"
		control.NextAction = "wait"
		return control
	}
	if st.Approval != nil && st.Approval.Status == "" {
		return approvalControl(control, st, st.Approval)
	}
	if st.Phase == task.PhaseNeedsInput || st.PendingQuestion != "" {
		control.Status = "needs_input"
		control.NextAction = answerAction(kind, st.PendingQuestionSource)
		control.Question = st.PendingQuestion
		control.Source = st.PendingQuestionSource
		return control
	}
	if isExhaustedState(st) {
		control.Status = "exhausted"
		control.NextAction = "stop"
		return control
	}
	if st.ReviewProgress != nil {
		switch st.ReviewProgress.Status {
		case "approved":
			control.Status = "approved"
			control.NextAction = "advance"
			return control
		case "revised", "fixed":
			control.Status = st.ReviewProgress.Status
			control.NextAction = reviewAction(kind)
			control.CanReview = true
			return control
		case "no_progress":
			control.Status = "no_progress"
			control.NextAction = "inspect"
			control.CanReview = true
			return control
		case "needs_input":
			control.Status = "needs_input"
			control.NextAction = answerAction(kind, st.PendingQuestionSource)
			control.Question = st.PendingQuestion
			control.Source = st.PendingQuestionSource
			return control
		case "recovery":
			control.Status = "recovery"
			control.NextAction = "inspect"
			return control
		case "review_needed":
			control.Status = "review_needed"
			control.NextAction = "retry"
			return control
		case "failed":
			control.Status = "failed"
			if retryResumable(st.Retry) {
				control.NextAction = "retry"
			} else {
				control.NextAction = "inspect"
			}
			return control
		}
	}
	switch st.Phase {
	case task.PhasePlanned:
		control.Status = "planned"
		control.NextAction = "plan_review"
		control.CanReview = true
	case task.PhasePlanApproved:
		if isImplementationReadyPlan(st) {
			control.Status = "ready"
			control.NextAction = "implement"
		} else if humanPlanApproved(st) {
			control.Status = "approved"
			control.NextAction = "advance"
		} else if planReviewerEvidenceSufficient(st) {
			control.Status = "approval_required"
			control.NextAction = "approval_respond"
			control.ApprovalKind = string(task.ApprovalKindPlan)
			control.Question = planApprovalQuestion
			control.Choices = defaultApprovalChoices()
			control.ApprovalTaskID = st.ID
		} else {
			control.Status = "review_needed"
			control.NextAction = "plan_review"
			control.CanReview = true
		}
	case task.PhaseImplementationReady:
		control.Status = "implementation_ready"
		control.NextAction = reviewAction(kind)
		control.CanReview = true
	case task.PhaseApproved:
		if isMaterializedWorkUnitChild(st) || humanCodeApproved(st) {
			control.Status = "approved"
			control.NextAction = "advance"
		} else if codeReviewerEvidenceSufficient(st) {
			control.Status = "approval_required"
			control.NextAction = "approval_respond"
			control.ApprovalKind = string(task.ApprovalKindCode)
			control.Question = codeApprovalQuestion
			control.Choices = defaultApprovalChoices()
			control.ApprovalTaskID = st.ID
		} else {
			control.Status = "review_needed"
			control.NextAction = "code_review"
			control.CanReview = true
		}
	case task.PhaseAwaitingApproval:
		control.Status = "approval_required"
		control.NextAction = "approval_respond"
		control.ApprovalKind = kind
		control.ApprovalTaskID = st.ID
		control.Choices = defaultApprovalChoices()
		if kind == "plan" {
			control.Question = planApprovalQuestion
		} else {
			control.Question = codeApprovalQuestion
		}
	case task.PhaseReviewNeeded:
		control.Status = "review_needed"
		control.NextAction = "retry"
	case task.PhaseFailed:
		control.Status = "failed"
		if retryResumable(st.Retry) {
			control.NextAction = "retry"
		}
	case task.PhasePlanReviewing, task.PhaseCodeReviewing, task.PhaseImplementing:
		control.Status = "in_flight"
		control.NextAction = "wait"
	default:
		control.Status = st.Phase
	}
	return control
}

const (
	planApprovalQuestion = "Approve this reviewed plan to begin execution."
	codeApprovalQuestion = "Approve this reviewed code candidate to complete the workflow."
)

func defaultApprovalChoices() []ApprovalChoice {
	return []ApprovalChoice{
		{Value: string(task.ApprovalDecisionApprove), Label: "Approve"},
		{Value: string(task.ApprovalDecisionRequestChanges), Label: "Request changes"},
		{Value: string(task.ApprovalDecisionDiscuss), Label: "Discuss"},
	}
}

func approvalControl(control Control, st task.State, record *task.ApprovalRecord) Control {
	control.Status = "approval_required"
	control.CanReview = false
	control.NextAction = "approval_respond"
	control.ApprovalID = record.GateID
	control.ApprovalTaskID = approvalTaskID(st, record)
	control.ApprovalKind = string(record.Kind)
	control.Question = record.Question
	if control.Question == "" {
		if record.Kind == task.ApprovalKindPlan {
			control.Question = planApprovalQuestion
		} else {
			control.Question = codeApprovalQuestion
		}
	}
	control.Choices = defaultApprovalChoices()
	if record.Scope != "" {
		control.Scope = record.Scope
	} else if st.Scope != "" {
		control.Scope = st.Scope
	} else {
		control.Scope = st.PlannedScope
	}
	control.ChangedFiles = append([]task.ApprovalChangedFile(nil), record.ChangedFiles...)
	if len(control.ChangedFiles) == 0 && record.Kind == task.ApprovalKindCode {
		control.ChangedFiles = approvalChangedFiles(st)
	}
	control.ArtifactPath = record.ArtifactRef
	return control
}

func approvalTaskID(st task.State, record *task.ApprovalRecord) string {
	if record != nil && record.ReviewerEvidence != nil {
		if strings.TrimSpace(record.ReviewerEvidence.SourceTaskID) != "" {
			return record.ReviewerEvidence.SourceTaskID
		}
		if strings.TrimSpace(record.ReviewerEvidence.SourceTask) != "" {
			return record.ReviewerEvidence.SourceTask
		}
	}
	return st.ID
}

func humanPlanApproved(st task.State) bool {
	if isImplementationReadyPlan(st) {
		return st.Phase == task.PhasePlanApproved && st.PlanHash != "" && st.ApprovedPlanHash == st.PlanHash
	}
	record := st.Approval
	return st.Phase == task.PhasePlanApproved && record != nil && record.Kind == task.ApprovalKindPlan &&
		record.Status == task.ApprovalDecisionApprove && record.SubjectFingerprint != "" &&
		record.SubjectFingerprint == planReviewFingerprint(st) &&
		st.PlanHash != "" && st.ApprovedPlanHash == st.PlanHash
}

func humanCodeApproved(st task.State) bool {
	record := st.Approval
	return st.Phase == task.PhaseApproved && record != nil && record.Kind == task.ApprovalKindCode &&
		record.Status == task.ApprovalDecisionApprove && record.SubjectFingerprint != "" &&
		record.SubjectFingerprint == st.CandidateManifestHash && st.CandidateManifestHash != "" &&
		st.ApprovedManifestHash == st.CandidateManifestHash
}

func planReviewerEvidenceSufficient(st task.State) bool {
	return st.PlanHash != "" && strings.TrimSpace(st.Plan) != "" && st.PlanHash == hash(st.Plan) &&
		strings.TrimSpace(st.PlanReviewerSessionID) != "" && reviewRound(st, "plan") > 0 && st.ReviewProgress != nil &&
		st.ReviewProgress.Kind == "plan" && st.ReviewProgress.Status == "approved"
}

func codeReviewerEvidenceSufficient(st task.State) bool {
	kind := "code"
	if st.IntegrationReview {
		kind = "integration"
	}
	return st.CandidateManifestHash != "" && task.HashManifest(st.CandidateManifest) == st.CandidateManifestHash &&
		st.ApprovedManifestHash == st.CandidateManifestHash && strings.TrimSpace(st.CodeReviewerSessionID) != "" && reviewRound(st, kind) > 0 &&
		st.ReviewProgress != nil && st.ReviewProgress.Kind == kind && st.ReviewProgress.Status == "approved"
}

func approvalScopeFor(st task.State) string {
	if st.Scope != "" {
		return st.Scope
	}
	if st.PlannedScope != "" {
		return st.PlannedScope
	}
	var parts []string
	for _, unit := range st.WorkUnits {
		parts = append(parts, task.ScopePatterns(unit.Scope)...)
	}
	if len(parts) == 0 {
		return ""
	}
	scope, err := task.CanonicalScope(strings.Join(parts, ","))
	if err != nil {
		return strings.Join(parts, ",")
	}
	return scope
}

func approvalChangedFiles(st task.State) []task.ApprovalChangedFile {
	files := make([]task.ApprovalChangedFile, 0, len(st.ChangeManifest))
	for _, entry := range st.ChangeManifest {
		files = append(files, task.ApprovalChangedFile{Path: entry.Path, Kind: entry.Kind})
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Path == files[j].Path {
			return files[i].Kind < files[j].Kind
		}
		return files[i].Path < files[j].Path
	})
	return files
}

func approvalRequiredResult(st task.State) (Result, error) {
	return Result{State: st, Status: "approval_required"}, problem(ApprovalRequiredCode, "human approval is required before this workflow can advance", st.ID, ExitNeedsInput, true, nil)
}

func answerAction(kind, source string) string {
	if source == string(runner.RoleImplementer) || kind == "code" || kind == "integration" {
		return "implement_answer"
	}
	return "plan_answer"
}

func reviewAction(kind string) string {
	switch kind {
	case "plan":
		return "plan_review"
	case "integration":
		return "work_integrate"
	default:
		return "code_review"
	}
}

func reviewKind(st task.State) string {
	if st.InFlight != nil {
		if kind := kindForRoleLoop(st.InFlight.Role, st.InFlight.Loop, st.IntegrationReview); kind != "" {
			return kind
		}
	}
	if st.Phase == task.PhaseNeedsInput && st.PendingQuestionSource != "" {
		if kind := kindForRoleLoop(st.PendingQuestionSource, st.InterruptedLoop, st.IntegrationReview); kind != "" {
			return kind
		}
	}
	if st.Retry != nil {
		if kind := kindForRoleLoop(st.Retry.Role, st.Retry.Loop, st.IntegrationReview); kind != "" {
			return kind
		}
	}
	if st.ReviewProgress != nil && st.ReviewProgress.Kind != "" {
		return st.ReviewProgress.Kind
	}
	if st.IntegrationReview {
		return "integration"
	}
	switch st.Phase {
	case task.PhasePlanned, task.PhasePlanReviewing:
		return "plan"
	case task.PhasePlanApproved:
		if isImplementationReadyPlan(st) {
			return ""
		}
		return "plan"
	case task.PhaseImplementationReady, task.PhaseImplementing, task.PhaseCodeReviewing, task.PhaseApproved, task.PhaseReviewNeeded:
		return "code"
	case task.PhaseFailed:
		if (st.PlanRound > 0 || (st.Round > 0 && st.CodeRound == 0 && st.Scope == "")) && st.CodeRound == 0 {
			return "plan"
		}
		if st.CodeRound > 0 || (st.Round > 0 && st.Scope != "") {
			return "code"
		}
	}
	return ""
}

func isMaterializedWorkUnitChild(st task.State) bool {
	return st.ParentTaskID != "" && st.WorkUnitID != "" && !st.IntegrationReview
}

func isImplementationReadyPlan(st task.State) bool {
	return st.DirectImplementation || isMaterializedWorkUnitChild(st)
}

func kindForRoleLoop(role, loop string, integration bool) string {
	if integration {
		return "integration"
	}
	switch runner.Role(role) {
	case runner.RolePlanner, runner.RolePlanReviewer:
		return "plan"
	case runner.RoleImplementer, runner.RoleCodeReviewer:
		return "code"
	}
	if strings.HasPrefix(loop, "plan") {
		return "plan"
	}
	if strings.HasPrefix(loop, "code") || loop == "implement" {
		return "code"
	}
	return ""
}

func reviewRound(st task.State, kind string) int {
	// A task with either the durable policy snapshot or one of the split
	// counters is on the new accounting scheme. Its compatibility Round field
	// may still contain the plan count while code review has not started.
	splitRounds := st.ReviewPolicy != nil || st.PlanRound != 0 || st.CodeRound != 0
	switch kind {
	case "plan":
		if splitRounds {
			return st.PlanRound
		}
	case "code", "integration":
		if splitRounds {
			return st.CodeRound
		}
	}
	return st.Round
}

func acceptReviewRound(st *task.State, kind string) int {
	round := reviewRound(*st, kind) + 1
	switch kind {
	case "plan":
		st.PlanRound = round
	case "code", "integration":
		st.CodeRound = round
	}
	st.Round = round
	return round
}

type Service struct {
	RepoRoot string
	Store    *task.Store
	Config   config.Config
	Adapters map[string]runner.Adapter
	// ModelCachePath is explicit so library users and tests never inherit the
	// process owner's cache accidentally. An empty path disables caching.
	ModelCachePath string
	Capabilities   func(provider string, role runner.Role, taskText string) CapabilityContext
	Diagnostic     func(string)
	Observe        func(string) ([]task.FileEntry, error)
	Capture        func([]task.FileEntry, string, string) ([]task.FileEntry, error)
	WritePlan      func(string, string) error
	Now            func() time.Time
	Token          func() string
	ProcessID      func() int
	ProcessAlive   func(int) bool
}

type CapabilityContext struct {
	Note             string
	SkillDirectories []string
}

func New(repoRoot string, cfg config.Config, adapters map[string]runner.Adapter) *Service {
	root, _ := filepath.Abs(repoRoot)
	store := task.NewStore(root)
	worktree := task.NewWorktree(root)
	s := &Service{RepoRoot: filepath.Clean(root), Store: store, Config: cfg, Adapters: adapters, Now: time.Now, Token: task.NewToken, ProcessID: os.Getpid}
	s.Observe = worktree.ManifestForScope
	s.Capture = func(entries []task.FileEntry, label, id string) ([]task.FileEntry, error) {
		privateRoot := filepath.Dir(store.Dir)
		captured, err := task.CaptureContentRefs(entries, root, privateRoot, label, id)
		if err != nil {
			return nil, err
		}
		return task.CaptureIndexRefs(captured, root, privateRoot, label, id)
	}
	s.WritePlan = func(id, contents string) error { return task.WritePlan(root, id, contents) }
	return s
}

func (s *Service) gateID(st task.State, kind string, fingerprint string) string {
	seed := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", kind, fingerprint, len(st.ApprovalHistory), s.now().UnixNano(), s.newToken())
	return "gate-" + hash(seed)[:32]
}

func reviewerEvidence(st task.State, sourceTask, role, verdict, fingerprint, session string, round int) *task.ReviewerEvidence {
	evidence := &task.ReviewerEvidence{
		SourceTask:          sourceTask,
		SourceTaskID:        sourceTask,
		Verdict:             verdict,
		ReviewerRole:        role,
		ReviewerSessionID:   session,
		ReviewerSession:     session,
		AcceptedRound:       round,
		ReviewedFingerprint: fingerprint,
	}
	if profile, ok := st.ProfilesSnapshot[role]; ok {
		copyProfile := profile
		evidence.ReviewerProfile = &copyProfile
		evidence.Profile = &copyProfile
	}
	return evidence
}

func planApprovalRecord(st task.State, evidence *task.ReviewerEvidence) task.ApprovalRecord {
	return task.ApprovalRecord{
		GateID:             "",
		Kind:               task.ApprovalKindPlan,
		SubjectFingerprint: planReviewFingerprint(st),
		Question:           planApprovalQuestion,
		Choices:            []string{string(task.ApprovalDecisionApprove), string(task.ApprovalDecisionRequestChanges), string(task.ApprovalDecisionDiscuss)},
		Scope:              approvalScopeFor(st),
		ReviewerEvidence:   evidence,
		CreatedAt:          st.UpdatedAt,
	}
}

func codeApprovalRecord(st task.State, evidence *task.ReviewerEvidence) task.ApprovalRecord {
	return task.ApprovalRecord{
		Kind:               task.ApprovalKindCode,
		SubjectFingerprint: st.CandidateManifestHash,
		Question:           codeApprovalQuestion,
		Choices:            []string{string(task.ApprovalDecisionApprove), string(task.ApprovalDecisionRequestChanges), string(task.ApprovalDecisionDiscuss)},
		Scope:              st.Scope,
		ChangedPaths:       changedPaths(st.ChangeManifest),
		ChangedFiles:       approvalChangedFiles(st),
		ReviewerEvidence:   evidence,
		CreatedAt:          st.UpdatedAt,
	}
}

func changedPaths(entries []task.FileEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	sort.Strings(paths)
	return paths
}

func (s *Service) reopenCodeApproval(id string, st task.State, reason string) (task.State, error) {
	updated, err := s.Store.Update(id, func(current *task.State) error {
		if current.Approval == nil || current.Approval.Kind != task.ApprovalKindCode {
			return task.ErrStaleOperation
		}
		archived := *current.Approval
		archived.HumanFeedback = reason
		now := s.now()
		archived.DecidedAt = &now
		current.ApprovalHistory = append(current.ApprovalHistory, archived)
		current.Approval = nil
		current.ApprovedManifestHash = ""
		current.Phase = task.PhaseImplementationReady
		current.ReviewProgress = nil
		return nil
	})
	if err != nil {
		return st, classify(id, err)
	}
	return updated, nil
}

func initialIntegrationEvidence(child task.State, candidate []task.FileEntry) bool {
	return child.CandidateManifestHash != "" && task.HashManifest(child.CandidateManifest) == child.CandidateManifestHash &&
		child.ApprovedManifestHash == child.CandidateManifestHash && strings.TrimSpace(child.CodeReviewerSessionID) != "" && child.CodeRound > 0 &&
		child.ReviewProgress != nil && child.ReviewProgress.Kind == "code" && child.ReviewProgress.Status == "approved" &&
		!task.ManifestChanged(child.CandidateManifest, candidate) && task.HashManifest(candidate) == child.CandidateManifestHash
}

func plannerApprovalFeedbackPrompt(feedback string) string {
	return "Continue the existing plan-review revision in this same planner session. Apply this concise human feedback and return only the planner JSON envelope.\nHuman feedback:\n" + feedback
}

func codeApprovalFeedbackPrompt(st task.State, feedback string, initialIntegration bool) string {
	prompt := "Address this concise human feedback in the current implementation and return only the implementer JSON envelope.\nHuman feedback:\n" + feedback
	if initialIntegration {
		prompt += "\nInitial integration context:\nScope: " + st.Scope + "\nWork graph:\n" + mustJSON(st.WorkUnits)
	}
	return prompt
}

func (s *Service) loadEffective(id string) (string, task.State, error) {
	requested, err := s.Store.Load(id)
	if err != nil {
		return id, task.State{}, classify(id, err)
	}
	if requested.ParentTaskID == "" {
		integrationID := task.IntegrationTaskID(id)
		if integration, integrationErr := s.Store.Load(integrationID); integrationErr == nil {
			return integrationID, integration, nil
		} else if !errors.Is(integrationErr, task.ErrNotFound) {
			return integrationID, task.State{}, classify(integrationID, integrationErr)
		}
	}
	return id, requested, nil
}

func (s *Service) repairApprovalArtifact(id string, st task.State) (task.State, error) {
	if st.Approval == nil || st.Approval.Status != "" {
		return st, nil
	}
	record := *st.Approval
	persisted, _, err := s.Store.PersistApprovalArtifact(st, record)
	if err != nil {
		return st, problem(ApprovalArtifactCode, "approval artifact cannot be materialized or repaired: "+err.Error(), id, ExitAction, true, err)
	}
	if persisted.ArtifactRef == record.ArtifactRef && persisted.ArtifactDigest == record.ArtifactDigest && persisted.Artifact != nil && record.Artifact != nil {
		return st, nil
	}
	updated, updateErr := s.Store.Update(id, func(current *task.State) error {
		if current.Approval == nil || current.Approval.Status != "" || current.Approval.GateID != record.GateID {
			return task.ErrStaleOperation
		}
		current.Approval = &persisted
		current.ApprovalGateSchemaVersion = task.ApprovalGateSchemaVersion
		return nil
	})
	if updateErr != nil {
		return st, classify(id, updateErr)
	}
	return updated, nil
}

func (s *Service) createPlanApprovalGate(id string, st task.State) (task.State, error) {
	if !planReviewerEvidenceSufficient(st) {
		return st, nil
	}
	record := planApprovalRecord(st, reviewerEvidence(st, st.ID, string(runner.RolePlanReviewer), "approved", planReviewFingerprint(st), st.PlanReviewerSessionID, reviewRound(st, "plan")))
	record.GateID = s.gateID(st, string(task.ApprovalKindPlan), record.SubjectFingerprint)
	record.CreatedAt = s.now()
	updated, err := s.Store.Update(id, func(current *task.State) error {
		if current.Approval != nil {
			if current.Approval.Status == "" {
				return nil
			}
			return task.ErrStaleOperation
		}
		if !planReviewerEvidenceSufficient(*current) {
			return task.ErrStaleOperation
		}
		record.SubjectFingerprint = planReviewFingerprint(*current)
		record.ReviewerEvidence.ReviewedFingerprint = record.SubjectFingerprint
		current.ApprovedPlanHash = ""
		current.Phase = task.PhaseAwaitingApproval
		current.Approval = &record
		current.ApprovalGateSchemaVersion = task.ApprovalGateSchemaVersion
		return nil
	})
	if err != nil {
		return st, classify(id, err)
	}
	return s.repairApprovalArtifact(id, updated)
}

func (s *Service) createCodeApprovalGate(id string, st task.State) (task.State, error) {
	if !codeReviewerEvidenceSufficient(st) {
		return st, nil
	}
	kind := string(task.ApprovalKindCode)
	record := codeApprovalRecord(st, reviewerEvidence(st, st.ID, string(runner.RoleCodeReviewer), "approved", st.CandidateManifestHash, st.CodeReviewerSessionID, reviewRound(st, kind)))
	record.GateID = s.gateID(st, kind, record.SubjectFingerprint)
	record.CreatedAt = s.now()
	updated, err := s.Store.Update(id, func(current *task.State) error {
		if current.Approval != nil {
			if current.Approval.Status == "" {
				return nil
			}
			return task.ErrStaleOperation
		}
		if !codeReviewerEvidenceSufficient(*current) {
			return task.ErrStaleOperation
		}
		record.SubjectFingerprint = current.CandidateManifestHash
		record.ReviewerEvidence.ReviewedFingerprint = record.SubjectFingerprint
		current.ApprovedManifestHash = ""
		current.Phase = task.PhaseAwaitingApproval
		current.Approval = &record
		current.ApprovalGateSchemaVersion = task.ApprovalGateSchemaVersion
		return nil
	})
	if err != nil {
		return st, classify(id, err)
	}
	return s.repairApprovalArtifact(id, updated)
}

// ensureLegacyBoundary upgrades actionable pre-approval task records without
// inventing reviewer evidence. It also repairs a pending artifact before a
// caller can expose or act on the gate.
func (s *Service) ensureLegacyBoundary(id string, st task.State) (task.State, error) {
	if st.Approval != nil && st.Approval.Status == "" {
		return s.repairApprovalArtifact(id, st)
	}
	if st.Phase == task.PhaseAwaitingApproval && st.Approval == nil {
		if st.IntegrationReview || st.CodeRound > 0 || st.Scope != "" {
			if codeReviewerEvidenceSufficient(st) {
				return s.createCodeApprovalGate(id, st)
			}
		} else if planReviewerEvidenceSufficient(st) {
			return s.createPlanApprovalGate(id, st)
		}
		return st, problem(ApprovalRecoveryCode, "the task is awaiting approval but its durable gate is missing reviewer evidence", id, ExitAction, false, nil)
	}
	if !isImplementationReadyPlan(st) && !humanPlanApproved(st) &&
		(st.Phase == task.PhasePlanApproved || st.Phase == task.PhasePlanned) && planReviewerEvidenceSufficient(st) {
		return s.createPlanApprovalGate(id, st)
	}
	if !isMaterializedWorkUnitChild(st) && !humanCodeApproved(st) && st.Phase == task.PhaseApproved && codeReviewerEvidenceSufficient(st) {
		return s.createCodeApprovalGate(id, st)
	}
	return st, nil
}

// Approval returns the durable host-facing approval control. It is deliberately
// provider-free; legacy gates are materialized from saved reviewer evidence,
// and artifacts are repaired from the deterministic state projection.
func (s *Service) Approval(id string) (Control, error) {
	targetID, st, err := s.loadEffective(id)
	if err != nil {
		return Control{}, err
	}
	st, err = s.ensureLegacyBoundary(targetID, st)
	if err != nil {
		control := ControlFor(st)
		if control.ApprovalTaskID == "" && targetID != id {
			control.ApprovalTaskID = targetID
		}
		return control, err
	}
	control := ControlFor(st)
	if targetID != id && control.ApprovalTaskID == "" && (st.Approval != nil || st.Phase == task.PhaseAwaitingApproval || st.Phase == task.PhaseApproved) {
		control.ApprovalTaskID = targetID
	}
	return control, nil
}

func normalizeApprovalDecision(value any) (task.ApprovalDecision, error) {
	switch decision := value.(type) {
	case task.ApprovalDecision:
		return decision, nil
	case string:
		return task.ApprovalDecision(strings.TrimSpace(decision)), nil
	default:
		return "", fmt.Errorf("approval decision must be approve, request_changes, or discuss")
	}
}

func validApprovalDecision(decision task.ApprovalDecision) bool {
	switch decision {
	case task.ApprovalDecisionApprove, task.ApprovalDecisionRequestChanges, task.ApprovalDecisionDiscuss:
		return true
	default:
		return false
	}
}

func approvalConflict(id, message string) error {
	return problem(ApprovalConflictCode, message, id, ExitAction, false, nil)
}

func approvalStale(id, message string) error {
	return problem(ApprovalStaleCode, message, id, ExitReviewNeeded, true, nil)
}

func approvalRecovery(id, message string) error {
	return problem(ApprovalRecoveryCode, message, id, ExitAction, false, nil)
}

func approvalHistoryRecord(st task.State, gateID string) (task.ApprovalRecord, bool) {
	for i := len(st.ApprovalHistory) - 1; i >= 0; i-- {
		if st.ApprovalHistory[i].GateID == gateID {
			return st.ApprovalHistory[i], true
		}
	}
	return task.ApprovalRecord{}, false
}

func (s *Service) approvalReplay(st task.State, record task.ApprovalRecord, decision task.ApprovalDecision, feedback string) (Result, error) {
	if record.Status != decision {
		return Result{}, approvalConflict(st.ID, "approval decision conflicts with the recorded decision for this gate")
	}
	if decision == task.ApprovalDecisionRequestChanges && strings.TrimSpace(record.HumanFeedback) != strings.TrimSpace(feedback) {
		return Result{}, approvalConflict(st.ID, "approval feedback conflicts with the recorded decision for this gate")
	}
	if st.Approval != nil && st.Approval.Status == "" {
		return approvalRequiredResult(st)
	}
	if st.InFlight != nil {
		return Result{State: st, Status: ControlFor(st).Status}, classify(st.ID, task.ErrOperationInFlight)
	}
	if st.ReviewProgress != nil && st.ReviewProgress.Status == "recovery" {
		return Result{State: st, Status: "recovery"}, approvalRecovery(st.ID, "the prior approval feedback needs durable session recovery")
	}
	if st.Phase == task.PhaseNeedsInput {
		return needsInputResult(st)
	}
	if isExhaustedState(st) {
		return exhaustedResult(st)
	}
	return Result{State: st, Status: ControlFor(st).Status}, nil
}

func (s *Service) liveCandidateMatches(st task.State, record task.ApprovalRecord) (bool, error) {
	if st.Scope == "" || record.SubjectFingerprint == "" || st.CandidateManifestHash != record.SubjectFingerprint {
		return false, nil
	}
	live, err := s.Observe(st.Scope)
	if err != nil {
		return false, err
	}
	return !task.ManifestChanged(st.CandidateManifest, live) && task.HashManifest(live) == record.SubjectFingerprint, nil
}

// RespondApproval applies one decision to exactly one pending gate. The
// decision is validated against the saved generation before the short state
// mutation; provider calls, when feedback is requested, happen only after the
// owned continuation has been persisted.
func (s *Service) RespondApproval(ctx context.Context, id, gateID string, decisionValue any, feedback string) (Result, error) {
	decision, err := normalizeApprovalDecision(decisionValue)
	if err != nil || !validApprovalDecision(decision) {
		return Result{}, problem("USAGE", "decision must be approve, request_changes, or discuss", id, ExitUsage, false, err)
	}
	feedback = strings.TrimSpace(feedback)
	if decision == task.ApprovalDecisionRequestChanges && feedback == "" {
		return Result{}, problem("USAGE", "request_changes requires nonblank feedback", id, ExitUsage, false, nil)
	}
	targetID, st, err := s.loadEffective(id)
	if err != nil {
		return Result{}, err
	}
	st, err = s.ensureLegacyBoundary(targetID, st)
	if err != nil {
		return Result{State: st, Status: ControlFor(st).Status}, err
	}
	if st.Approval == nil {
		if record, ok := approvalHistoryRecord(st, gateID); ok {
			return s.approvalReplay(st, record, decision, feedback)
		}
		return Result{State: st, Status: ControlFor(st).Status}, approvalConflict(targetID, "approval gate is no longer current")
	}
	active := *st.Approval
	if active.GateID != gateID {
		if record, ok := approvalHistoryRecord(st, gateID); ok {
			return s.approvalReplay(st, record, decision, feedback)
		}
		return Result{State: st, Status: ControlFor(st).Status}, approvalConflict(targetID, "approval gate is stale or belongs to another task generation")
	}
	if active.Status != "" {
		if active.Status == decision {
			return s.approvalReplay(st, active, decision, feedback)
		}
		return Result{State: st, Status: ControlFor(st).Status}, approvalConflict(targetID, "approval decision conflicts with the recorded decision for this gate")
	}
	if decision == task.ApprovalDecisionDiscuss {
		return approvalRequiredResult(st)
	}
	if active.Kind == task.ApprovalKindPlan {
		if active.SubjectFingerprint == "" || active.SubjectFingerprint != planReviewFingerprint(st) {
			return Result{State: st, Status: ControlFor(st).Status}, approvalStale(targetID, "the reviewed plan changed; run plan review before approving it")
		}
	} else if active.Kind == task.ApprovalKindCode {
		matches, liveErr := s.liveCandidateMatches(st, active)
		if liveErr != nil {
			return Result{State: st, Status: ControlFor(st).Status}, problem("APPROVAL_SCOPE", "cannot validate the live code candidate: "+liveErr.Error(), targetID, ExitAction, true, liveErr)
		}
		if !matches {
			return Result{State: st, Status: ControlFor(st).Status}, approvalStale(targetID, "the reviewed code candidate changed; run code review before approving it")
		}
	} else {
		return Result{State: st, Status: ControlFor(st).Status}, approvalConflict(targetID, "approval gate has an unknown kind")
	}

	var preAll []task.FileEntry
	if decision == task.ApprovalDecisionRequestChanges && active.Kind == task.ApprovalKindCode {
		preAll, err = s.Observe("**")
		if err != nil {
			return Result{State: st, Status: ControlFor(st).Status}, classify(targetID, err)
		}
	}
	feedbackToken := s.newToken()
	var runRole runner.Role
	var runLoop string
	var runToken string
	var exhausted bool
	var recovery bool
	updated, updateErr := s.Store.Update(targetID, func(current *task.State) error {
		if current.InFlight != nil {
			return task.ErrOperationInFlight
		}
		if current.Approval == nil || current.Approval.GateID != gateID || current.Approval.Status != "" {
			return task.ErrStaleOperation
		}
		if current.Approval.Kind != active.Kind || current.Approval.SubjectFingerprint != active.SubjectFingerprint {
			return task.ErrStaleOperation
		}
		now := s.now()
		if decision == task.ApprovalDecisionApprove {
			current.Approval.Status = task.ApprovalDecisionApprove
			current.Approval.DecidedAt = &now
			if active.Kind == task.ApprovalKindPlan {
				current.ApprovedPlanHash = current.PlanHash
				current.Phase = task.PhasePlanApproved
			} else {
				current.ApprovedManifestHash = current.CandidateManifestHash
				current.Phase = task.PhaseApproved
			}
			current.Retry = nil
			current.InFlight = nil
			return nil
		}

		archived := *current.Approval
		archived.Status = task.ApprovalDecisionRequestChanges
		archived.DecidedAt = &now
		archived.HumanFeedback = feedback
		current.ApprovalHistory = append(current.ApprovalHistory, archived)
		current.Approval = nil
		current.ApprovalGateSchemaVersion = task.ApprovalGateSchemaVersion
		current.Retry = nil
		current.PendingQuestion, current.PendingQuestionSource = "", ""
		current.PendingAnswer = ""
		limit := maxRounds(current)
		kind := string(active.Kind)
		if active.Kind == task.ApprovalKindPlan {
			current.ApprovedPlanHash = ""
			current.PlanReviewCheckpointHash = active.SubjectFingerprint
			current.Phase = task.PhasePlanned
			if limit > 0 && reviewRound(*current, kind) >= limit {
				exhausted = true
				current.Phase = task.PhaseFailed
				setReviewProgress(current, kind, "exhausted")
				return nil
			}
			if strings.TrimSpace(current.PlannerSessionID) == "" {
				recovery = true
				setReviewProgress(current, kind, "recovery")
				current.Diagnostics = append(current.Diagnostics, "human plan feedback could not resume the saved planner session")
				return nil
			}
			runRole, runLoop, runToken = runner.RolePlanner, "plan_review", feedbackToken
			archived.FeedbackOperationRef = feedbackToken
			current.ApprovalHistory[len(current.ApprovalHistory)-1] = archived
			prompt := plannerApprovalFeedbackPrompt(feedback)
			current.InFlight = s.ownedFlight(task.InFlight{Token: feedbackToken, Operation: "plan_revision", Role: string(runRole), StartedAt: now, KnownSession: true, SessionID: current.PlannerSessionID, PreviousPhase: task.PhasePlanned, Prompt: prompt, Findings: cloneFindings(current.Findings), Loop: runLoop})
			setReviewProgress(current, kind, "pending")
			return nil
		}

		current.ApprovedManifestHash = ""
		current.ReviewCheckpoint = cloneManifest(current.CandidateManifest)
		current.ReviewCheckpointHash = active.SubjectFingerprint
		current.ReviewCheckpointFindings = cloneFindings(current.Findings)
		current.Phase = task.PhaseImplementationReady
		if limit > 0 && reviewRound(*current, kind) >= limit {
			exhausted = true
			current.Phase = task.PhaseFailed
			setReviewProgress(current, kind, "exhausted")
			return nil
		}
		if !current.IntegrationReview && strings.TrimSpace(current.ImplementerSessionID) == "" {
			recovery = true
			setReviewProgress(current, kind, "recovery")
			current.Diagnostics = append(current.Diagnostics, "human code feedback could not resume the saved implementer session")
			return nil
		}
		runRole, runLoop, runToken = runner.RoleImplementer, "code_review", feedbackToken
		knownSession := strings.TrimSpace(current.ImplementerSessionID) != ""
		prompt := codeApprovalFeedbackPrompt(*current, feedback, !knownSession)
		archived.FeedbackOperationRef = feedbackToken
		current.ApprovalHistory[len(current.ApprovalHistory)-1] = archived
		current.InFlight = s.ownedFlight(task.InFlight{Token: feedbackToken, Operation: "code_revision", Role: string(runRole), StartedAt: now, KnownSession: knownSession, SessionID: current.ImplementerSessionID, SnapshotManifest: cloneManifest(preAll), PreviousPhase: task.PhaseImplementationReady, Prompt: prompt, Findings: cloneFindings(current.Findings), Scope: current.Scope, Loop: runLoop})
		setReviewProgress(current, kind, "pending")
		return nil
	})
	if updateErr != nil {
		return s.errorResult(targetID, classify(targetID, updateErr))
	}
	if exhausted {
		return Result{State: updated, Status: "exhausted"}, reviewExhaustedError(updated)
	}
	if recovery {
		return Result{State: updated, Status: "recovery"}, approvalRecovery(targetID, "human feedback was saved, but the required provider session is unavailable")
	}
	if decision == task.ApprovalDecisionApprove {
		return Result{State: updated, Status: "approved"}, nil
	}
	if runToken == "" {
		return Result{State: updated, Status: ControlFor(updated).Status}, nil
	}
	if runRole == runner.RolePlanner {
		return s.runPlanner(ctx, targetID, runToken, runLoop)
	}
	return s.runImplementer(ctx, targetID, runToken, runLoop)
}

func (s *Service) StartPlan(ctx context.Context, text, id string) (Result, error) {
	if strings.TrimSpace(text) == "" {
		return Result{}, problem("USAGE", "--task must not be empty", id, ExitUsage, false, nil)
	}
	profiles, runtimes, err := s.snapshots(ctx)
	if err != nil {
		var providerErr *runner.ProviderError
		if errors.As(err, &providerErr) {
			return Result{}, problem(providerErr.Code, providerErr.Message, id, ExitAction, providerErr.Retryable, err)
		}
		return Result{}, problem("CONFIGURATION", err.Error(), id, ExitUsage, false, err)
	}
	if id == "" {
		id = s.generatedID()
	}
	token := s.newToken()
	prompt := plannerPrompt(text, nil)
	now := s.now()
	limit := s.Config.EffectiveReviewMaxRounds()
	st := task.State{
		ID: id, RepoRoot: s.RepoRoot, Phase: task.PhasePlanned, Task: text, Prompt: prompt,
		ProfilesSnapshot: profiles, RuntimeSnapshot: runtimes, MaxRounds: limit,
		ReviewPolicy: &task.ReviewPolicy{MaxRounds: limit},
		UpdatedAt:    now, InFlight: s.ownedFlight(task.InFlight{Token: token, Operation: "plan_start", Role: string(runner.RolePlanner), StartedAt: now, PreviousPhase: task.PhasePlanned, Prompt: prompt, Loop: "plan_initial"}),
	}
	if err := s.Store.Create(st); err != nil {
		return Result{}, classify(id, err)
	}
	return s.runPlanner(ctx, id, token, "plan_initial")
}

// StartQuick creates a reviewed-code workflow without spending planner or
// plan-reviewer turns. The host orchestrator owns the execution packet for
// this deliberately small, exact-scope task.
func (s *Service) StartQuick(text, rawScope, id string) (Result, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Result{}, problem("USAGE", "--task must not be empty", id, ExitUsage, false, nil)
	}
	canonical, err := task.CanonicalScope(rawScope)
	if err != nil {
		return Result{}, problem("INVALID_SCOPE", err.Error(), id, ExitUsage, false, err)
	}
	if strings.TrimSpace(rawScope) == "" || canonical == "**" {
		return Result{}, problem("INVALID_SCOPE", "quick start requires an explicit narrow --scope", id, ExitUsage, false, nil)
	}
	profiles, runtimes, err := s.configuredSnapshots(runner.RoleImplementer, runner.RoleCodeReviewer)
	if err != nil {
		return Result{}, problem("CONFIGURATION", err.Error(), id, ExitUsage, false, err)
	}
	if id == "" {
		id = s.generatedID()
	}
	planHash := hash(text)
	limit := s.Config.EffectiveReviewMaxRounds()
	st := task.State{
		ID: id, RepoRoot: s.RepoRoot, Phase: task.PhasePlanApproved,
		Task: text, Plan: text, PlanHash: planHash, ApprovedPlanHash: planHash,
		PlannedScope: canonical, Complexity: task.ComplexityTrivial, DirectImplementation: true,
		ProfilesSnapshot: profiles, RuntimeSnapshot: runtimes, MaxRounds: limit,
		ReviewPolicy: &task.ReviewPolicy{MaxRounds: limit}, UpdatedAt: s.now(),
	}
	if err := s.Store.Create(st); err != nil {
		return Result{}, classify(id, err)
	}
	if err := s.WritePlan(id, st.Plan); err != nil {
		return Result{State: st}, problem("PLAN_WRITE", err.Error(), id, ExitAction, false, err)
	}
	return Result{State: st, Status: "ready"}, nil
}

func (s *Service) AnswerPlan(ctx context.Context, id, answer string) (Result, error) {
	if strings.TrimSpace(answer) == "" {
		return Result{}, problem("USAGE", "--answer must not be empty", id, ExitUsage, false, nil)
	}
	targetID, current, err := s.loadEffective(id)
	if err != nil {
		return Result{}, err
	}
	id = targetID
	current, err = s.ensureLegacyBoundary(id, current)
	if err != nil {
		return Result{State: current, Status: ControlFor(current).Status}, err
	}
	if current.InFlight != nil {
		return Result{State: current, Status: ControlFor(current).Status}, classify(id, task.ErrOperationInFlight)
	}
	if current.Approval != nil && current.Approval.Status == "" {
		return approvalRequiredResult(current)
	}
	var loop string
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight != nil {
			return task.ErrOperationInFlight
		}
		if st.Phase != task.PhaseNeedsInput || st.PendingQuestionSource != string(runner.RolePlanner) {
			return task.ErrInvalidPhase
		}
		loop = st.InterruptedLoop
		if loop == "" {
			loop = "plan_initial"
		}
		st.PromptInputs = append(st.PromptInputs, "Question: "+st.PendingQuestion, "Answer: "+answer)
		st.PendingAnswer = answer
		st.Phase = st.ReturnPhase
		if st.Phase == "" {
			st.Phase = task.PhasePlanned
		}
		if loop == "plan_review" {
			setReviewProgress(st, "plan", "pending")
		} else {
			st.ReviewProgress = nil
		}
		token := s.newToken()
		prompt := plannerAnswerPrompt(st.PendingQuestion, answer, loop == "plan_review", st.Findings)
		st.InFlight = s.ownedFlight(task.InFlight{Token: token, Operation: "plan_answer", Role: string(runner.RolePlanner), StartedAt: s.now(), KnownSession: st.PlannerSessionID != "", SessionID: st.PlannerSessionID, PreviousPhase: st.Phase, Prompt: prompt, Findings: cloneFindings(st.Findings), Loop: loop})
		st.PendingQuestion, st.PendingQuestionSource = "", ""
		return nil
	})
	if err != nil {
		return Result{}, classify(id, err)
	}
	return s.runPlanner(ctx, id, st.InFlight.Token, loop)
}

func (s *Service) ReviewPlan(ctx context.Context, id string) (Result, error) {
	targetID, current, err := s.loadEffective(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	id = targetID
	current, err = s.ensureLegacyBoundary(id, current)
	if err != nil {
		return Result{State: current, Status: ControlFor(current).Status}, err
	}
	if current.InFlight != nil {
		return Result{State: current, Status: ControlFor(current).Status}, classify(id, task.ErrOperationInFlight)
	}
	if current.Approval != nil && current.Approval.Status == "" {
		return approvalRequiredResult(current)
	}
	if isExhaustedState(current) {
		return exhaustedResult(current)
	}
	if current.Phase == task.PhaseNeedsInput {
		return needsInputResult(current)
	}
	if current.Phase == task.PhaseFailed || current.Phase == task.PhaseReviewNeeded {
		return existingWorkflowResult(current)
	}
	if current.Phase == task.PhasePlanApproved {
		if isImplementationReadyPlan(current) || humanPlanApproved(current) {
			return Result{State: current, Status: ControlFor(current).Status}, nil
		}
		if current.Scope != "" || current.CandidateManifestHash != "" {
			return Result{}, classify(id, task.ErrInvalidPhase)
		}
		_, err = s.Store.Update(id, func(st *task.State) error {
			if st.InFlight != nil || st.Approval != nil {
				return task.ErrOperationInFlight
			}
			st.Phase = task.PhasePlanned
			st.ApprovedPlanHash = ""
			st.ReviewProgress = nil
			return nil
		})
		if err != nil {
			return s.errorResult(id, classify(id, err))
		}
		current, err = s.Store.Load(id)
		if err != nil {
			return Result{}, classify(id, err)
		}
	}
	if current.Phase != task.PhasePlanned || strings.TrimSpace(current.Plan) == "" || current.PlanHash != hash(current.Plan) {
		return Result{}, classify(id, task.ErrInvalidPhase)
	}
	var exhausted bool
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight != nil {
			return task.ErrOperationInFlight
		}
		if st.Phase != task.PhasePlanned || strings.TrimSpace(st.Plan) == "" || st.PlanHash != hash(st.Plan) {
			return task.ErrInvalidPhase
		}
		if limit := maxRounds(st); limit > 0 && reviewRound(*st, "plan") >= limit {
			exhausted = true
			st.Phase = task.PhaseFailed
			st.InFlight, st.Retry = nil, nil
			setReviewProgress(st, "plan", "exhausted")
			return nil
		}
		token := s.newToken()
		prompt := planReviewPrompt(st.Task, st.Plan, st.Complexity, st.WorkUnits, st.PlanReviewerSessionID != "")
		st.Phase = task.PhasePlanReviewing
		setReviewProgress(st, "plan", "pending")
		st.InFlight = s.ownedFlight(task.InFlight{Token: token, Operation: "plan_review", Role: string(runner.RolePlanReviewer), StartedAt: s.now(), KnownSession: st.PlanReviewerSessionID != "", SessionID: st.PlanReviewerSessionID, PreviousPhase: task.PhasePlanned, Prompt: prompt, Loop: "plan_review"})
		return nil
	})
	if err != nil {
		return Result{State: current, Status: ControlFor(current).Status}, classify(id, err)
	}
	if exhausted {
		return exhaustedResult(st)
	}
	return s.runPlanReviewer(ctx, id, st.InFlight.Token)
}

func (s *Service) Implement(ctx context.Context, id, rawScope string) (Result, error) {
	targetID, current, err := s.loadEffective(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	id = targetID
	current, err = s.ensureLegacyBoundary(id, current)
	if err != nil {
		return Result{State: current, Status: ControlFor(current).Status}, err
	}
	if current.InFlight != nil {
		return Result{State: current, Status: ControlFor(current).Status}, classify(id, task.ErrOperationInFlight)
	}
	if current.Approval != nil && current.Approval.Status == "" {
		return approvalRequiredResult(current)
	}
	if current.Phase != task.PhasePlanApproved || !humanPlanApproved(current) {
		return Result{State: current, Status: ControlFor(current).Status}, classify(id, task.ErrInvalidPhase)
	}
	if current.ParentTaskID == "" && current.WorkGraph {
		return Result{}, problem("WORK_GRAPH_REQUIRED", "this plan uses work units; use rolemux work start <task-id> <unit-id>", id, ExitUsage, false, nil)
	}
	if strings.TrimSpace(rawScope) == "" {
		if current.PlannedScope != "" {
			rawScope = current.PlannedScope
		} else if len(current.WorkUnits) == 1 {
			rawScope = current.WorkUnits[0].Scope
		}
	}
	canonical, err := task.CanonicalScope(rawScope)
	if err != nil {
		return Result{}, problem("INVALID_SCOPE", err.Error(), id, ExitUsage, false, err)
	}
	if current.Scope != "" && current.Scope != canonical {
		return Result{}, problem("SCOPE_IMMUTABLE", "task scope cannot be changed after implementation starts", id, ExitUsage, false, nil)
	}
	baseline, err := s.Observe(canonical)
	if err != nil {
		return Result{}, classify(id, err)
	}
	if current.Scope == "" {
		baseline, err = s.Capture(baseline, "baseline", id)
		if err != nil {
			return Result{}, classify(id, err)
		}
	}
	preAll, err := s.Observe("**")
	if err != nil {
		return Result{}, classify(id, err)
	}
	advisories := s.scopeAdvisories(id, canonical, baseline)
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight != nil {
			return task.ErrOperationInFlight
		}
		if st.Phase != task.PhasePlanApproved || !humanPlanApproved(*st) {
			return task.ErrInvalidPhase
		}
		if st.Scope != "" && st.Scope != canonical {
			return task.ErrInvalidPhase
		}
		if st.Scope == "" {
			st.Scope = canonical
			st.ScopeSpecHash = task.ScopeSpecHash(canonical)
			st.ScopedBaseline = cloneManifest(baseline)
			st.ScopedBaselineHash = task.HashManifest(baseline)
		}
		st.Advisories = mergeDiagnostics(st.Advisories, advisories)
		token := s.newToken()
		prompt := implementPrompt(st.Task, st.Plan, canonical, nil)
		st.Phase = task.PhaseImplementing
		st.ReviewProgress = nil
		st.PlanReviewCheckpointHash = ""
		st.InFlight = s.ownedFlight(task.InFlight{Token: token, Operation: "implement", Role: string(runner.RoleImplementer), StartedAt: s.now(), KnownSession: st.ImplementerSessionID != "", SessionID: st.ImplementerSessionID, SnapshotManifest: cloneManifest(preAll), PreviousPhase: task.PhasePlanApproved, Prompt: prompt, Scope: canonical, Loop: "implement"})
		return nil
	})
	if err != nil {
		return Result{}, classify(id, err)
	}
	return s.runImplementer(ctx, id, st.InFlight.Token, "implement")
}

func (s *Service) AnswerImplement(ctx context.Context, id, answer string) (Result, error) {
	if strings.TrimSpace(answer) == "" {
		return Result{}, problem("USAGE", "--answer must not be empty", id, ExitUsage, false, nil)
	}
	targetID, current, err := s.loadEffective(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	id = targetID
	current, err = s.ensureLegacyBoundary(id, current)
	if err != nil {
		return Result{State: current, Status: ControlFor(current).Status}, err
	}
	if current.InFlight != nil {
		return Result{State: current, Status: ControlFor(current).Status}, classify(id, task.ErrOperationInFlight)
	}
	if current.Approval != nil && current.Approval.Status == "" {
		return approvalRequiredResult(current)
	}
	preAll, err := s.Observe("**")
	if err != nil {
		return Result{}, classify(id, err)
	}
	var loop string
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight != nil {
			return task.ErrOperationInFlight
		}
		if st.Phase != task.PhaseNeedsInput || st.PendingQuestionSource != string(runner.RoleImplementer) {
			return task.ErrInvalidPhase
		}
		loop = st.InterruptedLoop
		if loop == "" {
			loop = "implement"
		}
		st.PromptInputs = append(st.PromptInputs, "Question: "+st.PendingQuestion, "Answer: "+answer)
		st.PendingAnswer = answer
		if loop == "code_review" {
			setReviewProgress(st, reviewKind(*st), "pending")
		} else {
			st.ReviewProgress = nil
		}
		token := s.newToken()
		prompt := implementAnswerPrompt(st.PendingQuestion, answer, st.Findings)
		st.Phase = task.PhaseImplementing
		st.InFlight = s.ownedFlight(task.InFlight{Token: token, Operation: "implement_answer", Role: string(runner.RoleImplementer), StartedAt: s.now(), KnownSession: st.ImplementerSessionID != "", SessionID: st.ImplementerSessionID, SnapshotManifest: cloneManifest(preAll), PreviousPhase: st.ReturnPhase, Prompt: prompt, Findings: cloneFindings(st.Findings), Scope: st.Scope, Loop: loop})
		st.PendingQuestion, st.PendingQuestionSource = "", ""
		return nil
	})
	if err != nil {
		return Result{}, classify(id, err)
	}
	return s.runImplementer(ctx, id, st.InFlight.Token, loop)
}

func (s *Service) ReviewCode(ctx context.Context, id string) (Result, error) {
	targetID, current, err := s.loadEffective(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	id = targetID
	current, err = s.ensureLegacyBoundary(id, current)
	if err != nil {
		return Result{State: current, Status: ControlFor(current).Status}, err
	}
	if current.Approval != nil && current.Approval.Status == "" {
		if current.Approval.Kind != task.ApprovalKindCode {
			return approvalRequiredResult(current)
		}
		matches, liveErr := s.liveCandidateMatches(current, *current.Approval)
		if liveErr != nil {
			return Result{State: current, Status: ControlFor(current).Status}, problem("APPROVAL_SCOPE", "cannot validate the live code candidate: "+liveErr.Error(), id, ExitAction, true, liveErr)
		}
		if !matches {
			current, err = s.reopenCodeApproval(id, current, "the code candidate changed after the approval request")
			if err != nil {
				return Result{State: current, Status: ControlFor(current).Status}, err
			}
		} else {
			return approvalRequiredResult(current)
		}
	}
	if current.InFlight != nil {
		return Result{State: current, Status: ControlFor(current).Status}, classify(id, task.ErrOperationInFlight)
	}
	if isExhaustedState(current) {
		return exhaustedResult(current)
	}
	if current.Phase == task.PhaseNeedsInput {
		return needsInputResult(current)
	}
	if current.Phase == task.PhaseReviewNeeded || current.Phase == task.PhaseFailed {
		return existingWorkflowResult(current)
	}
	if current.Phase == task.PhaseApproved {
		if isMaterializedWorkUnitChild(current) {
			return existingWorkflowResult(current)
		}
		if humanCodeApproved(current) {
			matches, liveErr := s.liveCandidateMatches(current, *current.Approval)
			if liveErr != nil {
				return Result{State: current, Status: ControlFor(current).Status}, problem("APPROVAL_SCOPE", "cannot validate the live code candidate: "+liveErr.Error(), id, ExitAction, true, liveErr)
			}
			if matches {
				return Result{State: current, Status: "approved"}, nil
			}
			current, err = s.reopenCodeApproval(id, current, "the human-approved code candidate changed")
			if err != nil {
				return Result{State: current, Status: ControlFor(current).Status}, err
			}
		} else {
			updated, updateErr := s.Store.Update(id, func(st *task.State) error {
				if st.InFlight != nil || st.Approval != nil {
					return task.ErrOperationInFlight
				}
				st.ApprovedManifestHash = ""
				st.Phase = task.PhaseImplementationReady
				st.ReviewProgress = nil
				return nil
			})
			if updateErr != nil {
				return s.errorResult(id, classify(id, updateErr))
			}
			current = updated
		}
	}
	if current.Phase != task.PhaseImplementationReady || current.Scope == "" {
		return Result{}, classify(id, task.ErrInvalidPhase)
	}
	before, err := s.Observe(current.Scope)
	if err != nil {
		return Result{}, classify(id, err)
	}
	candidate := cloneManifest(current.CandidateManifest)
	if task.ManifestChanged(candidate, before) {
		label := "candidate-review-start-" + shortToken(s.newToken()) + "-" + fmt.Sprint(current.CodeRound)
		candidate, err = s.Capture(before, label, id)
		if err != nil {
			return Result{}, classify(id, err)
		}
	}
	st, err := s.beginCodeReview(id, before, candidate, "code_review", "")
	if err != nil {
		if st.ID != "" {
			return Result{State: st, Status: ControlFor(st).Status}, err
		}
		return Result{State: current, Status: ControlFor(current).Status}, err
	}
	return s.runCodeReviewer(ctx, id, st.InFlight.Token)
}

func (s *Service) Retry(ctx context.Context, id string) (Result, error) {
	targetID, current, err := s.loadEffective(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	id = targetID
	current, err = s.ensureLegacyBoundary(id, current)
	if err != nil {
		return Result{State: current, Status: ControlFor(current).Status}, err
	}
	if current.Approval != nil && current.Approval.Status == "" {
		return approvalRequiredResult(current)
	}
	if isExhaustedState(current) {
		return exhaustedResult(current)
	}
	if current.InFlight != nil {
		current, err = s.recoverAbandoned(id, current.InFlight.Token)
		if err != nil {
			return Result{State: current, Status: ControlFor(current).Status}, err
		}
	}
	if current.Retry == nil || (current.Phase != task.PhaseFailed && current.Phase != task.PhaseReviewNeeded) {
		if current.Phase == task.PhaseFailed {
			return existingWorkflowResult(current)
		}
		return Result{}, classify(id, task.ErrInvalidPhase)
	}
	retry := *current.Retry
	reviewCandidateChanged := current.Phase == task.PhaseReviewNeeded && retry.Role == string(runner.RoleCodeReviewer)
	candidateChanged := false
	var barrier []task.FileEntry
	var refreshedCandidate []task.FileEntry
	if retry.Role == string(runner.RoleCodeReviewer) {
		barrier, err = s.Observe(current.Scope)
		if err != nil {
			return Result{}, classify(id, err)
		}
		refreshedCandidate = cloneManifest(current.CandidateManifest)
		if task.ManifestChanged(refreshedCandidate, barrier) {
			candidateChanged = true
			label := "candidate-review-retry-" + shortToken(s.newToken()) + "-" + fmt.Sprint(current.CodeRound)
			refreshedCandidate, err = s.Capture(barrier, label, id)
			if err != nil {
				return Result{}, classify(id, err)
			}
		}
	} else {
		barrier = cloneManifest(retry.SnapshotManifest)
	}
	var exhausted bool
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight != nil {
			return task.ErrOperationInFlight
		}
		if st.Retry == nil || !sameRetry(*st.Retry, retry) {
			return task.ErrStaleOperation
		}
		if isReviewerRole(retry.Role) {
			kind := reviewKind(*st)
			if limit := maxRounds(st); limit > 0 && reviewRound(*st, kind) >= limit {
				exhausted = true
				st.Phase = task.PhaseFailed
				st.InFlight, st.Retry = nil, nil
				setReviewProgress(st, kind, "exhausted")
				return nil
			}
		}
		token := s.newToken()
		if retry.Role == string(runner.RoleCodeReviewer) {
			st.CandidateManifest = cloneManifest(refreshedCandidate)
			st.CandidateManifestHash = task.HashManifest(refreshedCandidate)
			st.ChangeManifest = changeEntries(st.ScopedBaseline, refreshedCandidate)
			if retry.KnownSession && !candidateChanged && !reviewCandidateChanged {
				retry.Prompt = codeReviewContinuePrompt(*st)
			} else {
				retry.Prompt = codeReviewPrompt(*st, st.CodeReviewerSessionID != "")
			}
			if reviewCandidateChanged || candidateChanged {
				retry.Prompt = "The scoped candidate changed while your previous review was running. Discard that verdict and review this current candidate.\n" + retry.Prompt
			}
		}
		st.Phase = retryPhase(retry)
		if retry.Role == string(runner.RolePlanReviewer) || retry.Role == string(runner.RoleCodeReviewer) || retry.Loop == "plan_review" || retry.Loop == "code_review" {
			setReviewProgress(st, reviewKind(*st), "pending")
		} else {
			st.ReviewProgress = nil
		}
		st.InFlight = s.ownedFlight(task.InFlight{Token: token, Operation: retry.Operation, Role: retry.Role, StartedAt: s.now(), KnownSession: retry.KnownSession, SessionID: retry.SessionID, SnapshotManifest: cloneManifest(barrier), PreviousPhase: retry.PreviousPhase, Prompt: retry.Prompt, Findings: cloneFindings(retry.Findings), Scope: retry.Scope, Loop: retry.Loop})
		st.Retry = nil
		return nil
	})
	if err != nil {
		return s.errorResult(id, classify(id, err))
	}
	if exhausted {
		return exhaustedResult(st)
	}
	switch runner.Role(retry.Role) {
	case runner.RolePlanner:
		return s.runPlanner(ctx, id, st.InFlight.Token, retry.Loop)
	case runner.RolePlanReviewer:
		return s.runPlanReviewer(ctx, id, st.InFlight.Token)
	case runner.RoleImplementer:
		return s.runImplementer(ctx, id, st.InFlight.Token, retry.Loop)
	case runner.RoleCodeReviewer:
		return s.runCodeReviewer(ctx, id, st.InFlight.Token)
	default:
		return Result{}, problem("INVALID_RETRY", "saved retry has an unknown role", id, ExitUsage, false, nil)
	}
}

func (s *Service) Status(id string) (task.State, error) {
	targetID, st, err := s.loadEffective(id)
	if err != nil {
		return task.State{}, err
	}
	st, err = s.ensureLegacyBoundary(targetID, st)
	if targetID != id {
		st.ID = id
	}
	return st, classify(id, err)
}

func (s *Service) List() ([]task.State, error) { return s.Store.List() }

type WorkNode struct {
	task.WorkUnit
	TaskID    string   `json:"task_id"`
	Status    string   `json:"status"`
	Ready     bool     `json:"ready"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

type WorkGraph struct {
	TaskID     string     `json:"task_id"`
	Phase      string     `json:"phase"`
	Complexity string     `json:"complexity,omitempty"`
	Waves      [][]string `json:"waves"`
	Ready      []string   `json:"ready"`
	Nodes      []WorkNode `json:"nodes"`
	Control    Control    `json:"control"`
}

// Graph returns live scheduling state for a planner-produced DAG. It never
// starts workers; the host orchestrator remains responsible for concurrency.
func (s *Service) Graph(id string) (WorkGraph, error) {
	parent, err := s.Store.Load(id)
	if err != nil {
		return WorkGraph{}, classify(id, err)
	}
	parent, err = s.ensureLegacyBoundary(id, parent)
	if err != nil {
		return WorkGraph{TaskID: id, Phase: parent.Phase, Complexity: parent.Complexity, Control: ControlFor(parent)}, err
	}
	effective := parent
	effectiveID := id
	if integration, integrationErr := s.Store.Load(task.IntegrationTaskID(id)); integrationErr == nil {
		effectiveID = integration.ID
		effective, err = s.ensureLegacyBoundary(effectiveID, integration)
		if err != nil {
			return WorkGraph{TaskID: id, Phase: effective.Phase, Complexity: effective.Complexity, Control: ControlFor(effective)}, err
		}
	} else if !errors.Is(integrationErr, task.ErrNotFound) {
		return WorkGraph{}, classify(task.IntegrationTaskID(id), integrationErr)
	}
	units, err := task.NormalizeWorkUnits(parent.WorkUnits, parent.Plan)
	if err != nil {
		return WorkGraph{}, problem("INVALID_WORK_GRAPH", err.Error(), id, ExitUsage, false, err)
	}
	waves, err := task.WorkUnitWaves(units)
	if err != nil {
		return WorkGraph{}, problem("INVALID_WORK_GRAPH", err.Error(), id, ExitUsage, false, err)
	}
	approved := map[string]bool{}
	states := map[string]task.State{}
	planApproved := humanPlanApproved(parent)
	for _, unit := range units {
		childID := task.WorkTaskID(parent.ID, unit.ID)
		child, loadErr := s.Store.Load(childID)
		if loadErr == nil {
			states[unit.ID] = child
			approved[unit.ID] = child.Phase == task.PhaseApproved
		} else if !errors.Is(loadErr, task.ErrNotFound) {
			return WorkGraph{}, classify(childID, loadErr)
		}
	}
	graph := WorkGraph{TaskID: parent.ID, Phase: effective.Phase, Complexity: parent.Complexity, Waves: waves, Ready: []string{}, Nodes: make([]WorkNode, 0, len(units)), Control: ControlFor(effective)}
	if effectiveID != id && graph.Control.ApprovalTaskID == "" {
		graph.Control.ApprovalTaskID = effectiveID
	}
	for _, unit := range units {
		node := WorkNode{WorkUnit: unit, TaskID: task.WorkTaskID(parent.ID, unit.ID), Status: "not_started"}
		for _, dependency := range unit.DependsOn {
			if !approved[dependency] {
				node.BlockedBy = append(node.BlockedBy, dependency)
			}
		}
		if child, exists := states[unit.ID]; exists {
			node.Status = child.Phase
		} else if planApproved && len(node.BlockedBy) == 0 {
			node.Ready = true
			node.Status = "ready"
			graph.Ready = append(graph.Ready, unit.ID)
		} else if !planApproved {
			node.BlockedBy = append(node.BlockedBy, "plan_approval")
			node.Status = "blocked"
		} else {
			node.Status = "blocked"
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	sort.Strings(graph.Ready)
	return graph, nil
}

// StartWork materializes one ready DAG node as an independently resumable
// task. Independent nodes therefore have independent provider sessions and
// task locks while sharing the same checkout.
func (s *Service) StartWork(parentID, unitID string) (Result, error) {
	graph, err := s.Graph(parentID)
	if err != nil {
		parent, loadErr := s.Status(parentID)
		if loadErr == nil {
			return Result{State: parent, Status: ControlFor(parent).Status}, err
		}
		return Result{}, err
	}
	if graph.Control.Status == "approval_required" {
		parent, loadErr := s.Status(parentID)
		if loadErr != nil {
			return Result{}, classify(parentID, loadErr)
		}
		return approvalRequiredResult(parent)
	}
	if graph.Control.Status == "recovery" {
		parent, loadErr := s.Status(parentID)
		if loadErr != nil {
			return Result{}, classify(parentID, loadErr)
		}
		return Result{State: parent, Status: "recovery"}, approvalRecovery(parentID, "the parent workflow requires recovery before work can start")
	}
	parent, err := s.Store.Load(parentID)
	if err != nil {
		return Result{}, classify(parentID, err)
	}
	parent, err = s.ensureLegacyBoundary(parentID, parent)
	if err != nil {
		return Result{State: parent, Status: ControlFor(parent).Status}, err
	}
	if parent.Approval != nil && parent.Approval.Status == "" {
		return approvalRequiredResult(parent)
	}
	if !humanPlanApproved(parent) {
		return Result{State: parent, Status: ControlFor(parent).Status}, problem("WORK_GRAPH_REQUIRED", "the parent plan has not received human approval", parentID, ExitUsage, false, nil)
	}
	var selected *WorkNode
	for index := range graph.Nodes {
		if graph.Nodes[index].ID == unitID {
			selected = &graph.Nodes[index]
			break
		}
	}
	if selected == nil {
		return Result{}, problem("UNKNOWN_WORK_UNIT", "plan has no work unit "+unitID, parentID, ExitUsage, false, nil)
	}
	if selected.Status != "ready" {
		if existing, loadErr := s.Store.Load(selected.TaskID); loadErr == nil && existing.ParentTaskID == parentID && existing.WorkUnitID == unitID {
			return Result{State: existing, Status: existing.Phase}, nil
		}
		return Result{}, problem("WORK_UNIT_BLOCKED", "work unit "+unitID+" is blocked by "+strings.Join(selected.BlockedBy, ", "), parentID, ExitUsage, false, nil)
	}
	planHash := hash(selected.ExecutionPacket)
	policy := resolvedReviewPolicy(&parent)
	child := task.State{
		ID: selected.TaskID, RepoRoot: s.RepoRoot, Phase: task.PhasePlanApproved,
		Task: selected.Objective, Plan: selected.ExecutionPacket, PlanHash: planHash, ApprovedPlanHash: planHash,
		ParentTaskID: parentID, WorkUnitID: unitID, PlannedScope: selected.Scope,
		Complexity:       parent.Complexity,
		ProfilesSnapshot: parent.ProfilesSnapshot, RuntimeSnapshot: parent.RuntimeSnapshot,
		MaxRounds: policy.MaxRounds, ReviewPolicy: &policy, UpdatedAt: s.now(),
	}
	if err := s.Store.Create(child); err != nil {
		if errors.Is(err, task.ErrTaskExists) {
			existing, loadErr := s.Store.Load(child.ID)
			if loadErr == nil && existing.ParentTaskID == parentID && existing.WorkUnitID == unitID {
				return Result{State: existing, Status: existing.Phase}, nil
			}
		}
		return Result{}, classify(child.ID, err)
	}
	if err := s.WritePlan(child.ID, child.Plan); err != nil {
		return Result{State: child}, problem("PLAN_WRITE", err.Error(), child.ID, ExitAction, false, err)
	}
	return Result{State: child, Status: "ready"}, nil
}

// ReviewIntegration performs the one-time deep review after every DAG node
// has passed its task-local review. Findings are handed to one fresh
// integration implementer session, while subsequent fix/review rounds resume
// the same integration implementer and reviewer sessions.
func (s *Service) ReviewIntegration(ctx context.Context, parentID string) (Result, error) {
	integrationID := task.IntegrationTaskID(parentID)
	if existing, err := s.Store.Load(integrationID); err == nil {
		existing, prepareErr := s.ensureLegacyBoundary(integrationID, existing)
		if prepareErr != nil {
			return Result{State: existing, Status: ControlFor(existing).Status}, prepareErr
		}
		if existing.Approval != nil && existing.Approval.Status == "" {
			if existing.Approval.Kind == task.ApprovalKindCode {
				matches, liveErr := s.liveCandidateMatches(existing, *existing.Approval)
				if liveErr != nil {
					return Result{State: existing, Status: ControlFor(existing).Status}, problem("APPROVAL_SCOPE", "cannot validate the live code candidate: "+liveErr.Error(), integrationID, ExitAction, true, liveErr)
				}
				if !matches {
					return s.ReviewCode(ctx, integrationID)
				}
			}
			return approvalRequiredResult(existing)
		}
		switch existing.Phase {
		case task.PhaseApproved, task.PhaseImplementationReady:
			return s.ReviewCode(ctx, integrationID)
		default:
			return existingWorkflowResult(existing)
		}
	} else if !errors.Is(err, task.ErrNotFound) {
		return Result{}, classify(integrationID, err)
	}
	parent, err := s.Store.Load(parentID)
	if err != nil {
		return Result{}, classify(parentID, err)
	}
	parent, err = s.ensureLegacyBoundary(parentID, parent)
	if err != nil {
		return Result{State: parent, Status: ControlFor(parent).Status}, err
	}
	if parent.Approval != nil && parent.Approval.Status == "" {
		return approvalRequiredResult(parent)
	}
	if !humanPlanApproved(parent) {
		return Result{State: parent, Status: ControlFor(parent).Status}, problem("WORK_GRAPH_REQUIRED", "the parent plan has not received human approval", parentID, ExitUsage, false, nil)
	}
	graph, err := s.Graph(parentID)
	if err != nil {
		return Result{}, err
	}
	for _, node := range graph.Nodes {
		if node.Status != task.PhaseApproved {
			return Result{}, problem("WORK_GRAPH_INCOMPLETE", "all work units must pass task review before integration review", parentID, ExitUsage, false, nil)
		}
	}
	parent, err = s.Store.Load(parentID)
	if err != nil {
		return Result{}, classify(parentID, err)
	}
	parent, err = s.ensureLegacyBoundary(parentID, parent)
	if err != nil {
		return Result{State: parent, Status: ControlFor(parent).Status}, err
	}
	if parent.Approval != nil && parent.Approval.Status == "" {
		return approvalRequiredResult(parent)
	}
	if !humanPlanApproved(parent) {
		return Result{State: parent, Status: ControlFor(parent).Status}, problem("WORK_GRAPH_REQUIRED", "the parent plan has not received human approval", parentID, ExitUsage, false, nil)
	}
	var scopeParts []string
	workUnits := make([]task.WorkUnit, 0, len(graph.Nodes))
	children := make(map[string]task.State, len(graph.Nodes))
	for _, node := range graph.Nodes {
		workUnits = append(workUnits, node.WorkUnit)
		scopeParts = append(scopeParts, task.ScopePatterns(node.Scope)...)
	}
	baselineByPath := map[string]task.FileEntry{}
	for _, wave := range graph.Waves {
		for _, unitID := range wave {
			child, loadErr := s.Store.Load(task.WorkTaskID(parentID, unitID))
			if loadErr != nil {
				return Result{}, classify(parentID, loadErr)
			}
			children[unitID] = child
			for _, entry := range child.ScopedBaseline {
				if _, exists := baselineByPath[entry.Path]; !exists {
					baselineByPath[entry.Path] = entry
				}
			}
			for _, advisory := range child.Advisories {
				if advisory.Code == "OUT_OF_SCOPE_CHANGE" {
					scopeParts = append(scopeParts, advisory.Paths...)
				}
			}
		}
	}
	scope, err := task.CanonicalScope(strings.Join(scopeParts, ","))
	if err != nil {
		return Result{}, problem("INVALID_SCOPE", err.Error(), integrationID, ExitUsage, false, err)
	}
	baseline := make([]task.FileEntry, 0, len(baselineByPath))
	for _, entry := range baselineByPath {
		baseline = append(baseline, entry)
	}
	sort.Slice(baseline, func(i, j int) bool { return baseline[i].Path < baseline[j].Path })
	candidate, err := s.Observe(scope)
	if err != nil {
		return Result{}, classify(integrationID, err)
	}
	candidate, err = s.Capture(candidate, "integration-candidate", integrationID)
	if err != nil {
		return Result{}, classify(integrationID, err)
	}
	planHash := hash(parent.Plan)
	policy := resolvedReviewPolicy(&parent)
	integration := task.State{
		ID: integrationID, RepoRoot: s.RepoRoot, Phase: task.PhaseImplementationReady,
		Task: parent.Task, Plan: parent.Plan, PlanHash: planHash, ApprovedPlanHash: planHash,
		ParentTaskID: parentID, IntegrationReview: true, WorkUnits: workUnits,
		Complexity: parent.Complexity,
		Scope:      scope, ScopeSpecHash: task.ScopeSpecHash(scope), ScopedBaseline: baseline, ScopedBaselineHash: task.HashManifest(baseline),
		CandidateManifest: candidate, CandidateManifestHash: task.HashManifest(candidate), ChangeManifest: changeEntries(baseline, candidate),
		ProfilesSnapshot: parent.ProfilesSnapshot, RuntimeSnapshot: parent.RuntimeSnapshot,
		MaxRounds: policy.MaxRounds, ReviewPolicy: &policy, UpdatedAt: s.now(),
	}
	if err := s.Store.Create(integration); err != nil {
		return Result{}, classify(integrationID, err)
	}
	if err := s.WritePlan(integrationID, integration.Plan); err != nil {
		return Result{State: integration}, problem("PLAN_WRITE", err.Error(), integrationID, ExitAction, false, err)
	}
	if len(graph.Nodes) == 1 && (parent.Complexity == task.ComplexityTrivial || parent.Complexity == task.ComplexitySmall) && initialIntegrationEvidence(children[graph.Nodes[0].ID], candidate) {
		// The focused child review is the evidence for this initial trivial
		// integration boundary. It is deliberately never reused after feedback.
		child := children[graph.Nodes[0].ID]
		integration.CodeRound = child.CodeRound
		integration.ReviewProgress = &task.ReviewProgress{Kind: "integration", Status: "approved"}
		evidence := reviewerEvidence(integration, integrationID, string(runner.RoleCodeReviewer), "approved", integration.CandidateManifestHash, child.CodeReviewerSessionID, child.CodeRound)
		record := codeApprovalRecord(integration, evidence)
		record.GateID = s.gateID(integration, string(task.ApprovalKindCode), record.SubjectFingerprint)
		integration.Approval = &record
		integration.ApprovalGateSchemaVersion = task.ApprovalGateSchemaVersion
		if _, saveErr := s.Store.Update(integrationID, func(st *task.State) error {
			if st.Phase != task.PhaseImplementationReady || st.Approval != nil {
				return task.ErrStaleOperation
			}
			st.CodeRound = integration.CodeRound
			st.ReviewProgress = integration.ReviewProgress
			st.Approval = integration.Approval
			st.ApprovalGateSchemaVersion = integration.ApprovalGateSchemaVersion
			st.Phase = task.PhaseAwaitingApproval
			return nil
		}); saveErr != nil {
			return s.errorResult(integrationID, classify(integrationID, saveErr))
		}
		integration, saveErr := s.Store.Load(integrationID)
		if saveErr != nil {
			return Result{}, classify(integrationID, saveErr)
		}
		integration, saveErr = s.repairApprovalArtifact(integrationID, integration)
		if saveErr != nil {
			return Result{State: integration, Status: ControlFor(integration).Status}, saveErr
		}
		return approvalRequiredResult(integration)
	}
	return s.ReviewCode(ctx, integrationID)
}

func (s *Service) runPlanner(ctx context.Context, id, token, loop string) (Result, error) {
	resp, err := s.call(ctx, id, token, runner.RolePlanner)
	if err != nil {
		return s.errorResult(id, err)
	}
	env, err := envelope(resp, runner.RolePlanner)
	if err != nil {
		return s.errorResult(id, s.fail(id, token, err))
	}
	if env.Status == "needs_input" {
		st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
			st.Phase = task.PhaseNeedsInput
			st.PendingQuestion = env.Question
			st.PendingQuestionSource = string(runner.RolePlanner)
			if loop == "plan_review" {
				st.ReturnPhase = task.PhasePlanReviewing
			} else {
				st.ReturnPhase = task.PhasePlanned
			}
			st.InterruptedLoop = loop
			setReviewProgress(st, "plan", "needs_input")
			st.InFlight, st.Retry = nil, nil
			return nil
		})
		if saveErr != nil {
			return s.errorResult(id, classify(id, saveErr))
		}
		return Result{State: st, Status: "needs_input"}, problem("NEEDS_INPUT", env.Question, id, ExitNeedsInput, true, nil)
	}
	workUnits, graphErr := task.NormalizeWorkUnits(env.WorkUnits, env.Plan)
	if graphErr != nil {
		return s.errorResult(id, s.fail(id, token, fmt.Errorf("invalid planner work graph: %w", graphErr)))
	}
	if err := task.ValidateWorkUnitsForComplexity(env.Complexity, workUnits); err != nil {
		return s.errorResult(id, s.fail(id, token, fmt.Errorf("invalid planner task sizing: %w", err)))
	}
	if err := s.WritePlan(id, env.Plan); err != nil {
		return s.errorResult(id, s.fail(id, token, err))
	}
	isRevision := loop == "plan_review"
	var noProgress bool
	st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		st.Plan, st.PlanHash = env.Plan, hash(env.Plan)
		st.Complexity = task.NormalizeComplexity(env.Complexity)
		st.WorkGraph = len(env.WorkUnits) > 0
		st.WorkUnits = append([]task.WorkUnit(nil), workUnits...)
		st.Prompt = st.InFlight.Prompt
		st.PendingAnswer = ""
		if isRevision {
			noProgress = st.PlanReviewCheckpointHash != "" && st.PlanReviewCheckpointHash == planReviewFingerprint(*st)
			st.Phase = task.PhasePlanned
			st.InFlight, st.Retry = nil, nil
			if noProgress {
				setReviewProgress(st, "plan", "no_progress")
			} else {
				setReviewProgress(st, "plan", "revised")
			}
		} else {
			st.Phase = task.PhasePlanned
			st.InFlight, st.Retry = nil, nil
			st.ReviewProgress = nil
		}
		return nil
	})
	if saveErr != nil {
		return s.errorResult(id, classify(id, saveErr))
	}
	if noProgress {
		return Result{State: st, Status: "no_progress"}, problem("REVIEW_NO_PROGRESS", "plan revision did not change the reviewed plan", id, ExitAction, false, nil)
	}
	if isRevision {
		return Result{State: st, Status: "revised"}, nil
	}
	return Result{State: st, Status: "planned"}, nil
}

func (s *Service) runPlanReviewer(ctx context.Context, id, token string) (Result, error) {
	before, err := s.Store.Load(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	if before.InFlight == nil || before.InFlight.Token != token || before.InFlight.Role != string(runner.RolePlanReviewer) {
		return s.errorResult(id, classify(id, task.ErrStaleOperation))
	}
	if limit := maxRounds(&before); limit > 0 && reviewRound(before, "plan") >= limit {
		var exhausted bool
		st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
			if limit := maxRounds(st); limit > 0 && reviewRound(*st, "plan") >= limit {
				exhausted = true
				st.Phase = task.PhaseFailed
				st.InFlight, st.Retry = nil, nil
				setReviewProgress(st, "plan", "exhausted")
			}
			return nil
		})
		if saveErr != nil {
			return s.errorResult(id, classify(id, saveErr))
		}
		if exhausted {
			return exhaustedResult(st)
		}
	}
	reviewedPlanHash := before.PlanHash
	reviewedFingerprint := planReviewFingerprint(before)
	resp, err := s.call(ctx, id, token, runner.RolePlanReviewer)
	if err != nil {
		return s.errorResult(id, err)
	}
	env, err := envelope(resp, runner.RolePlanReviewer)
	if err != nil {
		return s.errorResult(id, s.fail(id, token, err))
	}
	var exhausted bool
	var humanGate bool
	st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		if st.PlanHash != reviewedPlanHash || planReviewFingerprint(*st) != reviewedFingerprint {
			return task.ErrStaleOperation
		}
		acceptReviewRound(st, "plan")
		st.Findings = cloneFindings(env.Findings)
		if env.Verdict == "approved" {
			if isImplementationReadyPlan(*st) {
				st.ApprovedPlanHash = st.PlanHash
				st.Phase = task.PhasePlanApproved
			} else {
				evidence := reviewerEvidence(*st, st.ID, string(runner.RolePlanReviewer), "approved", planReviewFingerprint(*st), st.PlanReviewerSessionID, reviewRound(*st, "plan"))
				record := planApprovalRecord(*st, evidence)
				record.GateID = s.gateID(*st, string(task.ApprovalKindPlan), record.SubjectFingerprint)
				record.CreatedAt = s.now()
				st.ApprovedPlanHash = ""
				st.Phase = task.PhaseAwaitingApproval
				st.Approval = &record
				st.ApprovalGateSchemaVersion = task.ApprovalGateSchemaVersion
				humanGate = true
			}
			setReviewProgress(st, "plan", "approved")
			st.InFlight, st.Retry = nil, nil
			return nil
		}
		st.PlanReviewCheckpointHash = planReviewFingerprint(*st)
		if limit := maxRounds(st); limit > 0 && reviewRound(*st, "plan") >= limit {
			exhausted = true
			st.Phase = task.PhaseFailed
			setReviewProgress(st, "plan", "exhausted")
			st.InFlight, st.Retry = nil, nil
			return nil
		}
		st.Phase = task.PhasePlanned
		st.InFlight.Operation = "plan_revision"
		st.InFlight.Role = string(runner.RolePlanner)
		st.InFlight.KnownSession = st.PlannerSessionID != ""
		st.InFlight.SessionID = st.PlannerSessionID
		st.InFlight.Prompt = plannerRevisionPrompt(st.Plan, env.Findings, st.PlannerSessionID != "")
		st.InFlight.Findings = cloneFindings(env.Findings)
		st.InFlight.StartedAt = s.now()
		setReviewProgress(st, "plan", "pending")
		return nil
	})
	if saveErr != nil {
		return s.errorResult(id, classify(id, saveErr))
	}
	if exhausted {
		return Result{State: st, Status: "exhausted"}, reviewExhaustedError(st)
	}
	if env.Verdict == "approved" {
		if humanGate {
			st, artifactErr := s.repairApprovalArtifact(id, st)
			if artifactErr != nil {
				return Result{State: st, Status: ControlFor(st).Status}, artifactErr
			}
			return approvalRequiredResult(st)
		}
		return Result{State: st, Status: "approved"}, nil
	}
	return s.runPlanner(ctx, id, token, "plan_review")
}

func (s *Service) runImplementer(ctx context.Context, id, token, loop string) (Result, error) {
	resp, err := s.call(ctx, id, token, runner.RoleImplementer)
	if err != nil {
		return s.errorResult(id, err)
	}
	env, err := envelope(resp, runner.RoleImplementer)
	if err != nil {
		return s.errorResult(id, s.fail(id, token, err))
	}
	if env.Status == "needs_input" {
		st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
			st.Phase = task.PhaseNeedsInput
			st.PendingQuestion, st.PendingQuestionSource = env.Question, string(runner.RoleImplementer)
			if loop == "code_review" {
				st.ReturnPhase = task.PhaseCodeReviewing
			} else {
				st.ReturnPhase = task.PhaseImplementing
			}
			st.InterruptedLoop = loop
			setReviewProgress(st, reviewKind(*st), "needs_input")
			st.InFlight, st.Retry = nil, nil
			return nil
		})
		if saveErr != nil {
			return s.errorResult(id, classify(id, saveErr))
		}
		return Result{State: st, Status: "needs_input"}, problem("NEEDS_INPUT", env.Question, id, ExitNeedsInput, true, nil)
	}
	current, loadErr := s.Store.Load(id)
	if loadErr != nil {
		return s.errorResult(id, classify(id, loadErr))
	}
	candidate, observeErr := s.Observe(current.Scope)
	if observeErr != nil {
		return s.errorResult(id, s.fail(id, token, observeErr))
	}
	label := "candidate-" + shortToken(token) + "-" + fmt.Sprint(current.CodeRound)
	candidate, observeErr = s.Capture(candidate, label, id)
	if observeErr != nil {
		return s.errorResult(id, s.fail(id, token, observeErr))
	}
	afterAll, observeErr := s.Observe("**")
	if observeErr != nil {
		return s.errorResult(id, s.fail(id, token, observeErr))
	}
	preAll := cloneManifest(current.InFlight.SnapshotManifest)
	outside := outsideScope(preAll, afterAll, current.Scope)
	candidateHash := task.HashManifest(candidate)
	var noProgress bool
	st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		st.CandidateManifest = cloneManifest(candidate)
		st.CandidateManifestHash = candidateHash
		st.ChangeManifest = changeEntries(st.ScopedBaseline, candidate)
		if len(outside) > 0 {
			st.Advisories = mergeDiagnostics(st.Advisories, []task.Diagnostic{{Code: "OUT_OF_SCOPE_CHANGE", Severity: "warning", Message: "files outside this task scope changed while the implementer was running", TaskID: id, Paths: outside}})
		}
		st.PendingAnswer = ""
		if loop == "code_review" {
			noProgress = st.ReviewCheckpointHash != "" && candidateHash == st.ReviewCheckpointHash
			st.Phase = task.PhaseImplementationReady
			st.InFlight, st.Retry = nil, nil
			if noProgress {
				setReviewProgress(st, reviewKind(*st), "no_progress")
			} else {
				setReviewProgress(st, reviewKind(*st), "fixed")
			}
		} else {
			st.Phase = task.PhaseImplementationReady
			st.InFlight, st.Retry = nil, nil
			st.ReviewProgress = nil
		}
		return nil
	})
	if saveErr != nil {
		return s.errorResult(id, classify(id, saveErr))
	}
	if noProgress {
		return Result{State: st, Status: "no_progress"}, problem("REVIEW_NO_PROGRESS", "code review fix did not change the scoped candidate", id, ExitAction, false, nil)
	}
	if loop == "code_review" {
		return Result{State: st, Status: "fixed"}, nil
	}
	return Result{State: st, Status: "implementation_ready"}, nil
}

func (s *Service) runCodeReviewer(ctx context.Context, id, token string) (Result, error) {
	beforeState, err := s.Store.Load(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	if beforeState.InFlight == nil || beforeState.InFlight.Token != token || beforeState.InFlight.Role != string(runner.RoleCodeReviewer) {
		return s.errorResult(id, classify(id, task.ErrStaleOperation))
	}
	kind := reviewKind(beforeState)
	if limit := maxRounds(&beforeState); limit > 0 && reviewRound(beforeState, "code") >= limit {
		var exhausted bool
		st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
			if limit := maxRounds(st); limit > 0 && reviewRound(*st, "code") >= limit {
				exhausted = true
				st.Phase = task.PhaseFailed
				st.InFlight, st.Retry = nil, nil
				setReviewProgress(st, kind, "exhausted")
			}
			return nil
		})
		if saveErr != nil {
			return s.errorResult(id, classify(id, saveErr))
		}
		if exhausted {
			return exhaustedResult(st)
		}
	}
	barrier := cloneManifest(beforeState.InFlight.SnapshotManifest)
	reviewedCandidateHash := beforeState.CandidateManifestHash
	live, observeErr := s.Observe(beforeState.Scope)
	if observeErr != nil {
		return s.errorResult(id, s.fail(id, token, observeErr))
	}
	if task.ManifestChanged(barrier, live) {
		label := "candidate-review-refresh-" + shortToken(token) + "-" + fmt.Sprint(beforeState.CodeRound)
		captured, captureErr := s.Capture(live, label, id)
		if captureErr != nil {
			return s.errorResult(id, s.fail(id, token, captureErr))
		}
		beforeState, err = s.Store.UpdateOwned(id, token, func(st *task.State) error {
			st.CandidateManifest = cloneManifest(captured)
			st.CandidateManifestHash = task.HashManifest(captured)
			st.ChangeManifest = changeEntries(st.ScopedBaseline, captured)
			st.InFlight.SnapshotManifest = cloneManifest(live)
			st.InFlight.Prompt = codeReviewPrompt(*st, st.CodeReviewerSessionID != "")
			return nil
		})
		if err != nil {
			return s.errorResult(id, classify(id, err))
		}
		barrier = cloneManifest(live)
		reviewedCandidateHash = beforeState.CandidateManifestHash
	}
	resp, callErr := s.call(ctx, id, token, runner.RoleCodeReviewer)
	if callErr != nil {
		return s.errorResult(id, callErr)
	}
	after, observeErr := s.Observe(beforeState.Scope)
	if observeErr != nil {
		return s.errorResult(id, s.fail(id, token, observeErr))
	}
	if task.ManifestChanged(barrier, after) {
		label := "candidate-review-change-" + shortToken(token) + "-" + fmt.Sprint(beforeState.CodeRound)
		captured, captureErr := s.Capture(after, label, id)
		if captureErr != nil {
			return s.errorResult(id, s.fail(id, token, captureErr))
		}
		st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
			st.CandidateManifest = cloneManifest(captured)
			st.CandidateManifestHash = task.HashManifest(captured)
			st.ChangeManifest = changeEntries(st.ScopedBaseline, captured)
			retry := retryFrom(st, st.InFlight)
			retry.Prompt = "The scoped candidate changed while your previous review was running. Discard that verdict and review this current candidate.\n" + codeReviewPrompt(*st, true)
			st.Phase = task.PhaseReviewNeeded
			setReviewProgress(st, kind, "review_needed")
			st.Retry = retry
			st.InFlight = nil
			return nil
		})
		if saveErr != nil {
			return s.errorResult(id, classify(id, saveErr))
		}
		return Result{State: st, Status: "review_needed"}, problem("REVIEW_NEEDED", "scoped files changed during code review", id, ExitReviewNeeded, true, task.ErrScopeChanged)
	}
	env, envErr := envelope(resp, runner.RoleCodeReviewer)
	if envErr != nil {
		return s.errorResult(id, s.fail(id, token, envErr))
	}
	var exhausted bool
	var humanGate bool
	st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		if st.CandidateManifestHash != reviewedCandidateHash || task.HashManifest(st.CandidateManifest) != reviewedCandidateHash {
			return task.ErrStaleOperation
		}
		acceptReviewRound(st, kind)
		st.Findings = cloneFindings(env.Findings)
		if env.Verdict == "approved" {
			if isMaterializedWorkUnitChild(*st) {
				st.ApprovedManifestHash = st.CandidateManifestHash
				st.Phase = task.PhaseApproved
			} else {
				evidence := reviewerEvidence(*st, st.ID, string(runner.RoleCodeReviewer), "approved", st.CandidateManifestHash, st.CodeReviewerSessionID, reviewRound(*st, kind))
				record := codeApprovalRecord(*st, evidence)
				record.GateID = s.gateID(*st, string(task.ApprovalKindCode), record.SubjectFingerprint)
				record.CreatedAt = s.now()
				st.ApprovedManifestHash = ""
				st.Phase = task.PhaseAwaitingApproval
				st.Approval = &record
				st.ApprovalGateSchemaVersion = task.ApprovalGateSchemaVersion
				humanGate = true
			}
			setReviewProgress(st, kind, "approved")
			st.InFlight, st.Retry = nil, nil
			return nil
		}
		st.ReviewCheckpoint = cloneManifest(st.CandidateManifest)
		st.ReviewCheckpointHash = st.CandidateManifestHash
		st.ReviewCheckpointFindings = cloneFindings(env.Findings)
		if limit := maxRounds(st); limit > 0 && reviewRound(*st, kind) >= limit {
			exhausted = true
			st.Phase = task.PhaseFailed
			setReviewProgress(st, kind, "exhausted")
			st.InFlight, st.Retry = nil, nil
			return nil
		}
		st.Phase = task.PhaseImplementing
		st.InFlight.Operation = "code_revision"
		st.InFlight.Role = string(runner.RoleImplementer)
		st.InFlight.KnownSession = st.ImplementerSessionID != ""
		st.InFlight.SessionID = st.ImplementerSessionID
		st.InFlight.SnapshotManifest = nil
		st.InFlight.Prompt = implementReviewFixPrompt(*st, env.Findings)
		st.InFlight.Findings = cloneFindings(env.Findings)
		st.InFlight.StartedAt = s.now()
		setReviewProgress(st, kind, "pending")
		return nil
	})
	if saveErr != nil {
		return s.errorResult(id, classify(id, saveErr))
	}
	if exhausted {
		return Result{State: st, Status: "exhausted"}, reviewExhaustedError(st)
	}
	if env.Verdict == "approved" {
		if humanGate {
			st, artifactErr := s.repairApprovalArtifact(id, st)
			if artifactErr != nil {
				return Result{State: st, Status: ControlFor(st).Status}, artifactErr
			}
			return approvalRequiredResult(st)
		}
		return Result{State: st, Status: "approved"}, nil
	}
	preAll, observeErr := s.Observe("**")
	if observeErr != nil {
		return s.errorResult(id, s.fail(id, token, observeErr))
	}
	_, saveErr = s.Store.UpdateOwned(id, token, func(st *task.State) error {
		st.InFlight.SnapshotManifest = cloneManifest(preAll)
		return nil
	})
	if saveErr != nil {
		return s.errorResult(id, classify(id, saveErr))
	}
	return s.runImplementer(ctx, id, token, "code_review")
}

func (s *Service) beginCodeReview(id string, before, candidate []task.FileEntry, loop, prompt string) (task.State, error) {
	var exhausted bool
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight != nil {
			return task.ErrOperationInFlight
		}
		if st.Approval != nil && st.Approval.Status == "" {
			return problem(ApprovalRequiredCode, "human approval is required before another code review can start", id, ExitNeedsInput, true, nil)
		}
		if st.Phase != task.PhaseImplementationReady || st.Scope == "" || st.CandidateManifestHash == "" {
			return task.ErrInvalidPhase
		}
		kind := "code"
		if st.IntegrationReview {
			kind = "integration"
		}
		if limit := maxRounds(st); limit > 0 && reviewRound(*st, kind) >= limit {
			exhausted = true
			st.Phase = task.PhaseFailed
			st.InFlight, st.Retry = nil, nil
			setReviewProgress(st, kind, "exhausted")
			return nil
		}
		st.CandidateManifest = cloneManifest(candidate)
		st.CandidateManifestHash = task.HashManifest(candidate)
		st.ChangeManifest = changeEntries(st.ScopedBaseline, candidate)
		if prompt == "" {
			prompt = codeReviewPrompt(*st, st.CodeReviewerSessionID != "")
		}
		token := s.newToken()
		st.Phase = task.PhaseCodeReviewing
		setReviewProgress(st, kind, "pending")
		st.InFlight = s.ownedFlight(task.InFlight{Token: token, Operation: "code_review", Role: string(runner.RoleCodeReviewer), StartedAt: s.now(), KnownSession: st.CodeReviewerSessionID != "", SessionID: st.CodeReviewerSessionID, SnapshotManifest: cloneManifest(before), PreviousPhase: task.PhaseImplementationReady, Prompt: prompt, Scope: st.Scope, Loop: loop})
		return nil
	})
	if err != nil {
		return st, classify(id, err)
	}
	if exhausted {
		return st, reviewExhaustedError(st)
	}
	return st, nil
}

func (s *Service) call(ctx context.Context, id, token string, role runner.Role) (runner.Response, error) {
	st, err := s.Store.Load(id)
	if err != nil {
		return runner.Response{}, classify(id, err)
	}
	if st.InFlight == nil || st.InFlight.Token != token || st.InFlight.Role != string(role) {
		return runner.Response{}, classify(id, task.ErrStaleOperation)
	}
	profile, ok := st.ProfilesSnapshot[string(role)]
	if !ok {
		return runner.Response{}, s.fail(id, token, fmt.Errorf("task has no %s profile snapshot", role))
	}
	adapter := s.Adapters[profile.Provider]
	if adapter == nil {
		return runner.Response{}, s.fail(id, token, &runner.ProviderError{Code: "PROVIDER_UNAVAILABLE", Message: fmt.Sprintf("%s provider is unavailable; install or log in with its CLI", profile.Provider), Retryable: false, KnownSession: st.InFlight.KnownSession, SessionID: st.InFlight.SessionID})
	}
	sandbox := "read-only"
	if role == runner.RoleImplementer {
		sandbox = "workspace-write"
	}
	request := runner.Request{Role: role, Operation: st.InFlight.Operation, Prompt: st.InFlight.Prompt, Model: profile.Model, Effort: profile.Effort, Speed: profile.Speed, RepoRoot: s.RepoRoot, Scope: st.InFlight.Scope, SessionID: st.InFlight.SessionID, Resume: st.InFlight.KnownSession && st.InFlight.SessionID != "", Runtime: st.RuntimeSnapshot[string(role)], Sandbox: sandbox}
	if s.Capabilities != nil {
		available := s.Capabilities(profile.Provider, role, st.Task)
		request.SkillDirectories = append([]string(nil), available.SkillDirectories...)
		if !request.Resume && strings.TrimSpace(available.Note) != "" {
			request.Prompt = strings.TrimSpace(available.Note) + "\n\n" + request.Prompt
		}
	}
	callContext := ctx
	cancel := func() {}
	if seconds := s.Config.ProviderTurnTimeoutSeconds; seconds > 0 {
		callContext, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	}
	defer cancel()
	resp, callErr := adapter.Run(callContext, request, runner.Callbacks{SessionStarted: func(session string) error {
		if strings.TrimSpace(session) == "" {
			return runner.ErrMissingSession
		}
		_, updateErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
			setSession(st, role, session)
			st.InFlight.KnownSession, st.InFlight.SessionID = true, session
			return nil
		})
		return updateErr
	}, Diagnostic: s.Diagnostic})
	turnUsage := resp.Usage
	// Requests and prompt bytes are host measurements. Keep provider token
	// counters intact, but do not let provider-side aggregate fields inflate
	// the host-owned counters.
	turnUsage.Requests = 1
	turnUsage.PromptBytes = int64(len(request.Prompt))
	turnUsage.UnreportedRequests = 0
	turnUsage.IncompleteRequests = 0
	usageStatus, usageReported := classifyUsage(resp, callErr)
	switch usageStatus {
	case runner.UsageIncomplete:
		turnUsage.IncompleteRequests = 1
	case runner.UsageUnreported:
		turnUsage.UnreportedRequests = 1
	}
	if _, updateErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		if st.Usage == nil {
			st.Usage = map[string]task.TokenUsage{}
		}
		total := st.Usage[string(role)]
		if resp.UsageCumulative && usageReported {
			if st.ProviderUsageCumulative == nil {
				st.ProviderUsageCumulative = map[string]task.TokenUsage{}
			}
			previous := st.ProviderUsageCumulative[string(role)]
			turnUsage.InputTokens = tokenDelta(turnUsage.InputTokens, previous.InputTokens)
			turnUsage.CachedInputTokens = tokenDelta(turnUsage.CachedInputTokens, previous.CachedInputTokens)
			turnUsage.CacheWriteTokens = tokenDelta(turnUsage.CacheWriteTokens, previous.CacheWriteTokens)
			turnUsage.OutputTokens = tokenDelta(turnUsage.OutputTokens, previous.OutputTokens)
			turnUsage.ReasoningTokens = tokenDelta(turnUsage.ReasoningTokens, previous.ReasoningTokens)
			turnUsage.TotalTokens = tokenDelta(turnUsage.TotalTokens, previous.TotalTokens)
			st.ProviderUsageCumulative[string(role)] = resp.Usage
		}
		total.Add(turnUsage)
		st.Usage[string(role)] = total
		return nil
	}); updateErr != nil {
		return runner.Response{}, classify(id, updateErr)
	}
	if callErr != nil {
		return resp, s.fail(id, token, callErr)
	}
	if resp.SessionID != "" {
		_, updateErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
			if st.InFlight.SessionID != "" && st.InFlight.SessionID != resp.SessionID {
				return task.ErrStaleOperation
			}
			setSession(st, role, resp.SessionID)
			st.InFlight.KnownSession, st.InFlight.SessionID = true, resp.SessionID
			return nil
		})
		if updateErr != nil {
			return runner.Response{}, classify(id, updateErr)
		}
	}
	return resp, nil
}

// classifyUsage resolves the explicit adapter status and retains a
// conservative compatibility path for adapters written before UsageStatus
// existed. Only token counters establish that a legacy adapter reported a
// snapshot; host counters and aggregate availability metadata are ignored.
func classifyUsage(resp runner.Response, callErr error) (runner.UsageStatus, bool) {
	switch resp.UsageStatus {
	case runner.UsageComplete:
		return runner.UsageComplete, true
	case runner.UsageIncomplete:
		return runner.UsageIncomplete, true
	case runner.UsageUnreported:
		return runner.UsageUnreported, false
	case "":
		if !hasTokenUsage(resp.Usage) {
			return runner.UsageUnreported, false
		}
		if callErr != nil {
			return runner.UsageIncomplete, true
		}
		return runner.UsageComplete, true
	default:
		return runner.UsageUnreported, false
	}
}

func hasTokenUsage(usage task.TokenUsage) bool {
	return usage.InputTokens != 0 ||
		usage.CachedInputTokens != 0 ||
		usage.CacheWriteTokens != 0 ||
		usage.OutputTokens != 0 ||
		usage.ReasoningTokens != 0 ||
		usage.TotalTokens != 0
}

func tokenDelta(current, previous int64) int64 {
	if current < previous {
		// Some CLIs reset counters when a resumed conversation starts in a
		// new process. Treat the new counter as this turn rather than losing it.
		return current
	}
	return current - previous
}

func (s *Service) fail(id, token string, cause error) error {
	var pe *runner.ProviderError
	errors.As(cause, &pe)
	code, message, retryable, known, session := "PROVIDER_ERROR", cause.Error(), false, false, ""
	if pe != nil {
		if pe.Code != "" {
			code = pe.Code
		}
		message, retryable, known, session = pe.Message, pe.Retryable, pe.KnownSession, pe.SessionID
	}
	_, err := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		kind := reviewKind(*st)
		if session == "" && st.InFlight != nil {
			session, known = st.InFlight.SessionID, st.InFlight.KnownSession
		}
		if known && session != "" {
			setSession(st, runner.Role(st.InFlight.Role), session)
			st.Retry = retryFrom(st, st.InFlight)
			st.Retry.SessionID, st.Retry.KnownSession = session, true
		} else {
			st.Retry = nil
		}
		st.Phase = task.PhaseFailed
		setReviewProgress(st, kind, "failed")
		st.InFlight = nil
		return nil
	})
	if err != nil {
		return classify(id, err)
	}
	return problem(code, message, id, ExitAction, retryable && known, cause)
}

func (s *Service) snapshots(ctx context.Context) (map[string]task.ProfileSnapshot, map[string]task.RuntimeSnapshot, error) {
	effective, err := s.Config.EffectiveProfiles()
	if err != nil {
		return nil, nil, err
	}
	profiles := map[string]task.ProfileSnapshot{}
	runtimes := map[string]task.RuntimeSnapshot{}
	modelsByProvider := map[string][]runner.ModelInfo{}
	cat := catalog.New(s.Adapters, s.Config, s.ModelCachePath)
	for _, role := range []string{config.RolePlanner, config.RolePlanReviewer, config.RoleImplementer, config.RoleCodeReviewer} {
		p, providerConfig := s.Config.ResolveProfile(effective[role])
		if err := config.ValidateProfile(p); err != nil {
			return nil, nil, fmt.Errorf("profile %s: %w", role, err)
		}
		runtime := RuntimeSnapshot(p.Provider, providerConfig)
		adapter := s.Adapters[p.Provider]
		if adapter == nil {
			return nil, nil, fmt.Errorf("profile %s: %w", role, &runner.ProviderError{Code: "PROVIDER_UNAVAILABLE", Message: fmt.Sprintf("provider %s is unavailable; let the orchestrator choose the next action", p.Provider)})
		}
		models := modelsByProvider[p.Provider]
		if models == nil {
			models, err = cat.Models(ctx, p.Provider, true, runner.ModelListRequest{Refresh: true, Runtime: runtime})
			if err != nil {
				return nil, nil, fmt.Errorf("profile %s: %w", role, err)
			}
			modelsByProvider[p.Provider] = models
		}
		if err := runner.ValidateSelection(runner.Role(role), p.Model, p.Effort, p.Speed, models, adapter); err != nil {
			return nil, nil, fmt.Errorf("profile %s: %w", role, err)
		}
		profiles[role] = task.ProfileSnapshot{Provider: p.Provider, Model: p.Model, Effort: p.Effort, Speed: p.Speed}
		runtimes[role] = runtime
	}
	return profiles, runtimes, nil
}

// configuredSnapshots intentionally performs no model discovery. Quick tasks
// use the already configured selections and let the actual provider call fail
// closed if an account/model has since become unavailable.
func (s *Service) configuredSnapshots(roles ...runner.Role) (map[string]task.ProfileSnapshot, map[string]task.RuntimeSnapshot, error) {
	effective, err := s.Config.EffectiveProfiles()
	if err != nil {
		return nil, nil, err
	}
	profiles := map[string]task.ProfileSnapshot{}
	runtimes := map[string]task.RuntimeSnapshot{}
	for _, role := range roles {
		name := string(role)
		profile, providerConfig := s.Config.ResolveProfile(effective[name])
		if err := config.ValidateProfile(profile); err != nil {
			return nil, nil, fmt.Errorf("profile %s: %w", name, err)
		}
		if s.Adapters[profile.Provider] == nil {
			return nil, nil, fmt.Errorf("profile %s: provider %s is unavailable", name, profile.Provider)
		}
		profiles[name] = task.ProfileSnapshot{Provider: profile.Provider, Model: profile.Model, Effort: profile.Effort, Speed: profile.Speed}
		runtimes[name] = RuntimeSnapshot(profile.Provider, providerConfig)
	}
	return profiles, runtimes, nil
}

// RuntimeSnapshot converts validated provider configuration to durable,
// non-secret routing metadata. CLI discovery and workflow creation share this
// conversion so fresh, resume, and model-list calls use the same route.
func RuntimeSnapshot(provider string, p config.Provider) task.RuntimeSnapshot {
	refs := []string{}
	for _, value := range []string{p.EnvKey, p.BearerTokenEnv, p.APIKeyEnv, p.AuthTokenEnv, p.BedrockProfileEnv, p.BedrockRegionEnv, p.VertexProjectEnv, p.VertexRegionEnv, p.FoundryEndpointEnv, p.FoundryAPIKeyEnv} {
		if value != "" {
			refs = append(refs, value)
		}
	}
	for _, value := range p.EnvRefs {
		refs = append(refs, value)
	}
	for _, value := range p.EnvHTTPHeaders {
		refs = append(refs, value)
	}
	sort.Strings(refs)
	refs = unique(refs)
	endpoint := p.GatewayURL
	if endpoint == "" {
		endpoint = p.BaseURL
	}
	auth := map[string]any{}
	if p.Auth.Command != "" {
		auth = map[string]any{"command": p.Auth.Command, "args": append([]string(nil), p.Auth.Args...), "timeout_ms": p.Auth.TimeoutMS, "refresh_interval_ms": p.Auth.RefreshIntervalMS}
	}
	sdk := map[string]any{}
	providerID := ""
	if provider == "codex" {
		for key, value := range map[string]any{
			"name":                           p.Name,
			"env_key":                        p.EnvKey,
			"env_http_headers":               p.EnvHTTPHeaders,
			"query_params":                   p.QueryParams,
			"request_max_retries":            p.RequestMaxRetries,
			"stream_max_retries":             p.StreamMaxRetries,
			"stream_idle_timeout_ms":         p.StreamIdleTimeoutMS,
			"supports_standalone_web_search": p.SupportsStandaloneWebSearch,
			"requires_openai_auth":           p.RequiresOpenAIAuth,
		} {
			if !isZero(value) {
				sdk[key] = value
			}
		}
		if p.GatewayURL != "" || p.BaseURL != "" || p.WireAPI != "" || len(sdk) > 0 || len(auth) > 0 {
			providerID = "rolemux"
		}
	} else if provider == "claude" {
		envMap := map[string]string{}
		for target, source := range p.EnvRefs {
			envMap[target] = source
		}
		for target, source := range map[string]string{
			"ANTHROPIC_API_KEY":       p.APIKeyEnv,
			"CLAUDE_CODE_OAUTH_TOKEN": p.AuthTokenEnv,
			"AWS_PROFILE":             p.BedrockProfileEnv,
			"AWS_REGION":              p.BedrockRegionEnv,
			"CLOUD_ML_PROJECT_ID":     p.VertexProjectEnv,
			"CLOUD_ML_REGION":         p.VertexRegionEnv,
			"FOUNDRY_ENDPOINT":        p.FoundryEndpointEnv,
			"FOUNDRY_API_KEY":         p.FoundryAPIKeyEnv,
		} {
			if source != "" {
				envMap[target] = source
			}
		}
		if len(envMap) > 0 {
			sdk["env_map"] = envMap
		}
	} else if provider == "copilot" {
		providerID = "copilot"
		for key, value := range map[string]any{"type": p.Type, "wire_api": p.WireAPI, "transport": p.Transport, "base_url": endpoint, "model_id": p.ModelID, "wire_model": p.WireModel, "max_prompt_tokens": p.MaxPromptTokens, "max_output_tokens": p.MaxOutputTokens, "headers": p.Headers, "bearer_token_env": p.BearerTokenEnv} {
			if !isZero(value) {
				sdk[key] = value
			}
		}
	}
	return task.RuntimeSnapshot{ProviderType: provider, ProviderID: providerID, Endpoint: endpoint, WireAPI: p.WireAPI, AuthEnvRefs: refs, Auth: auth, CLIPath: p.CLIPath, SDKSettings: sdk}
}

func envelope(resp runner.Response, role runner.Role) (runner.Envelope, error) {
	if resp.Envelope != nil {
		if err := runner.ValidateEnvelope(*resp.Envelope, role); err != nil {
			return runner.Envelope{}, err
		}
		return *resp.Envelope, nil
	}
	return runner.DecodeEnvelope([]byte(resp.Text), role)
}

func (s *Service) generatedID() string {
	var data [4]byte
	_, _ = rand.Read(data[:])
	return fmt.Sprintf("task-%s-%s", s.now().UTC().Format("20060102-150405"), hex.EncodeToString(data[:]))
}

func (s *Service) newToken() string {
	if s.Token != nil {
		return s.Token()
	}
	return task.NewToken()
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) ownedFlight(in task.InFlight) *task.InFlight {
	if s.ProcessID != nil {
		in.OwnerPID = s.ProcessID()
	}
	return &in
}

func (s *Service) processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if s.ProcessAlive != nil {
		return s.ProcessAlive(pid)
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (s *Service) operationAbandoned(in *task.InFlight) bool {
	if in == nil {
		return false
	}
	if in.OwnerPID > 0 {
		return !s.processAlive(in.OwnerPID)
	}
	seconds := s.Config.ProviderTurnTimeoutSeconds
	if seconds <= 0 || in.StartedAt.IsZero() {
		return false
	}
	deadline := in.StartedAt.Add(time.Duration(seconds)*time.Second + abandonedOperationGrace)
	return !s.now().Before(deadline)
}

// recoverAbandoned converts a turn whose owning RoleMux process is gone into
// the ordinary same-session retry state. It never silently starts a fresh
// provider conversation because the interrupted turn may already have made
// progress that only its durable session can explain.
func (s *Service) recoverAbandoned(id, token string) (task.State, error) {
	var unrecoverable bool
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight == nil || st.InFlight.Token != token {
			return task.ErrStaleOperation
		}
		if !s.operationAbandoned(st.InFlight) {
			return task.ErrOperationInFlight
		}
		session := strings.TrimSpace(st.InFlight.SessionID)
		if session == "" {
			session = sessionFor(st, runner.Role(st.InFlight.Role))
		}
		if session == "" {
			unrecoverable = true
			st.Phase = task.PhaseFailed
			setReviewProgress(st, reviewKind(*st), "recovery")
			st.Diagnostics = append(st.Diagnostics, "the abandoned operation has no durable provider session")
			st.InFlight, st.Retry = nil, nil
			return nil
		}
		retry := retryFrom(st, st.InFlight)
		retry.SessionID, retry.KnownSession, retry.CreatedAt = session, true, s.now()
		st.Phase = task.PhaseFailed
		setReviewProgress(st, reviewKind(*st), "failed")
		st.Retry = retry
		st.InFlight = nil
		return nil
	})
	if err != nil {
		return task.State{}, classify(id, err)
	}
	if unrecoverable {
		return st, problem("INTERRUPTED_UNRECOVERABLE", "the abandoned operation has no durable provider session; RoleMux will not duplicate it in a fresh session", id, ExitAction, false, nil)
	}
	return st, nil
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func maxRounds(st *task.State) int {
	if st != nil && st.ReviewPolicy != nil {
		return st.ReviewPolicy.MaxRounds
	}
	if st != nil && st.MaxRounds > 0 {
		return st.MaxRounds
	}
	return config.DefaultReviewMaxRounds
}

func resolvedReviewPolicy(st *task.State) task.ReviewPolicy {
	return task.ReviewPolicy{MaxRounds: maxRounds(st)}
}

func setReviewProgress(st *task.State, kind, status string) {
	if strings.TrimSpace(kind) == "" {
		st.ReviewProgress = nil
		return
	}
	st.ReviewProgress = &task.ReviewProgress{Kind: kind, Status: status}
}

func planReviewFingerprint(st task.State) string {
	units, err := task.NormalizeWorkUnits(st.WorkUnits, st.Plan)
	if err != nil {
		units = cloneWorkUnits(st.WorkUnits)
	}
	return hash(mustJSON(struct {
		Plan       string          `json:"plan"`
		Complexity string          `json:"complexity"`
		WorkGraph  bool            `json:"work_graph"`
		WorkUnits  []task.WorkUnit `json:"work_units"`
	}{Plan: st.Plan, Complexity: st.Complexity, WorkGraph: st.WorkGraph, WorkUnits: units}))
}

func cloneWorkUnits(units []task.WorkUnit) []task.WorkUnit {
	if units == nil {
		return nil
	}
	out := make([]task.WorkUnit, len(units))
	for i, unit := range units {
		out[i] = unit
		out[i].DependsOn = append([]string(nil), unit.DependsOn...)
		out[i].AcceptanceCriteria = append([]string(nil), unit.AcceptanceCriteria...)
		out[i].ValidationCommands = append([]string(nil), unit.ValidationCommands...)
	}
	return out
}

func isReviewerRole(role string) bool {
	return role == string(runner.RolePlanReviewer) || role == string(runner.RoleCodeReviewer)
}

func retryResumable(retry *task.RetryState) bool {
	return retry != nil && retry.KnownSession && strings.TrimSpace(retry.SessionID) != ""
}

func sameRetry(left, right task.RetryState) bool {
	return left.Token == right.Token &&
		left.Operation == right.Operation &&
		left.Role == right.Role &&
		left.PreviousPhase == right.PreviousPhase &&
		left.Scope == right.Scope &&
		left.SessionID == right.SessionID &&
		left.KnownSession == right.KnownSession &&
		left.Loop == right.Loop &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func isExhaustedState(st task.State) bool {
	if st.InFlight != nil || st.Phase == task.PhaseNeedsInput {
		return false
	}
	if st.ReviewProgress != nil && st.ReviewProgress.Status == "exhausted" {
		return true
	}
	// Older task files have no ReviewProgress. Only infer exhaustion for a
	// failed, unapproved task with no resumable operation, preserving ordinary
	// provider failures as retryable/inspectable failures.
	if st.Phase != task.PhaseFailed || st.Retry != nil || st.ReviewProgress != nil {
		return false
	}
	limit := maxRounds(&st)
	if limit <= 0 {
		return false
	}
	kind := reviewKind(st)
	if kind == "plan" {
		if st.ApprovedPlanHash != "" && st.ApprovedPlanHash == st.PlanHash {
			return false
		}
	} else if st.ApprovedManifestHash != "" && st.ApprovedManifestHash == st.CandidateManifestHash {
		return false
	}
	return reviewRound(st, kind) >= limit
}

func reviewExhaustedError(st task.State) error {
	label := reviewKind(st)
	if label == "" {
		label = "task"
	}
	return problem("REVIEW_EXHAUSTED", fmt.Sprintf("%s review exhausted %d rounds", label, maxRounds(&st)), st.ID, ExitExhausted, false, nil)
}

func exhaustedResult(st task.State) (Result, error) {
	return Result{State: st, Status: "exhausted"}, reviewExhaustedError(st)
}

func needsInputResult(st task.State) (Result, error) {
	return Result{State: st, Status: "needs_input"}, problem("NEEDS_INPUT", st.PendingQuestion, st.ID, ExitNeedsInput, true, nil)
}

func existingWorkflowResult(st task.State) (Result, error) {
	result := Result{State: st, Status: ControlFor(st).Status}
	if isExhaustedState(st) {
		return exhaustedResult(st)
	}
	if st.InFlight != nil {
		return result, classify(st.ID, task.ErrOperationInFlight)
	}
	if st.Approval != nil && st.Approval.Status == "" {
		return approvalRequiredResult(st)
	}
	if st.Phase == task.PhaseNeedsInput {
		return needsInputResult(st)
	}
	switch st.Phase {
	case task.PhaseReviewNeeded:
		return result, problem("REVIEW_NEEDED", "review must be retried before another review can start", st.ID, ExitReviewNeeded, true, task.ErrScopeChanged)
	case task.PhasePlanReviewing, task.PhaseCodeReviewing, task.PhaseImplementing:
		return result, classify(st.ID, task.ErrOperationInFlight)
	case task.PhaseFailed:
		return result, problem("OPERATION_FAILED", "the saved operation failed; inspect the task or retry its durable session", st.ID, ExitAction, retryResumable(st.Retry), nil)
	case task.PhaseAwaitingApproval:
		return approvalRequiredResult(st)
	default:
		return result, classify(st.ID, task.ErrInvalidPhase)
	}
}

// errorResult loads the state saved by fail before returning a provider-side
// error. Successful paths return the state produced by their CAS directly and
// therefore do not need this extra read.
func (s *Service) errorResult(id string, err error) (Result, error) {
	if err == nil {
		return Result{}, nil
	}
	st, loadErr := s.Store.Load(id)
	if loadErr != nil {
		return Result{}, err
	}
	return Result{State: st, Status: ControlFor(st).Status}, err
}

func shortToken(token string) string {
	if len(token) > 10 {
		return token[:10]
	}
	return token
}

func setSession(st *task.State, role runner.Role, session string) {
	switch role {
	case runner.RolePlanner:
		st.PlannerSessionID = session
	case runner.RolePlanReviewer:
		st.PlanReviewerSessionID = session
	case runner.RoleImplementer:
		st.ImplementerSessionID = session
	case runner.RoleCodeReviewer:
		st.CodeReviewerSessionID = session
	}
}

func sessionFor(st *task.State, role runner.Role) string {
	switch role {
	case runner.RolePlanner:
		return st.PlannerSessionID
	case runner.RolePlanReviewer:
		return st.PlanReviewerSessionID
	case runner.RoleImplementer:
		return st.ImplementerSessionID
	case runner.RoleCodeReviewer:
		return st.CodeReviewerSessionID
	default:
		return ""
	}
}

func retryFrom(st *task.State, in *task.InFlight) *task.RetryState {
	if in == nil {
		return nil
	}
	return &task.RetryState{Token: in.Token, Operation: in.Operation, Role: in.Role, PreviousPhase: in.PreviousPhase, Prompt: in.Prompt, Findings: cloneFindings(in.Findings), Scope: in.Scope, SessionID: in.SessionID, KnownSession: in.KnownSession, Loop: in.Loop, SnapshotManifest: cloneManifest(in.SnapshotManifest), CreatedAt: time.Now().UTC()}
}

func retryPhase(r task.RetryState) string {
	switch runner.Role(r.Role) {
	case runner.RolePlanner:
		return task.PhasePlanned
	case runner.RolePlanReviewer:
		return task.PhasePlanReviewing
	case runner.RoleImplementer:
		return task.PhaseImplementing
	case runner.RoleCodeReviewer:
		return task.PhaseCodeReviewing
	default:
		return task.PhaseFailed
	}
}

func (s *Service) scopeAdvisories(id, scope string, entries []task.FileEntry) []task.Diagnostic {
	var out []task.Diagnostic
	if unmatched := task.UnmatchedScopePatterns(entries, scope); len(unmatched) > 0 {
		out = append(out, task.Diagnostic{Code: "SCOPE_UNMATCHED", Severity: "warning", Message: "one or more scope patterns currently match no files", TaskID: id, Paths: unmatched})
	}
	states, _ := s.Store.List()
	for _, other := range states {
		if other.ID != id && other.Scope != "" && task.ScopesOverlap(scope, other.Scope) && scopeStateRelevant(other) {
			out = append(out, task.Diagnostic{Code: "SCOPE_OVERLAP", Severity: "warning", Message: "scope may overlap another task; the orchestrator owns writer coordination", TaskID: other.ID})
		}
	}
	return out
}

func scopeStateRelevant(st task.State) bool {
	if st.InFlight != nil && scopedOperationRole(st.InFlight.Role) {
		return true
	}
	if isExhaustedState(st) {
		return false
	}
	switch st.Phase {
	case task.PhaseApproved:
		return false
	case task.PhaseFailed, task.PhaseReviewNeeded:
		return st.Retry != nil && scopedOperationRole(st.Retry.Role) && retryResumable(st.Retry)
	case task.PhaseNeedsInput:
		return st.PendingQuestionSource == string(runner.RoleImplementer)
	default:
		return true
	}
}

func scopedOperationRole(role string) bool {
	return role == string(runner.RoleImplementer) || role == string(runner.RoleCodeReviewer)
}

func cloneFindings(v []task.Finding) []task.Finding { return append([]task.Finding(nil), v...) }
func cloneManifest(v []task.FileEntry) []task.FileEntry {
	b, _ := json.Marshal(v)
	var out []task.FileEntry
	_ = json.Unmarshal(b, &out)
	return out
}
func mergeDiagnostics(a, b []task.Diagnostic) []task.Diagnostic {
	return append(append([]task.Diagnostic(nil), a...), b...)
}
func outsideScope(before, after []task.FileEntry, scope string) []string {
	delta := task.ManifestDelta(filterDiagnosticEntries(before), filterDiagnosticEntries(after))
	paths := delta.Paths()
	seen := make(map[string]struct{}, len(paths))
	var out []string
	for _, p := range paths {
		if !task.ScopeMatches(scope, p) {
			if _, exists := seen[p]; exists {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func filterDiagnosticEntries(entries []task.FileEntry) []task.FileEntry {
	filtered := make([]task.FileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Kind == "directory" && !meaningfulIndexState(entry.Index) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func meaningfulIndexState(index task.IndexState) bool {
	return index.Present || index.Mode != "" || index.Blob != "" || len(index.Stages) > 0 || index.Ref != nil || len(index.Entries) > 0
}
func changeEntries(before, after []task.FileEntry) []task.FileEntry {
	d := task.ManifestDelta(before, after)
	out := append([]task.FileEntry{}, d.Added...)
	out = append(out, d.Changed...)
	out = append(out, d.Removed...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
func unique(v []string) []string {
	if len(v) < 2 {
		return v
	}
	out := v[:0]
	for _, x := range v {
		if len(out) == 0 || out[len(out)-1] != x {
			out = append(out, x)
		}
	}
	return out
}
func isZero(v any) bool {
	switch x := v.(type) {
	case string:
		return x == ""
	case int:
		return x == 0
	case map[string]string:
		return len(x) == 0
	case bool:
		return !x
	}
	return v == nil
}

const tokenDiscipline = "Use tokens deliberately without sacrificing correctness: start at the repository root, inspect only task-relevant files, combine related reads/searches, keep individual command output below 8 KiB, never inspect .git/rolemux or provider session logs except an explicitly listed immutable baseline reference, never reread unchanged evidence already present in this session, reuse session context, do not restate inputs, and keep the result concise but complete."

const (
	implementerRoleDiscipline  = "Implementer execution discipline: before the first edit, use at most three batched pre-edit read/search tool calls; do not run git status/diff/log, repository-wide searches, repository-wide surveys, or a post-green survey; make cohesive edits; run only the narrow validation commands named by the execution packet; never run the full repository suite; wait at least 30 seconds for a background test or build instead of one-second polling; stop immediately when focused validation passes."
	reviewerRoleDiscipline     = "Reviewer execution discipline: treat the supplied delta and evidence as authoritative; inspect only changed files and their direct blast radius; use no git commands or full suite; return the validated verdict promptly."
	planReviewerRoleDiscipline = "Plan-reviewer execution discipline: validate the supplied task, plan, and work graph without redoing repository research; use the supplied evidence as authoritative and return the validated verdict promptly."
)

func plannerPrompt(text string, inputs []string) string {
	return tokenDiscipline + "\nYou are the primary research and architecture brain. First classify complexity as trivial, small, medium, large, or system, then make the smallest plan justified by that class. Trivial work must be exactly one unit containing implementation, tests, and validation. Small work may use at most two units and medium at most six; never create separate renderer, documentation, or validation-only units for a local change. Split only for a real dependency or material parallel wall-time benefit. Large/system work may use the full graph needed. Research relevant repository and external contracts once, including each direct blast radius, then produce an execution-ready plan so implementers do not rediscover the system. Every node needs a stable ID, exact non-overlapping write scope, dependencies, a self-contained execution packet with named files/symbols/contracts/steps, acceptance criteria, and validation commands. Scope is machine data: use only bare comma-separated repository paths or globs, never prose, punctuation, or annotations such as 'Write only' or '(new)'. Put truly independent nodes in the same dependency wave; add an edge when write scopes overlap. Resolve uncertainty now or return needs_input with an empty work_units array. Return only the required planner JSON envelope.\n\nTask:\n" + text + "\n\nAdditional context:\n" + strings.Join(inputs, "\n")
}
func plannerAnswerPrompt(question, answer string, reviewing bool, findings []task.Finding) string {
	return fmt.Sprintf("Continue in this same planning session. Question: %s\nAnswer: %s\nReview loop: %t\nFindings: %s\nReturn only the planner JSON envelope.", question, answer, reviewing, mustJSON(findings))
}
func plannerRevisionPrompt(plan string, findings []task.Finding, resumed bool) string {
	prompt := "Revise the plan to address every review finding. Return only the planner JSON envelope.\nFindings:\n" + mustJSON(findings)
	if resumed {
		return "Continue in this same planning session; the task and current plan are already in context.\n" + prompt
	}
	return tokenDiscipline + "\n" + prompt + "\nCurrent plan:\n" + plan
}
func planReviewPrompt(text, plan, complexity string, units []task.WorkUnit, resumed bool) string {
	if resumed {
		return planReviewerRoleDiscipline + "\nReview only the revision in this same review session; the task and previously validated evidence are unchanged. Verify that every prior finding is fixed and that each execution packet is implementation-ready. Validate the revised complexity and dependency graph, especially over-decomposition, cycles, and concurrent write-scope isolation. Scope is canonically represented as a comma-separated string of bare repository paths/globs; commas between entries are valid and must not be rejected as punctuation or ambiguity. Do not repeat repository research or restate inputs. Return only the plan_reviewer JSON envelope.\nComplexity:\n" + complexity + "\nRevised plan:\n" + plan + "\nRevised work graph:\n" + mustJSON(units)
	}
	return tokenDiscipline + "\n" + planReviewerRoleDiscipline + " Review this plan against the task and its declared complexity. Reject over-decomposition: trivial work must have one unit with implementation and tests together; small work at most two; medium at most six; validation-only units are not justified for local changes. Scope is canonically represented as a comma-separated string of bare repository paths/globs; commas between entries are valid. Reject prose or annotations attached to an entry, not valid separators. Also reject if an implementer would need to rediscover architecture, affected symbols, contracts, dependencies, blast radius, acceptance criteria, or validation commands. Verify that the graph is acyclic and same-wave scopes are disjoint. This is plan review, not code review. Return only the required plan_reviewer JSON envelope.\nTask:\n" + text + "\nComplexity:\n" + complexity + "\nPlan:\n" + plan + "\nWork graph:\n" + mustJSON(units)
}
func implementPrompt(text, plan, scope string, findings []task.Finding) string {
	return tokenDiscipline + "\n" + implementerRoleDiscipline + "\nImplement the approved execution packet in this existing shared checkout. Treat its architecture and named evidence as authoritative. Before the first edit, batch exact named files and symbols. Do not inspect unrelated tests before editing. After the first edit, read more only to resolve a concrete compiler/test failure or a named direct dependency. If the packet is insufficient or the checkout contradicts it, return one precise needs_input question instead of researching outward. You are the only source-code writer for this task. Other RoleMux tasks may run concurrently only in disjoint scopes. Change only the declared scope; do not run git mutation commands. Return only the implementer JSON envelope.\nTask:\n" + text + "\nExecution packet / approved plan:\n" + plan + "\nScope:\n" + scope + "\nFindings:\n" + mustJSON(findings)
}
func implementAnswerPrompt(question, answer string, findings []task.Finding) string {
	return implementerRoleDiscipline + "\nContinue in this same implementation session.\nQuestion: " + question + "\nAnswer: " + answer + "\nFindings:\n" + mustJSON(findings) + "\nReturn only the implementer JSON envelope."
}
func implementRevisionPrompt(findings []task.Finding) string {
	return implementerRoleDiscipline + "\nAddress every code-review finding in this same implementation session. The original execution packet and repository context are already present: inspect only the finding paths and their direct blast radius, and do not repeat broad research. Do not modify outside the task scope or run git mutation commands.\nFindings:\n" + mustJSON(findings) + "\nReturn only the implementer JSON envelope."
}
func implementReviewFixPrompt(st task.State, findings []task.Finding) string {
	if !st.IntegrationReview {
		return implementRevisionPrompt(findings)
	}
	continuation := "Start a fresh integration-fix session"
	if st.ImplementerSessionID != "" {
		continuation = "Continue in this same integration-fix session"
	}
	return implementerRoleDiscipline + "\n" + continuation + ". Address every deep integration-review finding across the affected work-unit scopes in one coherent fix. The reviewed plan, dependency graph, and current checkout are supplied; inspect only finding paths and the cross-unit blast radius needed to resolve them. Do not run git mutation commands.\nWork graph:\n" + mustJSON(st.WorkUnits) + "\nFindings:\n" + mustJSON(findings) + "\nReturn only the implementer JSON envelope."
}
func codeReviewPrompt(st task.State, resumed bool) string {
	base := st.ScopedBaseline
	label := "Task baseline-to-candidate delta"
	findings := []task.Finding(nil)
	if resumed && st.ReviewCheckpointHash != "" {
		base = st.ReviewCheckpoint
		label = "Fix delta since the previous completed review"
		findings = st.ReviewCheckpointFindings
	}
	delta := compactReviewDelta(base, st.CandidateManifest)
	if st.IntegrationReview {
		boundary := reviewerRoleDiscipline + " Deep integration-review boundary: review the combined approved work-unit deltas as one system. Check cross-unit contracts, dependency assumptions, shared types/configuration, end-to-end behavior, regressions, and the union-scope validation. This is the single broad review for the approved plan, so follow relevant blast radius across work-unit boundaries without surveying unrelated repository areas."
		if resumed && st.ReviewCheckpointHash != "" {
			return boundary + "\nVerify every previous finding against the integration fix delta and check regressions caused by those fixes. Reuse this review session and do not repeat unchanged research. Return only the code_reviewer JSON envelope.\nPrevious findings:\n" + mustJSON(findings) + "\nWork graph:\n" + mustJSON(st.WorkUnits) + "\nScope:\n" + st.Scope + "\n" + label + ":\n" + mustJSON(delta)
		}
		return tokenDiscipline + "\n" + boundary + " Read current source files directly and use the immutable baseline references only when comparison is needed. Return only the code_reviewer JSON envelope.\nTask:\n" + st.Task + "\nApproved plan:\n" + st.Plan + "\nWork graph:\n" + mustJSON(st.WorkUnits) + "\nScope:\n" + st.Scope + "\n" + label + ":\n" + mustJSON(delta)
	}
	boundary := reviewerRoleDiscipline + " Task-review boundary: review only the listed delta and its direct blast radius (direct callers, implemented interfaces, shared types/config consumers, and adjacent tests). Do not perform a whole-repository or cross-task integration audit. Do not browse external sources unless the approved plan names an external contract whose supplied evidence is insufficient. Reuse evidence already in this session and do not reread unchanged files. If a concern requires broader exploration, defer it to the one-time plan integration review rather than expanding this task review."
	if resumed && st.ReviewCheckpointHash != "" {
		return boundary + "\nVerify the previous findings against only the fix delta, plus regressions directly caused by those fixes. Do not restart the original review. Return only the code_reviewer JSON envelope.\nPrevious findings:\n" + mustJSON(findings) + "\nScope:\n" + st.Scope + "\n" + label + ":\n" + mustJSON(delta)
	}
	return tokenDiscipline + "\n" + boundary + " Read current source files directly; use only the explicitly listed immutable baseline references when comparison is needed. Do not inspect RoleMux task state. Ignore unrelated checkout changes. Return only the required code_reviewer JSON envelope.\nTask:\n" + st.Task + "\nApproved execution packet / plan:\n" + st.Plan + "\nScope:\n" + st.Scope + "\n" + label + ":\n" + mustJSON(delta)
}

func codeReviewContinuePrompt(st task.State) string {
	return reviewerRoleDiscipline + "\nContinue the interrupted task review in this same session and return a bounded verdict now. The candidate is unchanged (manifest " + st.CandidateManifestHash + "); do not reread files or repeat repository/external research already completed. Stay inside the original task delta and direct blast radius. Return only the code_reviewer JSON envelope."
}

type reviewDeltaEntry struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	BaselineRef  string `json:"baseline_ref,omitempty"`
	CandidateRef string `json:"candidate_ref,omitempty"`
}

type reviewDeltaSummary struct {
	Added   []reviewDeltaEntry `json:"added,omitempty"`
	Changed []reviewDeltaEntry `json:"changed,omitempty"`
	Removed []reviewDeltaEntry `json:"removed,omitempty"`
}

func compactReviewDelta(before, after []task.FileEntry) reviewDeltaSummary {
	beforeByPath := map[string]task.FileEntry{}
	for _, entry := range before {
		beforeByPath[entry.Path] = entry
	}
	delta := task.ManifestDelta(before, after)
	result := reviewDeltaSummary{}
	for _, entry := range delta.Added {
		result.Added = append(result.Added, compactReviewEntry(task.FileEntry{}, entry))
	}
	for _, entry := range delta.Changed {
		result.Changed = append(result.Changed, compactReviewEntry(beforeByPath[entry.Path], entry))
	}
	for _, entry := range delta.Removed {
		result.Removed = append(result.Removed, compactReviewEntry(entry, task.FileEntry{}))
	}
	return result
}

func compactReviewEntry(before, after task.FileEntry) reviewDeltaEntry {
	entry := reviewDeltaEntry{Path: after.Path, Kind: after.Kind}
	if entry.Path == "" {
		entry.Path, entry.Kind = before.Path, before.Kind
	}
	if before.Worktree.Ref != nil {
		entry.BaselineRef = before.Worktree.Ref.Path
	}
	if after.Worktree.Ref != nil {
		entry.CandidateRef = after.Worktree.Ref.Path
	}
	return entry
}
func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

var _ = os.ErrNotExist
