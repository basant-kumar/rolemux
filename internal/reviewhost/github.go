// Package reviewhost publishes reviewed RoleMux candidates to optional human
// review surfaces. GitHub support uses the installed git and gh CLIs, and
// never owns credentials.
package reviewhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

const (
	maxCommandOutput = 4 << 20
	maxFeedbackBytes = 32 << 10
)

var ErrUnavailable = errors.New("GitHub review is unavailable")

// GitHub is an injectable host adapter. Production uses RunProcess; tests can
// intercept only network-mutating push/gh calls while retaining real local Git.
type GitHub struct {
	RepoRoot string
	GitPath  string
	GHPath   string
	Env      []string
	Process  runner.ProcessFunc
	Now      func() time.Time
}

type Feedback struct {
	Text   string
	Review task.ExternalReview
}

func NewGitHub(repoRoot string, environ []string) (*GitHub, error) {
	gitPath, err := runner.ResolveExecutable("GIT_CLI_PATH", "git", environ)
	if err != nil {
		return nil, fmt.Errorf("%w: git is not installed", ErrUnavailable)
	}
	ghPath, err := runner.ResolveExecutable("GH_CLI_PATH", "gh", environ)
	if err != nil {
		return nil, fmt.Errorf("%w: install GitHub CLI (gh) to publish a draft PR", ErrUnavailable)
	}
	root, err := task.DiscoverRepository(repoRoot)
	if err != nil {
		return nil, err
	}
	return &GitHub{RepoRoot: root, GitPath: gitPath, GHPath: ghPath, Env: runner.SanitizedEnv(environ), Process: runner.RunProcess, Now: time.Now}, nil
}

func (g *GitHub) command(ctx context.Context, path, dir, stdin string, args ...string) ([]byte, error) {
	return g.commandWithEnv(ctx, path, dir, stdin, g.Env, args...)
}

func (g *GitHub) commandWithEnv(ctx context.Context, path, dir, stdin string, env []string, args ...string) ([]byte, error) {
	process := g.Process
	if process == nil {
		process = runner.RunProcess
	}
	result, err := process(ctx, runner.ProcessSpec{Path: path, Args: args, Dir: dir, Env: env, Stdin: stdin, MaxOutputBytes: maxCommandOutput})
	if err != nil {
		return nil, err
	}
	return result.Stdout, nil
}

func (g *GitHub) git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := g.command(ctx, g.GitPath, dir, "", args...)
	return strings.TrimSpace(string(out)), err
}

func (g *GitHub) gitWithEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	out, err := g.commandWithEnv(ctx, g.GitPath, dir, "", env, args...)
	return strings.TrimSpace(string(out)), err
}

func (g *GitHub) gh(ctx context.Context, args ...string) ([]byte, error) {
	return g.command(ctx, g.GHPath, g.RepoRoot, "", args...)
}

type remoteInfo struct {
	Name       string
	Host       string
	Repository string
}

func parseGitHubRemote(name, raw string) (remoteInfo, error) {
	value := strings.TrimSpace(raw)
	var host, path string
	switch {
	case strings.HasPrefix(value, "git@"):
		rest := strings.TrimPrefix(value, "git@")
		var ok bool
		host, path, ok = strings.Cut(rest, ":")
		if !ok {
			return remoteInfo{}, fmt.Errorf("%w: unsupported GitHub remote %q", ErrUnavailable, value)
		}
	case strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "ssh://"):
		withoutScheme, _, _ := strings.Cut(value, "://")
		rest := strings.TrimPrefix(value, withoutScheme+"://")
		host, path, _ = strings.Cut(rest, "/")
	default:
		return remoteInfo{}, fmt.Errorf("%w: remote %q is not a GitHub URL", ErrUnavailable, value)
	}
	host = strings.TrimSpace(strings.Split(host, "@")[len(strings.Split(host, "@"))-1])
	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return remoteInfo{}, fmt.Errorf("%w: cannot determine GitHub repository from %q", ErrUnavailable, value)
	}
	return remoteInfo{Name: name, Host: host, Repository: path}, nil
}

