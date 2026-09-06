package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basant-kumar/rolemux/internal/catalog"
	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
	"github.com/basant-kumar/rolemux/internal/workflow"
)

type loginAdapter struct {
	authenticated bool
	authCalls     int
	loginCalls    int
	listRequests  []runner.ModelListRequest
	models        []runner.ModelInfo
}

type localHintAdapter struct {
	*loginAdapter
	hint runner.AuthStatus
}

func (f *localHintAdapter) LocalAuthHint() runner.AuthStatus { return f.hint }

func (f *loginAdapter) Run(context.Context, runner.Request, runner.Callbacks) (runner.Response, error) {
	return runner.Response{}, nil
}
func (f *loginAdapter) ListModels(_ context.Context, req runner.ModelListRequest) (runner.ModelPage, error) {
	f.listRequests = append(f.listRequests, req)
	models := f.models
	if models == nil {
		models = []runner.ModelInfo{{ID: "model-1", Label: "Model 1", Availability: "available"}}
	}
	return runner.ModelPage{Models: models}, nil
}
func (f *loginAdapter) Version(context.Context) (string, error) { return "1.0", nil }
func (f *loginAdapter) Auth(context.Context) (runner.AuthStatus, error) {
	f.authCalls++
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

func markCLIPlanApproved(t *testing.T, state *task.State) {
	t.Helper()
	units, err := task.NormalizeWorkUnits(state.WorkUnits, state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(struct {
		Plan       string          `json:"plan"`
		Complexity string          `json:"complexity"`
		WorkGraph  bool            `json:"work_graph"`
		WorkUnits  []task.WorkUnit `json:"work_units"`
	}{Plan: state.Plan, Complexity: state.Complexity, WorkGraph: state.WorkGraph, WorkUnits: units})
	if err != nil {
		t.Fatal(err)
	}
	state.ApprovalGateSchemaVersion = task.ApprovalGateSchemaVersion
	state.Approval = &task.ApprovalRecord{
		GateID: "test-plan-gate", Kind: task.ApprovalKindPlan, Status: task.ApprovalDecisionApprove,
		SubjectFingerprint: task.ScopeSpecHash(string(payload)),
	}
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
	code, output, stderr := runTestApp(t, root, "", "--version", "--json")
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

func TestPlanGraphAndWorkStartCommands(t *testing.T) {
	root := cliRepo(t)
	units, err := task.NormalizeWorkUnits([]task.WorkUnit{{ID: "T1", Objective: "one", Scope: "one.go", ExecutionPacket: "packet", AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test"}}}, "plan")
	if err != nil {
		t.Fatal(err)
	}
	store := task.NewStore(root)
	planHash := task.ScopeSpecHash("plan")
	state := task.State{ID: "graph-cli", RepoRoot: root, Phase: task.PhasePlanApproved, Plan: "plan", PlanHash: planHash, ApprovedPlanHash: planHash, WorkGraph: true, WorkUnits: units}
	markCLIPlanApproved(t, &state)
	if err := store.Create(state); err != nil {
		t.Fatal(err)
	}
	code, output, stderr := runTestApp(t, root, "", "plan", "graph", "graph-cli", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("graph code=%d stderr=%q output=%s", code, stderr, output)
	}
	payload := decodeSingleObject(t, output)
	result := payload["result"].(map[string]any)
	if ready := result["ready"].([]any); len(ready) != 1 || ready[0] != "T1" {
		t.Fatalf("graph payload=%#v", payload)
	}
	code, output, stderr = runTestApp(t, root, "", "work", "start", "graph-cli", "T1", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("work start code=%d stderr=%q output=%s", code, stderr, output)
	}
	payload = decodeSingleObject(t, output)
	child := payload["task"].(map[string]any)
	if child["parent_task_id"] != "graph-cli" || child["work_unit_id"] != "T1" {
		t.Fatalf("work payload=%#v", payload)
	}
}

func TestHelpInvocationsAdvertiseSupportedForms(t *testing.T) {
	root := t.TempDir()
	expectedLines := []string{"  rolemux help", "  rolemux -h", "  rolemux --help"}
	for _, invocation := range []string{"help", "-h", "--help"} {
		t.Run(invocation, func(t *testing.T) {
			code, output, stderr := runTestApp(t, root, "", invocation)
			if code != workflow.ExitOK || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			for _, line := range expectedLines {
				if !strings.Contains(string(output), line+"\n") {
					t.Fatalf("help output missing complete line %q: %s", line, output)
				}
			}
		})
	}
	code, output, stderr := runTestApp(t, root, "", "help")
	if code != workflow.ExitOK || stderr != "" {
		t.Fatalf("help code=%d stderr=%q", code, stderr)
	}
	for _, phrase := range []string{
		"Review safety limit: --review-max-rounds N defaults to 5",
		"0 for unlimited",
		"newly created tasks",
	} {
		if !strings.Contains(string(output), phrase) {
			t.Fatalf("help output missing %q: %s", phrase, output)
		}
	}
}

func TestHelpInvocationsJSON(t *testing.T) {
	root := t.TempDir()
	code, humanOutput, stderr := runTestApp(t, root, "", "help")
	if code != workflow.ExitOK || stderr != "" {
		t.Fatalf("human help code=%d stderr=%q", code, stderr)
	}

	for _, invocation := range []string{"help", "-h", "--help"} {
		t.Run(invocation, func(t *testing.T) {
			code, output, stderr := runTestApp(t, root, "", invocation, "--json")
			if code != workflow.ExitOK || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}

			decoder := json.NewDecoder(bytes.NewReader(output))
			var value map[string]any
			if err := decoder.Decode(&value); err != nil {
				t.Fatalf("decode JSON: %v (%s)", err, output)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("JSON did not contain exactly one object: err=%v output=%s", err, output)
			}
			if value["ok"] != true || value["command"] != "help" {
				t.Fatalf("payload=%#v", value)
			}
			result, ok := value["result"].(map[string]any)
			if !ok {
				t.Fatalf("result=%#v", value["result"])
			}
			usage, ok := result["usage"].(string)
			if !ok || usage != string(humanOutput) {
				t.Fatalf("usage=%q, human=%q", usage, humanOutput)
			}
		})
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

func TestSummarizeUsageTreatsClaudeAndAntigravityCacheAsSeparate(t *testing.T) {
	state := task.State{
		ProfilesSnapshot: map[string]task.ProfileSnapshot{
			"planner": {Provider: "antigravity"}, "implementer": {Provider: "claude"},
		},
		Usage: map[string]task.TokenUsage{
			"planner": {InputTokens: 10, CachedInputTokens: 40}, "implementer": {InputTokens: 20, CachedInputTokens: 50},
		},
	}
	summary := summarizeUsage(state)
	if summary.Roles[0].UncachedInputTokens != 20 || summary.Roles[1].UncachedInputTokens != 10 || summary.Totals.UncachedInputTokens != 30 {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestConfigureProjectDirectAndImport(t *testing.T) {
	root := cliRepo(t)
	fake := &loginAdapter{authenticated: true, models: []runner.ModelInfo{{
		ID: "gpt-5.6-sol", Availability: "available", Efforts: []string{"max"}, EffortOptions: []runner.ModelOption{{ID: "max"}}, SpeedOptions: []runner.ModelOption{{ID: "priority"}},
	}}}
	registry := runner.NewRegistry()
	if err := registry.Register("codex", func(_, _ string) (runner.Adapter, string, error) { return fake, "/bin/codex", nil }); err != nil {
		t.Fatal(err)
	}
	var stdout, errout bytes.Buffer
	a := &app{ctx: context.Background(), in: strings.NewReader(""), out: &stdout, errOut: &errout, version: "vtest", cwd: root, environ: []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}, runners: registry}
	code := a.run([]string{"configure", "--project", "--role", "planner", "--runner", "codex", "--model", "gpt-5.6-sol", "--effort", "max", "--speed", "priority", "--review-max-rounds", "10", "--json"})
	output, stderr := stdout.Bytes(), errout.String()
	if code != 0 || stderr != "" {
		t.Fatalf("configure code=%d output=%s stderr=%s", code, output, stderr)
	}
	path := filepath.Join(root, ".rolemux.toml")
	cfg, err := config.LoadWithEnv(root, []string{"HOME=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Profiles[config.RolePlanner]; got.Model != "gpt-5.6-sol" || got.Effort != "max" || got.Speed != "priority" || cfg.EffectiveReviewMaxRounds() != 10 {
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
	if cfg, err = config.LoadWithEnv(root, []string{"HOME=" + t.TempDir()}); err != nil || cfg.EffectiveReviewMaxRounds() != 10 {
		t.Fatalf("import changed review max rounds: cfg=%#v err=%v", cfg, err)
	}
}

func TestConfigureReviewMaxRoundsStandaloneValidatesAndRetainsZero(t *testing.T) {
	root := cliRepo(t)
	code, output, stderr := runTestApp(t, root, "", "configure", "--project", "--review-max-rounds", "0", "--json")
	if code != workflow.ExitOK || stderr != "" {
		t.Fatalf("limit code=%d output=%s stderr=%s", code, output, stderr)
	}
	value := decodeSingleObject(t, output)
	if value["ok"] != true {
		t.Fatalf("limit payload=%#v", value)
	}
	cfg, err := config.LoadWithEnv(root, []string{"HOME=" + t.TempDir()})
	if err != nil || cfg.ReviewMaxRounds == nil || *cfg.ReviewMaxRounds != 0 {
		t.Fatalf("review max rounds=%#v err=%v", cfg.ReviewMaxRounds, err)
	}

	for _, raw := range []string{"-1", "not-a-number", "999999999999999999999999999999", ""} {
		args := []string{"configure", "--project", "--review-max-rounds"}
		if raw != "" {
			args = append(args, raw)
		}
		args = append(args, "--json")
		code, output, stderr = runTestApp(t, root, "", args...)
		if code != workflow.ExitUsage || stderr != "" {
			t.Fatalf("raw=%q code=%d output=%s stderr=%s", raw, code, output, stderr)
		}
		payload := decodeSingleObject(t, output)
		if payload["ok"] != false || payload["error"].(map[string]any)["code"] != "USAGE" {
			t.Fatalf("raw=%q payload=%#v", raw, payload)
		}
	}
	cfg, err = config.LoadWithEnv(root, []string{"HOME=" + t.TempDir()})
	if err != nil || cfg.EffectiveReviewMaxRounds() != 0 {
		t.Fatalf("invalid setting changed zero: cfg=%#v err=%v", cfg, err)
	}
}

func TestConfigureFromConsumesOptionLikePath(t *testing.T) {
	root := cliRepo(t)
	code, output, stderr := runTestApp(t, root, "", "configure", "--project", "--from", "--review-max-rounds", "--json")
	if code != workflow.ExitUsage || stderr != "" {
		t.Fatalf("from option-like path code=%d output=%s stderr=%s", code, output, stderr)
	}
	payload := decodeSingleObject(t, output)
	if payload["ok"] != false || payload["error"].(map[string]any)["code"] != "CONFIGURATION" {
		t.Fatalf("from option-like path payload=%#v", payload)
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
		ctx: context.Background(), in: bytes.NewBufferString("\r\r\r\r\r\r\r\r\r"), out: &output, errOut: &output,
		cwd: root, environ: []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}, runners: registry,
	}
	target, profiles, _, err := a.pickInteractiveConfiguration(root, true, false)
	if err != nil {
		t.Fatalf("%v; output=%q", err, output.Bytes())
	}
	if target == "" || len(profiles.profiles) != 3 || profiles.profiles[config.RolePlanner].Model != "model-1" || profiles.profiles[config.RoleImplementer].Provider != "test" || profiles.profiles[config.RoleReviewer].Model != "model-1" {
		t.Fatalf("target=%q profiles=%#v", target, profiles)
	}
	if fake.loginCalls != 1 {
		t.Fatalf("login calls=%d", fake.loginCalls)
	}
	if len(fake.listRequests) != 1 || !fake.listRequests[0].Refresh {
		t.Fatalf("model requests=%#v", fake.listRequests)
	}
	if !bytes.Contains(output.Bytes(), []byte("login required for Planner")) || !bytes.Contains(output.Bytes(), []byte("\x1b[?1049l")) || !bytes.Contains(output.Bytes(), []byte("\x1b[?1049h")) {
		t.Fatalf("login transition output=%q", output.Bytes())
	}
}

func TestInteractiveConfigurationShowsRoleBeforeProviderProbes(t *testing.T) {
	root := cliRepo(t)
	fake := &loginAdapter{authenticated: true}
	registry := runner.NewRegistry()
	if err := registry.Register("test", func(_, _ string) (runner.Adapter, string, error) {
		return fake, "/bin/test", nil
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	a := &app{
		ctx: context.Background(), in: bytes.NewBuffer([]byte{0x1b}), out: &output, errOut: &output,
		cwd: root, environ: []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}, runners: registry,
	}
	_, _, _, err := a.pickInteractiveConfiguration(root, true, false)
	if err == nil || !strings.Contains(err.Error(), "configuration cancelled") {
		t.Fatalf("err=%v output=%q", err, output.Bytes())
	}
	if fake.authCalls != 0 || !bytes.Contains(output.Bytes(), []byte("Choose which role to configure")) {
		t.Fatalf("auth calls=%d output=%q", fake.authCalls, output.Bytes())
	}
}

func TestInteractiveConfigurationReviewLimitSkipsProviderDiscovery(t *testing.T) {
	root := cliRepo(t)
	fake := &loginAdapter{authenticated: true}
	registry := runner.NewRegistry()
	if err := registry.Register("test", func(_, _ string) (runner.Adapter, string, error) {
		return fake, "/bin/test", nil
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	input := strings.Repeat("\x1b[B", 6) + "\r" + strings.Repeat("\x1b[B", 2) + "\r"
	a := &app{ctx: context.Background(), in: bytes.NewBufferString(input), out: &output, errOut: &output, cwd: root, environ: []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}, runners: registry}
	_, draft, _, err := a.pickInteractiveConfiguration(root, true, false)
	if err != nil {
		t.Fatalf("err=%v output=%q", err, output.Bytes())
	}
	if len(draft.profiles) != 0 || draft.reviewMaxRounds == nil || *draft.reviewMaxRounds != 0 {
		t.Fatalf("draft=%#v", draft)
	}
	if fake.authCalls != 0 || fake.loginCalls != 0 || len(fake.listRequests) != 0 {
		t.Fatalf("provider discovery happened: auth=%d login=%d requests=%#v", fake.authCalls, fake.loginCalls, fake.listRequests)
	}
	if !bytes.Contains(output.Bytes(), []byte("Review safety limit")) || !bytes.Contains(output.Bytes(), []byte("--review-max-rounds N")) {
		t.Fatalf("limit screen output=%q", output.Bytes())
	}
	options := reviewMaxRoundsOptions(5)
	if len(options) != 3 || !strings.Contains(options[0].Label, "Current") || !strings.Contains(options[0].Label, "Default (5)") || options[1].ID != "10" || options[2].ID != "0" {
		t.Fatalf("deduplicated options=%#v", options)
	}
}

func TestProviderInspectionUsesLocalAuthHintWithoutStartingCLI(t *testing.T) {
	fake := &loginAdapter{}
	adapter := &localHintAdapter{loginAdapter: fake, hint: runner.AuthStatus{Authenticated: true, Account: "credential-fingerprint"}}
	a := &app{ctx: context.Background(), environ: []string{"HOME=" + t.TempDir()}}
	ready := a.inspectProvider("test", config.Default(), adapter, nil)
	if !ready.authenticated || ready.account != "credential-fingerprint" || fake.authCalls != 0 {
		t.Fatalf("readiness=%#v auth calls=%d", ready, fake.authCalls)
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
		ctx: ctx, in: &configureEscapeReader{chunks: [][]byte{{'\r'}, {0x1b}, nil, {0x03}}}, out: &output, errOut: &output,
		cwd: root, environ: []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}, runners: registry,
	}
	_, _, _, err := a.pickInteractiveConfiguration(root, true, false)
	if err == nil || !strings.Contains(err.Error(), "configuration cancelled") {
		t.Fatalf("err=%v output=%q", err, output.Bytes())
	}
	if got := bytes.Count(output.Bytes(), []byte("Role: Planner")); got != 1 {
		t.Fatalf("provider role badge count=%d output=%q", got, output.Bytes())
	}
	if got := bytes.Count(output.Bytes(), []byte("Configure RoleMux · Provider")); got != 1 {
		t.Fatalf("provider step count=%d output=%q", got, output.Bytes())
	}
	if got := bytes.Count(output.Bytes(), []byte("Choose which role to configure")); got != 2 {
		t.Fatalf("role screen count=%d output=%q", got, output.Bytes())
	}
	if fake.authCalls != 0 {
		t.Fatalf("opening the provider screen made %d blocking auth calls", fake.authCalls)
	}
}

func TestInteractiveConfigurationCanUpdateOneRoleOnly(t *testing.T) {
	root := cliRepo(t)
	fake := &loginAdapter{authenticated: true}
	registry := runner.NewRegistry()
	if err := registry.Register("test", func(_, _ string) (runner.Adapter, string, error) { return fake, "/bin/test", nil }); err != nil {
		t.Fatal(err)
	}
	// Down twice selects Implementer, then select provider and model.
	input := "\x1b[B\x1b[B\r\r\r"
	a := &app{ctx: context.Background(), in: bytes.NewBufferString(input), out: io.Discard, errOut: io.Discard, cwd: root, environ: []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}, runners: registry}
	_, profiles, _, err := a.pickInteractiveConfiguration(root, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles.profiles) != 1 || profiles.profiles[config.RoleImplementer].Model != "model-1" {
		t.Fatalf("profiles=%#v", profiles)
	}
}

func TestInteractiveConfigurationUsesCachedModelsAndRefreshesInBackground(t *testing.T) {
	root, home := cliRepo(t), t.TempDir()
	fake := &loginAdapter{authenticated: true}
	registry := runner.NewRegistry()
	if err := registry.Register("test", func(_, _ string) (runner.Adapter, string, error) { return fake, "/bin/test", nil }); err != nil {
		t.Fatal(err)
	}
	environ := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	cachePath := catalog.DefaultCachePath(environ)
	if _, err := catalog.New(map[string]runner.Adapter{"test": fake}, config.Default(), cachePath).Models(context.Background(), "test", true, runner.ModelListRequest{}); err != nil {
		t.Fatal(err)
	}
	fake.listRequests = nil
	refreshed := false
	a := &app{
		ctx: context.Background(), in: bytes.NewBufferString("\x1b[B\x1b[B\r\r\r"), out: io.Discard, errOut: io.Discard,
		cwd: root, environ: environ, runners: registry,
		refreshModels: func(_ config.Config, gotPath, provider string, adapter runner.Adapter, request runner.ModelListRequest) {
			refreshed = gotPath == cachePath && provider == "test" && adapter == fake && request.Refresh
		},
	}
	_, profiles, _, err := a.pickInteractiveConfiguration(root, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if profiles.profiles[config.RoleImplementer].Model != "model-1" || len(fake.listRequests) != 0 {
		t.Fatalf("profiles=%#v blocking model calls=%#v", profiles, fake.listRequests)
	}
	if !refreshed {
		t.Fatal("cached selection did not schedule a background refresh")
	}
}

func TestCompactStatusOmitsPromptsPlansAndManifests(t *testing.T) {
	state := task.State{
		ID: "compact", Phase: task.PhaseCodeReviewing, Task: strings.Repeat("task", 100), Plan: strings.Repeat("plan", 100),
		ScopedBaseline: []task.FileEntry{{Path: "large", Hash: strings.Repeat("a", 1000)}},
		InFlight:       &task.InFlight{Operation: "code_review", Role: "code_reviewer", OwnerPID: 4242, Prompt: strings.Repeat("prompt", 100), KnownSession: true, SessionID: "session"},
		ReviewPolicy:   &task.ReviewPolicy{MaxRounds: 0},
	}
	data, err := json.Marshal(compactStatus(state))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"scoped_baseline", "promptprompt", "planplan", "tasktask"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("compact status leaked %q: %s", forbidden, data)
		}
	}
	if !bytes.Contains(data, []byte(`"owner_pid":4242`)) || !bytes.Contains(data, []byte(`"known_session":true`)) || !bytes.Contains(data, []byte(`"session_id":"session"`)) {
		t.Fatalf("compact status omitted recovery data: %s", data)
	}
	if !bytes.Contains(data, []byte(`"status":"in_flight"`)) || !bytes.Contains(data, []byte(`"max_rounds":0`)) || !bytes.Contains(data, []byte(`"next_action":"wait"`)) {
		t.Fatalf("compact status omitted control: %s", data)
	}
}

func TestWorkflowFailureJSONUsesCompactControlAndLoadsExistingTask(t *testing.T) {
	root := cliRepo(t)
	state := task.State{ID: "needs-answer", RepoRoot: root, Phase: task.PhaseNeedsInput, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 0}, PendingQuestion: "which API?", PendingQuestionSource: "planner"}
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}
	var output, errout bytes.Buffer
	a := &app{ctx: context.Background(), out: &output, errOut: &errout, cwd: root}
	err := &workflow.Error{Code: "NEEDS_INPUT", Message: state.PendingQuestion, TaskID: state.ID, Retryable: true, ExitCode: workflow.ExitNeedsInput}
	code := a.fail("plan-answer", err, true, workflow.Result{})
	if code != workflow.ExitNeedsInput || errout.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errout.String())
	}
	payload := decodeSingleObject(t, output.Bytes())
	result := payload["result"].(map[string]any)
	if result["status"] != "needs_input" || result["next_action"] != "plan_answer" || result["max_rounds"] != float64(0) || result["question"] != state.PendingQuestion || result["source"] != state.PendingQuestionSource {
		t.Fatalf("result=%#v", result)
	}
	taskPayload := payload["task"].(map[string]any)
	if taskPayload["max_rounds"] != float64(0) {
		t.Fatalf("task summary=%#v", taskPayload)
	}
}

func TestTaskCommandPreflightFailureLoadsExistingTaskControl(t *testing.T) {
	root := cliRepo(t)
	state := task.State{
		ID: "preflight-task", RepoRoot: root, Phase: task.PhaseImplementationReady,
		Scope: "app.go", ReviewPolicy: &task.ReviewPolicy{MaxRounds: 0},
	}
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rolemux.toml"), []byte("review_max_rounds = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := runTestApp(t, root, "", "code", "review", state.ID, "--json")
	if code != workflow.ExitUsage || stderr != "" {
		t.Fatalf("code=%d output=%s stderr=%q", code, output, stderr)
	}
	payload := decodeSingleObject(t, output)
	errPayload := payload["error"].(map[string]any)
	if errPayload["code"] != "CONFIGURATION" || errPayload["task_id"] != state.ID {
		t.Fatalf("error=%#v", errPayload)
	}
	taskPayload := payload["task"].(map[string]any)
	if taskPayload["id"] != state.ID || taskPayload["phase"] != task.PhaseImplementationReady || taskPayload["max_rounds"] != float64(0) {
		t.Fatalf("task=%#v", taskPayload)
	}
	control := payload["result"].(map[string]any)
	if control["status"] != "failed" || control["max_rounds"] != float64(0) || control["next_action"] != "code_review" || control["can_review"] != true {
		t.Fatalf("control=%#v", control)
	}
}

func TestIntegrationPreflightFailureUsesDerivedTaskControl(t *testing.T) {
	root := cliRepo(t)
	parentID := "integration-preflight-parent"
	integrationID := task.IntegrationTaskID(parentID)
	state := task.State{
		ID: integrationID, RepoRoot: root, Phase: task.PhaseNeedsInput,
		IntegrationReview: true, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 0},
		PendingQuestion: "which integration contract?", PendingQuestionSource: "implementer",
		ReviewProgress: &task.ReviewProgress{Kind: "integration", Status: "needs_input"},
	}
	if err := task.NewStore(root).Create(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rolemux.toml"), []byte("review_max_rounds = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := runTestApp(t, root, "", "work", "integrate", parentID, "--json")
	if code != workflow.ExitUsage || stderr != "" {
		t.Fatalf("code=%d output=%s stderr=%q", code, output, stderr)
	}
	payload := decodeSingleObject(t, output)
	errPayload := payload["error"].(map[string]any)
	if errPayload["code"] != "CONFIGURATION" || errPayload["task_id"] != integrationID {
		t.Fatalf("error=%#v", errPayload)
	}
	taskPayload := payload["task"].(map[string]any)
	if taskPayload["id"] != integrationID || taskPayload["phase"] != task.PhaseNeedsInput || taskPayload["max_rounds"] != float64(0) {
		t.Fatalf("task=%#v", taskPayload)
	}
	control := payload["result"].(map[string]any)
	if control["status"] != "failed" || control["review_kind"] != "integration" || control["next_action"] != "implement_answer" || control["question"] != state.PendingQuestion || control["source"] != state.PendingQuestionSource {
		t.Fatalf("control=%#v", control)
	}
}

func TestWorkflowFailureOverridesStaleSuccessStatus(t *testing.T) {
	var output bytes.Buffer
	a := &app{ctx: context.Background(), out: &output, errOut: io.Discard, cwd: t.TempDir()}
	state := task.State{ID: "stale", Phase: task.PhaseApproved, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 5}}
	err := &workflow.Error{Code: "INVALID_TASK_STATE", Message: "command is not valid", TaskID: state.ID, ExitCode: workflow.ExitUsage}
	if code := a.fail("code-review", err, true, workflow.Result{State: state, Status: "approved"}); code != workflow.ExitUsage {
		t.Fatalf("code=%d", code)
	}
	result := decodeSingleObject(t, output.Bytes())["result"].(map[string]any)
	if result["status"] != "failed" {
		t.Fatalf("stale result status=%#v", result)
	}
}

func TestConfigureDirectSettingsClassifyMissingAndInvalidRolesAsUsage(t *testing.T) {
	root := cliRepo(t)
	cases := []struct {
		name string
		args []string
		code string
	}{
		{name: "missing role", args: []string{"--runner", "codex", "--model", "model-1"}, code: "USAGE"},
		{name: "invalid role", args: []string{"--role", "unknown", "--runner", "codex", "--model", "model-1"}, code: "CONFIGURATION"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"configure", "--project"}, test.args...)
			args = append(args, "--json")
			code, output, stderr := runTestApp(t, root, "", args...)
			if code != workflow.ExitUsage || stderr != "" {
				t.Fatalf("code=%d output=%s stderr=%s", code, output, stderr)
			}
			payload := decodeSingleObject(t, output)
			if payload["error"].(map[string]any)["code"] != test.code {
				t.Fatalf("payload=%#v", payload)
			}
		})
	}
}

