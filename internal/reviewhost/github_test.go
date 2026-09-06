package reviewhost

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

func reviewRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Review Test"}, {"config", "user.email", "review@example.invalid"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "app.txt"}, {"commit", "-qm", "baseline"}, {"remote", "add", "origin", "https://github.com/example/project.git"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, output)
		}
	}
	return root
}

func capturedState(t *testing.T, root, value, label string) task.State {
	t.Helper()
	store := task.NewStore(root)
	worktree := task.NewWorktree(root)
	baseline, err := worktree.ManifestForScope("app.txt")
	if err != nil {
		t.Fatal(err)
	}
	privateRoot := filepath.Dir(store.Dir)
	baseline, err = task.CaptureContentRefs(baseline, root, privateRoot, "baseline-"+label, "review-task")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err = task.CaptureIndexRefs(baseline, root, privateRoot, "baseline-"+label, "review-task")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
	candidate, err := worktree.ManifestForScope("app.txt")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = task.CaptureContentRefs(candidate, root, privateRoot, "candidate-"+label, "review-task")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = task.CaptureIndexRefs(candidate, root, privateRoot, "candidate-"+label, "review-task")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := task.HashManifest(candidate)
	return task.State{ID: "review-task", RepoRoot: root, Scope: "app.txt", ScopedBaseline: baseline, CandidateManifest: candidate, CandidateManifestHash: fingerprint, ChangeManifest: candidate, Approval: &task.ApprovalRecord{GateID: "gate-code-review", Kind: task.ApprovalKindCode, SubjectFingerprint: fingerprint}}
}

type fakeNetwork struct {
	mu       sync.Mutex
	branches map[string]string
	prCreate int
}

func (f *fakeNetwork) process(ctx context.Context, spec runner.ProcessSpec) (runner.ProcessResult, error) {
	base := filepath.Base(spec.Path)
	if base == "gh" {
		joined := strings.Join(spec.Args, " ")
		switch {
		case strings.HasPrefix(joined, "auth status"):
			return runner.ProcessResult{ProcessStarted: true}, nil
		case strings.HasPrefix(joined, "pr list"):
			return runner.ProcessResult{Stdout: []byte("[]"), ProcessStarted: true}, nil
		case strings.HasPrefix(joined, "pr create"):
			f.mu.Lock()
			f.prCreate++
			f.mu.Unlock()
			return runner.ProcessResult{Stdout: []byte("https://github.com/example/project/pull/7\n"), ProcessStarted: true}, nil
		case strings.HasPrefix(joined, "pr view"):
			return runner.ProcessResult{Stdout: []byte(`{"number":7,"url":"https://github.com/example/project/pull/7","state":"OPEN","isDraft":true}`), ProcessStarted: true}, nil
		default:
			return runner.ProcessResult{}, fmt.Errorf("unexpected gh command: %s", joined)
		}
	}
	if base == "git" && len(spec.Args) > 0 {
		switch spec.Args[0] {
		case "ls-remote":
			branch := strings.TrimPrefix(spec.Args[len(spec.Args)-1], "refs/heads/")
			f.mu.Lock()
			sha := f.branches[branch]
			f.mu.Unlock()
			if sha == "" {
				return runner.ProcessResult{ProcessStarted: true}, nil
			}
			return runner.ProcessResult{Stdout: []byte(sha + "\trefs/heads/" + branch + "\n"), ProcessStarted: true}, nil
		case "push":
			refspec := spec.Args[len(spec.Args)-1]
			sha, ref, ok := strings.Cut(refspec, ":refs/heads/")
			if !ok {
				return runner.ProcessResult{}, fmt.Errorf("unexpected push refspec %q", refspec)
			}
			f.mu.Lock()
			f.branches[ref] = sha
			f.mu.Unlock()
			return runner.ProcessResult{ProcessStarted: true}, nil
		}
	}
	return runner.RunProcess(ctx, spec)
}

