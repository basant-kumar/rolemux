package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/workflow"
)

func TestConfigureExplicitPathOverridesTargetAndReportsReplacement(t *testing.T) {
	root := cliRepo(t)
	home := t.TempDir()
	replacement := filepath.Join(t.TempDir(), "replacement.toml")
	global := filepath.Join(home, ".rolemux", "config.toml")
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("title = 'global'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rolemux.toml"), []byte("title = 'project'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("title = 'replacement'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	a := &app{
		ctx: context.Background(), in: strings.NewReader("[profiles.planner]\nprovider = 'codex'\nmodel = 'imported'\n"), out: &stdout, errOut: &stderr,
		cwd: root, environ: []string{"HOME=" + home, "ROLEMUX_CONFIG=" + replacement, "PATH=" + os.Getenv("PATH")},
	}
	code := a.run([]string{"configure", "--global", "--from", "-", "--json"})
	if code != workflow.ExitOK || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%q", code, stdout.Bytes(), stderr.String())
	}
	payload := decodeSingleObject(t, stdout.Bytes())
	result := payload["result"].(map[string]any)
	if result["path"] != replacement || result["status"] != "imported" {
		t.Fatalf("result=%#v", result)
	}
	data, err := os.ReadFile(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("title = \"replacement\"")) || !bytes.Contains(data, []byte("model = \"imported\"")) {
		t.Fatalf("replacement config=%s", data)
	}
	if globalData, err := os.ReadFile(global); err != nil || !bytes.Contains(globalData, []byte("global")) {
		t.Fatalf("global config changed: data=%s err=%v", globalData, err)
	}
	if projectData, err := os.ReadFile(filepath.Join(root, ".rolemux.toml")); err != nil || !bytes.Contains(projectData, []byte("project")) {
		t.Fatalf("project config changed: data=%s err=%v", projectData, err)
	}
}

func TestConfigureExplicitPathRetainsFlagMutualExclusion(t *testing.T) {
	root := cliRepo(t)
	home := t.TempDir()
	replacement := filepath.Join(t.TempDir(), "replacement.toml")
	if err := os.WriteFile(replacement, []byte("review_max_rounds = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := &app{
		ctx: context.Background(), in: strings.NewReader(""), out: &stdout, errOut: &stderr,
		cwd: root, environ: []string{"HOME=" + home, "ROLEMUX_CONFIG=" + replacement, "PATH=" + os.Getenv("PATH")},
	}
	code := a.run([]string{"configure", "--global", "--project", "--review-max-rounds", "0", "--json"})
	if code != workflow.ExitUsage || stderr.String() != "" {
		t.Fatalf("code=%d output=%s stderr=%q", code, stdout.Bytes(), stderr.String())
	}
	payload := decodeSingleObject(t, stdout.Bytes())
	if payload["error"].(map[string]any)["code"] != "USAGE" {
		t.Fatalf("payload=%#v", payload)
	}
	data, err := os.ReadFile(replacement)
	if err != nil || !bytes.Contains(data, []byte("review_max_rounds = 5")) {
		t.Fatalf("replacement changed: data=%s err=%v", data, err)
	}
}

func TestInteractiveExplicitPathUsesReturnedTargetHashAndSuccessPath(t *testing.T) {
	root := cliRepo(t)
	home := t.TempDir()
	replacement := filepath.Join(t.TempDir(), "replacement.toml")
	if err := os.WriteFile(replacement, []byte("title = 'keep'\nreview_max_rounds = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &loginAdapter{authenticated: true}
	registry := runner.NewRegistry()
	if err := registry.Register("test", func(_, _ string) (runner.Adapter, string, error) {
		return fake, "/bin/test", nil
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	input := strings.Repeat("\x1b[B", 6) + "\r" + strings.Repeat("\x1b[B", 2) + "\r"
	a := &app{
		ctx: context.Background(), in: bytes.NewBufferString(input), out: &output, errOut: &output,
		cwd: root, environ: []string{"HOME=" + home, "ROLEMUX_CONFIG=" + replacement, "PATH=" + os.Getenv("PATH")}, runners: registry,
	}
	target, draft, before, err := a.pickInteractiveConfiguration(root, false, false)
	if err != nil {
		t.Fatalf("pick interactive configuration: %v; output=%q", err, output.Bytes())
	}
	if target != replacement || before == "" || draft.reviewMaxRounds == nil || *draft.reviewMaxRounds != 0 || len(draft.profiles) != 0 {
		t.Fatalf("target=%q draft=%#v before=%q", target, draft, before)
	}
	if fake.authCalls != 0 || fake.loginCalls != 0 || len(fake.listRequests) != 0 {
		t.Fatalf("provider discovery happened: auth=%d login=%d requests=%#v", fake.authCalls, fake.loginCalls, fake.listRequests)
	}
	if err := config.ConfigureSettingsAtomic(target, draft.profiles, draft.reviewMaxRounds, before); err != nil {
		t.Fatal(err)
	}
	if code := a.configureSuccess(target, "updated", false); code != workflow.ExitOK {
		t.Fatalf("success code=%d", code)
	}
	data, err := os.ReadFile(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("title = \"keep\"")) || !bytes.Contains(data, []byte("review_max_rounds = 0")) || !bytes.Contains(output.Bytes(), []byte("updated "+replacement)) {
		t.Fatalf("data=%s output=%q", data, output.Bytes())
	}
}
