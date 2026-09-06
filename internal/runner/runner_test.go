package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/basant-kumar/rolemux/internal/task"
	copilotsdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestCodexArgumentPlacementFreshAndResume(t *testing.T) {
	req := Request{RepoRoot: "/repo", Sandbox: "workspace-write", Model: "gpt-5.6-luna", Effort: "max", Speed: "priority"}
	got, err := BuildCodexArgs(req, "/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-C", "/repo", "-s", "workspace-write", "-a", "never", "--search", "exec", "--ignore-user-config", "--ignore-rules", "--json", "--model", "gpt-5.6-luna", "--output-schema", "/schema.json", "--config", "model_reasoning_effort=max", "--config", "service_tier=priority", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh args\n got: %#v\nwant: %#v", got, want)
	}
	req.Resume, req.SessionID = true, "thread-123"
	got, err = BuildCodexArgs(req, "/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"-C", "/repo", "-s", "workspace-write", "-a", "never", "--search", "exec", "resume", "--ignore-user-config", "--ignore-rules", "--json", "--model", "gpt-5.6-luna", "--output-schema", "/schema.json", "--config", "model_reasoning_effort=max", "--config", "service_tier=priority", "thread-123", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resume args\n got: %#v\nwant: %#v", got, want)
	}
}

func TestCodexRoutingIsDeterministicAndRejectsSecrets(t *testing.T) {
	runtime := task.RuntimeSnapshot{
		ProviderID: "gateway",
		Endpoint:   "https://gateway.example.invalid/v1",
		WireAPI:    "responses",
		SDKSettings: map[string]any{
			"env_key":             "OPENAI_API_KEY",
			"request_max_retries": 3,
			"query_params":        map[string]string{"region": "west", "team": "tools"},
		},
	}
	first, err := CodexConfigOverrides(runtime)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CodexConfigOverrides(runtime)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("nondeterministic overrides: %#v %#v %v", first, second, err)
	}
	joined := strings.Join(first, " ")
	for _, want := range []string{"model_provider=\"gateway\"", "model_providers.gateway.base_url=\"https://gateway.example.invalid/v1\"", "model_providers.gateway.env_key=\"OPENAI_API_KEY\""} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	runtime.SDKSettings["query_params"] = map[string]string{"token": "sk-secret"}
	if _, err := CodexConfigOverrides(runtime); err == nil {
		t.Fatal("literal credential was accepted")
	}
}

func TestCodexOutputRequiresThreadAndStrictEnvelope(t *testing.T) {
	line1 := `{"type":"thread.started","thread_id":"abc"}`
	line2 := `{"type":"item.completed","item":{"type":"agent_message","text":"{\"role\":\"implementer\",\"status\":\"ready\"}"}}`
	called := ""
	session, text, _, _, err := parseCodexOutput([]byte(line1+"\n"+line2+"\n"), RoleImplementer, Callbacks{SessionStarted: func(id string) error { called = id; return nil }})
	if err != nil || session != "abc" || called != "abc" {
		t.Fatalf("session=%q callback=%q text=%q err=%v", session, called, text, err)
	}
	if _, err := DecodeEnvelope([]byte(text), RoleImplementer); err != nil {
		t.Fatal(err)
	}
}

