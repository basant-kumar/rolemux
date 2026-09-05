package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basant-kumar/rolemux/internal/task"
)

const fakePXPipeEnv = "ROLEMUX_FAKE_PXPIPE"

func TestCodexSuggestsPXPipeOnlyForFreshSession(t *testing.T) {
	codex := &Codex{Path: "/bin/codex", Env: []string{"PATH=/missing"}, Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
		return ProcessResult{Stdout: []byte("ok"), ProcessStarted: true}, nil
	}}
	var diagnostics []string
	callbacks := Callbacks{Diagnostic: func(message string) { diagnostics = append(diagnostics, message) }}
	provider := ProcessSpec{Path: "/bin/codex", Env: []string{"PATH=/missing"}}
	if _, err := codex.runCodexTask(context.Background(), Request{}, provider, callbacks, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := codex.runCodexTask(context.Background(), Request{Resume: true}, provider, callbacks, ""); err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0], pxpipeInstallCommand) || !strings.Contains(diagnostics[0], "running Codex directly") {
		t.Fatalf("diagnostics=%v", diagnostics)
	}
}

// The subprocess fixture exercises the real ProcessFunc/server boundary. It
// is deliberately a proxy-shaped HTTP fixture, not compression evidence: the
// installed pxpipe binary remains the authority for real image transformation.
func TestMain(m *testing.M) {
	if os.Getenv(fakePXPipeEnv) != "" {
		if len(os.Args) > 1 && os.Args[1] == "warp" {
			os.Exit(runFakePXPipeWarp())
		}
		os.Exit(runFakePXPipeServer())
	}
	os.Exit(m.Run())
}

