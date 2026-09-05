// Package task contains durable task state, exact repository observations, and
// short ownership-safe task locks. Provider calls deliberately do not live in
// this package: callers take a token, release the lock, call the provider, and
// use the token for the compare-and-swap completion.
package task

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	PhasePlanned             = "planned"
	PhasePlanReviewing       = "plan_reviewing"
	PhasePlanApproved        = "plan_approved"
	PhaseImplementing        = "implementing"
	PhaseNeedsInput          = "needs_input"
	PhaseImplementationReady = "implementation_ready"
	PhaseCodeReviewing       = "code_reviewing"
	PhaseApproved            = "approved"
	PhaseReviewNeeded        = "review_needed"
	PhaseFailed              = "failed"
)

var (
	ErrNotFound          = errors.New("task not found")
	ErrTaskExists        = errors.New("task already exists")
	ErrInvalidTaskID     = errors.New("invalid task id")
	ErrOperationInFlight = errors.New("task operation is already in flight")
	ErrStaleOperation    = errors.New("stale task operation")
	ErrInvalidPhase      = errors.New("invalid task phase")
	ErrScopeChanged      = errors.New("task scope changed during review barrier")
	ErrNotGitRepository  = errors.New("not inside a git worktree")
)

// ContentRef points at an immutable, content-addressed copy in private
// RoleMux state. Baseline and candidate roots are separate by construction.
type ContentRef struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
}

type ContentState struct {
	Present bool        `json:"present"`
	Mode    string      `json:"mode,omitempty"`
	Hash    string      `json:"hash,omitempty"`
	Size    int64       `json:"size,omitempty"`
	Ref     *ContentRef `json:"ref,omitempty"`
}

type IndexStage struct {
	Stage int         `json:"stage"`
	Mode  string      `json:"mode"`
	Blob  string      `json:"blob"`
	Ref   *ContentRef `json:"ref,omitempty"`
}

type IndexState struct {
	Present bool         `json:"present"`
	Mode    string       `json:"mode,omitempty"`
	Blob    string       `json:"blob,omitempty"`
	Stages  []int        `json:"stages,omitempty"`
	Ref     *ContentRef  `json:"ref,omitempty"`
	Entries []IndexStage `json:"entries,omitempty"`
}

// FileEntry is an exact worktree/index observation. Status is only a derived
// advisory label and is intentionally excluded from HashManifest. The legacy
// top-level fields are retained in JSON for small embedders that used the
// first preview API; new code should use Worktree and Index.
type FileEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Status string `json:"status,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Size   int64  `json:"size,omitempty"`

	Worktree ContentState `json:"worktree,omitempty"`
	Index    IndexState   `json:"index,omitempty"`
}

type Finding struct {
	Severity string `json:"severity,omitempty"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// Diagnostic is advisory. It never causes RoleMux to stash, revert, commit,
// merge, or globally serialize an unrelated task.
type Diagnostic struct {
	Code     string   `json:"code"`
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
	TaskID   string   `json:"task_id,omitempty"`
	Paths    []string `json:"paths,omitempty"`
}