func TestHumanStatusDisplaysUnlimitedAndNextAction(t *testing.T) {
	var output bytes.Buffer
	printState(&output, task.State{ID: "human", Phase: task.PhaseImplementationReady, ReviewPolicy: &task.ReviewPolicy{MaxRounds: 0}})
	text := output.String()
	if !strings.Contains(text, "plan rounds: 0/unlimited") || !strings.Contains(text, "code rounds: 0/unlimited") || !strings.Contains(text, "next action: code_review") {
		t.Fatalf("status=%q", text)
	}
	if strings.Contains(text, "prompt") || strings.Contains(text, "manifest") {
		t.Fatalf("human status leaked internal details: %q", text)
	}
}

func TestWizardStartsFromConfiguredDynamicModelSettings(t *testing.T) {
	cfg := config.Default()
	cfg.Profiles[config.RolePlanner] = config.Profile{Provider: "claude", Model: "claude-current", Effort: "max", Speed: "fast"}
	drafts := map[string]*profileDraft{config.RolePlanner: {
		provider: "claude",
		models:   []runner.ModelInfo{{ID: "current", Aliases: []string{"claude-current"}, IsDefault: true}},
		model:    runner.ModelInfo{ID: "current", Aliases: []string{"claude-current"}},
	}}
	for kind, want := range map[wizardScreenKind]string{wizardProvider: "claude", wizardModel: "current", wizardEffort: "max", wizardSpeed: "fast"} {
		if got := wizardInitialID(wizardScreen{kind: kind, role: config.RolePlanner}, cfg, drafts); got != want {
			t.Fatalf("kind=%v got=%q want=%q", kind, got, want)
		}
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
	opts, err = parse([]string{"task-1", "--answer", "--no", "--json"}, map[string]bool{"--answer": true, "--json": false})
	if err != nil || opts.value("--answer") != "--no" || !opts.json() {
		t.Fatalf("option-like value was rejected: opts=%#v err=%v", opts, err)
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
