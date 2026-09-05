package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPathsUseRoleMuxHomeDirectory(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	global, project := ConfigPaths(root, []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(t.TempDir(), "xdg"),
	})
	if want := filepath.Join(home, ".rolemux", "config.toml"); global != want {
		t.Fatalf("global config path = %q, want %q", global, want)
	}
	if want := filepath.Join(root, ".rolemux.toml"); project != want {
		t.Fatalf("project config path = %q, want %q", project, want)
	}
}

func TestLoadLayersAndSharedReviewerExpansion(t *testing.T) {
	root := t.TempDir()
	globalHome := t.TempDir()
	global := filepath.Join(globalHome, ".rolemux", "config.toml")
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[profiles.planner]\nprovider='codex'\nmodel='global-plan'\n[profiles.reviewer]\nprovider='claude'\nmodel='review'\neffort='high'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rolemux.toml"), []byte("[profiles.implementer]\nprovider='codex'\nmodel='project-impl'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithEnv(root, []string{"HOME=" + globalHome})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := cfg.EffectiveProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if profiles[RolePlanner].Model != "global-plan" || profiles[RoleImplementer].Model != "project-impl" {
		t.Fatalf("layering failed: %#v", profiles)
	}
	if profiles[RolePlanReviewer] != profiles[RoleCodeReviewer] || profiles[RolePlanReviewer].Provider != "claude" {
		t.Fatalf("reviewer expansion failed: %#v", profiles)
	}
}

func TestProviderTurnTimeoutDefaultsAndValidates(t *testing.T) {
	if got := Default().ProviderTurnTimeoutSeconds; got != 900 {
		t.Fatalf("default provider timeout = %d", got)
	}
	for _, seconds := range []int{29, 7201} {
		cfg := Default()
		cfg.ProviderTurnTimeoutSeconds = seconds
		if err := Validate(cfg); err == nil {
			t.Fatalf("timeout %d was accepted", seconds)
		}
	}
	for _, seconds := range []int{0, 30, 900, 7200} {
		cfg := Default()
		cfg.ProviderTurnTimeoutSeconds = seconds
		if err := Validate(cfg); err != nil {
			t.Fatalf("timeout %d rejected: %v", seconds, err)
		}
	}
}

func TestExplicitConfigReplacesGlobalAndProject(t *testing.T) {
	root, home, explicit := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "explicit.toml")
	global := filepath.Join(home, ".rolemux", "config.toml")
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[profiles.planner]\nprovider='codex'\nmodel='global'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rolemux.toml"), []byte("[profiles.planner]\nprovider='codex'\nmodel='project'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicit, []byte("[profiles.planner]\nprovider='codex'\nmodel='explicit'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithEnv(root, []string{"HOME=" + home, "ROLEMUX_CONFIG=" + explicit})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Profiles[RolePlanner].Model; got != "explicit" {
		t.Fatalf("explicit replacement got %q", got)
	}
}