func TestRunProcessStreamsSessionLineBeforeChildExit(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	seen := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, err := RunProcess(context.Background(), ProcessSpec{
			Path: "/bin/sh", Args: []string{"-c", `printf '{"type":"thread.started","thread_id":"live"}\n'; sleep 1`},
			StdoutLine: func(line []byte) error { seen <- string(line); return nil },
		})
		done <- err
	}()
	select {
	case line := <-seen:
		if !strings.Contains(line, `"thread_id":"live"`) {
			t.Fatalf("line=%q", line)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stdout line was buffered until process exit")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAntigravityArgumentsInputAndOutput(t *testing.T) {
	req := Request{Role: RoleImplementer, RepoRoot: "/repo", Model: "gemini-3.8-flash-low", Effort: "low", SessionID: "conversation-1", Resume: true, Prompt: "private prompt"}
	args, err := BuildAntigravityArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--input-format stream-json", "--output-format stream-json", "--model gemini-3.8-flash-low", "--effort low", "--mode accept-edits", "--sandbox", "--conversation conversation-1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, req.Prompt) || strings.Contains(joined, " -p ") {
		t.Fatalf("prompt leaked into argv: %s", joined)
	}
	input := antigravityInput(req.Prompt)
	if !strings.Contains(input, `"event":"user"`) || !strings.Contains(input, req.Prompt) {
		t.Fatalf("input=%s", input)
	}
	data := strings.Join([]string{
		`{"event":"init","conversation_id":"conversation-1","init":{"model":"gemini-3.8-flash-low","permission_mode":"request-review"}}`,
		`{"event":"result","result":{"conversation_id":"conversation-1","status":"SUCCESS","response":"{\"role\":\"implementer\",\"status\":\"ready\"}","structured_output":{"role":"implementer","status":"ready"},"usage":{"input_tokens":20,"output_tokens":4,"thinking_tokens":2,"cache_read_tokens":10,"total_tokens":24}}}`,
	}, "\n")
	response, err := parseAntigravityOutput([]byte(data), RoleImplementer, Callbacks{})
	if err != nil || response.SessionID != "conversation-1" || response.ReportedModel != req.Model || !response.UsageCumulative || response.Usage.ReasoningTokens != 2 || response.Envelope == nil {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestAntigravityAuthUsesModelCatalogWithoutGeneratingATurn(t *testing.T) {
	home := t.TempDir()
	credentialDir := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, "jetski-standalone-oauth-token"), []byte("metadata-only-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []ProcessSpec
	adapter := &Antigravity{Path: "/bin/agy", Env: []string{"HOME=" + home}, Process: func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
		calls = append(calls, spec)
		if reflect.DeepEqual(spec.Args, []string{"--version"}) {
			return ProcessResult{Stdout: []byte("1.1.27\n")}, nil
		}
		if reflect.DeepEqual(spec.Args, []string{"models"}) {
			return ProcessResult{Stdout: []byte("gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n")}, nil
		}
		return ProcessResult{}, errors.New("unexpected Antigravity auth command")
	}}
	hint := adapter.LocalAuthHint()
	if !hint.Authenticated || !strings.HasPrefix(hint.Account, "credential-metadata:") || len(calls) != 0 {
		t.Fatalf("local hint=%#v calls=%#v", hint, calls)
	}

	status, err := adapter.Auth(context.Background())
	if err != nil || !status.Authenticated || status.Version != "1.1.27" || !strings.HasPrefix(status.Account, "credential-metadata:") {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if len(calls) != 2 || !reflect.DeepEqual(calls[1].Args, []string{"models"}) {
		t.Fatalf("auth calls=%#v", calls)
	}
}

func TestAntigravityAcceptsOnlyStrictEnvelopeFromFencedFallback(t *testing.T) {
	payload := map[string]any{"response": "Here is the result:\n```json\n{\"role\":\"Planner\",\"status\":\"ready\",\"plan\":\"one step\",\"question\":\"\"}\n```\n"}
	text := antigravityEnvelopeText(payload, RolePlanner)
	if text == "" || strings.Contains(text, "```") {
		t.Fatalf("fallback=%q", text)
	}
	if _, err := DecodeEnvelope([]byte(text), RolePlanner); err != nil {
		t.Fatal(err)
	}
	payload["response"] = "```json\n{\"role\":\"code_reviewer\",\"verdict\":\"approved\",\"findings\":[]}\n```"
	if text := antigravityEnvelopeText(payload, RolePlanner); !strings.Contains(text, "```") {
		t.Fatalf("semantic mismatch was normalized: %q", text)
	}
}

func TestAntigravityRejectsUnsupportedSettings(t *testing.T) {
	base := Request{Role: RolePlanner, RepoRoot: "/repo", Model: "model"}
	for _, mutate := range []func(*Request){
		func(req *Request) { req.Effort = "max" },
		func(req *Request) { req.Speed = "fast" },
		func(req *Request) { req.Resume, req.SessionID = true, "" },
	} {
		req := base
		mutate(&req)
		if _, err := BuildAntigravityArgs(req); err == nil {
			t.Fatalf("accepted unsupported request %#v", req)
		}
	}
}

func TestAntigravityModelDiscoveryKeepsEffortBoundToVariant(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	adapter := &Antigravity{Path: executable, Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
		return ProcessResult{Stdout: []byte("gemini-3.8-flash-high   Gemini 3.8 Flash (High)\nclaude-sonnet-4-6   Claude Sonnet 4.6 (Thinking)\n")}, nil
	}}
	page, err := adapter.ListModels(context.Background(), ModelListRequest{})
	if err != nil || len(page.Models) != 2 {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	if got := page.Models[0]; got.ID != "gemini-3.8-flash-high" || len(got.Efforts) != 1 || got.Efforts[0] != "high" || got.DefaultEffort != "high" {
		t.Fatalf("gemini=%#v", got)
	}
	if got := page.Models[1]; got.ID != "claude-sonnet-4-6" || len(got.Efforts) != 0 || got.DefaultEffort != "" {
		t.Fatalf("claude=%#v", got)
	}
}

func TestSelectionCapabilityMatrix(t *testing.T) {
	model := ModelInfo{
		ID: "capable", Availability: "available",
		Efforts: []string{"low", "high"}, EffortOptions: []ModelOption{{ID: "low"}, {ID: "high"}},
		SpeedOptions: []ModelOption{{ID: "priority"}},
	}
	adapter := &selectionTestAdapter{}
	tests := []struct {
		name, model, effort, speed string
		role                       Role
		wantErr                    bool
	}{
		{"defaults", "capable", "", "", RolePlanner, false},
		{"standard", "capable", "high", "standard", RolePlanner, false},
		{"advertised speed", "capable", "low", "priority", RoleCodeReviewer, false},
		{"unknown model", "missing", "", "", RolePlanner, true},
		{"unsupported effort", "capable", "max", "", RolePlanner, true},
		{"unsupported speed", "capable", "", "fast", RolePlanner, true},
		{"unavailable", "gone", "", "", RolePlanner, true},
	}
	models := []ModelInfo{model, {ID: "gone", Availability: "unavailable"}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateSelection(test.role, test.model, test.effort, test.speed, models, adapter)
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v wantErr=%t", err, test.wantErr)
			}
		})
	}
	if err := ValidateSelection(RoleImplementer, "capable", "", "", models, &Copilot{}); err != nil {
		t.Fatalf("Copilot implementer selection was rejected: %v", err)
	}
}

