package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/basant-kumar/rolemux/internal/task"
)

func TestCodexArgumentPlacementFreshAndResume(t *testing.T) {
	req := Request{RepoRoot: "/repo", Sandbox: "workspace-write", Model: "gpt-5.6-luna", Effort: "max"}
	got, err := BuildCodexArgs(req, "/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-C", "/repo", "-s", "workspace-write", "-a", "never", "--search", "exec", "--ignore-user-config", "--ignore-rules", "--json", "--model", "gpt-5.6-luna", "--output-schema", "/schema.json", "--config", "model_reasoning_effort=max", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh args\n got: %#v\nwant: %#v", got, want)
	}
	req.Resume, req.SessionID = true, "thread-123"
	got, err = BuildCodexArgs(req, "/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"-C", "/repo", "-s", "workspace-write", "-a", "never", "--search", "exec", "resume", "--ignore-user-config", "--ignore-rules", "--json", "--model", "gpt-5.6-luna", "--output-schema", "/schema.json", "--config", "model_reasoning_effort=max", "thread-123", "-"}
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

func TestClaudeArgumentsAndNestedResultAreStrict(t *testing.T) {
	req := Request{Role: RoleImplementer, RepoRoot: "/repo", Model: "claude-fable-5", Effort: "max", SessionID: "123e4567-e89b-12d3-a456-426614174000"}
	args, err := BuildClaudeArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, required := range []string{"--safe-mode", "--restricted", "--permission-mode dontAsk", "--permission-prompts none", "Read,Glob,Grep,WebSearch,WebFetch,Edit,Write", "--strict-mcp-config", "--session-id " + req.SessionID, "--model claude-fable-5", "--effort max"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q in %s", required, joined)
		}
	}
	if strings.Contains(joined, "Bash") || strings.Contains(joined, "dangerously-skip") {
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
	for _, required := range []string{`"required":["role","status","plan","question"]`, `"additionalProperties":false`} {
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

func TestCopilotImplementerFailsClosedBeforeStartingSDK(t *testing.T) {
	c := &Copilot{}
	_, err := c.Run(context.Background(), Request{Role: RoleImplementer}, Callbacks{})
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code != "UNSUPPORTED_PROVIDER" || !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("unexpected error: %#v", err)
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