func TestConfigureProfileIsAtomicAndPreservesUnrelatedTables(t *testing.T) {
	name := filepath.Join(t.TempDir(), "config.toml")
	original := "title='keep me'\n[profiles.planner]\nprovider='codex'\nmodel='old'\n[profiles.implementer]\nprovider='codex'\nmodel='other'\n[providers.codex]\ncli_path='/bin/codex'\n"
	if err := os.WriteFile(name, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := FileHash(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfigureProfile(name, "planner", Profile{Provider: "codex", Model: "new", Effort: "high", Speed: "priority"}, before); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"title = \"keep me\"", "model = \"new\"", "model = \"other\"", "cli_path = \"/bin/codex\"", "effort = \"high\"", "speed = \"priority\""} {
		if !strings.Contains(s, want) {
			t.Errorf("updated config missing %q:\n%s", want, s)
		}
	}
	if info, err := os.Stat(name); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}

func TestConfigureProfileDetectsOnlyHashDrift(t *testing.T) {
	name := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(name, []byte("[profiles.planner]\nprovider='codex'\nmodel='old'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := FileHash(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("[profiles.planner]\nprovider='codex'\nmodel='changed'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = ConfigureProfile(name, "planner", Profile{Provider: "codex", Model: "new"}, before)
	if !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.toml")
	if err := ConfigureProfile(missing, "planner", Profile{Provider: "codex", Model: "new"}, ""); err != nil {
		t.Fatalf("new file should be allowed: %v", err)
	}
}

func TestValidateRejectsSecretsAndUnsafeAuthCommand(t *testing.T) {
	if err := ValidateEnvRef("not-an-env"); err == nil {
		t.Fatal("invalid env ref accepted")
	}
	if err := ValidateAuthCommand(AuthCommand{Command: "sh", Args: []string{"-c", "echo $TOKEN"}}); err == nil {
		t.Fatal("shell command accepted")
	}
	if err := ValidateAuthCommand(AuthCommand{Command: "tools/login"}); err == nil {
		t.Fatal("relative command path accepted")
	}
	cfg := Default()
	cfg.Profiles[RolePlanner] = Profile{Provider: "codex", Model: "sk-secret"}
	if err := Validate(cfg); !errors.Is(err, ErrUnsafeConfig) {
		t.Fatalf("expected unsafe config, got %v", err)
	}
	if err := ValidateProvider("claude", Provider{APIKeyEnv: "ANTHROPIC_API_KEY"}); err != nil {
		t.Fatal(err)
	}
}

func TestImportConfigReplacesOwnedTablesAndPreservesUnrelatedKeys(t *testing.T) {
	name := filepath.Join(t.TempDir(), "config.toml")
	original := "title='keep'\n[profiles.planner]\nprovider='codex'\nmodel='old'\n[providers.codex]\ncli_path='/old/codex'\n"
	if err := os.WriteFile(name, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := FileHash(name)
	if err != nil {
		t.Fatal(err)
	}
	fragment := []byte("[profiles.planner]\nprovider='claude'\nmodel='claude-fable-5'\neffort='max'\n[providers.claude]\napi_key_env='ANTHROPIC_API_KEY'\n[models.claude.fable]\nid='claude-fable-5'\navailability='unknown'\n")
	if err := ImportConfigAtomic(name, fragment, before); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"title = \"keep\"", "model = \"claude-fable-5\"", "api_key_env = \"ANTHROPIC_API_KEY\"", "availability = \"unknown\""} {
		if !strings.Contains(text, want) {
			t.Errorf("import missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "/old/codex") || strings.Contains(text, "model = \"old\"") {
		t.Fatalf("owned tables were not replaced:\n%s", text)
	}
}

func TestProviderAndCustomModelRoundTripKeepsSafeFields(t *testing.T) {
	name := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.Profiles[RolePlanner] = Profile{Provider: "codex", Model: "local", Effort: "max"}
	cfg.Providers["codex"] = Provider{
		CLIPath: "/opt/codex", GatewayURL: "https://gateway.example.invalid", WireAPI: "responses",
		EnvKey: "OPENAI_API_KEY", EnvHTTPHeaders: map[string]string{"X-Team": "TEAM_HEADER"},
		QueryParams: map[string]string{"region": "west"}, RequestMaxRetries: 2, StreamMaxRetries: 3,
		StreamIdleTimeoutMS: 4000, SupportsStandaloneWebSearch: true,
		Auth: AuthCommand{Command: "/usr/bin/security", Args: []string{"find-generic-password"}, TimeoutMS: 5000},
	}
	cfg.Models["codex"] = map[string]CustomModel{"local": {
		ID: "local", Availability: "unknown", BaseURL: "https://models.example.invalid", WireAPI: "responses",
		EnvKey: "LOCAL_API_KEY", Efforts: []string{"max"}, DefaultEffort: "max",
	}}
	if err := WriteConfigAtomic(name, cfg, ""); err != nil {
		t.Fatal(err)
	}
	loaded := Default()
	if err := mergeFile(&loaded, name); err != nil {
		t.Fatal(err)
	}
	got := loaded.Providers["codex"]
	if got.GatewayURL != cfg.Providers["codex"].GatewayURL || got.EnvHTTPHeaders["X-Team"] != "TEAM_HEADER" || got.Auth.Command != "/usr/bin/security" {
		t.Fatalf("provider fields lost: %#v", got)
	}
	if model := loaded.Models["codex"]["local"]; model.BaseURL == "" || model.DefaultEffort != "max" {
		t.Fatalf("custom model fields lost: %#v", model)
	}
}

func TestResolveProfileUsesCustomAliasAndRoute(t *testing.T) {
	cfg := Default()
	cfg.Providers["codex"] = Provider{GatewayURL: "https://default.example.invalid", WireAPI: "responses"}
	cfg.Models["codex"] = map[string]CustomModel{"fast": {
		ID: "wire-model", Aliases: []string{"friendly"}, Availability: "unknown",
		Name: "private", BaseURL: "https://models.example.invalid", EnvKey: "MODEL_API_KEY",
	}}
	profile, route := cfg.ResolveProfile(Profile{Provider: "codex", Model: "friendly", Effort: "max"})
	if profile.Model != "wire-model" || route.Name != "private" || route.BaseURL != "https://models.example.invalid" || route.GatewayURL != "" || route.EnvKey != "MODEL_API_KEY" {
		t.Fatalf("profile=%#v route=%#v", profile, route)
	}
}

func TestValidateRejectsAmbiguousCustomModelSelectors(t *testing.T) {
	cfg := Default()
	cfg.Models["codex"] = map[string]CustomModel{
		"one": {ID: "model-one", Aliases: []string{"friendly"}, Availability: "unknown"},
		"two": {ID: "friendly", Availability: "unknown"},
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "selector") {
		t.Fatalf("ambiguous selectors accepted: %v", err)
	}
}
