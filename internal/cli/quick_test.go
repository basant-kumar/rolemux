package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
)

func TestQuickStartSkipsPlannerAndModelDiscovery(t *testing.T) {
	root := cliRepo(t)
	home := t.TempDir()
	configPath := filepath.Join(home, ".rolemux", "config.toml")
	cfg := config.Default()
	cfg.Profiles = map[string]config.Profile{
		config.RolePlanner:     {Provider: "codex", Model: "planner-model"},
		config.RoleImplementer: {Provider: "codex", Model: "implementer-model"},
		config.RoleReviewer:    {Provider: "codex", Model: "reviewer-model"},
	}
	if err := config.WriteConfigAtomic(configPath, cfg, ""); err != nil {
		t.Fatal(err)
	}
	adapter := &loginAdapter{authenticated: true}
	registry := runner.NewRegistry()
	if err := registry.Register("codex", func(_, _ string) (runner.Adapter, string, error) {
		return adapter, "/bin/fixture", nil
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := &app{ctx: context.Background(), in: bytes.NewReader(nil), out: &stdout, errOut: &stderr, cwd: root, environ: []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, runners: registry}
	code := a.run([]string{"quick", "start", "--task", "fix the role badge", "--scope", "internal/cli/configure_view.go", "--id", "quick-cli", "--json"})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q output=%s", code, stderr.String(), stdout.String())
	}
	payload := decodeSingleObject(t, stdout.Bytes())
	result := payload["result"].(map[string]any)
	taskSummary := payload["task"].(map[string]any)
	if result["status"] != "ready" || result["next_action"] != "implement" || taskSummary["complexity"] != "trivial" || taskSummary["direct_implementation"] != true {
		t.Fatalf("payload=%#v", payload)
	}
	if adapter.authCalls != 0 || len(adapter.listRequests) != 0 {
		t.Fatalf("quick start probed provider: auth=%d models=%d", adapter.authCalls, len(adapter.listRequests))
	}
}