func (g *GitHub) remote(ctx context.Context, preferred string) (remoteInfo, error) {
	name := strings.TrimSpace(preferred)
	if name == "" {
		name = "origin"
	}
	raw, err := g.git(ctx, g.RepoRoot, "remote", "get-url", "--push", name)
	if err != nil {
		return remoteInfo{}, fmt.Errorf("%w: Git remote %q is not configured", ErrUnavailable, name)
	}
	return parseGitHubRemote(name, raw)
}

// Check verifies the local prerequisites without changing repository or
// remote state.
func (g *GitHub) Check(ctx context.Context, preferredRemote string) (remoteInfo, error) {
	remote, err := g.remote(ctx, preferredRemote)
	if err != nil {
		return remoteInfo{}, err
	}
	if _, err := g.gh(ctx, "auth", "status", "--hostname", remote.Host); err != nil {
		return remoteInfo{}, fmt.Errorf("%w: run gh auth login --hostname %s", ErrUnavailable, remote.Host)
	}
	return remote, nil
}

func reviewBranchPart(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		result = "task"
	}
	if len(result) > 48 {
		result = strings.Trim(result[:48], "-")
	}
	return result
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:10]
}

func changedFilePaths(st task.State) []string {
	set := map[string]bool{}
	for _, entry := range st.ChangeManifest {
		if entry.Kind != "directory" && strings.TrimSpace(entry.Path) != "" {
			set[entry.Path] = true
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func manifestMap(entries []task.FileEntry) map[string]task.FileEntry {
	result := make(map[string]task.FileEntry, len(entries))
	for _, entry := range entries {
		result[entry.Path] = entry
	}
	return result
}

func secureSnapshotPath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", errors.New("snapshot path is absolute")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("snapshot path escapes review worktree")
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("snapshot path crosses a symlink or non-directory")
		}
	}
	return filepath.Join(root, clean), nil
}

