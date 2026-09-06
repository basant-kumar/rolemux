package task

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ApprovalGateSchemaVersion is the schema version used by newly written
// approval gates. A zero value remains valid for tasks written before approval
// gates were persisted.
const ApprovalGateSchemaVersion = 1

type ApprovalKind string

const (
	ApprovalKindPlan ApprovalKind = "plan"
	ApprovalKindCode ApprovalKind = "code"

	// Short aliases keep the values convenient for callers that already use
	// the domain words as constants.
	ApprovalPlan ApprovalKind = ApprovalKindPlan
	ApprovalCode ApprovalKind = ApprovalKindCode
)

type ApprovalDecision string

const (
	ApprovalDecisionApprove        ApprovalDecision = "approve"
	ApprovalDecisionRequestChanges ApprovalDecision = "request_changes"
	ApprovalDecisionDiscuss        ApprovalDecision = "discuss"

	ApprovalApprove        ApprovalDecision = ApprovalDecisionApprove
	ApprovalRequestChanges ApprovalDecision = ApprovalDecisionRequestChanges
	ApprovalDiscuss        ApprovalDecision = ApprovalDecisionDiscuss
	DecisionApprove        ApprovalDecision = ApprovalDecisionApprove
	DecisionRequestChanges ApprovalDecision = ApprovalDecisionRequestChanges
	DecisionDiscuss        ApprovalDecision = ApprovalDecisionDiscuss
)

var (
	ErrInvalidGateID           = errors.New("invalid approval gate id")
	ErrInvalidArtifactDigest   = errors.New("invalid approval artifact digest")
	ErrArtifactConflict        = errors.New("approval artifact conflicts with existing bytes")
	ErrArtifactCorrupt         = errors.New("approval artifact content digest mismatch")
	ErrApprovalArtifactMissing = errors.New("approval artifact not found")
	ErrInvalidArtifactPath     = errors.New("invalid approval artifact path")
	ErrPrivatePath             = errors.New("private RoleMux path contains a symlink or non-directory")
)

type ApprovalArtifactRef = ContentRef

// ApprovalChangedFile is the compact changed-file context carried by an
// approval. It intentionally does not contain file bytes or immutable content
// references.
type ApprovalChangedFile struct {
	Path       string `json:"path"`
	Kind       string `json:"kind,omitempty"`
	ChangeKind string `json:"change_kind,omitempty"`
}

type ApprovalFileChange = ApprovalChangedFile
type ChangedFile = ApprovalChangedFile

// ReviewerEvidence is copied into the approval record before it is persisted.
// It is evidence about a completed review, rather than a live provider
// session. The compatibility aliases allow old callers to retain their
// existing session/profile field names while the canonical fields remain
// explicit.
type ReviewerEvidence struct {
	SourceTask          string           `json:"source_task,omitempty"`
	SourceTaskID        string           `json:"source_task_id,omitempty"`
	Verdict             string           `json:"verdict"`
	ReviewerRole        string           `json:"reviewer_role,omitempty"`
	ReviewerSessionID   string           `json:"reviewer_session_id,omitempty"`
	ReviewerSession     string           `json:"reviewer_session,omitempty"`
	ReviewerProfile     *ProfileSnapshot `json:"reviewer_profile,omitempty"`
	Profile             *ProfileSnapshot `json:"profile,omitempty"`
	AcceptedRound       int              `json:"accepted_round,omitempty"`
	ReviewedFingerprint string           `json:"reviewed_fingerprint,omitempty"`
}

