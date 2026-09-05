package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
	"github.com/basant-kumar/rolemux/internal/workflow"
)

type loginAdapter struct {
	authenticated bool
	loginCalls    int
	listRequests  []runner.ModelListRequest
}

func (f *loginAdapter) Run(context.Context, runner.Request, runner.Callbacks) (runner.Response, error) {
	return runner.Response{}, nil
}
func (f *loginAdapter) ListModels(_ context.Context, req runner.ModelListRequest) (runner.ModelPage, error) {
	f.listRequests = append(f.listRequests, req)
	return runner.ModelPage{Models: []runner.ModelInfo{{ID: "model-1", Label: "Model 1", Availability: "available"}}}, nil
}
func (f *loginAdapter) Version(context.Context) (string, error) { return "1.0", nil }
func (f *loginAdapter) Auth(context.Context) (runner.AuthStatus, error) {
	return runner.AuthStatus{Authenticated: f.authenticated, Message: "not logged in"}, nil
}
func (f *loginAdapter) Login(context.Context, runner.LoginRequest) error {
	f.loginCalls++
	f.authenticated = true
	return nil
}

func cliRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "-C", root, "init", "-q")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, output)
	}
	return root
}

func runTestApp(t *testing.T, root string, input string, args ...string) (int, []byte, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := &app{ctx: context.Background(), in: bytes.NewBufferString(input), out: &stdout, errOut: &stderr, version: "vtest", cwd: root, environ: []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}}
	code := a.run(args)
	return code, stdout.Bytes(), stderr.String()
}

func decodeSingleObject(t *testing.T, data []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON: %v (%s)", err, data)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		t.Fatalf("JSON contained a second value: %s", data)
	}
	return value
}