func runFakePXPipeServer() int {
	host := os.Getenv("HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("PORT")
	if port == "" {
		return 2
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return 2
	}
	server := &http.Server{Handler: http.HandlerFunc(fakePXPipeHandler)}
	go func() { _ = server.Serve(listener) }()
	_, _ = fmt.Fprintf(os.Stdout, "[pxpipe] listening on http://%s\n", net.JoinHostPort(host, port))
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	<-signals
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	return 0
}

func fakePXPipeHandler(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/proxy-stats" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		http.Error(w, "server received provider API key", http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	upstream := strings.TrimRight(os.Getenv("FAKE_UPSTREAM_URL"), "/")
	if upstream == "" {
		http.Error(w, "missing fake upstream", http.StatusBadGateway)
		return
	}
	forward, err := http.NewRequestWithContext(request.Context(), request.Method, upstream+request.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	forward.Header = request.Header.Clone()
	response, err := http.DefaultClient.Do(forward)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	// This is a deliberately fake eligibility fixture. It records the arm that
	// a model-enabled/disabled pxpipe test would select, but it never claims
	// real image transformation or compression.
	compressed := false
	for _, model := range strings.Split(os.Getenv("PXPIPE_MODELS"), ",") {
		if strings.TrimSpace(model) == "exact/model" {
			compressed = true
			break
		}
	}
	inputMode := "text"
	if compressed {
		inputMode = "image"
	}
	if logPath := os.Getenv("PXPIPE_LOG"); logPath != "" {
		if file, openErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); openErr == nil {
			_ = json.NewEncoder(file).Encode(map[string]any{
				"status": response.StatusCode, "model": "exact/model", "compressed": compressed,
				"input": map[string]string{"mode": inputMode},
			})
			_ = file.Close()
		}
	}
	for key, values := range response.Header {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func runFakePXPipeWarp() int {
	if len(os.Args) == 3 && os.Args[1] == "warp" && os.Args[2] == "--help" {
		_, _ = io.WriteString(os.Stdout, "pxpipe warp [--route PATTERN=TARGET]... -- CMD\n")
		return 0
	}
	if len(os.Args) < 6 || os.Args[1] != "warp" {
		return 2
	}
	if os.Getenv("FAKE_PROVIDER_SECRET") != "provider-secret" {
		return 2
	}
	routeIndex := -1
	for index := 2; index+1 < len(os.Args); index++ {
		if os.Args[index] == "--route" {
			routeIndex = index + 1
			break
		}
	}
	if routeIndex < 0 {
		return 2
	}
	route := os.Args[routeIndex]
	parts := strings.SplitN(route, "=", 2)
	if len(parts) != 2 || parts[0] != pxpipeRoutePattern {
		return 2
	}
	dash := -1
	for index, arg := range os.Args {
		if arg == "--" {
			dash = index
			break
		}
	}
	if dash < 0 || dash+1 >= len(os.Args) || os.Args[dash+1] != "/bin/codex" {
		return 2
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil || string(stdin) != "prompt" {
		return 2
	}
	request, err := http.NewRequest(http.MethodPost, parts[1]+"/backend-api/codex/responses?stream=true", strings.NewReader(`{"model":"exact/model"}`))
	if err != nil {
		return 2
	}
	request.Header.Set("Authorization", os.Getenv("FAKE_AUTH"))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 2
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 2
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_, _ = os.Stdout.Write(codexTaskOutput("integration-session", RolePlanner))
	return 0
}

func TestParseCodexAuthStatusConservativeModes(t *testing.T) {
	tests := []struct {
		name string
		data string
		want CodexAuthMode
		bad  bool
	}{
		{"chatgpt", "Logged in using ChatGPT", CodexAuthChatGPT, false},
		{"api key", "Logged in using an API key - stored in the Codex credentials file", CodexAuthAPIKey, false},
		{"unauthenticated", "Not logged in", CodexAuthUnauthenticated, false},
		{"json chatgpt", `{"auth_method":"chatgpt"}`, CodexAuthChatGPT, false},
		{"json api key", `{"authenticated":true,"method":"api_key"}`, CodexAuthAPIKey, false},
		{"json unauthenticated", `{"authenticated":false}`, CodexAuthUnauthenticated, false},
		{"unknown", "Logged in with an unknown provider", CodexAuthUnknown, true},
		{"contradictory", "Logged in using ChatGPT\nLogged in using API key", CodexAuthUnknown, true},
		{"malformed", "{not-json", CodexAuthUnknown, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseCodexAuthStatus([]byte(test.data))
			if got != test.want || (err != nil) != test.bad {
				t.Fatalf("mode=%q err=%v, want mode=%q bad=%t", got, err, test.want, test.bad)
			}
		})
	}
}

func TestCodexAuthAcceptsStoredAPIKeyStatusOutput(t *testing.T) {
	c := &Codex{Path: "/bin/codex", Env: []string{"PATH=/bin"}, Process: func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
		if reflect.DeepEqual(spec.Args, []string{"--version"}) {
			return ProcessResult{Stdout: []byte("codex 0.153.3\n")}, nil
		}
		if reflect.DeepEqual(spec.Args, []string{"login", "status"}) {
			return ProcessResult{Stdout: []byte("Logged in using an API key - stored in the Codex credentials file\n")}, nil
		}
		return ProcessResult{}, errors.New("unexpected Codex probe")
	}}
	status, err := c.Auth(context.Background())
	if err != nil || !status.Authenticated || status.Message != "authenticated with API key" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestCodexGenericAuthAcceptsDirectOnlyModes(t *testing.T) {
	for _, output := range []string{
		"Logged in using access token",
		"Logged in using personal access token",
		"Logged in using workload identity",
		"Logged in using Amazon Bedrock API key",
		"Logged in using Amazon Bedrock AWS access keys",
	} {
		t.Run(output, func(t *testing.T) {
			authenticated, message, err := ParseCodexLoginStatus([]byte(output))
			if err != nil || !authenticated || message != "authenticated with Codex CLI" {
				t.Fatalf("authenticated=%t message=%q err=%v", authenticated, message, err)
			}
			if mode, strictErr := ParseCodexAuthStatus([]byte(output)); strictErr == nil || mode != CodexAuthUnknown {
				t.Fatalf("pxpipe routing accepted direct-only auth: mode=%q err=%v", mode, strictErr)
			}
		})
	}
}

func TestCodexAuthEvidenceUsesSelectedCLIAndEnvironmentDirectly(t *testing.T) {
	var probe ProcessSpec
	c := &Codex{Process: func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
		probe = spec
		return ProcessResult{Stdout: []byte("Logged in using ChatGPT\n")}, nil
	}}
	evidence, err := c.codexAuthEvidence(context.Background(), "/selected/codex", []string{"CODEX_HOME=/private/codex"})
	if err != nil || evidence.Mode != CodexAuthChatGPT {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
	if probe.Path != "/selected/codex" || !reflect.DeepEqual(probe.Args, []string{"login", "status"}) || !reflect.DeepEqual(probe.Env, []string{"CODEX_HOME=/private/codex"}) {
		t.Fatalf("authentication probe was not direct/selected: %#v", probe)
	}
}

func TestCodexChatGPTRouteGateAndOverlayDoNotMutateRuntime(t *testing.T) {
	runtime := task.RuntimeSnapshot{
		ProviderType: "codex",
		SDKSettings:  map[string]any{"request_max_retries": 3},
	}
	if CodexChatGPTRouteSupported(runtime, []string{"PATH=/bin"}) {
		t.Fatal("custom runtime settings were treated as the default ChatGPT route")
	}
	runtime.SDKSettings = nil
	if !CodexChatGPTRouteSupported(runtime, []string{"PATH=/bin"}) {
		t.Fatal("empty Codex routing was not recognized as the possible default route")
	}
	if !CodexChatGPTRouteSupported(task.RuntimeSnapshot{ProviderType: "codex", Endpoint: codexChatGPTBaseURL, WireAPI: "responses"}, []string{"OPENAI_BASE_URL=" + codexChatGPTBaseURL}) {
		t.Fatal("equivalent explicit ChatGPT route was rejected")
	}
	if CodexChatGPTRouteSupported(task.RuntimeSnapshot{ProviderType: "codex", Endpoint: "https://gateway.example.invalid/v1"}, nil) {
		t.Fatal("unrelated route was accepted")
	}
	if CodexChatGPTRouteSupported(task.RuntimeSnapshot{ProviderType: "codex", AuthEnvRefs: []string{"OPENAI_API_KEY"}}, nil) {
		t.Fatal("environment-based API-key routing was accepted")
	}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY"} {
		if CodexChatGPTRouteSupported(task.RuntimeSnapshot{ProviderType: "codex"}, []string{key + "=secret"}) {
			t.Fatalf("ambient %s was accepted for ChatGPT wrapping", key)
		}
	}
	if !CodexChatGPTRouteSupported(task.RuntimeSnapshot{ProviderType: "codex", Endpoint: codexChatGPTBaseURL + "/", WireAPI: "RESPONSES", SDKSettings: map[string]any{"supports_websockets": false}}, nil) {
		t.Fatal("equivalent ChatGPT route with safe settings was rejected")
	}
	if CodexChatGPTRouteSupported(task.RuntimeSnapshot{ProviderType: "codex", Endpoint: codexChatGPTBaseURL, WireAPI: "responses"}, []string{"OPENAI_BASE_URL=" + codexChatGPTBaseURL, "CODEX_BASE_URL=https://gateway.example.invalid/v1"}) {
		t.Fatal("conflicting environment routes were accepted")
	}

	original := task.RuntimeSnapshot{ProviderType: "codex", Endpoint: codexChatGPTBaseURL, WireAPI: "responses", Auth: map[string]any{"nested": map[string]any{"value": "kept"}}}
	overlay := CodexChatGPTRuntimeOverlay(original)
	if original.ProviderID != "" || original.Auth["nested"].(map[string]any)["value"] != "kept" {
		t.Fatal("overlay mutated input runtime")
	}
	if overlay.Endpoint != codexChatGPTBaseURL || overlay.WireAPI != "responses" || overlay.ProviderID == "" || overlay.SDKSettings["name"] != "RoleMux ChatGPT" || overlay.SDKSettings["requires_openai_auth"] != true || overlay.SDKSettings["supports_websockets"] != false {
		t.Fatalf("unexpected overlay: %#v", overlay)
	}
	if _, err := CodexConfigOverrides(overlay); err != nil {
		t.Fatal(err)
	}
}

func TestCodexBooleanRoutingValidation(t *testing.T) {
	for _, key := range []string{"requires_openai_auth", "supports_websockets", "supports_standalone_web_search"} {
		_, err := CodexConfigOverrides(task.RuntimeSnapshot{ProviderID: "test", SDKSettings: map[string]any{key: "true"}})
		if err == nil {
			t.Fatalf("accepted non-boolean %s", key)
		}
	}
}

type captureCodexLauncher struct {
	mu    sync.Mutex
	calls []PXPipeLaunchSpec
	fn    func(PXPipeLaunchSpec) (ProcessResult, error)
}

func (l *captureCodexLauncher) Launch(_ context.Context, spec PXPipeLaunchSpec) (ProcessResult, error) {
	l.mu.Lock()
	l.calls = append(l.calls, spec)
	l.mu.Unlock()
	if l.fn != nil {
		return l.fn(spec)
	}
	return ProcessResult{}, nil
}

func (l *captureCodexLauncher) Calls() []PXPipeLaunchSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]PXPipeLaunchSpec(nil), l.calls...)
}

