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
	"time"

	"github.com/basant/rolemux/internal/config"
	"github.com/basant/rolemux/internal/runner"
	"github.com/basant/rolemux/internal/task"
)

const MaxRounds = 5

type Result struct {
	State  task.State
	Status string
}

type Service struct {
	RepoRoot  string
	Store     *task.Store
	Config    config.Config
	Adapters  map[string]runner.Adapter
	Observe   func(string) ([]task.FileEntry, error)
	Capture   func([]task.FileEntry, string, string) ([]task.FileEntry, error)
	WritePlan func(string, string) error
	Now       func() time.Time
	Token     func() string
}

func New(repoRoot string, cfg config.Config, adapters map[string]runner.Adapter) *Service {
	root, _ := filepath.Abs(repoRoot)
	store := task.NewStore(root)
	worktree := task.NewWorktree(root)
	s := &Service{RepoRoot: filepath.Clean(root), Store: store, Config: cfg, Adapters: adapters, Now: time.Now, Token: task.NewToken}
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

func (s *Service) StartPlan(ctx context.Context, text, id string) (Result, error) {
	if strings.TrimSpace(text) == "" {
		return Result{}, problem("USAGE", "--task must not be empty", id, ExitUsage, false, nil)
	}
	profiles, runtimes, err := s.snapshots()
	if err != nil {
		return Result{}, problem("CONFIGURATION", err.Error(), id, ExitUsage, false, err)
	}
	if id == "" {
		id = s.generatedID()
	}
	token := s.newToken()
	prompt := plannerPrompt(text, nil)
	now := s.now()
	st := task.State{
		ID: id, RepoRoot: s.RepoRoot, Phase: task.PhasePlanned, Task: text, Prompt: prompt,
		ProfilesSnapshot: profiles, RuntimeSnapshot: runtimes, MaxRounds: MaxRounds,
		UpdatedAt: now, InFlight: &task.InFlight{Token: token, Operation: "plan_start", Role: string(runner.RolePlanner), StartedAt: now, PreviousPhase: task.PhasePlanned, Prompt: prompt, Loop: "plan_initial"},
	}
	if err := s.Store.Create(st); err != nil {
		return Result{}, classify(id, err)
	}
	return s.runPlanner(ctx, id, token, "plan_initial")
}

func (s *Service) AnswerPlan(ctx context.Context, id, answer string) (Result, error) {
	if strings.TrimSpace(answer) == "" {
		return Result{}, problem("USAGE", "--answer must not be empty", id, ExitUsage, false, nil)
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
		token := s.newToken()
		prompt := plannerAnswerPrompt(st.PendingQuestion, answer, loop == "plan_review", st.Findings)
		st.InFlight = &task.InFlight{Token: token, Operation: "plan_answer", Role: string(runner.RolePlanner), StartedAt: s.now(), KnownSession: st.PlannerSessionID != "", SessionID: st.PlannerSessionID, PreviousPhase: st.Phase, Prompt: prompt, Findings: cloneFindings(st.Findings), Loop: loop}
		st.PendingQuestion, st.PendingQuestionSource = "", ""
		return nil
	})
	if err != nil {
		return Result{}, classify(id, err)
	}
	return s.runPlanner(ctx, id, st.InFlight.Token, loop)
}

func (s *Service) ReviewPlan(ctx context.Context, id string) (Result, error) {
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight != nil {
			return task.ErrOperationInFlight
		}
		if st.Phase != task.PhasePlanned || strings.TrimSpace(st.Plan) == "" || st.PlanHash != hash(st.Plan) {
			return task.ErrInvalidPhase
		}
		token := s.newToken()
		prompt := planReviewPrompt(st.Task, st.Plan, st.PlanReviewerSessionID != "")
		st.Phase = task.PhasePlanReviewing
		st.InFlight = &task.InFlight{Token: token, Operation: "plan_review", Role: string(runner.RolePlanReviewer), StartedAt: s.now(), KnownSession: st.PlanReviewerSessionID != "", SessionID: st.PlanReviewerSessionID, PreviousPhase: task.PhasePlanned, Prompt: prompt, Loop: "plan_review"}
		return nil
	})
	if err != nil {
		return Result{}, classify(id, err)
	}
	return s.runPlanReviewer(ctx, id, st.InFlight.Token)
}

