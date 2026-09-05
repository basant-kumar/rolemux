package capability

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, directory, name, description, body string) string {
	t.Helper()
	dir := filepath.Join(root, directory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	contents := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProviderPrecedenceIsIndependentAndMetadataOnly(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "design", "design", "personal Claude design", "SECRET BODY")
	writeSkill(t, filepath.Join(repo, ".claude", "skills"), "design", "design", "project Claude design", "PROJECT BODY")
	writeSkill(t, filepath.Join(home, ".copilot", "skills"), "audit", "audit", "personal Copilot audit", "SECRET BODY")
	writeSkill(t, filepath.Join(repo, ".github", "skills"), "audit", "audit", "project Copilot audit", "PROJECT BODY")

	claude := Discover(Options{Provider: "claude", Task: "design UI", RepoRoot: repo, Environ: []string{"HOME=" + home}})
	if len(claude.Skills) == 0 || claude.Skills[0].Description != "personal Claude design" || !claude.Skills[0].Native {
		t.Fatalf("Claude precedence = %#v", claude.Skills)
	}
	if note := claude.Note("planner"); strings.Contains(note, "SECRET BODY") || strings.Contains(note, "PROJECT BODY") {
		t.Fatalf("skill body leaked into note: %s", note)
	}

	copilot := Discover(Options{Provider: "copilot", Task: "audit code", RepoRoot: repo, Environ: []string{"HOME=" + home}})
	if len(copilot.Skills) == 0 || copilot.Skills[0].Description != "project Copilot audit" || !copilot.Skills[0].Native {
		t.Fatalf("Copilot precedence = %#v", copilot.Skills)
	}
}

func TestDiscoveryAttributesNativeAndHostSkillsAndDeduplicates(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "native", "ui-design", "Design interfaces", "ignored")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "host", "external-data", "Fetch external design data", "ignored")

	got := Discover(Options{Provider: "codex", Task: "design a UI with external data", RepoRoot: repo, Environ: []string{"HOME=" + home}})
	if len(got.Skills) != 2 {
		t.Fatalf("skills = %#v", got.Skills)
	}
	if got.Skills[0].Name != "ui-design" || !got.Skills[0].Native || got.Skills[0].Provider != "shared" {
		t.Fatalf("native skill = %#v", got.Skills[0])
	}
	if got.Skills[1].Name != "external-data" || got.Skills[1].Native || got.Skills[1].Provider != "claude" {
		t.Fatalf("host skill = %#v", got.Skills[1])
	}
	note := got.Note("implementer")
	for _, want := range []string{"native-discoverable", "host-mediated", "return needs_input", "resume this same session"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note missing %q: %s", want, note)
		}
	}
}

func TestMetadataBoundsMultilineSecretsAndSymlinks(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	root := filepath.Join(home, ".agents", "skills")
	dir := filepath.Join(root, "multiline")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := "---\nname: multiline\ndescription: >\n  Useful UI skill\n  with token sk-abcdefghijklmnop\n---\nBODY MUST NOT LEAK"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeSkill(t, outside, "escaped", "escaped", "must not appear", "")
	if err := os.Symlink(filepath.Join(outside, "escaped"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxSkills+12; index++ {
		writeSkill(t, root, "skill-"+strconv.Itoa(index), "skill-"+strconv.Itoa(index), strings.Repeat("x", maxDescriptionRunes+40), "body")
	}

	got := Discover(Options{Provider: "codex", Task: "UI", RepoRoot: repo, Environ: []string{"HOME=" + home}})
	if len(got.Skills) > maxSkills || len(got.Note("planner")) > maxNoteBytes {
		t.Fatalf("bounds exceeded: skills=%d note=%d", len(got.Skills), len(got.Note("planner")))
	}
	note := got.Note("planner")
	if strings.Contains(note, "abcdefghijklmnop") || strings.Contains(note, "BODY MUST NOT LEAK") || strings.Contains(note, "must not appear") {
		t.Fatalf("unsafe metadata leaked: %s", note)
	}
	if !strings.Contains(note, "[redacted]") {
		t.Fatalf("secret marker was not redacted: %s", note)
	}
}

func TestPXPipeHelperRequiresExecutableInSuppliedEnvironment(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "pxpipe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := Discover(Options{Provider: "claude", RepoRoot: repo, Environ: []string{"HOME=" + home, "PATH=" + bin}})
	if len(got.Helpers) != 1 || got.Helpers[0].Path != path || !strings.Contains(got.Note("code_reviewer"), "pxpipe itself decides") {
		t.Fatalf("helper = %#v note=%s", got.Helpers, got.Note("code_reviewer"))
	}
	if got := Discover(Options{Provider: "claude", RepoRoot: repo, Environ: []string{"HOME=" + home, "PATH=" + t.TempDir()}}); len(got.Helpers) != 0 {
		t.Fatalf("missing pxpipe discovered: %#v", got.Helpers)
	}
}

func TestMissingHomeNeverCreatesRelativePersonalRoots(t *testing.T) {
	roots := discoveryRoots("codex", "/repo", "", []string{"PATH=/bin"})
	for _, root := range roots {
		if root.path != "" && !filepath.IsAbs(root.path) {
			t.Fatalf("relative discovery root without HOME: %#v", root)
		}
	}
}

func TestCodexRetainsSameNamedSkillsFromDistinctNativeScopes(t *testing.T) {
	home, repo, admin := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(repo, ".agents", "skills"), "repo-copy", "duplicate", "Repository copy", "ignored")
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "user-copy", "duplicate", "User copy", "ignored")
	writeSkill(t, admin, "admin-copy", "duplicate", "Admin copy", "ignored")

	got := Discover(Options{Provider: "codex", Task: "duplicate", RepoRoot: repo, CodexAdminSkills: admin, Environ: []string{"HOME=" + home}})
	if len(got.Skills) != 3 {
		t.Fatalf("same-named Codex skills were collapsed: %#v", got.Skills)
	}
	for _, skill := range got.Skills {
		if !skill.Native || skill.Name != "duplicate" {
			t.Fatalf("unexpected Codex skill: %#v", skill)
		}
	}
}

func TestCopilotHomePluginsAndCustomSkillDirectories(t *testing.T) {
	home, repo, copilotHome, custom := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(home, ".copilot", "skills"), "ignored-default", "ignored-default", "Wrong home", "ignored")
	writeSkill(t, filepath.Join(copilotHome, "skills"), "personal", "personal", "Configured Copilot home", "ignored")
	pluginSkills := filepath.Join(copilotHome, "installed-plugins", "market", "plugin", "skills")
	writeSkill(t, pluginSkills, "plugin-skill", "plugin-skill", "Installed plugin", "ignored")
	writeSkill(t, custom, "custom-skill", "custom-skill", "Custom source", "ignored")

	got := Discover(Options{Provider: "copilot", RepoRoot: repo, Environ: []string{
		"HOME=" + home,
		"COPILOT_HOME=" + copilotHome,
		"COPILOT_SKILLS_DIRS=" + custom + ",relative-path",
	}})
	for _, want := range []string{filepath.Join(copilotHome, "skills"), pluginSkills, custom} {
		if !containsString(got.SkillDirectories, want) {
			t.Fatalf("Copilot skill directories=%#v, missing %q", got.SkillDirectories, want)
		}
	}
	for _, skill := range got.Skills {
		if skill.Name == "ignored-default" {
			t.Fatalf("default Copilot home leaked through override: %#v", got.Skills)
		}
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