func codexTaskOutput(session string, role Role) []byte {
	thread, _ := json.Marshal(map[string]any{"type": "thread.started", "thread_id": session})
	text, _ := json.Marshal(map[string]any{"role": string(role), "status": "ready", "plan": "ok", "question": ""})
	item, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"text": string(text)}})
	return append(append(thread, '\n'), append(item, '\n')...)
}

func TestCodexAuthGateUsesDirectLaunchForAPIKeyAndWrapsChatGPT(t *testing.T) {
	var direct []ProcessSpec
	provider := func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
		direct = append(direct, spec)
		if strings.EqualFold(spec.Args[0], "login") {
			t.Fatal("injected auth probe should have avoided the direct process seam")
		}
		return ProcessResult{Stdout: codexTaskOutput("direct", RolePlanner)}, nil
	}
	base := Request{Role: RolePlanner, RepoRoot: "/repo", Model: "exact/model", Effort: "high", Speed: "fast", Sandbox: "read-only", Prompt: "prompt", Runtime: task.RuntimeSnapshot{ProviderType: "codex"}}

	apiKey := &Codex{Path: "/bin/codex", Env: []string{"PATH=/bin"}, Process: provider, PXPipePath: "/bin/pxpipe", AuthProbe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
		return CodexAuthEvidence{Mode: CodexAuthAPIKey}, nil
	}, TaskLauncher: &captureCodexLauncher{}}
	if _, err := apiKey.Run(context.Background(), base, Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if len(direct) != 1 || !reflect.DeepEqual(direct[0].Args[0], "-C") {
		t.Fatalf("API-key path did not launch direct exactly once: %#v", direct)
	}

	direct = nil
	launcher := &captureCodexLauncher{fn: func(spec PXPipeLaunchSpec) (ProcessResult, error) {
		if spec.Provider.Path != "/bin/codex" || spec.Provider.Args[0] != "-C" || spec.Provider.Env == nil {
			t.Fatalf("provider spec was not preserved: %#v", spec.Provider)
		}
		return ProcessResult{Stdout: codexTaskOutput("wrapped", RolePlanner)}, nil
	}}
	chatgpt := &Codex{Path: "/bin/codex", Env: []string{"PATH=/bin"}, Process: provider, PXPipePath: "/bin/pxpipe", AuthProbe: func(_ context.Context, path string, env []string) (CodexAuthEvidence, error) {
		if path != "/bin/codex" || len(env) == 0 {
			t.Fatalf("auth probe did not receive task executable/environment: %q %#v", path, env)
		}
		return CodexAuthEvidence{Mode: CodexAuthChatGPT}, nil
	}, TaskLauncher: launcher}
	if _, err := chatgpt.Run(context.Background(), base, Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if len(direct) != 0 || len(launcher.Calls()) != 1 {
		t.Fatalf("ChatGPT path was not wrapped exactly once: direct=%d wrapped=%d", len(direct), len(launcher.Calls()))
	}
	call := launcher.Calls()[0]
	joined := strings.Join(call.Provider.Args, " ")
	for _, want := range []string{"model_providers.rolemux_chatgpt.base_url=", "wire_api", "requires_openai_auth=true", "supports_websockets=false", "exact/model", "model_reasoning_effort=high", "service_tier=fast"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("wrapped args missing %q: %s", want, joined)
		}
	}
	if !reflect.DeepEqual(base.Runtime.SDKSettings, map[string]any(nil)) {
		t.Fatalf("request runtime was mutated: %#v", base.Runtime)
	}
}

func TestCodexHelperFailureFallsBackOnlyBeforeTask(t *testing.T) {
	var calls int
	launcher := &captureCodexLauncher{fn: func(PXPipeLaunchSpec) (ProcessResult, error) {
		return ProcessResult{}, &PXPipeLaunchError{BeforeTask: true, Cause: errors.New("startup")}
	}}
	c := &Codex{Path: "/bin/codex", PXPipePath: "/bin/pxpipe", TaskLauncher: launcher, AuthProbe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
		return CodexAuthEvidence{Mode: CodexAuthChatGPT}, nil
	}, Process: func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
		calls++
		return ProcessResult{Stdout: codexTaskOutput("fallback", RolePlanner)}, nil
	}}
	if _, err := c.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "model"}, Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(launcher.Calls()) != 1 {
		t.Fatalf("fallback calls=%d helper=%d", calls, len(launcher.Calls()))
	}
}

func TestCodexWarpCapabilityFailureFallsBackBeforeCodex(t *testing.T) {
	dir := t.TempDir()
	pxpipe := filepath.Join(dir, "pxpipe")
	if err := os.WriteFile(pxpipe, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	directCalls, serverCalls := 0, 0
	launcher := &PXPipeTaskLauncher{
		Path:            pxpipe,
		CapabilityCheck: func(context.Context, ProcessSpec) error { return errors.New("warp is unsupported") },
		ServerFactory: func(context.Context, PXPipeServerSpec) (PXPipeServer, error) {
			serverCalls++
			return nil, errors.New("server must not start after preflight failure")
		},
	}
	c := &Codex{
		Path: pxpipe, Env: []string{"PATH=/bin"}, PXPipePath: pxpipe, TaskLauncher: launcher,
		AuthProbe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
			return CodexAuthEvidence{Mode: CodexAuthChatGPT}, nil
		},
		Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
			directCalls++
			return ProcessResult{Stdout: codexTaskOutput("preflight-fallback", RolePlanner)}, nil
		},
	}
	if _, err := c.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "model"}, Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if directCalls != 1 || serverCalls != 0 {
		t.Fatalf("capability fallback direct=%d server=%d", directCalls, serverCalls)
	}
}