type selectionTestAdapter struct{}

func (*selectionTestAdapter) Run(context.Context, Request, Callbacks) (Response, error) {
	return Response{}, nil
}
func (*selectionTestAdapter) ListModels(context.Context, ModelListRequest) (ModelPage, error) {
	return ModelPage{}, nil
}
func (*selectionTestAdapter) Version(context.Context) (string, error) { return "test", nil }
func (*selectionTestAdapter) Auth(context.Context) (AuthStatus, error) {
	return AuthStatus{Authenticated: true}, nil
}

func TestClaudeArgumentsAndNestedResultAreStrict(t *testing.T) {
	req := Request{Role: RoleImplementer, RepoRoot: "/repo", Model: "claude-fable-5", Effort: "max", Speed: "fast", SessionID: "123e4567-e89b-12d3-a456-426614174000"}
	args, err := BuildClaudeArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, required := range []string{"--restricted", "--permission-mode dontAsk", "--permission-prompts none", "Read,Glob,Grep,WebSearch,WebFetch,Skill,Edit,Write", "--strict-mcp-config", `--settings {"fastMode":true}`, "--session-id " + req.SessionID, "--model claude-fable-5", "--effort max"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q in %s", required, joined)
		}
	}
	if strings.Contains(joined, "--safe-mode") || strings.Contains(joined, "Bash") || strings.Contains(joined, "dangerously-skip") {
		t.Fatalf("unsafe Claude arguments: %s", joined)
	}
	envelope := map[string]any{"role": "implementer", "status": "ready"}
	nested, _ := json.Marshal(envelope)
	outer, _ := json.Marshal(map[string]any{"session_id": req.SessionID, "structured_output": json.RawMessage(nested)})
	session, got, _, _, err := parseClaudeResult(outer, req.SessionID, RoleImplementer)
	if err != nil || session != req.SessionID || string(got) != string(nested) {
		t.Fatalf("session=%q nested=%s err=%v", session, got, err)
	}
	outer = append(outer, []byte(` {}`)...)
	if _, _, _, _, err := parseClaudeResult(outer, req.SessionID, RoleImplementer); err == nil {
		t.Fatal("multiple outer JSON values were accepted")
	}
}