// TokenUsage is accumulated per workflow role. Token fields are populated
// only when the provider reports them; requests and prompt bytes are measured
// by RoleMux for every provider invocation.
type TokenUsage struct {
	Requests          int64 `json:"requests"`
	PromptBytes       int64 `json:"prompt_bytes"`
	InputTokens       int64 `json:"input_tokens,omitempty"`
	CachedInputTokens int64 `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int64 `json:"cache_write_tokens,omitempty"`
	OutputTokens      int64 `json:"output_tokens,omitempty"`
	ReasoningTokens   int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens       int64 `json:"total_tokens,omitempty"`
}

func (u TokenUsage) Empty() bool {
	return u.Requests == 0 && u.PromptBytes == 0 && u.InputTokens == 0 &&
		u.CachedInputTokens == 0 && u.CacheWriteTokens == 0 &&
		u.OutputTokens == 0 && u.ReasoningTokens == 0 && u.TotalTokens == 0
}

func (u *TokenUsage) Add(turn TokenUsage) {
	u.Requests += turn.Requests
	u.PromptBytes += turn.PromptBytes
	u.InputTokens += turn.InputTokens
	u.CachedInputTokens += turn.CachedInputTokens
	u.CacheWriteTokens += turn.CacheWriteTokens
	u.OutputTokens += turn.OutputTokens
	u.ReasoningTokens += turn.ReasoningTokens
	u.TotalTokens += turn.TotalTokens
}

type ProfileSnapshot struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
}

// RuntimeSnapshot stores routing metadata but never a credential value.
type RuntimeSnapshot struct {
	ProviderType string         `json:"provider_type"`
	ProviderID   string         `json:"provider_id,omitempty"`
	Endpoint     string         `json:"endpoint,omitempty"`
	WireAPI      string         `json:"wire_api,omitempty"`
	AuthEnvRefs  []string       `json:"auth_env_refs,omitempty"`
	Auth         map[string]any `json:"auth,omitempty"`
	CLIPath      string         `json:"cli_path,omitempty"`
	SDKSettings  map[string]any `json:"sdk_settings,omitempty"`
}

type InFlight struct {
	Token            string      `json:"token"`
	Operation        string      `json:"operation"`
	Role             string      `json:"role"`
	StartedAt        time.Time   `json:"started_at"`
	KnownSession     bool        `json:"known_session"`
	SessionID        string      `json:"session_id,omitempty"`
	SnapshotManifest []FileEntry `json:"snapshot_manifest,omitempty"`
	PreviousPhase    string      `json:"previous_phase,omitempty"`
	Prompt           string      `json:"prompt,omitempty"`
	Findings         []Finding   `json:"findings,omitempty"`
	Scope            string      `json:"scope,omitempty"`
	Loop             string      `json:"loop,omitempty"`
}

type RetryState struct {
	Token            string      `json:"token"`
	Operation        string      `json:"operation"`
	Role             string      `json:"role"`
	PreviousPhase    string      `json:"previous_phase"`
	Prompt           string      `json:"prompt,omitempty"`
	Findings         []Finding   `json:"findings,omitempty"`
	Scope            string      `json:"scope,omitempty"`
	SessionID        string      `json:"session_id,omitempty"`
	KnownSession     bool        `json:"known_session"`
	Loop             string      `json:"loop,omitempty"`
	SnapshotManifest []FileEntry `json:"snapshot_manifest,omitempty"`
	CreatedAt        time.Time   `json:"created_at"`
}

// State is the durable task record. Scope and its exact baseline are first
// established atomically by the first implement invocation.
type State struct {
	ID                   string `json:"id"`
	RepoRoot             string `json:"repo_root"`
	Phase                string `json:"phase"`
	Round                int    `json:"round"` // compatibility alias; plan/code are authoritative.
	Task                 string `json:"task,omitempty"`
	Prompt               string `json:"prompt,omitempty"`
	Plan                 string `json:"plan,omitempty"`
	PlanHash             string `json:"plan_hash,omitempty"`
	ApprovedPlanHash     string `json:"approved_plan_hash,omitempty"`
	ApprovedManifestHash string `json:"approved_manifest_hash,omitempty"`

	PlannerSessionID      string `json:"planner_session_id,omitempty"`
	PlanReviewerSessionID string `json:"plan_reviewer_session_id,omitempty"`
	ImplementerSessionID  string `json:"implementer_session_id,omitempty"`
	CodeReviewerSessionID string `json:"code_reviewer_session_id,omitempty"`

	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`

	Scope                 string                     `json:"scope,omitempty"`
	ScopeSpecHash         string                     `json:"scope_spec_hash,omitempty"`
	ScopedBaseline        []FileEntry                `json:"scoped_baseline_manifest,omitempty"`
	ScopedBaselineHash    string                     `json:"scoped_baseline_manifest_hash,omitempty"`
	CandidateManifest     []FileEntry                `json:"candidate_manifest,omitempty"`
	CandidateManifestHash string                     `json:"candidate_manifest_hash,omitempty"`
	ChangeManifest        []FileEntry                `json:"change_manifest,omitempty"`
	ProfilesSnapshot      map[string]ProfileSnapshot `json:"profiles_snapshot,omitempty"`
	RuntimeSnapshot       map[string]RuntimeSnapshot `json:"runtime_snapshot,omitempty"`
	MaxRounds             int                        `json:"max_rounds,omitempty"`
	PlanRound             int                        `json:"plan_round,omitempty"`
	CodeRound             int                        `json:"code_round,omitempty"`
	PendingQuestion       string                     `json:"pending_question,omitempty"`
	PendingQuestionSource string                     `json:"pending_question_source,omitempty"`
	PendingAnswer         string                     `json:"pending_answer,omitempty"`
	PromptInputs          []string                   `json:"prompt_inputs,omitempty"`
	ReturnPhase           string                     `json:"return_phase,omitempty"`
	InterruptedLoop       string                     `json:"interrupted_loop,omitempty"`
	Findings              []Finding                  `json:"findings,omitempty"`
	Advisories            []Diagnostic               `json:"advisories,omitempty"`
	Diagnostics           []string                   `json:"diagnostics,omitempty"`
	Usage                 map[string]TokenUsage      `json:"usage,omitempty"`
	InFlight              *InFlight                  `json:"in_flight,omitempty"`
	Retry                 *RetryState                `json:"retry,omitempty"`
}