func TestPXPipeCapabilityProbeRejectsServerOnlyHelperBeforeStartingServer(t *testing.T) {
	dir := t.TempDir()
	pxpipe := filepath.Join(dir, "pxpipe")
	if err := os.WriteFile(pxpipe, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	serverCalls, processCalls := 0, 0
	launcher := &PXPipeTaskLauncher{
		Path: pxpipe, MaxAttempts: 1,
		PortChooser: func(context.Context) ([]int, error) {
			t.Fatal("port chooser ran after capability failure")
			return nil, nil
		},
		ServerFactory: func(context.Context, PXPipeServerSpec) (PXPipeServer, error) {
			serverCalls++
			return nil, errors.New("server must not start")
		},
		Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
			processCalls++
			return ProcessResult{}, nil
		},
	}
	_, err := launcher.Launch(context.Background(), PXPipeLaunchSpec{
		PXPipePath: pxpipe,
		Provider:   ProcessSpec{Path: "/bin/codex", Env: []string{"PATH=/bin"}},
	})
	var launchErr *PXPipeLaunchError
	if !errors.As(err, &launchErr) || !launchErr.BeforeTask || serverCalls != 0 || processCalls != 0 {
		t.Fatalf("capability result err=%v server=%d process=%d", err, serverCalls, processCalls)
	}
}

func TestPXPipeRouteRejectionBeforeProviderOutputFallsBackDirectly(t *testing.T) {
	server := &fakePXPipeServer{done: make(chan struct{})}
	directCalls := 0
	launcher := &PXPipeTaskLauncher{
		Path: "/bin/pxpipe", MaxAttempts: 1,
		CapabilityCheck: func(context.Context, ProcessSpec) error { return nil },
		PortChooser:     func(context.Context) ([]int, error) { return []int{43223}, nil },
		ServerFactory: func(context.Context, PXPipeServerSpec) (PXPipeServer, error) {
			return server, nil
		},
		Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
			return ProcessResult{ProcessStarted: true, Stderr: []byte("an arbitrary setup failure")}, errors.New("pxpipe setup failed")
		},
	}
	adapter := &Codex{
		Path: "/bin/codex", Env: []string{"PATH=/bin"}, PXPipePath: "/bin/pxpipe", TaskLauncher: launcher,
		AuthProbe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
			return CodexAuthEvidence{Mode: CodexAuthChatGPT}, nil
		},
		Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
			directCalls++
			return ProcessResult{Stdout: codexTaskOutput("route-fallback", RolePlanner)}, nil
		},
	}
	if _, err := adapter.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "model"}, Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if directCalls != 1 {
		t.Fatalf("route rejection did not fall back exactly once: %d", directCalls)
	}
}

func TestPXPipeFailureAfterThreadStartIsNeverReplayed(t *testing.T) {
	server := &fakePXPipeServer{done: make(chan struct{})}
	launcher := &PXPipeTaskLauncher{
		Path: "/bin/pxpipe", MaxAttempts: 1,
		CapabilityCheck: func(context.Context, ProcessSpec) error { return nil },
		PortChooser:     func(context.Context) ([]int, error) { return []int{43224}, nil },
		ServerFactory: func(context.Context, PXPipeServerSpec) (PXPipeServer, error) {
			return server, nil
		},
		Process: func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
			if err := spec.StdoutLine([]byte(`{"type":"thread.started","thread_id":"started-session"}`)); err != nil {
				return ProcessResult{ProcessStarted: true}, err
			}
			return ProcessResult{ProcessStarted: true}, errors.New("provider failed after thread start")
		},
	}
	_, err := launcher.Launch(context.Background(), PXPipeLaunchSpec{
		PXPipePath: "/bin/pxpipe",
		Provider:   ProcessSpec{Path: "/bin/codex"},
	})
	var launchErr *PXPipeLaunchError
	if !errors.As(err, &launchErr) || launchErr.BeforeTask {
		t.Fatalf("post-thread failure was classified as replayable: %v", err)
	}
}