func TestPublishUsesIsolatedExactSnapshotAndReusesDraft(t *testing.T) {
	root := reviewRepo(t)
	state := capturedState(t, root, "after one\n", "one")
	beforeStatus, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	gitPath, _ := exec.LookPath("git")
	network := &fakeNetwork{branches: map[string]string{}}
	host := &GitHub{RepoRoot: root, GitPath: gitPath, GHPath: "/fake/gh", Env: os.Environ(), Process: network.process, Now: func() time.Time { return time.Unix(10, 0) }}
	review, err := host.Publish(context.Background(), state, *state.Approval)
	if err != nil {
		t.Fatal(err)
	}
	if review.Number != 7 || review.URL == "" || review.BaseCommit == "" || review.HeadCommit == "" || review.BaseCommit == review.HeadCommit || review.PublishedCandidateFingerprint != state.CandidateManifestHash {
		t.Fatalf("review=%#v", review)
	}
	afterStatus, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil || string(afterStatus) != string(beforeStatus) {
		t.Fatalf("publish changed current checkout: before=%q after=%q err=%v", beforeStatus, afterStatus, err)
	}
	diff, err := exec.Command("git", "-C", root, "diff", review.BaseCommit, review.HeadCommit, "--", "app.txt").Output()
	if err != nil || !strings.Contains(string(diff), "-before") || !strings.Contains(string(diff), "+after one") {
		t.Fatalf("review diff=%q err=%v", diff, err)
	}

	// A new approved candidate updates the same head branch and PR while the
	// original baseline remains stable.
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("after two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree := task.NewWorktree(root)
	candidate, err := worktree.ManifestForScope("app.txt")
	if err != nil {
		t.Fatal(err)
	}
	store := task.NewStore(root)
	candidate, err = task.CaptureContentRefs(candidate, root, filepath.Dir(store.Dir), "candidate-two", state.ID)
	if err != nil {
		t.Fatal(err)
	}
	state.CandidateManifest, state.ChangeManifest = candidate, candidate
	state.CandidateManifestHash = task.HashManifest(candidate)
	state.Approval.SubjectFingerprint = state.CandidateManifestHash
	state.Approval.ExternalReview = &review
	second, err := host.Publish(context.Background(), state, *state.Approval)
	if err != nil {
		t.Fatal(err)
	}
	if second.URL != review.URL || second.BaseBranch != review.BaseBranch || second.HeadBranch != review.HeadBranch || second.BaseCommit != review.BaseCommit || second.HeadCommit == review.HeadCommit || network.prCreate != 1 {
		t.Fatalf("updated review=%#v first=%#v creates=%d", second, review, network.prCreate)
	}
}

func TestFetchFeedbackUsesDurableCursors(t *testing.T) {
	host := &GitHub{RepoRoot: "/repo", GHPath: "/fake/gh", Now: func() time.Time { return time.Unix(20, 0) }}
	host.Process = func(_ context.Context, spec runner.ProcessSpec) (runner.ProcessResult, error) {
		endpoint := spec.Args[len(spec.Args)-1]
		var output string
		switch {
		case strings.Contains(endpoint, "/issues/"):
			output = `[[{"id":11,"body":"Please rename this.","user":{"login":"alice"}}]]`
		case strings.HasSuffix(strings.Split(endpoint, "?")[0], "/comments"):
			output = `[[{"id":21,"body":"Handle nil here.","path":"app.go","line":9,"user":{"login":"bob"}}]]`
		case strings.Contains(endpoint, "/reviews"):
			output = `[[{"id":31,"body":"","state":"CHANGES_REQUESTED","user":{"login":"carol"}}]]`
		default:
			return runner.ProcessResult{}, fmt.Errorf("unexpected endpoint %s", endpoint)
		}
		return runner.ProcessResult{Stdout: []byte(output), ProcessStarted: true}, nil
	}
	result, err := host.FetchFeedback(context.Background(), task.ExternalReview{Provider: "github", Number: 7, Repository: "example/project"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Please rename this.", "app.go:9", "Handle nil here.", "Changes requested on GitHub."} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("feedback missing %q: %s", want, result.Text)
		}
	}
	if result.Review.LastIssueCommentID != 11 || result.Review.LastReviewCommentID != 21 || result.Review.LastReviewID != 31 || result.Review.LastSyncedAt.IsZero() {
		t.Fatalf("cursors=%#v", result.Review)
	}
}

func TestParseGitHubRemote(t *testing.T) {
	for _, value := range []string{"https://github.com/acme/project.git", "git@github.com:acme/project.git", "ssh://git@github.com/acme/project.git"} {
		got, err := parseGitHubRemote("origin", value)
		if err != nil || got.Host != "github.com" || got.Repository != "acme/project" {
			t.Fatalf("remote %q => %#v err=%v", value, got, err)
		}
	}
}