// ApprovalRecord is the durable human-approval gate. An absent record is
// distinct from a record with an approval decision: loading legacy JSON never
// invents a human decision.
type ApprovalRecord struct {
	GateID               string                `json:"gate_id"`
	Kind                 ApprovalKind          `json:"kind"`
	Status               ApprovalDecision      `json:"status,omitempty"`
	ArtifactRef          string                `json:"artifact_ref,omitempty"`
	ArtifactDigest       string                `json:"artifact_digest,omitempty"`
	Artifact             *ContentRef           `json:"artifact,omitempty"`
	SubjectFingerprint   string                `json:"subject_fingerprint,omitempty"`
	Question             string                `json:"question,omitempty"`
	Choices              []string              `json:"choices,omitempty"`
	Scope                string                `json:"scope,omitempty"`
	ChangedPaths         []string              `json:"changed_paths,omitempty"`
	ChangedFiles         []ApprovalChangedFile `json:"changed_files,omitempty"`
	ReviewerEvidence     *ReviewerEvidence     `json:"reviewer_evidence,omitempty"`
	CreatedAt            time.Time             `json:"created_at,omitempty"`
	DecidedAt            *time.Time            `json:"decided_at,omitempty"`
	HumanFeedback        string                `json:"human_feedback,omitempty"`
	FeedbackOperationRef string                `json:"feedback_operation_ref,omitempty"`
}

// PlanApprovalArtifact is a reproducible, deliberately narrow report. It is
// generated from the saved task projection and approval record only.
type PlanApprovalArtifact struct {
	SchemaVersion      int               `json:"schema_version"`
	GateID             string            `json:"gate_id"`
	Kind               ApprovalKind      `json:"kind"`
	SubjectFingerprint string            `json:"subject_fingerprint,omitempty"`
	Question           string            `json:"question,omitempty"`
	Choices            []string          `json:"choices,omitempty"`
	Scope              string            `json:"scope,omitempty"`
	Plan               string            `json:"plan"`
	Complexity         string            `json:"complexity,omitempty"`
	WorkUnits          []WorkUnit        `json:"work_units"`
	ReviewerEvidence   *ReviewerEvidence `json:"reviewer_evidence,omitempty"`
}

// CodeApprovalArtifact is the compact code-review report. It carries the
// candidate identity and review evidence without serializing State, provider
// sessions, credentials, or conversation transcripts.
type CodeApprovalArtifact struct {
	SchemaVersion            int                   `json:"schema_version"`
	GateID                   string                `json:"gate_id"`
	Kind                     ApprovalKind          `json:"kind"`
	SubjectFingerprint       string                `json:"subject_fingerprint,omitempty"`
	CandidateFingerprint     string                `json:"candidate_fingerprint"`
	Question                 string                `json:"question,omitempty"`
	Choices                  []string              `json:"choices,omitempty"`
	Scope                    string                `json:"scope"`
	ChangedPaths             []string              `json:"changed_paths"`
	ChangedFiles             []ApprovalChangedFile `json:"changed_files"`
	ReviewerEvidence         *ReviewerEvidence     `json:"reviewer_evidence,omitempty"`
	Findings                 []Finding             `json:"findings,omitempty"`
	ReviewCheckpointFindings []Finding             `json:"review_checkpoint_findings,omitempty"`
	Advisories               []Diagnostic          `json:"advisories,omitempty"`
	Diagnostics              []string              `json:"diagnostics,omitempty"`
}

// PlanPath resolves the compatibility plan path in private Git state. It has
// no side effects, so callers can use it to persist the resolved path in a
// record before calling WritePlan.
func PlanPath(repoRoot, id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("%w %q", ErrInvalidTaskID, id)
	}
	root, err := DiscoverRepository(repoRoot)
	if err != nil {
		return "", err
	}
	gitDir, err := gitPath(root, "rolemux")
	if err != nil {
		return "", fmt.Errorf("discover private git state: %w", err)
	}
	path, err := filepath.Abs(filepath.Join(gitDir, "plans", id+".md"))
	if err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

// ResolvePlanPath is an explicit-name compatibility alias for PlanPath.
func ResolvePlanPath(repoRoot, id string) (string, error) {
	return PlanPath(repoRoot, id)
}

// WritePlanPath is a descriptive compatibility alias for PlanPath.
func WritePlanPath(repoRoot, id string) (string, error) {
	return PlanPath(repoRoot, id)
}