func (g *GitHub) materialize(store *task.Store, root string, entry *task.FileEntry, path string) error {
	target, err := secureSnapshotPath(root, path)
	if err != nil {
		return fmt.Errorf("materialize %s: %w", path, err)
	}
	if entry == nil || !entry.Worktree.Present || entry.Kind == "deleted" {
		return os.RemoveAll(target)
	}
	if entry.Kind == "directory" {
		return nil
	}
	if entry.Kind == "submodule" {
		return fmt.Errorf("GitHub draft review does not yet support submodule change %s; review locally", path)
	}
	if entry.Worktree.Ref == nil {
		return fmt.Errorf("captured content for %s is unavailable; review locally", path)
	}
	content, err := store.ReadContentRef(*entry.Worktree.Ref)
	if err != nil {
		return fmt.Errorf("read captured content for %s: %w", path, err)
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if entry.Kind == "symlink" {
		return os.Symlink(string(content), target)
	}
	mode := os.FileMode(0o644)
	if parsed, parseErr := strconv.ParseUint(entry.Worktree.Mode, 8, 32); parseErr == nil {
		mode = os.FileMode(parsed) & os.ModePerm
	}
	return os.WriteFile(target, content, mode)
}

func (g *GitHub) applySnapshot(store *task.Store, root string, entries map[string]task.FileEntry, paths []string) error {
	for _, path := range paths {
		entry, ok := entries[path]
		if !ok {
			if err := g.materialize(store, root, nil, path); err != nil {
				return err
			}
			continue
		}
		if err := g.materialize(store, root, &entry, path); err != nil {
			return err
		}
	}
	return nil
}

func withGitIdentity(env []string) []string {
	values := map[string]string{}
	for _, item := range env {
		if key, value, ok := strings.Cut(item, "="); ok {
			values[key] = value
		}
	}
	values["GIT_AUTHOR_NAME"] = "RoleMux Review"
	values["GIT_AUTHOR_EMAIL"] = "rolemux-review@users.noreply.github.com"
	values["GIT_COMMITTER_NAME"] = values["GIT_AUTHOR_NAME"]
	values["GIT_COMMITTER_EMAIL"] = values["GIT_AUTHOR_EMAIL"]
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

func (g *GitHub) commitSnapshot(ctx context.Context, worktree, message string, paths []string) (string, bool, error) {
	args := append([]string{"add", "-A", "--"}, paths...)
	if _, err := g.git(ctx, worktree, args...); err != nil {
		return "", false, err
	}
	status, err := g.git(ctx, worktree, "status", "--porcelain", "--", ".")
	if err != nil {
		return "", false, err
	}
	if status == "" {
		sha, err := g.git(ctx, worktree, "rev-parse", "HEAD")
		return sha, false, err
	}
	_, commitErr := g.gitWithEnv(ctx, worktree, withGitIdentity(g.Env), "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "--no-verify", "-m", message)
	if commitErr != nil {
		return "", false, commitErr
	}
	sha, err := g.git(ctx, worktree, "rev-parse", "HEAD")
	return sha, true, err
}

func (g *GitHub) reviewCommits(ctx context.Context, st task.State, previous *task.ExternalReview) (base, head string, err error) {
	paths := changedFilePaths(st)
	if len(paths) == 0 {
		return "", "", errors.New("reviewed candidate has no changed files")
	}
	store := task.NewStore(g.RepoRoot)
	tempRoot, err := os.MkdirTemp("", "rolemux-github-review-")
	if err != nil {
		return "", "", err
	}
	worktree := filepath.Join(tempRoot, "worktree")
	defer os.RemoveAll(tempRoot)
	start := "HEAD"
	if previous != nil && previous.BaseCommit != "" {
		start = previous.BaseCommit
		if _, err := g.git(ctx, g.RepoRoot, "cat-file", "-e", start+"^{commit}"); err != nil {
			if _, fetchErr := g.git(ctx, g.RepoRoot, "fetch", "--no-tags", previous.Remote, "refs/heads/"+previous.BaseBranch); fetchErr != nil {
				return "", "", fmt.Errorf("recover review base: %w", fetchErr)
			}
		}
	}
	if _, err := g.git(ctx, g.RepoRoot, "worktree", "add", "--detach", worktree, start); err != nil {
		return "", "", fmt.Errorf("create isolated review worktree: %w", err)
	}
	defer func() {
		_, _ = g.git(context.Background(), g.RepoRoot, "worktree", "remove", "--force", worktree)
	}()

	if previous == nil || previous.BaseCommit == "" {
		if err := g.applySnapshot(store, worktree, manifestMap(st.ScopedBaseline), paths); err != nil {
			return "", "", err
		}
		base, _, err = g.commitSnapshot(ctx, worktree, "RoleMux review baseline for "+st.ID, paths)
		if err != nil {
			return "", "", fmt.Errorf("create review baseline: %w", err)
		}
	} else {
		base = previous.BaseCommit
	}
	if err := g.applySnapshot(store, worktree, manifestMap(st.CandidateManifest), paths); err != nil {
		return "", "", err
	}
	head, changed, err := g.commitSnapshot(ctx, worktree, "RoleMux reviewed candidate for "+st.ID, paths)
	if err != nil {
		return "", "", fmt.Errorf("create review candidate: %w", err)
	}
	if !changed {
		return "", "", errors.New("reviewed candidate is identical to its captured baseline")
	}
	return base, head, nil
}

func parseRemoteSHA(output string) string {
	fields := strings.Fields(output)
	if len(fields) >= 1 {
		return fields[0]
	}
	return ""
}

func (g *GitHub) remoteSHA(ctx context.Context, remote, branch string) (string, error) {
	out, err := g.git(ctx, g.RepoRoot, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	return parseRemoteSHA(out), nil
}

func (g *GitHub) pushReviewRefs(ctx context.Context, remote remoteInfo, review task.ExternalReview) error {
	baseRemote, err := g.remoteSHA(ctx, remote.Name, review.BaseBranch)
	if err != nil {
		return err
	}
	if baseRemote == "" {
		lease := "--force-with-lease=refs/heads/" + review.BaseBranch + ":"
		if _, err := g.git(ctx, g.RepoRoot, "push", lease, remote.Name, review.BaseCommit+":refs/heads/"+review.BaseBranch); err != nil {
			return fmt.Errorf("push review baseline: %w", err)
		}
	} else if baseRemote != review.BaseCommit {
		return errors.New("remote review baseline changed unexpectedly; refusing to overwrite it")
	}
	headRemote, err := g.remoteSHA(ctx, remote.Name, review.HeadBranch)
	if err != nil {
		return err
	}
	lease := "--force-with-lease=refs/heads/" + review.HeadBranch + ":" + headRemote
	if _, err := g.git(ctx, g.RepoRoot, "push", lease, remote.Name, review.HeadCommit+":refs/heads/"+review.HeadBranch); err != nil {
		return fmt.Errorf("push reviewed candidate: %w", err)
	}
	return nil
}

type prInfo struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	State   string `json:"state"`
	IsDraft bool   `json:"isDraft"`
}

func decodePR(data []byte) (prInfo, error) {
	var info prInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return prInfo{}, err
	}
	if info.Number <= 0 || strings.TrimSpace(info.URL) == "" {
		return prInfo{}, errors.New("GitHub CLI returned incomplete pull request data")
	}
	return info, nil
}

func (g *GitHub) ensurePR(ctx context.Context, st task.State, review task.ExternalReview) (prInfo, error) {
	if review.Number > 0 {
		data, err := g.gh(ctx, "pr", "view", strconv.Itoa(review.Number), "--repo", review.Repository, "--json", "number,url,state,isDraft")
		if err == nil {
			info, decodeErr := decodePR(data)
			if decodeErr == nil && strings.EqualFold(info.State, "OPEN") {
				return info, nil
			}
		}
	}
	data, err := g.gh(ctx, "pr", "list", "--repo", review.Repository, "--head", review.HeadBranch, "--state", "open", "--limit", "1", "--json", "number,url,state,isDraft")
	if err != nil {
		return prInfo{}, err
	}
	var existing []prInfo
	if err := json.Unmarshal(data, &existing); err != nil {
		return prInfo{}, err
	}
	if len(existing) > 0 {
		return existing[0], nil
	}
	paths := changedFilePaths(st)
	body := "This draft PR is a temporary RoleMux human-review surface. Do not merge it.\n\n" +
		"Task: `" + st.ID + "`\n\nScope: `" + st.Scope + "`\n\nChanged files:\n"
	for _, path := range paths {
		body += "- `" + strings.ReplaceAll(path, "`", "\\`") + "`\n"
	}
	created, err := g.gh(ctx, "pr", "create", "--repo", review.Repository, "--draft", "--base", review.BaseBranch, "--head", review.HeadBranch, "--title", "RoleMux review: "+st.ID, "--body", body)
	if err != nil {
		return prInfo{}, err
	}
	url := strings.TrimSpace(string(created))
	if url == "" {
		return prInfo{}, errors.New("GitHub CLI created no pull request URL")
	}
	view, err := g.gh(ctx, "pr", "view", url, "--repo", review.Repository, "--json", "number,url,state,isDraft")
	if err != nil {
		return prInfo{}, err
	}
	return decodePR(view)
}

// Publish creates or updates one review-only draft PR without touching the
// user's current branch, index, or worktree.
func (g *GitHub) Publish(ctx context.Context, st task.State, record task.ApprovalRecord) (task.ExternalReview, error) {
	if record.Kind != task.ApprovalKindCode || record.Status != "" {
		return task.ExternalReview{}, errors.New("GitHub review requires a pending code approval")
	}
	var previous *task.ExternalReview
	if record.ExternalReview != nil && record.ExternalReview.Provider == "github" {
		copyReview := *record.ExternalReview
		previous = &copyReview
	}
	preferredRemote := ""
	if previous != nil {
		preferredRemote = previous.Remote
	}
	remote, err := g.Check(ctx, preferredRemote)
	if err != nil {
		return task.ExternalReview{}, err
	}
	base, head, err := g.reviewCommits(ctx, st, previous)
	if err != nil {
		return task.ExternalReview{}, err
	}
	review := task.ExternalReview{Provider: "github", Repository: remote.Repository, Remote: remote.Name, BaseCommit: base, HeadCommit: head, PublishedCandidateFingerprint: record.SubjectFingerprint}
	if previous == nil {
		suffix := shortHash(record.GateID)
		prefix := "rolemux-review/" + reviewBranchPart(st.ID) + "-" + suffix
		review.BaseBranch, review.HeadBranch = prefix+"-base", prefix+"-candidate"
	} else {
		review = *previous
		review.HeadCommit = head
		review.PublishedCandidateFingerprint = record.SubjectFingerprint
	}
	if err := g.pushReviewRefs(ctx, remote, review); err != nil {
		return task.ExternalReview{}, err
	}
	pr, err := g.ensurePR(ctx, st, review)
	if err != nil {
		return task.ExternalReview{}, fmt.Errorf("create or locate draft PR: %w", err)
	}
	review.Number, review.URL = pr.Number, pr.URL
	now := time.Now().UTC()
	if g.Now != nil {
		now = g.Now().UTC()
	}
	review.PublishedAt = now
	return review, nil
}

type apiComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	State     string `json:"state"`
	Submitted string `json:"submitted_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

func decodePages(data []byte) ([]apiComment, error) {
	var pages [][]apiComment
	if err := json.Unmarshal(data, &pages); err == nil {
		var result []apiComment
		for _, page := range pages {
			result = append(result, page...)
		}
		return result, nil
	}
	var result []apiComment
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (g *GitHub) comments(ctx context.Context, endpoint string) ([]apiComment, error) {
	data, err := g.gh(ctx, "api", "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, err
	}
	return decodePages(data)
}

func appendFeedbackLine(lines []string, kind string, comment apiComment) []string {
	body := strings.TrimSpace(comment.Body)
	if body == "" && strings.EqualFold(comment.State, "CHANGES_REQUESTED") {
		body = "Changes requested on GitHub."
	}
	if body == "" {
		return lines
	}
	where := ""
	if comment.Path != "" {
		where = " on " + comment.Path
		if comment.Line > 0 {
			where += ":" + strconv.Itoa(comment.Line)
		}
	}
	return append(lines, fmt.Sprintf("[%s by @%s%s]\n%s", kind, comment.User.Login, where, body))
}

// FetchFeedback reads only comments/reviews newer than the saved cursors.
// The caller decides whether importing them means request_changes.
func (g *GitHub) FetchFeedback(ctx context.Context, review task.ExternalReview) (Feedback, error) {
	if review.Provider != "github" || review.Number <= 0 || review.Repository == "" {
		return Feedback{}, errors.New("no GitHub draft review is attached to this approval")
	}
	issue, err := g.comments(ctx, fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", review.Repository, review.Number))
	if err != nil {
		return Feedback{}, fmt.Errorf("read PR conversation: %w", err)
	}
	inline, err := g.comments(ctx, fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100", review.Repository, review.Number))
	if err != nil {
		return Feedback{}, fmt.Errorf("read inline PR comments: %w", err)
	}
	reviews, err := g.comments(ctx, fmt.Sprintf("repos/%s/pulls/%d/reviews?per_page=100", review.Repository, review.Number))
	if err != nil {
		return Feedback{}, fmt.Errorf("read PR reviews: %w", err)
	}
	var lines []string
	for _, comment := range issue {
		if comment.ID > review.LastIssueCommentID {
			lines = appendFeedbackLine(lines, "PR comment", comment)
			if comment.ID > review.LastIssueCommentID {
				review.LastIssueCommentID = comment.ID
			}
		}
	}
	for _, comment := range inline {
		if comment.ID > review.LastReviewCommentID {
			lines = appendFeedbackLine(lines, "inline comment", comment)
			review.LastReviewCommentID = comment.ID
		}
	}
	for _, comment := range reviews {
		if comment.ID > review.LastReviewID {
			lines = appendFeedbackLine(lines, "PR review", comment)
			review.LastReviewID = comment.ID
		}
	}
	now := time.Now().UTC()
	if g.Now != nil {
		now = g.Now().UTC()
	}
	review.LastSyncedAt = now
	text := strings.Join(lines, "\n\n")
	if len(text) > maxFeedbackBytes {
		text = text[:maxFeedbackBytes] + "\n\n[RoleMux truncated additional GitHub feedback.]"
	}
	return Feedback{Text: text, Review: review}, nil
}