func TestClaudeUsesPrivatePXPipeOnlyForTaskRun(t *testing.T) {
	session := "123e4567-e89b-12d3-a456-426614174000"
	launcher := &captureCodexLauncher{}
	claude := &Claude{Path: "/bin/claude", Env: []string{"PATH=/bin"}, PXPipePath: "/bin/pxpipe", TaskLauncher: launcher}
	var specs []ProcessSpec
	claude.Process = func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
		specs = append(specs, spec)
		return ProcessResult{Stdout: []byte("version")}, nil
	}
	launcher.fn = func(spec PXPipeLaunchSpec) (ProcessResult, error) {
		outer, _ := json.Marshal(map[string]any{"session_id": session, "structured_output": map[string]any{"role": "planner", "status": "ready", "plan": "ok"}})
		return ProcessResult{Stdout: outer, ProcessStarted: true}, nil
	}
	if _, err := claude.Version(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := claude.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "claude-fable-5", SessionID: session}, Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Path != "/bin/claude" {
		t.Fatalf("process specs = %#v", specs)
	}
	calls := launcher.Calls()
	if len(calls) != 1 || calls[0].Provider.Path != "/bin/claude" || calls[0].RoutePrefix != pxpipeClaudeRoutePattern || !calls[0].TaskStartsOnLaunch || environmentValue(calls[0].ServerEnv, "ANTHROPIC_UPSTREAM") != claudeAPIBaseURL {
		t.Fatalf("launch specs = %#v", calls)
	}
}

func TestDetectPXPipePathUsesExplicitOrPATHExecutable(t *testing.T) {
	bin := t.TempDir()
	pxpipe := filepath.Join(bin, "pxpipe")
	if err := os.WriteFile(pxpipe, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DetectPXPipePath([]string{"PATH=" + bin}); got != pxpipe {
		t.Fatalf("PATH discovery=%q", got)
	}
	if got := DetectPXPipePath([]string{"PATH=/missing", "PXPIPE_CLI_PATH=" + pxpipe}); got != pxpipe {
		t.Fatalf("explicit discovery=%q", got)
	}
	if got := DetectPXPipePath([]string{"PATH=" + t.TempDir()}); got != "" {
		t.Fatalf("missing pxpipe=%q", got)
	}
}

func TestClaudePXPipeFallbackAndCustomRoute(t *testing.T) {
	session := "123e4567-e89b-12d3-a456-426614174001"
	outer, _ := json.Marshal(map[string]any{"session_id": session, "structured_output": map[string]any{"role": "planner", "status": "ready", "plan": "ok"}})
	for _, tc := range []struct {
		name       string
		runtime    task.RuntimeSnapshot
		launchErr  error
		wantLaunch int
		wantDirect int
	}{
		{name: "helper failure before task", launchErr: &PXPipeLaunchError{BeforeTask: true, Cause: errors.New("setup")}, wantLaunch: 1, wantDirect: 1},
		{name: "custom route", runtime: task.RuntimeSnapshot{ProviderType: "claude", Endpoint: "https://gateway.example.invalid"}, wantDirect: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directCalls := 0
			launcher := &captureCodexLauncher{fn: func(PXPipeLaunchSpec) (ProcessResult, error) {
				return ProcessResult{}, tc.launchErr
			}}
			claude := &Claude{Path: "/bin/claude", Env: []string{"PATH=/bin"}, PXPipePath: "/bin/pxpipe", TaskLauncher: launcher}
			claude.Process = func(context.Context, ProcessSpec) (ProcessResult, error) {
				directCalls++
				return ProcessResult{Stdout: outer, ProcessStarted: true}, nil
			}
			if _, err := claude.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "claude-fable-5", SessionID: session, Runtime: tc.runtime}, Callbacks{}); err != nil {
				t.Fatal(err)
			}
			if len(launcher.Calls()) != tc.wantLaunch || directCalls != tc.wantDirect {
				t.Fatalf("launch=%d direct=%d", len(launcher.Calls()), directCalls)
			}
		})
	}
}