func (s *Service) Implement(ctx context.Context, id, rawScope string) (Result, error) {
	canonical, err := task.CanonicalScope(rawScope)
	if err != nil {
		return Result{}, problem("INVALID_SCOPE", err.Error(), id, ExitUsage, false, err)
	}
	current, err := s.Store.Load(id)
	if err != nil {
		return Result{}, classify(id, err)
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
		if st.Phase != task.PhasePlanApproved || st.ApprovedPlanHash == "" || st.ApprovedPlanHash != st.PlanHash {
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
		st.InFlight = &task.InFlight{Token: token, Operation: "implement", Role: string(runner.RoleImplementer), StartedAt: s.now(), KnownSession: st.ImplementerSessionID != "", SessionID: st.ImplementerSessionID, SnapshotManifest: cloneManifest(preAll), PreviousPhase: task.PhasePlanApproved, Prompt: prompt, Scope: canonical, Loop: "implement"}
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
		token := s.newToken()
		prompt := implementAnswerPrompt(st.PendingQuestion, answer, st.Findings)
		st.Phase = task.PhaseImplementing
		st.InFlight = &task.InFlight{Token: token, Operation: "implement_answer", Role: string(runner.RoleImplementer), StartedAt: s.now(), KnownSession: st.ImplementerSessionID != "", SessionID: st.ImplementerSessionID, SnapshotManifest: cloneManifest(preAll), PreviousPhase: st.ReturnPhase, Prompt: prompt, Findings: cloneFindings(st.Findings), Scope: st.Scope, Loop: loop}
		st.PendingQuestion, st.PendingQuestionSource = "", ""
		return nil
	})
	if err != nil {
		return Result{}, classify(id, err)
	}
	return s.runImplementer(ctx, id, st.InFlight.Token, loop)
}

func (s *Service) ReviewCode(ctx context.Context, id string) (Result, error) {
	current, err := s.Store.Load(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	if current.Phase != task.PhaseImplementationReady || current.Scope == "" {
		return Result{}, classify(id, task.ErrInvalidPhase)
	}
	before, err := s.Observe(current.Scope)
	if err != nil {
		return Result{}, classify(id, err)
	}
	st, err := s.beginCodeReview(id, before, "code_review", "")
	if err != nil {
		return Result{}, err
	}
	return s.runCodeReviewer(ctx, id, st.InFlight.Token)
}

func (s *Service) Retry(ctx context.Context, id string) (Result, error) {
	current, err := s.Store.Load(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	if current.InFlight != nil || current.Retry == nil || (current.Phase != task.PhaseFailed && current.Phase != task.PhaseReviewNeeded) {
		return Result{}, classify(id, task.ErrInvalidPhase)
	}
	retry := *current.Retry
	var barrier []task.FileEntry
	if retry.Role == string(runner.RoleCodeReviewer) {
		barrier, err = s.Observe(current.Scope)
		if err != nil {
			return Result{}, classify(id, err)
		}
	} else {
		barrier = cloneManifest(retry.SnapshotManifest)
	}
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight != nil || st.Retry == nil {
			return task.ErrOperationInFlight
		}
		token := s.newToken()
		st.Phase = retryPhase(retry)
		st.InFlight = &task.InFlight{Token: token, Operation: retry.Operation, Role: retry.Role, StartedAt: s.now(), KnownSession: retry.KnownSession, SessionID: retry.SessionID, SnapshotManifest: cloneManifest(barrier), PreviousPhase: retry.PreviousPhase, Prompt: retry.Prompt, Findings: cloneFindings(retry.Findings), Scope: retry.Scope, Loop: retry.Loop}
		st.Retry = nil
		return nil
	})
	if err != nil {
		return Result{}, classify(id, err)
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
	st, err := s.Store.Load(id)
	return st, classify(id, err)
}

func (s *Service) List() ([]task.State, error) { return s.Store.List() }

func (s *Service) runPlanner(ctx context.Context, id, token, loop string) (Result, error) {
	resp, err := s.call(ctx, id, token, runner.RolePlanner)
	if err != nil {
		return Result{}, err
	}
	env, err := envelope(resp, runner.RolePlanner)
	if err != nil {
		return Result{}, s.fail(id, token, err)
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
			st.InFlight, st.Retry = nil, nil
			return nil
		})
		if saveErr != nil {
			return Result{}, classify(id, saveErr)
		}
		return Result{State: st, Status: "needs_input"}, problem("NEEDS_INPUT", env.Question, id, ExitNeedsInput, true, nil)
	}
	if err := s.WritePlan(id, env.Plan); err != nil {
		return Result{}, s.fail(id, token, err)
	}
	st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		st.Plan, st.PlanHash = env.Plan, hash(env.Plan)
		st.Prompt = st.InFlight.Prompt
		st.PendingAnswer = ""
		if loop == "plan_review" {
			st.Phase = task.PhasePlanReviewing
			st.InFlight.Operation = "plan_review"
			st.InFlight.Role = string(runner.RolePlanReviewer)
			st.InFlight.KnownSession = st.PlanReviewerSessionID != ""
			st.InFlight.SessionID = st.PlanReviewerSessionID
			st.InFlight.Prompt = planReviewPrompt(st.Task, st.Plan, st.PlanReviewerSessionID != "")
			st.InFlight.Findings = nil
			st.InFlight.StartedAt = s.now()
		} else {
			st.Phase = task.PhasePlanned
			st.InFlight, st.Retry = nil, nil
		}
		return nil
	})
	if saveErr != nil {
		return Result{}, classify(id, saveErr)
	}
	if loop == "plan_review" {
		return s.runPlanReviewer(ctx, id, token)
	}
	return Result{State: st, Status: "planned"}, nil
}