func StateFingerprint(st State) string {
	st.UpdatedAt = time.Time{}
	b, _ := json.Marshal(st)
	return digestBytes(b)
}

type Store struct {
	Root string
	Dir  string
	err  error
}

func NewStore(repoRoot string) *Store {
	root, err := DiscoverRepository(repoRoot)
	if err != nil {
		return &Store{err: err}
	}
	gitDir, err := gitPath(root, "rolemux")
	if err != nil {
		return &Store{Root: root, err: fmt.Errorf("discover private git state: %w", err)}
	}
	return &Store{Root: root, Dir: filepath.Join(gitDir, "tasks")}
}

func NewStoreAt(dir string) *Store {
	abs, _ := filepath.Abs(dir)
	return &Store{Root: filepath.Clean(abs), Dir: filepath.Clean(abs)}
}

// DiscoverRepository is the public repository boundary. It resolves nested
// working directories to the top level and fails closed for bare repositories
// and paths outside a Git worktree.
func DiscoverRepository(start string) (string, error) {
	if strings.TrimSpace(start) == "" {
		start = "."
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotGitRepository, err)
	}
	inside, err := gitOutput(filepath.Clean(abs), "rev-parse", "--is-inside-work-tree")
	if err != nil || inside != "true" {
		return "", fmt.Errorf("%w: %s", ErrNotGitRepository, filepath.Clean(abs))
	}
	root, err := gitOutput(filepath.Clean(abs), "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return "", fmt.Errorf("%w: %s", ErrNotGitRepository, filepath.Clean(abs))
	}
	resolved, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotGitRepository, err)
	}
	return filepath.Clean(resolved), nil
}

func gitPath(root, name string) (string, error) {
	out, err := gitOutputBytes(root, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return "", errors.New("git returned empty private path")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return filepath.Clean(p), nil
}

func validID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." || filepath.Base(id) != id {
		return false
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (s *Store) ensure() error {
	if s == nil {
		return errors.New("nil task store")
	}
	if s.err != nil {
		return s.err
	}
	if s.Dir == "" {
		return ErrNotGitRepository
	}
	return os.MkdirAll(s.Dir, 0o700)
}

func (s *Store) path(id string) (string, error) {
	if err := s.ensure(); err != nil {
		return "", err
	}
	if !validID(id) {
		return "", fmt.Errorf("%w %q", ErrInvalidTaskID, id)
	}
	return filepath.Join(s.Dir, id+".json"), nil
}

func (s *Store) lockPath(id string) (string, error) {
	if err := s.ensure(); err != nil {
		return "", err
	}
	if !validID(id) {
		return "", fmt.Errorf("%w %q", ErrInvalidTaskID, id)
	}
	return filepath.Join(s.Dir, id+".lock"), nil
}

type AdvisoryLock struct{ file *os.File }

// Lock holds only the requested task lock. The lock file is never unlinked,
// avoiding the classic create/delete race where one process removes another's
// lock. Callers must release it promptly and never across provider calls.
func (s *Store) Lock(id string) (*AdvisoryLock, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	name, err := s.lockPath(id)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &AdvisoryLock{file: f}, nil
}

func (l *AdvisoryLock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *Store) loadUnlocked(id string) (State, error) {
	name, err := s.path(id)
	if err != nil {
		return State{}, err
	}
	b, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, fmt.Errorf("%w %q", ErrNotFound, id)
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("decode task %s: %w", id, err)
	}
	return st, nil
}