func planPath(repoRoot, id string) (string, error) {
	return PlanPath(repoRoot, id)
}

func cloneApprovalEvidence(evidence *ReviewerEvidence) *ReviewerEvidence {
	if evidence == nil {
		return nil
	}
	copyEvidence := *evidence
	if evidence.ReviewerProfile != nil {
		profile := *evidence.ReviewerProfile
		copyEvidence.ReviewerProfile = &profile
	}
	if evidence.Profile != nil {
		profile := *evidence.Profile
		copyEvidence.Profile = &profile
	}
	return &copyEvidence
}

func cloneWorkUnits(units []WorkUnit) []WorkUnit {
	if units == nil {
		return nil
	}
	result := make([]WorkUnit, len(units))
	for i, unit := range units {
		result[i] = unit
		result[i].DependsOn = append([]string(nil), unit.DependsOn...)
		result[i].AcceptanceCriteria = append([]string(nil), unit.AcceptanceCriteria...)
		result[i].ValidationCommands = append([]string(nil), unit.ValidationCommands...)
	}
	return result
}

func cloneFindings(findings []Finding) []Finding {
	if findings == nil {
		return nil
	}
	return append([]Finding(nil), findings...)
}

func cloneDiagnostics(diagnostics []Diagnostic) []Diagnostic {
	if diagnostics == nil {
		return nil
	}
	result := make([]Diagnostic, len(diagnostics))
	for i, diagnostic := range diagnostics {
		result[i] = diagnostic
		result[i].Paths = append([]string(nil), diagnostic.Paths...)
	}
	return result
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneChangedFiles(files []ApprovalChangedFile) []ApprovalChangedFile {
	if files == nil {
		return nil
	}
	return append([]ApprovalChangedFile(nil), files...)
}

func approvalChangedFiles(st State, record ApprovalRecord) ([]ApprovalChangedFile, []string) {
	files := cloneChangedFiles(record.ChangedFiles)
	paths := cloneStrings(record.ChangedPaths)
	baseline := manifestByPath(st.ScopedBaseline)
	candidate := manifestByPath(st.CandidateManifest)
	if len(files) == 0 {
		files = make([]ApprovalChangedFile, 0, len(st.ChangeManifest))
		for _, entry := range st.ChangeManifest {
			files = append(files, ApprovalChangedFile{
				Path: entry.Path, Kind: entry.Kind,
				ChangeKind: approvalChangeKind(entry.Path, entry, baseline, candidate),
			})
		}
	} else {
		for i := range files {
			if files[i].ChangeKind == "" {
				entry := FileEntry{Path: files[i].Path, Kind: files[i].Kind}
				files[i].ChangeKind = approvalChangeKind(files[i].Path, entry, baseline, candidate)
			}
		}
	}
	if len(paths) == 0 {
		paths = make([]string, 0, len(files))
		for _, file := range files {
			paths = append(paths, file.Path)
		}
	}
	sort.Strings(paths)
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Path == files[j].Path {
			left := files[i].Kind + "\x00" + files[i].ChangeKind
			right := files[j].Kind + "\x00" + files[j].ChangeKind
			return left < right
		}
		return files[i].Path < files[j].Path
	})
	return files, paths
}

func manifestByPath(entries []FileEntry) map[string]FileEntry {
	result := make(map[string]FileEntry, len(entries))
	for _, entry := range entries {
		result[entry.Path] = entry
	}
	return result
}

func knownChangeKind(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "add", "added", "a":
		return "added"
	case "modify", "modified", "m":
		return "modified"
	case "delete", "deleted", "d":
		return "deleted"
	case "rename", "renamed", "r":
		return "renamed"
	case "copy", "copied", "c":
		return "copied"
	}
	status := strings.TrimSpace(value)
	for _, marker := range []struct {
		code string
		kind string
	}{
		{code: "A", kind: "added"},
		{code: "M", kind: "modified"},
		{code: "D", kind: "deleted"},
		{code: "R", kind: "renamed"},
		{code: "C", kind: "copied"},
	} {
		if strings.Contains(status, marker.code) {
			return marker.kind
		}
	}
	return ""
}