func (s *Service) runPlanReviewer(ctx context.Context, id, token string) (Result, error) {
	before, err := s.Store.Load(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	reviewedHash := before.PlanHash
	resp, err := s.call(ctx, id, token, runner.RolePlanReviewer)
	if err != nil {
		return Result{}, err
	}
	env, err := envelope(resp, runner.RolePlanReviewer)
	if err != nil {
		return Result{}, s.fail(id, token, err)
	}
	var exhausted bool
	st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		if st.PlanHash != reviewedHash {
			return task.ErrStaleOperation
		}
		st.PlanRound++
		st.Round = st.PlanRound
		st.Findings = cloneFindings(env.Findings)
		if env.Verdict == "approved" {
			st.ApprovedPlanHash = st.PlanHash
			st.Phase = task.PhasePlanApproved
			st.InFlight, st.Retry = nil, nil
			return nil
		}
		if st.PlanRound >= maxRounds(st) {
			exhausted = true
			st.Phase = task.PhaseFailed
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
		return nil
	})
	if saveErr != nil {
		return Result{}, classify(id, saveErr)
	}
	if exhausted {
		return Result{State: st, Status: "exhausted"}, problem("REVIEW_EXHAUSTED", "plan review exhausted five rounds", id, ExitExhausted, false, nil)
	}
	if env.Verdict == "approved" {
		return Result{State: st, Status: "approved"}, nil
	}
	return s.runPlanner(ctx, id, token, "plan_review")
}