func TestClaudeRunsDirectlyWhenPXPipeIsNotInstalled(t *testing.T) {
	session := "123e4567-e89b-12d3-a456-426614174003"
	outer, _ := json.Marshal(map[string]any{"session_id": session, "structured_output": map[string]any{"role": "planner", "status": "ready", "plan": "ok"}})
	directCalls := 0
	claude := &Claude{Path: "/bin/claude", Env: []string{"PATH=/missing"}, Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
		directCalls++
		return ProcessResult{Stdout: outer, ProcessStarted: true}, nil
	}}
	var diagnostics []string
	if _, err := claude.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "default", SessionID: session}, Callbacks{Diagnostic: func(message string) { diagnostics = append(diagnostics, message) }}); err != nil {
		t.Fatal(err)
	}
	if _, err := claude.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "default", SessionID: session, Resume: true}, Callbacks{Diagnostic: func(message string) { diagnostics = append(diagnostics, message) }}); err != nil {
		t.Fatal(err)
	}
	if directCalls != 2 || len(diagnostics) != 1 || !strings.Contains(diagnostics[0], pxpipeInstallCommand) || !strings.Contains(diagnostics[0], "running Claude directly") {
		t.Fatalf("direct=%d diagnostics=%v", directCalls, diagnostics)
	}
}

func TestClaudePXPipeFailureAfterProcessStartIsNotReplayed(t *testing.T) {
	directCalls := 0
	launcher := &captureCodexLauncher{fn: func(PXPipeLaunchSpec) (ProcessResult, error) {
		return ProcessResult{ProcessStarted: true}, &PXPipeLaunchError{BeforeTask: false, Cause: errors.New("pxpipe upstream status 429: lost proxy")}
	}}
	claude := &Claude{Path: "/bin/claude", Env: []string{"PATH=/bin"}, PXPipePath: "/bin/pxpipe", TaskLauncher: launcher, Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
		directCalls++
		return ProcessResult{}, nil
	}}
	_, err := claude.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "claude-fable-5", SessionID: "123e4567-e89b-12d3-a456-426614174002"}, Callbacks{})
	var providerErr *ProviderError
	if err == nil || !errors.As(err, &providerErr) || providerErr.Code != "RATE_LIMITED" || !providerErr.KnownSession || providerErr.SessionID != "123e4567-e89b-12d3-a456-426614174002" || directCalls != 0 || len(launcher.Calls()) != 1 {
		t.Fatalf("err=%v direct=%d launch=%d", err, directCalls, len(launcher.Calls()))
	}
}