func (s *Store) Load(id string) (State, error) {
	lock, err := s.Lock(id)
	if err != nil {
		return State{}, err
	}
	defer lock.Unlock()
	return s.loadUnlocked(id)
}

func (s *Store) Save(st State) error {
	if !validID(st.ID) {
		return fmt.Errorf("%w %q", ErrInvalidTaskID, st.ID)
	}
	lock, err := s.Lock(st.ID)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return s.writeUnlocked(st)
}

// Create rejects duplicate task IDs while holding the same task-scoped lock
// used by all subsequent state mutations.
func (s *Store) Create(st State) error {
	if !validID(st.ID) {
		return fmt.Errorf("%w %q", ErrInvalidTaskID, st.ID)
	}
	lock, err := s.Lock(st.ID)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	if _, err := s.loadUnlocked(st.ID); err == nil {
		return fmt.Errorf("%w %q", ErrTaskExists, st.ID)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	return s.writeUnlocked(st)
}

func (s *Store) SaveOwned(st State, token string) error {
	if token == "" || st.InFlight == nil || st.InFlight.Token != token {
		return ErrStaleOperation
	}
	lock, err := s.Lock(st.ID)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	current, err := s.loadUnlocked(st.ID)
	if err != nil {
		return err
	}
	if current.InFlight == nil || current.InFlight.Token != token {
		return ErrStaleOperation
	}
	return s.writeUnlocked(st)
}

// Update performs a short atomic state mutation under the task-scoped lock.
// The callback must not invoke providers or inspect the worktree.
func (s *Store) Update(id string, update func(*State) error) (State, error) {
	if update == nil {
		return State{}, errors.New("nil task update")
	}
	lock, err := s.Lock(id)
	if err != nil {
		return State{}, err
	}
	defer lock.Unlock()
	st, err := s.loadUnlocked(id)
	if err != nil {
		return State{}, err
	}
	if err := update(&st); err != nil {
		return State{}, err
	}
	if err := s.writeUnlocked(st); err != nil {
		return State{}, err
	}
	return st, nil
}

// UpdateOwned performs a short atomic token/CAS mutation. The callback must
// not perform provider or repository operations.
func (s *Store) UpdateOwned(id, token string, update func(*State) error) (State, error) {
	lock, err := s.Lock(id)
	if err != nil {
		return State{}, err
	}
	defer lock.Unlock()
	st, err := s.loadUnlocked(id)
	if err != nil {
		return State{}, err
	}
	if st.InFlight == nil || token == "" || st.InFlight.Token != token {
		return State{}, ErrStaleOperation
	}
	if err := update(&st); err != nil {
		return State{}, err
	}
	if err := s.writeUnlocked(st); err != nil {
		return State{}, err
	}
	return st, nil
}

func (s *Store) writeUnlocked(st State) error {
	if err := s.ensure(); err != nil {
		return err
	}
	st.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(s.Dir, ".task-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	name, err := s.path(st.ID)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpName, name); err != nil {
		return err
	}
	return syncDir(s.Dir)
}

func (s *Store) Delete(id string) error {
	lock, err := s.Lock(id)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	name, err := s.path(id)
	if err != nil {
		return err
	}
	err = os.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w %q", ErrNotFound, id)
	}
	return err
}