func (s *Service) runImplementer(ctx context.Context, id, token, loop string) (Result, error) {
	resp, err := s.call(ctx, id, token, runner.RoleImplementer)
	if err != nil {
		return Result{}, err
	}
	env, err := envelope(resp, runner.RoleImplementer)
	if err != nil {
		return Result{}, s.fail(id, token, err)
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
			st.InFlight, st.Retry = nil, nil
			return nil
		})
		if saveErr != nil {
			return Result{}, classify(id, saveErr)
		}
		return Result{State: st, Status: "needs_input"}, problem("NEEDS_INPUT", env.Question, id, ExitNeedsInput, true, nil)
	}
	current, loadErr := s.Store.Load(id)
	if loadErr != nil {
		return Result{}, classify(id, loadErr)
	}
	candidate, observeErr := s.Observe(current.Scope)
	if observeErr != nil {
		return Result{}, s.fail(id, token, observeErr)
	}
	label := "candidate-" + shortToken(token) + "-" + fmt.Sprint(current.CodeRound)
	candidate, observeErr = s.Capture(candidate, label, id)
	if observeErr != nil {
		return Result{}, s.fail(id, token, observeErr)
	}
	afterAll, observeErr := s.Observe("**")
	if observeErr != nil {
		return Result{}, s.fail(id, token, observeErr)
	}
	preAll := cloneManifest(current.InFlight.SnapshotManifest)
	outside := outsideScope(task.ManifestDelta(preAll, afterAll).Paths(), current.Scope)
	st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		st.CandidateManifest = cloneManifest(candidate)
		st.CandidateManifestHash = task.HashManifest(candidate)
		st.ChangeManifest = changeEntries(st.ScopedBaseline, candidate)
		if len(outside) > 0 {
			st.Advisories = mergeDiagnostics(st.Advisories, []task.Diagnostic{{Code: "OUT_OF_SCOPE_CHANGE", Severity: "warning", Message: "files outside this task scope changed while the implementer was running", TaskID: id, Paths: outside}})
		}
		st.PendingAnswer = ""
		if loop == "code_review" {
			st.Phase = task.PhaseCodeReviewing
			st.InFlight.Operation = "code_review"
			st.InFlight.Role = string(runner.RoleCodeReviewer)
			st.InFlight.KnownSession = st.CodeReviewerSessionID != ""
			st.InFlight.SessionID = st.CodeReviewerSessionID
			st.InFlight.SnapshotManifest = cloneManifest(candidate)
			st.InFlight.Prompt = codeReviewPrompt(*st, st.CodeReviewerSessionID != "")
			st.InFlight.Findings = nil
			st.InFlight.StartedAt = s.now()
		} else {
			st.Phase = task.PhaseImplementationReady
			st.InFlight, st.Retry = nil, nil
		}
		return nil
	})
	if saveErr != nil {
		return Result{}, classify(id, saveErr)
	}
	if loop == "code_review" {
		return s.runCodeReviewer(ctx, id, token)
	}
	return Result{State: st, Status: "implementation_ready"}, nil
}

func (s *Service) runCodeReviewer(ctx context.Context, id, token string) (Result, error) {
	beforeState, err := s.Store.Load(id)
	if err != nil {
		return Result{}, classify(id, err)
	}
	barrier := cloneManifest(beforeState.InFlight.SnapshotManifest)
	resp, callErr := s.call(ctx, id, token, runner.RoleCodeReviewer)
	if callErr != nil {
		return Result{}, callErr
	}
	after, observeErr := s.Observe(beforeState.Scope)
	if observeErr != nil {
		return Result{}, s.fail(id, token, observeErr)
	}
	if task.ManifestChanged(barrier, after) {
		label := "candidate-review-change-" + shortToken(token) + "-" + fmt.Sprint(beforeState.CodeRound)
		captured, captureErr := s.Capture(after, label, id)
		if captureErr != nil {
			return Result{}, s.fail(id, token, captureErr)
		}
		st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
			st.CandidateManifest = cloneManifest(captured)
			st.CandidateManifestHash = task.HashManifest(captured)
			st.ChangeManifest = changeEntries(st.ScopedBaseline, captured)
			retry := retryFrom(st, st.InFlight)
			retry.Prompt = "The scoped candidate changed while your previous review was running. Discard that verdict and review this current candidate.\n" + codeReviewPrompt(*st, true)
			st.Phase = task.PhaseReviewNeeded
			st.Retry = retry
			st.InFlight = nil
			return nil
		})
		if saveErr != nil {
			return Result{}, classify(id, saveErr)
		}
		return Result{State: st, Status: "review_needed"}, problem("REVIEW_NEEDED", "scoped files changed during code review", id, ExitReviewNeeded, true, task.ErrScopeChanged)
	}
	env, envErr := envelope(resp, runner.RoleCodeReviewer)
	if envErr != nil {
		return Result{}, s.fail(id, token, envErr)
	}
	var exhausted bool
	st, saveErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
		st.CodeRound++
		st.Round = st.CodeRound
		st.Findings = cloneFindings(env.Findings)
		if env.Verdict == "approved" {
			st.ApprovedManifestHash = task.HashManifest(after)
			st.Phase = task.PhaseApproved
			st.InFlight, st.Retry = nil, nil
			return nil
		}
		if st.CodeRound >= maxRounds(st) {
			exhausted = true
			st.Phase = task.PhaseFailed
			st.InFlight, st.Retry = nil, nil
			return nil
		}
		st.Phase = task.PhaseImplementing
		st.InFlight.Operation = "code_revision"
		st.InFlight.Role = string(runner.RoleImplementer)
		st.InFlight.KnownSession = st.ImplementerSessionID != ""
		st.InFlight.SessionID = st.ImplementerSessionID
		st.InFlight.SnapshotManifest = nil
		st.InFlight.Prompt = implementRevisionPrompt(env.Findings)
		st.InFlight.Findings = cloneFindings(env.Findings)
		st.InFlight.StartedAt = s.now()
		return nil
	})
	if saveErr != nil {
		return Result{}, classify(id, saveErr)
	}
	if exhausted {
		return Result{State: st, Status: "exhausted"}, problem("REVIEW_EXHAUSTED", "code review exhausted five rounds", id, ExitExhausted, false, nil)
	}
	if env.Verdict == "approved" {
		return Result{State: st, Status: "approved"}, nil
	}
	preAll, observeErr := s.Observe("**")
	if observeErr != nil {
		return Result{}, s.fail(id, token, observeErr)
	}
	_, saveErr = s.Store.UpdateOwned(id, token, func(st *task.State) error {
		st.InFlight.SnapshotManifest = cloneManifest(preAll)
		return nil
	})
	if saveErr != nil {
		return Result{}, classify(id, saveErr)
	}
	return s.runImplementer(ctx, id, token, "code_review")
}

