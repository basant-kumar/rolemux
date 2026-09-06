package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitConfigPathAndLoadReplaceNormalLocations(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	replacementDir := t.TempDir()
	replacement := filepath.Join(replacementDir, "replacement.toml")
	global := filepath.Join(home, ".rolemux", "config.toml")
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		global:                               "[profiles.planner]\nprovider='codex'\nmodel='global'\n",
		filepath.Join(root, ".rolemux.toml"): "[profiles.planner]\nprovider='codex'\nmodel='project'\n",
		replacement:                          "[profiles.planner]\nprovider='codex'\nmodel='replacement'\n",
	} {
		if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	environ := []string{"HOME=" + home, "ROLEMUX_CONFIG=  " + replacement + "  "}
	if got := ExplicitConfigPath(environ); got != replacement {
		t.Fatalf("explicit config path = %q, want %q", got, replacement)
	}
	cfg, err := LoadWithEnv(root, environ)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Profiles[RolePlanner].Model; got != "replacement" {
		t.Fatalf("replacement model = %q, want replacement", got)
	}
}

func TestLoadWithEnvExplicitConfigDoesNotRequireHomeOrRepository(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	replacement := filepath.Join(t.TempDir(), "replacement.toml")
	if err := os.WriteFile(replacement, []byte("review_max_rounds = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWithEnv("", []string{"ROLEMUX_CONFIG=" + replacement})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewMaxRounds == nil || cfg.EffectiveReviewMaxRounds() != 0 {
		t.Fatalf("explicit replacement setting = %#v", cfg.ReviewMaxRounds)
	}
	if global, project := ConfigPaths(root, []string{"HOME=" + home}); global == "" || project == "" {
		t.Fatalf("normal config paths were not available: global=%q project=%q", global, project)
	}
}