func approvalChangeKind(path string, entry FileEntry, baseline, candidate map[string]FileEntry) string {
	if kind := knownChangeKind(entry.Status); kind != "" {
		return kind
	}
	base, inBaseline := baseline[path]
	current, inCandidate := candidate[path]
	if inCandidate && current.Kind == "deleted" && inBaseline {
		return "deleted"
	}
	if !inBaseline && inCandidate {
		return "added"
	}
	if inBaseline && !inCandidate {
		return "deleted"
	}
	if inBaseline && inCandidate {
		if kind := knownChangeKind(current.Status); kind != "" {
			return kind
		}
		if kind := knownChangeKind(base.Status); kind != "" {
			return kind
		}
		return "modified"
	}
	if kind := knownChangeKind(entry.Kind); kind != "" {
		return kind
	}
	return "modified"
}

func approvalJSON(value any) ([]byte, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// BuildPlanApprovalArtifact creates the exact bytes used by the plan gate.
// It does not consult the repository or a provider.
func BuildPlanApprovalArtifact(st State, record ApprovalRecord) ([]byte, error) {
	if record.Kind != "" && record.Kind != ApprovalKindPlan {
		return nil, fmt.Errorf("approval record kind %q is not a plan gate", record.Kind)
	}
	report := PlanApprovalArtifact{
		SchemaVersion:      ApprovalGateSchemaVersion,
		GateID:             record.GateID,
		Kind:               ApprovalKindPlan,
		SubjectFingerprint: record.SubjectFingerprint,
		Question:           record.Question,
		Choices:            cloneStrings(record.Choices),
		Scope:              record.Scope,
		Plan:               st.Plan,
		Complexity:         st.Complexity,
		WorkUnits:          planWorkUnits(st),
		ReviewerEvidence:   cloneApprovalEvidence(record.ReviewerEvidence),
	}
	return approvalJSON(report)
}

func planWorkUnits(st State) []WorkUnit {
	if len(st.WorkUnits) != 0 {
		return cloneWorkUnits(st.WorkUnits)
	}
	units, err := NormalizeWorkUnits(nil, st.Plan)
	if err == nil {
		return units
	}
	return []WorkUnit{}
}

// BuildCodeApprovalArtifact creates the exact bytes used by the code gate.
// It is based only on the saved candidate projection, findings, diagnostics,
// and approval evidence.
func BuildCodeApprovalArtifact(st State, record ApprovalRecord) ([]byte, error) {
	if record.Kind != "" && record.Kind != ApprovalKindCode {
		return nil, fmt.Errorf("approval record kind %q is not a code gate", record.Kind)
	}
	files, paths := approvalChangedFiles(st, record)
	scope := record.Scope
	if scope == "" {
		scope = st.Scope
	}
	candidateFingerprint := record.SubjectFingerprint
	if candidateFingerprint == "" {
		candidateFingerprint = st.CandidateManifestHash
	}
	report := CodeApprovalArtifact{
		SchemaVersion:            ApprovalGateSchemaVersion,
		GateID:                   record.GateID,
		Kind:                     ApprovalKindCode,
		SubjectFingerprint:       record.SubjectFingerprint,
		CandidateFingerprint:     candidateFingerprint,
		Question:                 record.Question,
		Choices:                  cloneStrings(record.Choices),
		Scope:                    scope,
		ChangedPaths:             paths,
		ChangedFiles:             files,
		ReviewerEvidence:         cloneApprovalEvidence(record.ReviewerEvidence),
		Findings:                 cloneFindings(st.Findings),
		ReviewCheckpointFindings: cloneFindings(st.ReviewCheckpointFindings),
		Advisories:               cloneDiagnostics(st.Advisories),
		Diagnostics:              cloneStrings(st.Diagnostics),
	}
	return approvalJSON(report)
}

func validApprovalDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func (s *Store) artifactPrivateRoot() (string, error) {
	if err := s.ensure(); err != nil {
		return "", err
	}
	root := s.privateDir
	if root == "" {
		// Preserve compatibility with Store values made by older in-package
		// callers while keeping NewStore's gitPath resolution authoritative.
		root = s.Dir
		if filepath.Base(root) == "tasks" {
			root = filepath.Dir(root)
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Clean(root), nil
}

func privatePathRelative(root, target string) (string, string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", "", ErrPrivatePath
	}
	return root, rel, nil
}

func chmodPrivateDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Fchmod(fd, 0o700)
}

func ensurePrivateDirectory(root, target string) error {
	root, rel, err := privatePathRelative(root, target)
	if err != nil {
		return err
	}
	current := root
	parts := []string{}
	if rel != "." && rel != "" {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for _, part := range append([]string{current}, parts...) {
		if part != current {
			current = filepath.Join(current, part)
		}
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if mkdirErr := os.Mkdir(current, 0o700); mkdirErr != nil {
				if !errors.Is(mkdirErr, os.ErrExist) {
					return mkdirErr
				}
				info, statErr = os.Lstat(current)
			} else {
				info, statErr = os.Lstat(current)
			}
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: %s", ErrPrivatePath, current)
		}
		if err := chmodPrivateDirectory(current); err != nil {
			return fmt.Errorf("secure private directory %s: %w", current, err)
		}
	}
	return nil
}

func validatePrivateDirectory(root, target string) error {
	root, rel, err := privatePathRelative(root, target)
	if err != nil {
		return err
	}
	current := root
	parts := []string{}
	if rel != "." && rel != "" {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for _, part := range append([]string{current}, parts...) {
		if part != current {
			current = filepath.Join(current, part)
		}
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: %s", ErrPrivatePath, current)
		}
	}
	return nil
}

func (s *Store) approvalArtifactDir(taskID, gateID string) (string, error) {
	if !validID(taskID) {
		return "", fmt.Errorf("%w %q", ErrInvalidTaskID, taskID)
	}
	if !validID(gateID) {
		return "", fmt.Errorf("%w: %w %q", ErrInvalidGateID, ErrInvalidTaskID, gateID)
	}
	root, err := s.artifactPrivateRoot()
	if err != nil {
		return "", err
	}
	approvals := filepath.Join(root, "approvals")
	taskDir := filepath.Join(approvals, taskID)
	gateDir := filepath.Join(taskDir, gateID)
	if err := ensurePrivateDirectory(root, gateDir); err != nil {
		return "", err
	}
	return gateDir, nil
}

// ApprovalArtifactPath returns the absolute path for a content-addressed
// artifact. With no digest it returns the private gate directory, which is
// useful to callers that need to inspect or repair a gate directory.
func (s *Store) ApprovalArtifactPath(taskID, gateID string, digest ...string) (string, error) {
	if len(digest) > 1 || len(digest) == 1 && !validApprovalDigest(digest[0]) {
		return "", ErrInvalidArtifactDigest
	}
	dir, err := s.approvalArtifactDir(taskID, gateID)
	if err != nil {
		return "", err
	}
	if len(digest) == 0 {
		return dir, nil
	}
	return filepath.Join(dir, digest[0]), nil
}

// ArtifactPath is the shorter generic spelling for ApprovalArtifactPath.
func (s *Store) ArtifactPath(taskID, gateID, digest string) (string, error) {
	return s.ApprovalArtifactPath(taskID, gateID, digest)
}

func (s *Store) approvalArtifactPath(taskID, gateID, digest string) (string, error) {
	return s.ApprovalArtifactPath(taskID, gateID, digest)
}

func lockArtifactDirectory(dir, gateID string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(dir, ".artifact-"+gateID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func unlockArtifactDirectory(lock *os.File) error {
	if lock == nil {
		return nil
	}
	err := unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	closeErr := lock.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func existingArtifactBytes(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("%w: artifact is not a regular file", ErrArtifactConflict)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, true, err
	}
	return b, true, nil
}

func immutableArtifactRef(path string, contents []byte) ContentRef {
	return ContentRef{Digest: digestBytes(contents), Path: path, Size: int64(len(contents))}
}

func (s *Store) writeApprovalArtifactAt(dir, gateID, path string, contents []byte) (ContentRef, error) {
	lock, err := lockArtifactDirectory(dir, gateID)
	if err != nil {
		return ContentRef{}, err
	}
	defer unlockArtifactDirectory(lock)

	ref := immutableArtifactRef(path, contents)
	if existing, ok, err := existingArtifactBytes(path); err != nil {
		return ContentRef{}, err
	} else if ok {
		if !bytes.Equal(existing, contents) {
			return ContentRef{}, fmt.Errorf("%w: %s", ErrArtifactConflict, path)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return ContentRef{}, err
		}
		return ref, nil
	}

	tmp, err := os.CreateTemp(dir, ".artifact-*.tmp")
	if err != nil {
		return ContentRef{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	writeErr := error(nil)
	if err := tmp.Chmod(0o600); err != nil {
		writeErr = err
	} else if _, err := tmp.Write(contents); err != nil {
		writeErr = err
	} else if err := tmp.Sync(); err != nil {
		writeErr = err
	}
	if closeErr := tmp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return ContentRef{}, writeErr
	}

	// A hard link creates the destination atomically without replacing an
	// independently-created file. The temporary link is in the same directory
	// and is removed only after the no-replace operation succeeds.
	if err := os.Link(tmpName, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return ContentRef{}, err
		}
		existing, ok, readErr := existingArtifactBytes(path)
		if readErr != nil {
			return ContentRef{}, readErr
		}
		if !ok || !bytes.Equal(existing, contents) {
			return ContentRef{}, fmt.Errorf("%w: %s", ErrArtifactConflict, path)
		}
		return ref, nil
	}
	if err := os.Remove(tmpName); err != nil {
		return ContentRef{}, err
	}
	if err := syncDir(dir); err != nil {
		return ContentRef{}, err
	}
	return ref, nil
}

// WriteApprovalArtifact stores immutable bytes under private Git state and
// returns an absolute path, digest, and byte count. Repeating the exact write
// is successful; an existing path containing different bytes is rejected.
func (s *Store) WriteApprovalArtifact(taskID, gateID string, contents []byte) (ContentRef, error) {
	digest := digestBytes(contents)
	path, err := s.ApprovalArtifactPath(taskID, gateID, digest)
	if err != nil {
		return ContentRef{}, err
	}
	dir := filepath.Dir(path)
	return s.writeApprovalArtifactAt(dir, gateID, path, contents)
}

func (s *Store) writeApprovalArtifact(taskID, gateID string, contents []byte) (ContentRef, error) {
	return s.WriteApprovalArtifact(taskID, gateID, contents)
}

// ReadApprovalArtifact reads and verifies a content-addressed artifact. The
// variadic digest keeps the directory form of ApprovalArtifactPath useful
// while allowing the normal three-argument read call.
func (s *Store) ReadApprovalArtifact(taskID, gateID string, digest ...string) ([]byte, error) {
	if len(digest) != 1 || !validApprovalDigest(digest[0]) {
		return nil, ErrInvalidArtifactDigest
	}
	path, err := s.ApprovalArtifactPath(taskID, gateID, digest[0])
	if err != nil {
		return nil, err
	}
	b, ok, err := existingArtifactBytes(path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrApprovalArtifactMissing, path)
	}
	if digestBytes(b) != digest[0] {
		return nil, fmt.Errorf("%w: %s", ErrArtifactCorrupt, path)
	}
	return b, nil
}

func (s *Store) readApprovalArtifact(taskID, gateID, digest string) ([]byte, error) {
	return s.ReadApprovalArtifact(taskID, gateID, digest)
}

// ReadOrRepairApprovalArtifact verifies an existing artifact or durably
// recreates it when the content-addressed file is missing.
func (s *Store) ReadOrRepairApprovalArtifact(taskID, gateID string, contents []byte) (ContentRef, error) {
	return s.WriteApprovalArtifact(taskID, gateID, contents)
}

func (s *Store) readOrRepairApprovalArtifact(taskID, gateID string, contents []byte) (ContentRef, error) {
	return s.ReadOrRepairApprovalArtifact(taskID, gateID, contents)
}

// ReadOrRepairArtifact is a generic spelling retained for storage callers.
func (s *Store) ReadOrRepairArtifact(taskID, gateID string, contents []byte) (ContentRef, error) {
	return s.ReadOrRepairApprovalArtifact(taskID, gateID, contents)
}

// ReadApprovalArtifactRef verifies and reads a previously returned content
// reference without trusting the path or digest independently.
func (s *Store) ReadApprovalArtifactRef(ref ContentRef) ([]byte, error) {
	if !filepath.IsAbs(ref.Path) || !validApprovalDigest(ref.Digest) {
		return nil, ErrInvalidArtifactDigest
	}
	root, err := s.artifactPrivateRoot()
	if err != nil {
		return nil, err
	}
	artifactRoot := filepath.Join(root, "approvals")
	rel, err := filepath.Rel(artifactRoot, ref.Path)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return nil, ErrInvalidArtifactPath
	}
	if err := validatePrivateDirectory(artifactRoot, filepath.Dir(ref.Path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrApprovalArtifactMissing, ref.Path)
		}
		return nil, err
	}
	b, ok, err := existingArtifactBytes(ref.Path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrApprovalArtifactMissing, ref.Path)
	}
	if digestBytes(b) != ref.Digest {
		return nil, fmt.Errorf("%w: %s", ErrArtifactCorrupt, ref.Path)
	}
	return b, nil
}