func (s *Service) beginCodeReview(id string, before []task.FileEntry, loop, prompt string) (task.State, error) {
	st, err := s.Store.Update(id, func(st *task.State) error {
		if st.InFlight != nil {
			return task.ErrOperationInFlight
		}
		if st.Phase != task.PhaseImplementationReady || st.Scope == "" || st.CandidateManifestHash == "" {
			return task.ErrInvalidPhase
		}
		if prompt == "" {
			prompt = codeReviewPrompt(*st, st.CodeReviewerSessionID != "")
		}
		token := s.newToken()
		st.Phase = task.PhaseCodeReviewing
		st.InFlight = &task.InFlight{Token: token, Operation: "code_review", Role: string(runner.RoleCodeReviewer), StartedAt: s.now(), KnownSession: st.CodeReviewerSessionID != "", SessionID: st.CodeReviewerSessionID, SnapshotManifest: cloneManifest(before), PreviousPhase: task.PhaseImplementationReady, Prompt: prompt, Scope: st.Scope, Loop: loop}
		return nil
	})
	return st, classify(id, err)
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
	request := runner.Request{Role: role, Operation: st.InFlight.Operation, Prompt: st.InFlight.Prompt, Model: profile.Model, Effort: profile.Effort, RepoRoot: s.RepoRoot, Scope: st.InFlight.Scope, SessionID: st.InFlight.SessionID, Resume: st.InFlight.KnownSession && st.InFlight.SessionID != "", Runtime: st.RuntimeSnapshot[string(role)], Sandbox: "read-only"}
	if role == runner.RoleImplementer {
		request.Sandbox = "workspace-write"
	}
	resp, callErr := adapter.Run(ctx, request, runner.Callbacks{SessionStarted: func(session string) error {
		if strings.TrimSpace(session) == "" {
			return runner.ErrMissingSession
		}
		_, updateErr := s.Store.UpdateOwned(id, token, func(st *task.State) error {
			setSession(st, role, session)
			st.InFlight.KnownSession, st.InFlight.SessionID = true, session
			return nil
		})
		return updateErr
	}})
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
		st.InFlight = nil
		return nil
	})
	if err != nil {
		return classify(id, err)
	}
	return problem(code, message, id, ExitAction, retryable && known, cause)
}