func TestPXPipeProcessStartBoundaryIsNeverReplayed(t *testing.T) {
	server := &fakePXPipeServer{done: make(chan struct{})}
	eventsFile := filepath.Join(t.TempDir(), "events.jsonl")
	launcher := &PXPipeTaskLauncher{
		Path: "/bin/pxpipe", MaxAttempts: 1,
		CapabilityCheck: func(context.Context, ProcessSpec) error { return nil },
		PortChooser:     func(context.Context) ([]int, error) { return []int{43226}, nil },
		ServerFactory: func(context.Context, PXPipeServerSpec) (PXPipeServer, error) {
			return server, nil
		},
		Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
			if err := os.WriteFile(eventsFile, []byte(`{"status":429}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return ProcessResult{ProcessStarted: true}, errors.New("provider failed after process start")
		},
	}
	_, err := launcher.Launch(context.Background(), PXPipeLaunchSpec{
		PXPipePath: "/bin/pxpipe", Provider: ProcessSpec{Path: "/bin/claude"}, EventsFile: eventsFile, TaskStartsOnLaunch: true,
	})
	var launchErr *PXPipeLaunchError
	if !errors.As(err, &launchErr) || launchErr.BeforeTask || !strings.Contains(err.Error(), "pxpipe upstream status 429") {
		t.Fatalf("post-process-start failure was classified as replayable: %v", err)
	}
}

func TestPXPipeServerExitBeforeThreadStartRemainsReplayable(t *testing.T) {
	server := &fakePXPipeServer{done: make(chan struct{}), err: errors.New("server exited")}
	launcher := &PXPipeTaskLauncher{
		Path: "/bin/pxpipe", MaxAttempts: 1,
		CapabilityCheck: func(context.Context, ProcessSpec) error { return nil },
		PortChooser:     func(context.Context) ([]int, error) { return []int{43225}, nil },
		ServerFactory: func(context.Context, PXPipeServerSpec) (PXPipeServer, error) {
			server.err = nil // readiness succeeds; the server fails after launch.
			return server, nil
		},
		Process: func(ctx context.Context, _ ProcessSpec) (ProcessResult, error) {
			server.err = errors.New("server exited")
			server.once.Do(func() { close(server.done) })
			<-ctx.Done()
			return ProcessResult{ProcessStarted: true}, ctx.Err()
		},
	}
	_, err := launcher.Launch(context.Background(), PXPipeLaunchSpec{
		PXPipePath: "/bin/pxpipe",
		Provider:   ProcessSpec{Path: "/bin/codex"},
	})
	var launchErr *PXPipeLaunchError
	if !errors.As(err, &launchErr) || !launchErr.BeforeTask {
		t.Fatalf("pre-thread server exit was not replayable: %v", err)
	}
}

func TestBoundedDiagnosticIsSingleLineAndBounded(t *testing.T) {
	got := boundedDiagnostic("  setup failed\n" + strings.Repeat("x", 800) + " actionable-tail")
	if strings.ContainsAny(got, "\r\n\t") || len(got) > 724 || !strings.HasSuffix(got, "actionable-tail") || !strings.HasPrefix(got, "setup failed") {
		t.Fatalf("diagnostic=%q len=%d", got, len(got))
	}
}

func TestCodexAuthTransitionsRecheckOnResumedTurns(t *testing.T) {
	transitions := []struct {
		name  string
		modes []CodexAuthMode
	}{
		{name: "ChatGPT to API key", modes: []CodexAuthMode{CodexAuthChatGPT, CodexAuthAPIKey}},
		{name: "API key to ChatGPT", modes: []CodexAuthMode{CodexAuthAPIKey, CodexAuthChatGPT}},
	}
	for _, transition := range transitions {
		t.Run(transition.name, func(t *testing.T) {
			var probeCalls, directCalls int
			var directSpecs []ProcessSpec
			launcher := &captureCodexLauncher{fn: func(PXPipeLaunchSpec) (ProcessResult, error) {
				return ProcessResult{Stdout: codexTaskOutput("transition-session", RolePlanner)}, nil
			}}
			adapter := &Codex{
				Path: "/bin/codex", Env: []string{"PATH=/bin"}, PXPipePath: "/bin/pxpipe", TaskLauncher: launcher,
				AuthProbe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
					mode := transition.modes[probeCalls]
					probeCalls++
					return CodexAuthEvidence{Mode: mode}, nil
				},
				Process: func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
					directCalls++
					directSpecs = append(directSpecs, spec)
					return ProcessResult{Stdout: codexTaskOutput("transition-session", RolePlanner)}, nil
				},
			}
			request := Request{Role: RolePlanner, RepoRoot: "/repo", Model: "exact/model", Prompt: "first", Runtime: task.RuntimeSnapshot{ProviderType: "codex"}}
			if _, err := adapter.Run(context.Background(), request, Callbacks{}); err != nil {
				t.Fatal(err)
			}
			request.Resume, request.SessionID, request.Prompt = true, "transition-session", "resume"
			if _, err := adapter.Run(context.Background(), request, Callbacks{}); err != nil {
				t.Fatal(err)
			}
			if probeCalls != 2 {
				t.Fatalf("auth evidence was cached across turns: %d probes", probeCalls)
			}
			wantWrapped := transition.modes[0] == CodexAuthChatGPT
			if len(launcher.Calls()) != boolInt(wantWrapped)+boolInt(transition.modes[1] == CodexAuthChatGPT) || directCalls != boolInt(!wantWrapped)+boolInt(transition.modes[1] != CodexAuthChatGPT) {
				t.Fatalf("transition launches wrapped=%d direct=%d", len(launcher.Calls()), directCalls)
			}
			if transition.modes[1] != CodexAuthChatGPT {
				if len(directSpecs) != 2 && len(directSpecs) != 1 {
					t.Fatalf("unexpected direct specs: %#v", directSpecs)
				}
				last := directSpecs[len(directSpecs)-1]
				if !containsArg(last.Args, "resume") || !containsArg(last.Args, "transition-session") {
					t.Fatalf("resume arguments were not preserved: %#v", last.Args)
				}
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func containsArg(args []string, wanted string) bool {
	for _, arg := range args {
		if arg == wanted {
			return true
		}
	}
	return false
}

func TestCodexWarpFailureAfterTaskDoesNotReplayDirectly(t *testing.T) {
	directCalls := 0
	launcher := &captureCodexLauncher{fn: func(PXPipeLaunchSpec) (ProcessResult, error) {
		return ProcessResult{}, &PXPipeLaunchError{BeforeTask: false, Cause: errors.New("warp rejected route")}
	}}
	adapter := &Codex{
		Path: "/bin/codex", Env: []string{"PATH=/bin"}, PXPipePath: "/bin/pxpipe", TaskLauncher: launcher,
		AuthProbe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
			return CodexAuthEvidence{Mode: CodexAuthChatGPT}, nil
		},
		Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
			directCalls++
			return ProcessResult{Stdout: codexTaskOutput("unexpected-replay", RolePlanner)}, nil
		},
	}
	if _, err := adapter.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "model"}, Callbacks{}); err == nil {
		t.Fatal("warp failure unexpectedly succeeded")
	}
	if directCalls != 0 {
		t.Fatalf("task was replayed directly after warp started: %d", directCalls)
	}
}

func TestCodexAuthProbeFailureAndUnknownUseOneDirectLaunch(t *testing.T) {
	tests := []struct {
		name  string
		probe func(context.Context, string, []string) (CodexAuthEvidence, error)
	}{
		{name: "failed", probe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
			return CodexAuthEvidence{Mode: CodexAuthUnknown}, errors.New("status timeout")
		}},
		{name: "timed out", probe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
			return CodexAuthEvidence{Mode: CodexAuthUnknown, Reason: "authentication status probe timed out"}, context.DeadlineExceeded
		}},
		{name: "unknown", probe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
			return CodexAuthEvidence{Mode: CodexAuthUnknown, Reason: "authentication status format is unknown"}, nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directCalls := 0
			launcher := &captureCodexLauncher{}
			adapter := &Codex{
				Path: "/bin/codex", Env: []string{"PATH=/bin"}, PXPipePath: "/bin/pxpipe", TaskLauncher: launcher,
				AuthProbe: test.probe,
				Process: func(context.Context, ProcessSpec) (ProcessResult, error) {
					directCalls++
					return ProcessResult{Stdout: codexTaskOutput("direct-auth-fallback", RolePlanner)}, nil
				},
			}
			if _, err := adapter.Run(context.Background(), Request{Role: RolePlanner, RepoRoot: "/repo", Model: "model"}, Callbacks{}); err != nil {
				t.Fatal(err)
			}
			if directCalls != 1 || len(launcher.Calls()) != 0 {
				t.Fatalf("probe=%s direct=%d wrapped=%d", test.name, directCalls, len(launcher.Calls()))
			}
		})
	}
}

type fakePXPipeServer struct {
	done chan struct{}
	err  error
	once sync.Once
}

type blockingStopPXPipeServer struct {
	done      chan struct{}
	stopStart chan struct{}
	startOnce sync.Once
}

func (s *blockingStopPXPipeServer) WaitReady(context.Context) error { return nil }
func (s *blockingStopPXPipeServer) Done() <-chan struct{}           { return s.done }
func (s *blockingStopPXPipeServer) Err() error                      { return nil }
func (s *blockingStopPXPipeServer) Stop(ctx context.Context) error {
	s.startOnce.Do(func() { close(s.stopStart) })
	<-ctx.Done()
	return ctx.Err()
}

func (s *fakePXPipeServer) WaitReady(context.Context) error { return s.err }
func (s *fakePXPipeServer) Done() <-chan struct{}           { return s.done }
func (s *fakePXPipeServer) Err() error                      { return s.err }
func (s *fakePXPipeServer) Stop(context.Context) error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func TestPXPipeTaskLauncherBuildsPrivateWarpAndCleansServer(t *testing.T) {
	dir := t.TempDir()
	pxpipe := filepath.Join(dir, "pxpipe")
	if err := os.WriteFile(pxpipe, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := &fakePXPipeServer{done: make(chan struct{})}
	var serverSpec PXPipeServerSpec
	var processSpec ProcessSpec
	launcher := &PXPipeTaskLauncher{
		Path: pxpipe, MaxAttempts: 1, PortChooser: func(context.Context) ([]int, error) { return []int{43210}, nil },
		CapabilityCheck: func(context.Context, ProcessSpec) error { return nil },
		ServerFactory: func(_ context.Context, spec PXPipeServerSpec) (PXPipeServer, error) {
			serverSpec = spec
			return server, nil
		},
		Process: func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
			processSpec = spec
			return ProcessResult{Stdout: []byte("ok")}, nil
		},
	}
	var diagnostics []string
	_, err := launcher.Launch(context.Background(), PXPipeLaunchSpec{
		PXPipePath: pxpipe,
		Provider:   ProcessSpec{Path: "/bin/codex", Args: []string{"-C", "/repo"}, Env: []string{"OPENAI_API_KEY=secret", "PXPIPE_MODELS=exact", "CODEX_HOME=/home/codex"}},
		Diagnostic: func(message string) { diagnostics = append(diagnostics, message) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if serverSpec.Port != 43210 || processSpec.Path != pxpipe || !reflect.DeepEqual(processSpec.Args[:3], []string{"warp", "--route", "chatgpt.com/backend-api/codex/responses*=http://127.0.0.1:43210"}) {
		t.Fatalf("server=%#v process=%#v", serverSpec, processSpec)
	}
	serverEnv := strings.Join(serverSpec.Env, "\n")
	if strings.Contains(serverEnv, "OPENAI_API_KEY=secret") || !strings.Contains(serverEnv, "OPENAI_UPSTREAM="+codexChatGPTOrigin) || !strings.Contains(serverEnv, "PXPIPE_MODELS=exact") || !strings.Contains(serverEnv, "PORT=43210") {
		t.Fatalf("unsafe/private server environment: %#v", serverSpec.Env)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0], "pxpipe dashboard (this turn): http://127.0.0.1:43210/") || !strings.Contains(diagnostics[0], "events:") {
		t.Fatalf("diagnostics=%v", diagnostics)
	}
	select {
	case <-server.done:
	default:
		t.Fatal("server was not stopped")
	}
}

func TestPXPipeTaskLaunchersCanRunConcurrentlyWithoutSharedLifecycle(t *testing.T) {
	start := make(chan struct{})
	ports := []int{43220, 43221}
	servers := make([]*fakePXPipeServer, len(ports))
	for index := range servers {
		servers[index] = &fakePXPipeServer{done: make(chan struct{})}
	}
	errs := make(chan error, len(ports))
	var group sync.WaitGroup
	for index := range ports {
		index, port := index, ports[index]
		launcher := &PXPipeTaskLauncher{
			Path: "/bin/pxpipe", MaxAttempts: 1,
			CapabilityCheck: func(context.Context, ProcessSpec) error { return nil },
			PortChooser:     func(context.Context) ([]int, error) { return []int{port}, nil },
			ServerFactory: func(_ context.Context, spec PXPipeServerSpec) (PXPipeServer, error) {
				if spec.Port != port {
					return nil, fmt.Errorf("port=%d want=%d", spec.Port, port)
				}
				return servers[index], nil
			},
			Process: func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
				if !strings.Contains(strings.Join(spec.Args, " "), ":"+strconv.Itoa(port)) {
					return ProcessResult{}, fmt.Errorf("warp route did not retain port %d: %#v", port, spec.Args)
				}
				return ProcessResult{}, nil
			},
		}
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := launcher.Launch(context.Background(), PXPipeLaunchSpec{PXPipePath: "/bin/pxpipe", Provider: ProcessSpec{Path: "/bin/codex"}})
			errs <- err
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for index, server := range servers {
		select {
		case <-server.done:
		default:
			t.Fatalf("concurrent server %d was not stopped", index)
		}
	}
}

func TestPXPipeShutdownIsBoundedWhenServerStopStalls(t *testing.T) {
	server := &blockingStopPXPipeServer{done: make(chan struct{}), stopStart: make(chan struct{})}
	launcher := &PXPipeTaskLauncher{
		Path: "/bin/pxpipe", MaxAttempts: 1, ShutdownTimeout: 20 * time.Millisecond,
		CapabilityCheck: func(context.Context, ProcessSpec) error { return nil },
		PortChooser:     func(context.Context) ([]int, error) { return []int{43222}, nil },
		ServerFactory: func(context.Context, PXPipeServerSpec) (PXPipeServer, error) {
			return server, nil
		},
		Process: func(context.Context, ProcessSpec) (ProcessResult, error) { return ProcessResult{}, nil },
	}
	started := time.Now()
	if _, err := launcher.Launch(context.Background(), PXPipeLaunchSpec{PXPipePath: "/bin/pxpipe", Provider: ProcessSpec{Path: "/bin/codex"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.stopStart:
	default:
		t.Fatal("server stop was not attempted")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled server stop exceeded bound: %s", elapsed)
	}
}

func TestRunProcessBoundsOutputAndCallbackFailures(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	callbackErr := errors.New("session callback failed")
	_, err := RunProcess(context.Background(), ProcessSpec{
		Path: "/bin/sh", Args: []string{"-c", "printf 'line\\n'; sleep 1"},
		StdoutLine: func([]byte) error { return callbackErr }, MaxOutputBytes: 64,
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("callback error=%v", err)
	}
	_, err = RunProcess(context.Background(), ProcessSpec{
		Path: "/bin/sh", Args: []string{"-c", "printf '123456789'"}, MaxOutputBytes: 4,
	})
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("output limit error=%v", err)
	}
}

func TestPXPipeRetriesPortAndLeavesModelEligibilityToHelper(t *testing.T) {
	// These are helper-owned model settings. RoleMux passes the exact model and
	// never turns this fixture's enabled/disabled result into a static allowlist.
	modelCases := []struct {
		name       string
		models     string
		fakeResult string
	}{
		{name: "enabled", models: "exact/model", fakeResult: "image"},
		{name: "disabled", models: "off", fakeResult: "text"},
		{name: "unknown", models: "other/model", fakeResult: "text"},
	}
	for _, modelCase := range modelCases {
		t.Run(modelCase.name, func(t *testing.T) {
			dir := t.TempDir()
			pxpipe := filepath.Join(dir, "pxpipe")
			if err := os.WriteFile(pxpipe, []byte("#!/bin/sh\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			server := &fakePXPipeServer{done: make(chan struct{})}
			var attempts int
			var serverSpec PXPipeServerSpec
			launcher := &PXPipeTaskLauncher{
				Path: pxpipe, MaxAttempts: 2,
				CapabilityCheck: func(context.Context, ProcessSpec) error { return nil },
				PortChooser:     func(context.Context) ([]int, error) { return []int{43212, 43213}, nil },
				ServerFactory: func(_ context.Context, spec PXPipeServerSpec) (PXPipeServer, error) {
					attempts++
					if attempts == 1 {
						return nil, errors.New("occupied candidate port")
					}
					serverSpec = spec
					return server, nil
				},
				Process: func(_ context.Context, spec ProcessSpec) (ProcessResult, error) {
					if !containsArg(spec.Args, "exact/model") {
						t.Fatalf("exact model was changed: %#v", spec.Args)
					}
					return ProcessResult{Stdout: codexTaskOutput("model-session", RolePlanner)}, nil
				},
			}
			_, err := launcher.Launch(context.Background(), PXPipeLaunchSpec{
				PXPipePath: pxpipe,
				Provider:   ProcessSpec{Path: "/bin/codex", Args: []string{"--model", "exact/model"}, Env: []string{"PXPIPE_MODELS=" + modelCase.models}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if attempts != 2 || serverSpec.Port != 43213 {
				t.Fatalf("port retry attempts=%d spec=%#v", attempts, serverSpec)
			}
			if environmentValue(serverSpec.Env, "PXPIPE_MODELS") != modelCase.models {
				t.Fatalf("helper model configuration was changed: %#v", serverSpec.Env)
			}
			fakeTransformation := "text"
			if modelCase.models == "exact/model" {
				fakeTransformation = "image"
			}
			if fakeTransformation != modelCase.fakeResult {
				t.Fatalf("test fixture transformation=%q want=%q", fakeTransformation, modelCase.fakeResult)
			}
		})
	}
}

func TestPXPipeTaskLauncherSubprocessAndHTTPFixture(t *testing.T) {
	upstreamRequests := make(chan string, 1)
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		upstreamRequests <- strings.Join([]string{request.Method, request.URL.RequestURI(), request.Header.Get("Authorization"), string(body)}, "\x00")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fake-upstream"}`)
	})
	upstreamListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("sandbox does not permit local HTTP listeners: %v", err)
	}
	upstreamServer := &http.Server{Handler: upstreamHandler}
	go func() { _ = upstreamServer.Serve(upstreamListener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = upstreamServer.Shutdown(shutdown)
	}()
	upstreamURL := "http://" + upstreamListener.Addr().String()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	fakePath, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	providerEnv := []string{
		fakePXPipeEnv + "=1",
		"FAKE_PROVIDER_SECRET=provider-secret",
		"FAKE_AUTH=Bearer chatgpt-session",
		"FAKE_UPSTREAM_URL=" + upstreamURL,
		"PXPIPE_LOG=" + logPath,
		"PXPIPE_MODELS=exact/model",
		"CODEX_HOME=/tmp/codex-home",
	}
	var session string
	launcher := &PXPipeTaskLauncher{
		Path:        fakePath,
		MaxAttempts: 1,
		PortChooser: func(context.Context) ([]int, error) { return []int{port}, nil },
	}
	result, err := launcher.Launch(context.Background(), PXPipeLaunchSpec{
		PXPipePath: fakePath,
		Provider: ProcessSpec{
			Path: "/bin/codex", Args: []string{"-C", "/repo", "-s", "workspace-write", "--model", "exact/model"},
			Dir: t.TempDir(), Env: providerEnv, Stdin: "prompt", MaxOutputBytes: 1 << 20,
			StdoutLine: func(line []byte) error {
				if id, parseErr := codexSessionFromLine(line); parseErr == nil && id != "" {
					session = id
				}
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Stdout), "integration-session") || session != "integration-session" {
		t.Fatalf("subprocess output/session missing: %q / %q", result.Stdout, session)
	}
	select {
	case request := <-upstreamRequests:
		parts := strings.Split(request, "\x00")
		if len(parts) != 4 || parts[0] != http.MethodPost || parts[1] != "/backend-api/codex/responses?stream=true" || parts[2] != "Bearer chatgpt-session" || !strings.Contains(parts[3], `"model":"exact/model"`) {
			t.Fatalf("unexpected local upstream request: %q", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake warp did not reach the local upstream")
	}
}

func TestCodexPXPipeSubprocessResumeAndFakeEligibilityEvents(t *testing.T) {
	upstreamRequests := make(chan string, 8)
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		select {
		case upstreamRequests <- strings.Join([]string{request.Method, request.URL.RequestURI(), request.Header.Get("Authorization"), string(body)}, "\x00"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fake-upstream"}`)
	})
	upstreamListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("sandbox does not permit local HTTP listeners: %v", err)
	}
	upstreamServer := &http.Server{Handler: upstreamHandler}
	go func() { _ = upstreamServer.Serve(upstreamListener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = upstreamServer.Shutdown(shutdown)
	}()

	fakePath, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		models     string
		compressed bool
	}{
		{name: "enabled model uses fake image arm", models: "exact/model", compressed: true},
		{name: "disabled model uses fake text arm", models: "off", compressed: false},
		{name: "unknown model uses fake text arm", models: "other/model", compressed: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "events.jsonl")
			providerEnv := []string{
				fakePXPipeEnv + "=1",
				"FAKE_PROVIDER_SECRET=provider-secret",
				"FAKE_AUTH=Bearer chatgpt-session",
				"FAKE_UPSTREAM_URL=http://" + upstreamListener.Addr().String(),
				"PXPIPE_LOG=" + logPath,
				"PXPIPE_MODELS=" + test.models,
				"CODEX_HOME=" + filepath.Join(t.TempDir(), "codex-home"),
				"PATH=/bin",
			}
			adapter := &Codex{
				Path: "/bin/codex", Process: RunProcess, Env: providerEnv, PXPipePath: fakePath,
				AuthProbe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
					return CodexAuthEvidence{Mode: CodexAuthChatGPT}, nil
				},
			}
			request := Request{
				Role: RolePlanner, RepoRoot: t.TempDir(), Model: "exact/model", Prompt: "prompt",
				Runtime: task.RuntimeSnapshot{ProviderType: "codex"},
			}
			first, err := adapter.Run(context.Background(), request, Callbacks{})
			if err != nil {
				t.Fatal(err)
			}
			request.Resume, request.SessionID = true, first.SessionID
			second, err := adapter.Run(context.Background(), request, Callbacks{})
			if err != nil {
				t.Fatal(err)
			}
			if first.SessionID != "integration-session" || second.SessionID != first.SessionID {
				t.Fatalf("session continuity failed: first=%q second=%q", first.SessionID, second.SessionID)
			}
			file, err := os.Open(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var events []struct {
				Status     int  `json:"status"`
				Compressed bool `json:"compressed"`
				Input      struct {
					Mode string `json:"mode"`
				} `json:"input"`
			}
			decoder := json.NewDecoder(file)
			for {
				var event struct {
					Status     int  `json:"status"`
					Compressed bool `json:"compressed"`
					Input      struct {
						Mode string `json:"mode"`
					} `json:"input"`
				}
				if decodeErr := decoder.Decode(&event); errors.Is(decodeErr, io.EOF) {
					break
				} else if decodeErr != nil {
					t.Fatal(decodeErr)
				}
				events = append(events, event)
			}
			if len(events) != 2 {
				t.Fatalf("want one pxpipe event per resumed turn, got %#v", events)
			}
			for _, event := range events {
				if event.Status != http.StatusOK || event.Compressed != test.compressed {
					t.Fatalf("fake event=%#v want status 200 compressed=%t", event, test.compressed)
				}
				wantMode := "text"
				if test.compressed {
					wantMode = "image"
				}
				if event.Input.Mode != wantMode {
					t.Fatalf("fake event mode=%q want=%q", event.Input.Mode, wantMode)
				}
			}
		})
	}
	select {
	case <-upstreamRequests:
	default:
		t.Fatal("fake Responses requests did not reach the local upstream")
	}
}

func TestPXPipeReadinessPollsAfterSingleListeningLine(t *testing.T) {
	readinessCalls := 0
	server := &localPXPipeServer{
		done:    make(chan struct{}),
		lines:   make(chan string, 1),
		port:    43211,
		timeout: 500 * time.Millisecond,
		readiness: func(context.Context, string) error {
			readinessCalls++
			if readinessCalls < 2 {
				return errors.New("upstream is not ready yet")
			}
			return nil
		},
	}
	server.lines <- "[pxpipe] listening on http://127.0.0.1:43211"
	if err := server.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readinessCalls < 2 {
		t.Fatalf("readiness was not retried independently of helper logs: %d", readinessCalls)
	}
}

func TestCodexCancellationDoesNotFallbackAfterAuthProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	c := &Codex{Path: "/bin/codex", PXPipePath: "/bin/pxpipe", Process: func(ctx context.Context, _ ProcessSpec) (ProcessResult, error) {
		calls++
		<-ctx.Done()
		return ProcessResult{}, ctx.Err()
	}, AuthProbe: func(context.Context, string, []string) (CodexAuthEvidence, error) {
		cancel()
		return CodexAuthEvidence{Mode: CodexAuthChatGPT}, nil
	}, TaskLauncher: &captureCodexLauncher{}}
	_, err := c.Run(ctx, Request{Role: RolePlanner, RepoRoot: "/repo", Model: "model"}, Callbacks{})
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("err=%v direct calls=%d", err, calls)
	}
}