func TestClaudeModelHandshakeUsesProviderMetadata(t *testing.T) {
	line := `{"type":"control_response","response":{"subtype":"success","request_id":"rolemux-models","response":{"models":[{"value":"current[1m]","resolvedModel":"claude-current","displayName":"Current","description":"Live description","supportsEffort":true,"supportedEffortLevels":["low","max"],"supportsFastMode":true}],"account":{"email":"user@example.test"}}}}`
	page, err := parseClaudeModelHandshake([]byte(line + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if page.Account != "user@example.test" || len(page.Models) != 1 {
		t.Fatalf("page=%#v", page)
	}
	model := page.Models[0]
	if model.ID != "current[1m]" || model.Description != "Live description" || model.ContextWindowTokens != 1_000_000 || len(model.EffortOptions) != 2 || model.SpeedOptions[0].ID != "fast" {
		t.Fatalf("model=%#v", model)
	}
}

func TestCodexCacheEnrichesOnlyLiveModels(t *testing.T) {
	root := t.TempDir()
	data := `{"models":[{"slug":"live-model","description":"From CLI cache","context_window":272000,"max_context_window":872000,"supported_reasoning_levels":[{"effort":"high","description":"Deep"}],"service_tiers":[{"id":"priority","name":"Fast","description":"2x speed"}]}]}`
	if err := os.WriteFile(filepath.Join(root, "models_cache.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	models := []ModelInfo{{ID: "live-model"}, {ID: "not-in-cache"}}
	enrichCodexModelsFromCache(models, []string{"CODEX_HOME=" + root})
	if models[0].ContextWindowTokens != 272000 || models[0].MaxContextWindowTokens != 872000 || models[0].SpeedOptions[0].ID != "priority" || models[0].EffortOptions[0].Description != "Deep" {
		t.Fatalf("enriched=%#v", models[0])
	}
	if models[1].ContextWindowTokens != 0 {
		t.Fatalf("unmatched model was enriched: %#v", models[1])
	}
}

func TestCopilotModelDiscoveryPreservesCapabilities(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prompt, contextWindow := 120000, 128000
	adapter := &Copilot{Path: executable, Discover: func(context.Context) ([]copilotsdk.ModelInfo, error) {
		return []copilotsdk.ModelInfo{{
			ID: "copilot-current", Name: "Copilot Current",
			Capabilities:              copilotsdk.ModelCapabilities{Limits: copilotsdk.ModelLimits{MaxPromptTokens: &prompt, MaxContextWindowTokens: &contextWindow}},
			SupportedReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high",
		}}, nil
	}}
	page, err := adapter.ListModels(context.Background(), ModelListRequest{})
	if err != nil || len(page.Models) != 1 {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	model := page.Models[0]
	if model.ContextWindowTokens != contextWindow || model.MaxPromptTokens != prompt || len(model.EffortOptions) != 2 || model.DefaultEffort != "high" {
		t.Fatalf("model=%#v", model)
	}
}

func TestEnvelopeDecoderRejectsProseUnknownsAndInvalidVerdicts(t *testing.T) {
	bad := []string{
		"```json\n{}\n```",
		`{"role":"planner","status":"ready","plan":"ok","extra":true}`,
		`{"role":"code_reviewer","verdict":"approved","findings":[{"message":"still broken"}]}`,
		`{"role":"code_reviewer","verdict":"changes_requested","findings":[]}`,
	}
	for _, value := range bad {
		role := RolePlanner
		if strings.Contains(value, "code_reviewer") {
			role = RoleCodeReviewer
		}
		if _, err := DecodeEnvelope([]byte(value), role); err == nil {
			t.Errorf("accepted %s", value)
		}
	}
}

func TestEnvelopeAllowsMarkdownFenceInsideJSONString(t *testing.T) {
	value := "{\"role\":\"planner\",\"status\":\"ready\",\"plan\":\"Add:\\n```text\\nnote\\n```\",\"question\":\"\"}"
	if _, err := DecodeEnvelope([]byte(value), RolePlanner); err != nil {
		t.Fatalf("valid plan content was rejected: %v", err)
	}
}

func TestUsageNormalizationPreservesProviderCacheSemantics(t *testing.T) {
	codex := usageFromJSONLines([]byte("{\"type\":\"thread.started\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":120,\"cached_input_tokens\":100,\"output_tokens\":20,\"total_tokens\":140}}\n"), true)
	if codex.InputTokens != 120 || codex.CachedInputTokens != 100 || codex.OutputTokens != 20 || codex.TotalTokens != 140 {
		t.Fatalf("codex usage=%#v", codex)
	}
	claude := usageFromJSONDocument([]byte(`{"usage":{"input_tokens":10,"cache_read_input_tokens":40,"cache_creation_input_tokens":5,"output_tokens":8}}`), false)
	if claude.InputTokens != 10 || claude.CachedInputTokens != 40 || claude.CacheWriteTokens != 5 || claude.OutputTokens != 8 || claude.TotalTokens != 63 {
		t.Fatalf("claude usage=%#v", claude)
	}
}

func TestNativeWorkerSchemaRequiresEveryProperty(t *testing.T) {
	schema := NativeSchema(RolePlanner)
	for _, required := range []string{`"required":["role","status","plan","question","complexity","work_units"]`, `"complexity":{"type":"string","enum":["trivial","small","medium","large","system"]}`, `"required":["id","objective","scope","depends_on","execution_packet","acceptance_criteria","validation_commands"]`, `"additionalProperties":false`, `"enum":["planner"]`} {
		if !strings.Contains(schema, required) {
			t.Fatalf("schema missing %s: %s", required, schema)
		}
	}
}

func TestNativeReviewerSchemaRequiresEveryFindingProperty(t *testing.T) {
	schema := NativeSchema(RoleCodeReviewer)
	if !strings.Contains(schema, `"required":["severity","path","line","message"]`) {
		t.Fatalf("reviewer schema has a non-strict finding: %s", schema)
	}
}

func TestEnvironmentAndRepositoryReadConfinement(t *testing.T) {
	env := SanitizedEnv([]string{"PATH=/bin", "CODEX_APPROVAL_MODE=full", "SAFE=value", "THING_BYPASS_APPROVAL=yes"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "APPROVAL") || !strings.Contains(joined, "SAFE=value") {
		t.Fatalf("bad sanitized environment: %q", joined)
	}
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.WriteFile(inside, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PathWithinRepo(root, inside); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PathWithinRepo(root, link); err == nil {
		t.Fatal("symlink escape was accepted")
	}
	if SafeURL("https://user:pass@example.com") || !SafeURL("https://example.com/path") {
		t.Fatal("URL safety classification incorrect")
	}
}

func TestCopilotImplementerGetsOnlyScopeConfinedEditAccess(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	request := Request{Role: RoleImplementer, RepoRoot: root, Scope: "src/**"}
	config := (&Copilot{}).sessionConfig(request)
	if tools := strings.Join(config.AvailableTools, ","); !strings.Contains(tools, "builtin:edit") || strings.Contains(tools, "builtin:bash") {
		t.Fatalf("implementer tools=%q", tools)
	}
	approve := func(candidate string) rpc.PermissionDecision {
		decision, err := config.OnPermissionRequest(copilotsdk.PermissionRequestWrite{FileName: candidate}, copilotsdk.PermissionInvocation{})
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}
	if decision := approve(filepath.Join(root, "src", "new.go")); !isCopilotWriteApproved(decision) {
		t.Fatalf("new file inside declared scope was rejected: %#v", decision)
	}
	if _, ok := approve(filepath.Join(root, "other.go")).(*rpc.PermissionDecisionReject); !ok {
		t.Fatal("out-of-scope write was approved")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "src", "escape")); err != nil {
		t.Fatal(err)
	}
	if _, ok := approve(filepath.Join(root, "src", "escape", "leak.go")).(*rpc.PermissionDecisionReject); !ok {
		t.Fatal("symlink-escaped write was approved")
	}
	reviewer := (&Copilot{}).sessionConfig(Request{Role: RoleCodeReviewer, RepoRoot: root, Scope: "src/**"})
	if tools := strings.Join(reviewer.AvailableTools, ","); strings.Contains(tools, "builtin:edit") {
		t.Fatalf("reviewer tools=%q", tools)
	}
}

func isCopilotWriteApproved(decision rpc.PermissionDecision) bool {
	_, ok := decision.(*rpc.PermissionDecisionApproveOnce)
	return ok
}

func TestCopilotLoadsOnlyExplicitSkillDirectories(t *testing.T) {
	copilotRunner := &Copilot{}
	request := Request{SkillDirectories: []string{"/repo/.github/skills", "/home/.copilot/skills"}}
	fresh := copilotRunner.sessionConfig(request)
	resumed := copilotRunner.resumeConfig(request)
	if fresh.EnableConfigDiscovery == nil || *fresh.EnableConfigDiscovery || resumed.EnableConfigDiscovery == nil || *resumed.EnableConfigDiscovery {
		t.Fatal("ambient Copilot config discovery was enabled")
	}
	if fresh.EnableSkills == nil || !*fresh.EnableSkills || resumed.EnableSkills == nil || !*resumed.EnableSkills {
		t.Fatal("explicit Copilot skills were not enabled")
	}
	if !reflect.DeepEqual(fresh.SkillDirectories, request.SkillDirectories) || !reflect.DeepEqual(resumed.SkillDirectories, request.SkillDirectories) {
		t.Fatalf("fresh=%#v resumed=%#v", fresh.SkillDirectories, resumed.SkillDirectories)
	}
	empty := copilotRunner.sessionConfig(Request{})
	if empty.EnableSkills == nil || *empty.EnableSkills {
		t.Fatal("skills were enabled without explicit directories")
	}
}

func TestProviderLoginUsesOfficialInteractiveCommand(t *testing.T) {
	tests := []struct {
		name string
		path string
		args []string
		make func(InteractiveProcessFunc) Authenticator
	}{
		{"codex", "/bin/codex", []string{"login"}, func(run InteractiveProcessFunc) Authenticator {
			return &Codex{Path: "/bin/codex", Env: []string{"PATH=/bin"}, InteractiveProcess: run}
		}},
		{"claude", "/bin/claude", []string{"auth", "login"}, func(run InteractiveProcessFunc) Authenticator {
			return &Claude{Path: "/bin/claude", Env: []string{"PATH=/bin"}, InteractiveProcess: run}
		}},
		{"copilot", "/bin/copilot", []string{"login"}, func(run InteractiveProcessFunc) Authenticator {
			return &Copilot{Path: "/bin/copilot", Env: []string{"PATH=/bin"}, InteractiveProcess: run}
		}},
		{"antigravity", "/bin/agy", nil, func(run InteractiveProcessFunc) Authenticator {
			return &Antigravity{Path: "/bin/agy", Env: []string{"PATH=/bin"}, InteractiveProcess: run}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			run := func(_ context.Context, path string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				called = true
				if path != test.path || !reflect.DeepEqual(args, test.args) || dir != "/repo" || !reflect.DeepEqual(env, []string{"PATH=/bin"}) {
					t.Fatalf("path=%q args=%#v dir=%q env=%#v", path, args, dir, env)
				}
				if stdin == nil || stdout == nil || stderr == nil {
					t.Fatal("login did not inherit terminal streams")
				}
				return nil
			}
			authenticator := test.make(run)
			buffer := strings.NewReader("")
			if err := authenticator.Login(context.Background(), LoginRequest{RepoRoot: "/repo", Stdin: buffer, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("interactive login command was not called")
			}
		})
	}
}

func TestVerifyReportedSelectionRejectsProviderDrift(t *testing.T) {
	req := Request{Model: "wanted-model", Effort: "high"}
	if err := VerifyReportedSelection("codex", req, Response{}); err != nil {
		t.Fatalf("missing provider metadata should be accepted: %v", err)
	}
	if err := VerifyReportedSelection("codex", req, Response{ReportedModel: req.Model, ReportedEffort: req.Effort}); err != nil {
		t.Fatalf("matching provider metadata rejected: %v", err)
	}

	for _, test := range []struct {
		name     string
		response Response
		code     string
	}{
		{"model", Response{SessionID: "session-1", ReportedModel: "other-model", ReportedEffort: req.Effort}, "CODEX_MODEL_MISMATCH"},
		{"effort", Response{SessionID: "session-1", ReportedModel: req.Model, ReportedEffort: "low"}, "CODEX_EFFORT_MISMATCH"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := VerifyReportedSelection("codex", req, test.response)
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr.Code != test.code || !providerErr.KnownSession || providerErr.SessionID != "session-1" {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestVersionComparisonAndCapabilityProbes(t *testing.T) {
	if !versionAtLeast("codex-cli 0.153.3", 0, 153, 3) || versionAtLeast("2.1.259 (Claude Code)", 2, 1, 260) || !versionAtLeast("2.2.0", 2, 1, 260) {
		t.Fatal("semantic version comparison is incorrect")
	}
	codex := &Codex{Path: "/bin/codex", Env: []string{"PATH=/bin"}}
	codex.Process = func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
		if reflect.DeepEqual(spec.Args, []string{"--version"}) {
			return ProcessResult{Stdout: []byte("codex-cli 0.153.3")}, nil
		}
		return ProcessResult{Stdout: []byte("--cd --sandbox --ask-for-approval --search --ignore-user-config --ignore-rules --output-schema --json SESSION_ID")}, nil
	}
	if err := codex.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	claude := &Claude{Path: "/bin/claude", Env: []string{"PATH=/bin"}}
	claude.Process = func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
		if reflect.DeepEqual(spec.Args, []string{"--version"}) {
			return ProcessResult{Stdout: []byte("2.1.260 (Claude Code)")}, nil
		}
		return ProcessResult{Stdout: []byte("--safe-mode --restricted --permission-mode --permission-prompts --tools --allowed-tools --strict-mcp-config --mcp-config --session-id --resume --json-schema --effort")}, nil
	}
	if err := claude.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
}