func (s *Service) snapshots() (map[string]task.ProfileSnapshot, map[string]task.RuntimeSnapshot, error) {
	effective, err := s.Config.EffectiveProfiles()
	if err != nil {
		return nil, nil, err
	}
	profiles := map[string]task.ProfileSnapshot{}
	runtimes := map[string]task.RuntimeSnapshot{}
	for _, role := range []string{config.RolePlanner, config.RolePlanReviewer, config.RoleImplementer, config.RoleCodeReviewer} {
		p, providerConfig := s.Config.ResolveProfile(effective[role])
		if err := config.ValidateProfile(p); err != nil {
			return nil, nil, fmt.Errorf("profile %s: %w", role, err)
		}
		profiles[role] = task.ProfileSnapshot{Provider: p.Provider, Model: p.Model, Effort: p.Effort}
		runtimes[role] = RuntimeSnapshot(p.Provider, providerConfig)
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
		for key, value := range map[string]any{"type": p.Type, "wire_api": p.WireAPI, "transport": p.Transport, "base_url": p.BaseURL, "model_id": p.ModelID, "wire_model": p.WireModel, "max_prompt_tokens": p.MaxPromptTokens, "max_output_tokens": p.MaxOutputTokens, "headers": p.Headers, "bearer_token_env": p.BearerTokenEnv} {
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
func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func maxRounds(st *task.State) int {
	if st.MaxRounds > 0 {
		return st.MaxRounds
	}
	return MaxRounds
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
		if other.ID != id && other.Scope != "" && task.ScopesOverlap(scope, other.Scope) {
			out = append(out, task.Diagnostic{Code: "SCOPE_OVERLAP", Severity: "warning", Message: "scope may overlap another task; the orchestrator owns writer coordination", TaskID: other.ID})
		}
	}
	return out
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
func outsideScope(paths []string, scope string) []string {
	var out []string
	for _, p := range paths {
		if !task.ScopeMatches(scope, p) {
			out = append(out, p)
		}
	}
	return out
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

const tokenDiscipline = "Use tokens deliberately without sacrificing correctness: inspect only task-relevant files, reuse session context, do not restate inputs, and keep the result concise but complete."

func plannerPrompt(text string, inputs []string) string {
	return tokenDiscipline + "\nCreate an implementation plan for this task. Return only the required planner JSON envelope.\n\nTask:\n" + text + "\n\nAdditional context:\n" + strings.Join(inputs, "\n")
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
func planReviewPrompt(text, plan string, resumed bool) string {
	if resumed {
		return "Review the revised plan in this same review session; the task is unchanged and already in context. Do not restate it. Return only the plan_reviewer JSON envelope.\nRevised plan:\n" + plan
	}
	return tokenDiscipline + "\nReview this plan against the task. Return only the required plan_reviewer JSON envelope.\nTask:\n" + text + "\nPlan:\n" + plan
}
func implementPrompt(text, plan, scope string, findings []task.Finding) string {
	return tokenDiscipline + "\nImplement the approved plan in this existing shared checkout. Change only the declared scope; do not run git mutation commands. Return only the implementer JSON envelope.\nTask:\n" + text + "\nPlan:\n" + plan + "\nScope:\n" + scope + "\nFindings:\n" + mustJSON(findings)
}
func implementAnswerPrompt(question, answer string, findings []task.Finding) string {
	return "Continue in this same implementation session.\nQuestion: " + question + "\nAnswer: " + answer + "\nFindings:\n" + mustJSON(findings) + "\nReturn only the implementer JSON envelope."
}
func implementRevisionPrompt(findings []task.Finding) string {
	return "Address every code-review finding in this same implementation session. Do not modify outside the task scope or run git mutation commands.\nFindings:\n" + mustJSON(findings) + "\nReturn only the implementer JSON envelope."
}
func codeReviewPrompt(st task.State, resumed bool) string {
	delta := task.ManifestDelta(st.ScopedBaseline, st.CandidateManifest)
	if resumed {
		return "Continue in this same code-review session. The task and approved plan are unchanged; review only the current scoped candidate and unresolved issues. Return only the code_reviewer JSON envelope.\nScope:\n" + st.Scope + "\nCurrent scoped delta:\n" + mustJSON(delta)
	}
	return tokenDiscipline + "\nReview only this task's scoped baseline-to-candidate change. Ignore unrelated checkout changes. Return only the required code_reviewer JSON envelope.\nTask:\n" + st.Task + "\nApproved plan:\n" + st.Plan + "\nScope:\n" + st.Scope + "\nScoped delta:\n" + mustJSON(delta)
}
func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

var _ = os.ErrNotExist