func TestVersionAndUsageEmitExactlyOneJSONObject(t *testing.T) {
	root := cliRepo(t)
	code, output, stderr := runTestApp(t, root, "", "version", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	value := decodeSingleObject(t, output)
	if value["ok"] != true || value["command"] != "version" {
		t.Fatalf("payload=%#v", value)
	}
	code, output, _ = runTestApp(t, root, "", "unknown", "--json")
	if code != workflow.ExitUsage {
		t.Fatalf("unknown exit=%d", code)
	}
	value = decodeSingleObject(t, output)
	if value["ok"] != false || value["error"].(map[string]any)["code"] != "USAGE" {
		t.Fatalf("payload=%#v", value)
	}
}

func TestSummarizeUsageIsCompactSortedAndTotalsUncachedInput(t *testing.T) {
	state := task.State{
		ID: "task-1", Phase: task.PhaseApproved,
		ProfilesSnapshot: map[string]task.ProfileSnapshot{
			"planner":     {Provider: "codex", Model: "plan", Effort: "max"},
			"implementer": {Provider: "codex", Model: "code", Effort: "high"},
		},
		Usage: map[string]task.TokenUsage{
			"planner":     {Requests: 1, InputTokens: 100, CachedInputTokens: 80, OutputTokens: 10, TotalTokens: 110},
			"implementer": {Requests: 2, InputTokens: 200, CachedInputTokens: 50, OutputTokens: 20, TotalTokens: 220},
		},
	}
	summary := summarizeUsage(state)
	if len(summary.Roles) != 2 || summary.Roles[0].Role != "implementer" || summary.Roles[1].Role != "planner" {
		t.Fatalf("roles=%#v", summary.Roles)
	}
	if summary.Roles[0].UncachedInputTokens != 150 || summary.Totals.UncachedInputTokens != 170 {
		t.Fatalf("uncached role=%d total=%d", summary.Roles[0].UncachedInputTokens, summary.Totals.UncachedInputTokens)
	}
	if summary.Totals.Requests != 3 || summary.Totals.TotalTokens != 330 {
		t.Fatalf("totals=%#v", summary.Totals)
	}
}

func TestConfigureProjectDirectAndImport(t *testing.T) {
	root := cliRepo(t)
	code, output, stderr := runTestApp(t, root, "", "configure", "--project", "--role", "planner", "--runner", "codex", "--model", "gpt-5.6-sol", "--effort", "max", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("configure code=%d output=%s stderr=%s", code, output, stderr)
	}
	path := filepath.Join(root, ".rolemux.toml")
	cfg, err := config.LoadWithEnv(root, []string{"HOME=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Profiles[config.RolePlanner]; got.Model != "gpt-5.6-sol" || got.Effort != "max" {
		t.Fatalf("profile=%#v", got)
	}
	fragment := "title='preserved'\n[profiles.implementer]\nprovider='codex'\nmodel='gpt-5.6-luna'\neffort='max'\n"
	code, output, stderr = runTestApp(t, root, fragment, "configure", "--project", "--from", "-", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("import code=%d output=%s stderr=%s", code, output, stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("gpt-5.6-luna")) {
		t.Fatalf("config=%s err=%v", data, err)
	}
}

func TestInteractiveConfigurationLogsInThenUsesFreshModelsAcrossScreens(t *testing.T) {
	root := cliRepo(t)
	fake := &loginAdapter{}
	registry := runner.NewRegistry()
	if err := registry.Register("test", func(_, _ string) (runner.Adapter, string, error) {
		return fake, "/bin/test", nil
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	a := &app{
		ctx: context.Background(), in: bytes.NewBufferString("\r\r\r\r\r\r\r\r"), out: &output, errOut: &output,
		cwd: root, environ: []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}, runners: registry,
	}
	target, profiles, _, err := a.pickInteractiveConfiguration(root, true, false)
	if err != nil {
		t.Fatalf("%v; output=%q", err, output.Bytes())
	}
	if target == "" || len(profiles) != 3 || profiles[config.RolePlanner].Model != "model-1" || profiles[config.RoleImplementer].Provider != "test" || profiles[config.RoleReviewer].Model != "model-1" {
		t.Fatalf("target=%q profiles=%#v", target, profiles)
	}
	if fake.loginCalls != 1 {
		t.Fatalf("login calls=%d", fake.loginCalls)
	}
	if len(fake.listRequests) != 1 || !fake.listRequests[0].Refresh {
		t.Fatalf("model requests=%#v", fake.listRequests)
	}
	if !bytes.Contains(output.Bytes(), []byte("login required")) || !bytes.Contains(output.Bytes(), []byte("\x1b[?1049l")) || !bytes.Contains(output.Bytes(), []byte("\x1b[?1049h")) {
		t.Fatalf("login transition output=%q", output.Bytes())
	}
}

func TestInteractiveConfigurationEscapeReturnsToPreviousScreen(t *testing.T) {
	root := cliRepo(t)
	fake := &loginAdapter{authenticated: true}
	registry := runner.NewRegistry()
	if err := registry.Register("test", func(_, _ string) (runner.Adapter, string, error) {
		return fake, "/bin/test", nil
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a := &app{
		ctx: ctx, in: bytes.NewBuffer([]byte{'\r', 0x1b, 0x03}), out: &output, errOut: &output,
		cwd: root, environ: []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}, runners: registry,
	}
	_, _, _, err := a.pickInteractiveConfiguration(root, true, false)
	if err == nil || !strings.Contains(err.Error(), "configuration cancelled") {
		t.Fatalf("err=%v output=%q", err, output.Bytes())
	}
	if got := bytes.Count(output.Bytes(), []byte("Planner · Select provider")); got != 2 {
		t.Fatalf("provider screen count=%d output=%q", got, output.Bytes())
	}
	if got := bytes.Count(output.Bytes(), []byte("Planner · Select test model")); got != 1 {
		t.Fatalf("model screen count=%d output=%q", got, output.Bytes())
	}
}

func TestProviderReadinessExplainsMissingCLIAndCredentials(t *testing.T) {
	a := &app{ctx: context.Background(), environ: []string{"PRESENT=value"}}
	missing := a.inspectProvider("copilot", config.Config{}, nil, errors.New("not found"))
	if missing.status != "not installed" || !strings.Contains(missing.message, "brew install copilot-cli") {
		t.Fatalf("missing=%#v", missing)
	}
	cfg := config.Config{Providers: map[string]config.Provider{"test": {APIKeyEnv: "MISSING"}}}
	credentials := a.inspectProvider("test", cfg, &loginAdapter{}, nil)
	if credentials.status != "credentials required" || credentials.authenticated || !credentials.externalAuth {
		t.Fatalf("credentials=%#v", credentials)
	}
	options := providerWizardOptions([]string{"copilot"}, map[string]providerReadiness{"copilot": missing})
	if len(options) != 1 || !strings.Contains(options[0].Label, "not installed") {
		t.Fatalf("options=%#v", options)
	}
}

func TestInterspersedFlagsAndExitClassification(t *testing.T) {
	opts, err := parse([]string{"task-1", "--answer", "yes", "--json"}, map[string]bool{"--answer": true, "--json": false})
	if err != nil || len(opts.positionals) != 1 || opts.value("--answer") != "yes" || !opts.json() {
		t.Fatalf("opts=%#v err=%v", opts, err)
	}
	cases := []struct {
		err  error
		exit int
		code string
	}{
		{&workflow.Error{Code: "NEEDS_INPUT", Message: "question", ExitCode: workflow.ExitNeedsInput, Retryable: true}, 3, "NEEDS_INPUT"},
		{&workflow.Error{Code: "REVIEW_NEEDED", Message: "changed", ExitCode: workflow.ExitReviewNeeded, Retryable: true}, 4, "REVIEW_NEEDED"},
		{&workflow.Error{Code: "STALE_OPERATION", Message: "stale", ExitCode: workflow.ExitAction}, 5, "STALE_OPERATION"},
		{&workflow.Error{Code: "OPERATION_IN_FLIGHT", Message: "busy", ExitCode: workflow.ExitInFlight}, 6, "OPERATION_IN_FLIGHT"},
		{&workflow.Error{Code: "REVIEW_EXHAUSTED", Message: "done", ExitCode: workflow.ExitExhausted}, 7, "REVIEW_EXHAUSTED"},
	}
	for _, test := range cases {
		code, _, _, _, exit := classifyError(test.err)
		if code != test.code || exit != test.exit {
			t.Errorf("classify %s: code=%s exit=%d", test.code, code, exit)
		}
	}
}