func (s *Store) List() ([]State, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	states := make([]State, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, readErr := os.ReadFile(filepath.Join(s.Dir, entry.Name()))
		if readErr != nil {
			continue
		}
		var st State
		if json.Unmarshal(b, &st) == nil && validID(st.ID) {
			states = append(states, st)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	return states, nil
}

// Worktree exposes repository-relative exact observations.
type Worktree struct{ Root string }

func NewWorktree(root string) *Worktree {
	abs, _ := filepath.Abs(root)
	return &Worktree{Root: filepath.Clean(abs)}
}

func (w *Worktree) Head() (string, error) { return gitOutput(w.Root, "rev-parse", "HEAD") }

func (w *Worktree) Paths() ([]string, error) {
	tracked, err := gitOutputBytes(w.Root, "ls-files", "-co", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{})
	for _, p := range splitNUL(tracked) {
		if p != "" {
			set[p] = struct{}{}
		}
	}
	for _, p := range w.deletedPaths() {
		if p != "" {
			set[p] = struct{}{}
		}
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

func (w *Worktree) deletedPaths() []string {
	var result []string
	for _, args := range [][]string{
		{"diff", "--name-only", "--diff-filter=D", "-z"},
		{"diff", "--cached", "--name-only", "--diff-filter=D", "-z"},
	} {
		b, err := gitOutputBytes(w.Root, args...)
		if err == nil {
			result = append(result, splitNUL(b)...)
		}
	}
	return result
}

type indexEntry struct {
	Mode  string
	Blob  string
	Stage int
	Path  string
}

func (w *Worktree) indexEntries() map[string][]indexEntry {
	result := make(map[string][]indexEntry)
	b, err := gitOutputBytes(w.Root, "ls-files", "--stage", "-z")
	if err != nil {
		return result
	}
	for _, item := range splitNUL(b) {
		if item == "" {
			continue
		}
		space := strings.IndexByte(item, ' ')
		tab := strings.IndexByte(item, '\t')
		if space <= 0 || tab <= space {
			continue
		}
		mode := item[:space]
		meta := strings.Fields(item[space+1 : tab])
		if len(meta) < 2 {
			continue
		}
		stage, _ := strconv.Atoi(meta[1])
		p := filepath.ToSlash(item[tab+1:])
		result[p] = append(result[p], indexEntry{Mode: mode, Blob: meta[0], Stage: stage, Path: p})
	}
	return result
}

func (w *Worktree) statuses() map[string]string {
	result := make(map[string]string)
	b, err := gitOutputBytes(w.Root, "status", "--porcelain=v1", "-z")
	if err != nil {
		return result
	}
	items := splitNUL(b)
	for i := 0; i < len(items); i++ {
		item := items[i]
		if len(item) < 4 {
			continue
		}
		code := item[:2]
		p := filepath.ToSlash(item[3:])
		result[p] = code
		if strings.Contains(code, "R") || strings.Contains(code, "C") {
			if i+1 < len(items) && items[i+1] != "" {
				result[filepath.ToSlash(items[i+1])] = code
				i++
			}
		}
	}
	return result
}

func (w *Worktree) Manifest() ([]FileEntry, error) { return w.ManifestForScope("**") }

// ManifestForScope builds a HEAD-independent projection. It observes both
// worktree bytes and index mode/blob/stage entries, then adds structural
// ancestors required to explain paths. A directory's child-list hash includes
// only children that can affect the requested scope, so an unrelated sibling
// does not stale an approval.
func (w *Worktree) ManifestForScope(scope string) ([]FileEntry, error) {
	canonical, err := CanonicalScope(scope)
	if err != nil {
		return nil, err
	}
	paths, err := w.Paths()
	if err != nil {
		return nil, err
	}
	idx := w.indexEntries()
	status := w.statuses()
	for p := range idx {
		if !containsString(paths, p) {
			paths = append(paths, p)
		}
	}
	for _, pattern := range ScopePatterns(canonical) {
		if !strings.ContainsAny(pattern, "*?[") {
			full := filepath.Join(w.Root, filepath.FromSlash(pattern))
			if info, statErr := os.Lstat(full); statErr == nil && info.IsDir() {
				_ = filepath.WalkDir(full, func(p string, d os.DirEntry, walkErr error) error {
					if walkErr == nil {
						rel, relErr := filepath.Rel(w.Root, p)
						if relErr == nil {
							paths = append(paths, filepath.ToSlash(rel))
						}
					}
					return nil
				})
			} else if statErr == nil {
				paths = append(paths, pattern)
			}
		}
	}
	pathSet := make(map[string]bool, len(paths))
	for _, p := range paths {
		p = filepath.ToSlash(filepath.Clean(p))
		if p != "." && !excludedPathForScope(p, canonical) && ScopeMatches(canonical, p) {
			pathSet[p] = true
		}
	}
	entries := make([]FileEntry, 0, len(pathSet))
	for p := range pathSet {
		full := filepath.Join(w.Root, filepath.FromSlash(p))
		e := FileEntry{Path: p, Status: status[p]}
		if list := idx[p]; len(list) > 0 {
			sort.Slice(list, func(i, j int) bool { return list[i].Stage < list[j].Stage })
			e.Index = IndexState{Present: true, Mode: list[0].Mode, Blob: list[0].Blob}
			for _, item := range list {
				e.Index.Stages = append(e.Index.Stages, item.Stage)
				e.Index.Entries = append(e.Index.Entries, IndexStage{Stage: item.Stage, Mode: item.Mode, Blob: item.Blob})
			}
		}
		info, statErr := os.Lstat(full)
		if errors.Is(statErr, os.ErrNotExist) {
			e.Kind = "deleted"
		} else if statErr != nil {
			return nil, statErr
		} else {
			e.Worktree = ContentState{Present: true, Mode: fileMode(info)}
			e.Mode = e.Worktree.Mode
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				target, readErr := os.Readlink(full)
				if readErr != nil {
					return nil, readErr
				}
				e.Kind = "symlink"
				e.Worktree.Hash = digestBytes([]byte(target))
				e.Worktree.Size = int64(len(target))
			case info.IsDir():
				e.Kind = "directory"
			case isSubmodule(e.Index.Mode):
				e.Kind = "submodule"
			default:
				e.Kind = "file"
				e.Worktree.Size = info.Size()
				hash, hashErr := digestFile(full)
				if hashErr != nil {
					return nil, hashErr
				}
				e.Worktree.Hash = hash
			}
		}
		if e.Worktree.Hash != "" {
			e.Hash = e.Worktree.Hash
			e.Size = e.Worktree.Size
		}
		if e.Index.Mode != "" && e.Mode == "" {
			e.Mode = e.Index.Mode
		}
		entries = append(entries, e)
	}
	entries = includeStructuralAncestors(w.Root, entries, canonical)
	// Hash deepest directories first so every parent commits to its already
	// finalized scoped child edge. This removes map-iteration dependence.
	directoryOrder := make([]int, 0)
	for i := range entries {
		if entries[i].Kind == "directory" {
			directoryOrder = append(directoryOrder, i)
		}
	}
	sort.Slice(directoryOrder, func(i, j int) bool {
		left := strings.Count(entries[directoryOrder[i]].Path, "/")
		right := strings.Count(entries[directoryOrder[j]].Path, "/")
		if left == right {
			return entries[directoryOrder[i]].Path > entries[directoryOrder[j]].Path
		}
		return left > right
	})
	for _, i := range directoryOrder {
		entries[i].Worktree.Hash = directoryHash(entries, entries[i].Path, canonical)
		entries[i].Hash = entries[i].Worktree.Hash
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func excludedPath(p string) bool {
	p = strings.TrimPrefix(filepath.ToSlash(p), "./")
	return p == ".git" || strings.HasPrefix(p, ".git/") || p == ".rolemux" || strings.HasPrefix(p, ".rolemux/")
}

func excludedPathForScope(p, scope string) bool {
	p = strings.TrimPrefix(filepath.ToSlash(p), "./")
	if p == ".git" || strings.HasPrefix(p, ".git/") {
		return true
	}
	if p != ".rolemux" && !strings.HasPrefix(p, ".rolemux/") {
		return false
	}
	if p == ".rolemux" || p == ".rolemux/plans" || strings.HasPrefix(p, ".rolemux/plans/") {
		for _, pattern := range ScopePatterns(scope) {
			if pattern == ".rolemux/plans" || strings.HasPrefix(pattern, ".rolemux/plans/") {
				return false
			}
		}
	}
	return true
}

func includeStructuralAncestors(root string, entries []FileEntry, scope string) []FileEntry {
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		seen[e.Path] = true
	}
	for _, e := range append([]FileEntry(nil), entries...) {
		dir := filepath.ToSlash(filepath.Dir(e.Path))
		for dir != "." && dir != "" {
			if !seen[dir] && !excludedPathForScope(dir, scope) && hasScopedDescendant(scope, dir) {
				full := filepath.Join(root, filepath.FromSlash(dir))
				if info, err := os.Lstat(full); err == nil && info.IsDir() {
					state := ContentState{Present: true}
					mode := ""
					if ScopeMatches(scope, dir) {
						state.Mode = fileMode(info)
						mode = state.Mode
					}
					entries = append(entries, FileEntry{Path: dir, Kind: "directory", Worktree: state, Mode: mode})
				}
				seen[dir] = true
			}
			dir = filepath.ToSlash(filepath.Dir(dir))
		}
	}
	return entries
}

func hasScopedDescendant(scope, dir string) bool {
	for _, p := range ScopePatterns(scope) {
		if p == "**" || ScopeMatches(p, dir) || strings.HasPrefix(p, dir+"/") || strings.HasPrefix(dir, p+"/") {
			return true
		}
	}
	return false
}

func directoryHash(entries []FileEntry, dir, scope string) string {
	children := make([]string, 0)
	prefix := dir + "/"
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(e.Path, prefix)
		if strings.ContainsRune(rest, '/') || !hasScopedDescendant(scope, e.Path) {
			continue
		}
		children = append(children, e.Path+"\x00"+e.Kind+"\x00"+e.Worktree.Mode+"\x00"+e.Worktree.Hash+"\x00"+e.Index.Mode+"\x00"+e.Index.Blob)
	}
	sort.Strings(children)
	return digestBytes([]byte(strings.Join(children, "\n")))
}

func fileMode(info os.FileInfo) string { return fmt.Sprintf("%04o", uint32(info.Mode().Perm())) }

func isSubmodule(mode string) bool { return mode == "160000" }

// CaptureContentRefs stores baseline/candidate worktree bytes in separate
// immutable roots. It does not alter the entries supplied by another snapshot.
func CaptureContentRefs(entries []FileEntry, repoRoot, privateRoot, label, taskID string) ([]FileEntry, error) {
	result := cloneFileEntries(entries)
	root := filepath.Join(privateRoot, "content", label, taskID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	for i := range result {
		e := &result[i]
		if !e.Worktree.Present || e.Kind == "directory" || e.Kind == "deleted" || e.Kind == "submodule" {
			continue
		}
		full := filepath.Join(repoRoot, filepath.FromSlash(e.Path))
		var data []byte
		var err error
		if e.Kind == "symlink" {
			target, readErr := os.Readlink(full)
			if readErr != nil {
				return nil, readErr
			}
			data = []byte(target)
		} else {
			data, err = os.ReadFile(full)
			if err != nil {
				return nil, err
			}
		}
		digest := digestBytes(data)
		if e.Worktree.Hash != "" && digest != e.Worktree.Hash {
			return nil, fmt.Errorf("%s changed while its snapshot was being captured", e.Path)
		}
		name, err := writeImmutableBlob(root, digest, data)
		if err != nil {
			return nil, fmt.Errorf("capture %s: %w", e.Path, err)
		}
		e.Worktree.Ref = &ContentRef{Digest: digest, Path: name, Size: int64(len(data))}
	}
	return result, nil
}

// CaptureIndexRefs copies staged blobs from the index into a distinct
// immutable root. Deleted worktree entries can therefore still be reviewed
// after HEAD moves.
func CaptureIndexRefs(entries []FileEntry, repoRoot, privateRoot, label, taskID string) ([]FileEntry, error) {
	result := cloneFileEntries(entries)
	root := filepath.Join(privateRoot, "content", label+"-index", taskID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	for i := range result {
		e := &result[i]
		if !e.Index.Present {
			continue
		}
		stages := e.Index.Entries
		if len(stages) == 0 && e.Index.Blob != "" {
			stage := 0
			if len(e.Index.Stages) > 0 {
				stage = e.Index.Stages[0]
			}
			stages = []IndexStage{{Stage: stage, Mode: e.Index.Mode, Blob: e.Index.Blob}}
		}
		for j := range stages {
			if stages[j].Blob == "" || strings.Trim(stages[j].Blob, "0") == "" || stages[j].Mode == "160000" {
				continue
			}
			b, err := gitOutputBytes(repoRoot, "cat-file", "blob", stages[j].Blob)
			if err != nil {
				return nil, fmt.Errorf("capture index %s stage %d: %w", e.Path, stages[j].Stage, err)
			}
			digest := digestBytes(b)
			name, err := writeImmutableBlob(root, digest, b)
			if err != nil {
				return nil, fmt.Errorf("capture index %s stage %d: %w", e.Path, stages[j].Stage, err)
			}
			stages[j].Ref = &ContentRef{Digest: digest, Path: name, Size: int64(len(b))}
			primaryStage := stages[0].Stage
			if len(e.Index.Stages) > 0 {
				primaryStage = e.Index.Stages[0]
			}
			if stages[j].Blob == e.Index.Blob && stages[j].Stage == primaryStage {
				e.Index.Ref = stages[j].Ref
			}
		}
		e.Index.Entries = stages
	}
	return result, nil
}

func cloneFileEntries(entries []FileEntry) []FileEntry {
	result := append([]FileEntry(nil), entries...)
	for i := range result {
		result[i].Index.Stages = append([]int(nil), entries[i].Index.Stages...)
		result[i].Index.Entries = append([]IndexStage(nil), entries[i].Index.Entries...)
		if entries[i].Worktree.Ref != nil {
			ref := *entries[i].Worktree.Ref
			result[i].Worktree.Ref = &ref
		}
		if entries[i].Index.Ref != nil {
			ref := *entries[i].Index.Ref
			result[i].Index.Ref = &ref
		}
		for j := range result[i].Index.Entries {
			if entries[i].Index.Entries[j].Ref != nil {
				ref := *entries[i].Index.Entries[j].Ref
				result[i].Index.Entries[j].Ref = &ref
			}
		}
	}
	return result
}

func writeImmutableBlob(root, digest string, data []byte) (string, error) {
	name := filepath.Join(root, digest)
	if _, err := os.Lstat(name); err == nil {
		got, hashErr := digestFile(name)
		if hashErr != nil {
			return "", hashErr
		}
		if got != digest {
			return "", errors.New("immutable content digest mismatch")
		}
		return name, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	tmp, err := os.CreateTemp(root, ".blob-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, name); err != nil {
		return "", err
	}
	if err := syncDir(root); err != nil {
		return "", err
	}
	return name, nil
}

func gitOutput(root string, args ...string) (string, error) {
	b, err := gitOutputBytes(root, args...)
	return strings.TrimSpace(string(b)), err
}

func gitOutputBytes(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	return cmd.Output()
}

func splitNUL(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, string(part))
	}
	return result
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func digestFile(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashManifest is derived solely from path, kind, worktree content/mode, and
// index blob/mode/stages. Git status labels, HEAD, timestamps, and ref paths
// cannot change this hash.
func HashManifest(entries []FileEntry) string {
	canonical := append([]FileEntry(nil), entries...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Path < canonical[j].Path })
	parts := make([]string, 0, len(canonical))
	for _, e := range canonical {
		stages := make([]int, len(e.Index.Stages))
		copy(stages, e.Index.Stages)
		sort.Ints(stages)
		indexEntries := append([]IndexStage(nil), e.Index.Entries...)
		sort.Slice(indexEntries, func(i, j int) bool { return indexEntries[i].Stage < indexEntries[j].Stage })
		indexParts := make([]string, 0, len(indexEntries))
		for _, entry := range indexEntries {
			indexParts = append(indexParts, fmt.Sprintf("%d:%s:%s", entry.Stage, entry.Mode, entry.Blob))
		}
		parts = append(parts, strings.Join([]string{
			e.Path, e.Kind, strconv.FormatBool(e.Worktree.Present), e.Worktree.Mode,
			e.Worktree.Hash, strconv.FormatInt(e.Worktree.Size, 10),
			strconv.FormatBool(e.Index.Present), e.Index.Mode, e.Index.Blob,
			fmt.Sprint(stages), strings.Join(indexParts, ","),
		}, "\x00"))
	}
	return digestBytes([]byte(strings.Join(parts, "\n")))
}

func NewToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return digestBytes([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
}

// WritePlan performs a same-directory durable atomic plan write. The caller
// must pass a validated task ID; no arbitrary path is accepted.
func WritePlan(repoRoot, id, contents string) error {
	if !validID(id) {
		return fmt.Errorf("%w %q", ErrInvalidTaskID, id)
	}
	dir := filepath.Join(repoRoot, ".rolemux", "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".plan-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.WriteString(tmp, contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, id+".md")); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
