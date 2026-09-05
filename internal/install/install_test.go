package install

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEmbeddedSkillMatchesRepositoryRoot(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	rootSkill := filepath.Join(filepath.Dir(file), "..", "..", "SKILL.md")
	contents, err := os.ReadFile(rootSkill)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, Content()) {
		t.Fatal("root and embedded SKILL.md differ")
	}
}

func TestInstallGlobalIsIdempotentAndConflictSafe(t *testing.T) {
	home := t.TempDir()
	hosts, err := ParseHosts("codex,claude,codex")
	if err != nil {
		t.Fatal(err)
	}
	results, err := InstallGlobal(home, hosts, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Host != "claude" || results[1].Host != "codex" {
		t.Fatalf("unexpected results: %#v", results)
	}
	results, err = InstallGlobal(home, hosts, false)
	if err != nil || results[0].Status != "unchanged" || results[1].Status != "unchanged" {
		t.Fatalf("idempotent install: %#v, %v", results, err)
	}

	destination := filepath.Join(home, ".codex", "skills", "rolemux", "SKILL.md")
	if err := os.WriteFile(destination, []byte("user content"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destination, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallGlobal(home, []string{"codex"}, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	results, err = InstallGlobal(home, []string{"codex"}, true)
	if err != nil || results[0].Status != "replaced" {
		t.Fatalf("forced install: %#v, %v", results, err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("replacement mode = %o", info.Mode().Perm())
	}
}

func TestParseHostsRejectsUnknown(t *testing.T) {
	if _, err := ParseHosts("codex,unknown"); err == nil {
		t.Fatal("expected unknown host error")
	}
}
