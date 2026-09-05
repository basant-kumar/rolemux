package task

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitTest(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

func testRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.email", "tests@example.invalid")
	gitTest(t, root, "config", "user.name", "RoleMux Tests")
	return root
}

func TestCanonicalScopeAndOverlapAreRepositoryRelative(t *testing.T) {
	if got, err := CanonicalScope("./b, a, a"); err != nil || got != "a,b" {
		t.Fatalf("canonical scope %q %v", got, err)
	}
	for _, bad := range []string{"/tmp", "../outside", ".git/config", ".rolemux/state/tasks/x.json"} {
		if _, err := CanonicalScope(bad); err == nil {
			t.Errorf("accepted unsafe scope %q", bad)
		}
	}
	if got, err := CanonicalScope(".rolemux/plans/x.md"); err != nil || got != ".rolemux/plans/x.md" {
		t.Fatalf("tracked project plan scope should be accepted: %q %v", got, err)
	}
	if !ScopesOverlap("internal/a", "internal") || ScopesOverlap("internal/a", "cmd") {
		t.Fatal("scope overlap calculation incorrect")
	}
}

func TestManifestContainsExactWorktreeAndIndexAndExcludesSiblings(t *testing.T) {
	root := testRepo(t)
	for name, data := range map[string]string{"internal/a.txt": "old", "internal/b.txt": "b", "cmd/main.go": "main"} {
		name := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, root, "add", "internal/a.txt", "internal/b.txt", "cmd/main.go")
	if err := os.WriteFile(filepath.Join(root, "internal/a.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "internal/b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "internal/link")); err != nil {
		t.Fatal(err)
	}
	w := NewWorktree(root)
	entries, err := w.ManifestForScope("internal/a.txt,internal/link")
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileEntry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if _, ok := byPath["internal/b.txt"]; ok {
		t.Fatal("out-of-scope deleted file included")
	}
	if byPath["internal/a.txt"].Worktree.Hash == byPath["internal/a.txt"].Index.Blob {
		t.Fatal("worktree and index observations collapsed")
	}
	if byPath["internal/a.txt"].Index.Blob == "" || !byPath["internal/a.txt"].Index.Present {
		t.Fatalf("missing index state: %#v", byPath["internal/a.txt"])
	}
	if byPath["internal/link"].Kind != "symlink" || byPath["internal/link"].Worktree.Hash == "" {
		t.Fatalf("missing symlink target hash: %#v", byPath["internal/link"])
	}
	if _, ok := byPath["internal"]; !ok {
		t.Fatal("structural ancestor missing")
	}
	if err := os.WriteFile(filepath.Join(root, "internal/sibling.txt"), []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := w.ManifestForScope("internal/a.txt,internal/link")
	if err != nil {
		t.Fatal(err)
	}
	if HashManifest(entries) != HashManifest(after) {
		t.Fatal("unrelated out-of-scope sibling changed scoped hash")
	}
}

func TestDefaultScopeExcludesRoleMuxButExplicitPlanScopeWorks(t *testing.T) {
	root := testRepo(t)
	plan := filepath.Join(root, ".rolemux", "plans", "task.md")
	if err := os.MkdirAll(filepath.Dir(plan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan, []byte("plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewWorktree(root)
	all, err := w.ManifestForScope("**")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range all {
		if entry.Path == ".rolemux/plans/task.md" {
			t.Fatal("default scope included RoleMux plan state")
		}
	}
	explicit, err := w.ManifestForScope(".rolemux/plans/task.md")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range explicit {
		if entry.Path == ".rolemux/plans/task.md" && entry.Worktree.Hash != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("explicit plan scope missing: %#v", explicit)
	}
}

func TestStructuralAncestorModeDoesNotChangeFileScopeHash(t *testing.T) {
	root := testRepo(t)
	dir := filepath.Join(root, "internal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewWorktree(root)
	before, err := w.ManifestForScope("internal/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	after, err := w.ManifestForScope("internal/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if HashManifest(before) != HashManifest(after) {
		t.Fatalf("structural ancestor mode staled file scope\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestManifestHashIgnoresDerivedStatusAndHEADButTracksModeAndContent(t *testing.T) {
	a := FileEntry{Path: "a", Kind: "file", Status: " M", Worktree: ContentState{Present: true, Mode: "0600", Hash: "hash", Size: 4}, Index: IndexState{Present: true, Mode: "100644", Blob: "blob", Stages: []int{0}}}
	b := a
	b.Status = "A "
	b.Worktree.Ref = &ContentRef{Path: "/different/immutable/ref", Digest: "digest"}
	if HashManifest([]FileEntry{a}) != HashManifest([]FileEntry{b}) {
		t.Fatal("derived status/ref changed exact hash")
	}
	b.Worktree.Mode = "0700"
	if HashManifest([]FileEntry{a}) == HashManifest([]FileEntry{b}) {
		t.Fatal("mode-only change did not change hash")
	}
	b = a
	b.Index.Blob = "other"
	if HashManifest([]FileEntry{a}) == HashManifest([]FileEntry{b}) {
		t.Fatal("index blob change did not change hash")
	}
}

func TestImmutableBaselineAndCandidateRefsDoNotOverwrite(t *testing.T) {
	root := testRepo(t)
	name := filepath.Join(root, "a.txt")
	if err := os.WriteFile(name, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", "a.txt")
	w := NewWorktree(root)
	first, err := w.ManifestForScope("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, ".private-state")
	baseline, err := CaptureContentRefs(first, root, private, "baseline", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if baseline[0].Worktree.Ref == nil {
		t.Fatalf("missing baseline ref: %#v", baseline)
	}
	baselineBytes, err := os.ReadFile(baseline[0].Worktree.Ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := w.ManifestForScope("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := CaptureContentRefs(second, root, private, "candidate", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if candidate[0].Worktree.Ref == nil || candidate[0].Worktree.Ref.Path == baseline[0].Worktree.Ref.Path {
		t.Fatal("candidate overwrote baseline root")
	}
	if got, _ := os.ReadFile(baseline[0].Worktree.Ref.Path); string(got) != string(baselineBytes) {
		t.Fatal("baseline content changed")
	}
	if got, _ := os.ReadFile(candidate[0].Worktree.Ref.Path); string(got) != "two" {
		t.Fatalf("candidate content %q", got)
	}
}

func TestImmutableContentRefVerifiesExistingDigest(t *testing.T) {
	root := testRepo(t)
	name := filepath.Join(root, "a.txt")
	if err := os.WriteFile(name, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := NewWorktree(root).ManifestForScope("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, ".private")
	captured, err := CaptureContentRefs(entries, root, private, "baseline", "task")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(captured[0].Worktree.Ref.Path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureContentRefs(entries, root, private, "baseline", "task"); err == nil {
		t.Fatal("same-sized corrupted immutable blob was accepted")
	}
}

func TestCaptureIndexRefsPreservesEveryStage(t *testing.T) {
	root := testRepo(t)
	blob := func(contents string) string {
		cmd := exec.Command("git", "-C", root, "hash-object", "-w", "--stdin")
		cmd.Stdin = strings.NewReader(contents)
		output, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(output))
	}
	one, two := blob("one"), blob("two")
	entries := []FileEntry{{Path: "conflict.txt", Kind: "deleted", Index: IndexState{
		Present: true, Mode: "100644", Blob: one, Stages: []int{1, 2},
		Entries: []IndexStage{{Stage: 1, Mode: "100644", Blob: one}, {Stage: 2, Mode: "100644", Blob: two}},
	}}}
	captured, err := CaptureIndexRefs(entries, root, filepath.Join(root, ".private"), "baseline", "task")
	if err != nil {
		t.Fatal(err)
	}
	if len(captured[0].Index.Entries) != 2 || captured[0].Index.Entries[0].Ref == nil || captured[0].Index.Entries[1].Ref == nil {
		t.Fatalf("stage refs missing: %#v", captured)
	}
	changed := cloneFileEntries(entries)
	changed[0].Index.Entries[1].Blob = one
	if HashManifest(entries) == HashManifest(changed) {
		t.Fatal("secondary unmerged stage blob did not affect manifest hash")
	}
}

func TestStoreTokenCASAndPersistentFlock(t *testing.T) {
	s := NewStoreAt(filepath.Join(t.TempDir(), "state"))
	token := NewToken()
	st := State{ID: "task-1", RepoRoot: "/repo", Phase: PhaseImplementing, InFlight: &InFlight{Token: token, Operation: "implement"}}
	if err := s.Create(st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateOwned(st.ID, "wrong", func(st *State) error { st.Phase = PhaseApproved; return nil }); !errors.Is(err, ErrStaleOperation) {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := s.UpdateOwned(st.ID, token, func(st *State) error { st.Phase = PhaseImplementationReady; return nil }); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(st.ID); got.Phase != PhaseImplementationReady {
		t.Fatal("CAS update not persisted")
	}
	lock, err := s.Lock(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() {
		l, e := s.Lock(st.ID)
		if e == nil {
			e = l.Unlock()
		}
		second <- e
	}()
	select {
	case <-second:
		t.Fatal("second lock acquired before release")
	case <-time.After(30 * time.Millisecond):
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second lock did not acquire")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, st.ID+".lock")); err != nil {
		t.Fatalf("lock file must persist: %v", err)
	}
}

func TestWritePlanRejectsPathTraversalAndWritesAtomically(t *testing.T) {
	root := testRepo(t)
	if err := WritePlan(root, "a-task", "# plan\n"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, ".rolemux", "plans", "a-task.md")); err != nil || string(b) != "# plan\n" {
		t.Fatalf("plan write: %q %v", b, err)
	}
	if err := WritePlan(root, "../escape", "bad"); !errors.Is(err, ErrInvalidTaskID) {
		t.Fatalf("traversal: %v", err)
	}
}