func setApprovalArtifactRef(record *ApprovalRecord, ref ContentRef) {
	if record == nil {
		return
	}
	record.ArtifactRef = ref.Path
	record.ArtifactDigest = ref.Digest
	copyRef := ref
	record.Artifact = &copyRef
}

// WritePlanApprovalArtifact builds and stores the plan report.
func (s *Store) WritePlanApprovalArtifact(st State, record ApprovalRecord) (ContentRef, error) {
	contents, err := BuildPlanApprovalArtifact(st, record)
	if err != nil {
		return ContentRef{}, err
	}
	return s.WriteApprovalArtifact(st.ID, record.GateID, contents)
}

// WriteCodeApprovalArtifact builds and stores the code report.
func (s *Store) WriteCodeApprovalArtifact(st State, record ApprovalRecord) (ContentRef, error) {
	contents, err := BuildCodeApprovalArtifact(st, record)
	if err != nil {
		return ContentRef{}, err
	}
	return s.WriteApprovalArtifact(st.ID, record.GateID, contents)
}

// PersistApprovalArtifact returns a copy of the saved record with its
// immutable artifact reference attached. The caller can then persist that
// record through Store.Update; this helper does not mutate task state.
func (s *Store) PersistApprovalArtifact(st State, record ApprovalRecord) (ApprovalRecord, ContentRef, error) {
	var (
		ref ContentRef
		err error
	)
	switch record.Kind {
	case ApprovalKindPlan:
		ref, err = s.WritePlanApprovalArtifact(st, record)
	case ApprovalKindCode:
		ref, err = s.WriteCodeApprovalArtifact(st, record)
	default:
		err = fmt.Errorf("invalid approval kind %q", record.Kind)
	}
	if err != nil {
		return ApprovalRecord{}, ContentRef{}, err
	}
	setApprovalArtifactRef(&record, ref)
	return record, ref, nil
}