func TestPXPipeServerEnvironmentIsDeterministic(t *testing.T) {
	got := PXPipeServerEnvironment([]string{"Z=last", "OPENAI_API_KEY=secret", "PXPIPE_LOG=/tmp/events", "PXPIPE_MODELS=model", "CODEX_HOME=/tmp/codex", "HOST=bad", "PORT=1", "Z=final"})
	want := []string{"CODEX_HOME=/tmp/codex", "HOST=127.0.0.1", "OPENAI_UPSTREAM=" + codexChatGPTOrigin, "PXPIPE_LOG=/tmp/events", "PXPIPE_MODELS=model", "Z=final"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

func TestClaudePXPipeServerEnvironmentIsDeterministic(t *testing.T) {
	got := ClaudePXPipeServerEnvironment([]string{"Z=last", "ANTHROPIC_API_KEY=secret", "CLAUDE_CODE_OAUTH_TOKEN=secret", "ANTHROPIC_BASE_URL=https://gateway.invalid", "PXPIPE_LOG=/tmp/events", "PXPIPE_MODELS=model", "CLAUDE_CONFIG_DIR=/tmp/claude", "HOST=bad", "PORT=1", "Z=final"})
	want := []string{"ANTHROPIC_UPSTREAM=" + claudeAPIBaseURL, "CLAUDE_CONFIG_DIR=/tmp/claude", "HOST=127.0.0.1", "PXPIPE_LOG=/tmp/events", "PXPIPE_MODELS=model", "Z=final"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

func TestPXPipeTaskLauncherHonorsCancellationBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	launcher := &PXPipeTaskLauncher{Path: "/bin/sh", PortChooser: func(context.Context) ([]int, error) {
		t.Fatal("port chooser ran after cancellation")
		return nil, nil
	}}
	_, err := launcher.Launch(ctx, PXPipeLaunchSpec{Provider: ProcessSpec{Path: "/bin/codex"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
