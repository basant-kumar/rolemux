package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/basant-kumar/rolemux/internal/task"
)

type Codex struct {
	Path    string
	Process ProcessFunc
	Env     []string
	// ModelPages is a deterministic seam for discovery tests. Production uses
	// the app-server model/list protocol when it is nil.
	ModelPages func(context.Context, task.RuntimeSnapshot) ([]ModelInfo, string, string, error)
}

func NewCodex(path string) (*Codex, error) {
	var resolved string
	var err error
	if path != "" {
		resolved, err = resolveExplicitExecutable(path)
	} else {
		resolved, err = ResolveExecutable("CODEX_CLI_PATH", "codex", os.Environ())
	}
	if err != nil {
		return nil, err
	}
	return &Codex{Path: resolved, Process: RunProcess, Env: SanitizedEnv(os.Environ())}, nil
}

func (c *Codex) Run(ctx context.Context, req Request, callbacks Callbacks) (Response, error) {
	if c == nil || c.Path == "" || c.Process == nil {
		return Response{}, providerError("CODEX_UNAVAILABLE", "codex executable is not configured", false, false, "", nil)
	}
	if req.RepoRoot == "" {
		return Response{}, providerError("CODEX_REPO", "repository root is required", false, false, "", nil)
	}
	if strings.TrimSpace(req.Model) == "" {
		return Response{}, providerError("CODEX_MODEL", "codex model is required", false, false, "", nil)
	}
	path, pathErr := executableForRequest(c.Path, req.Runtime.CLIPath)
	if pathErr != nil {
		return Response{}, providerError("CODEX_UNAVAILABLE", pathErr.Error(), false, false, "", pathErr)
	}
	if req.Resume && req.SessionID == "" {
		return Response{}, providerError("CODEX_SESSION", "resume requires a session ID", false, false, "", ErrMissingSession)
	}
	_, schemaName, err := createSchemaFile(NativeSchema(req.Role))
	if err != nil {
		return Response{}, providerError("CODEX_SCHEMA", "cannot create output schema", false, false, "", err)
	}
	defer os.Remove(schemaName)
	args, err := BuildCodexArgs(req, schemaName)
	if err != nil {
		return Response{}, err
	}
	env, envErr := runtimeEnvironment(c.Env, req.Runtime.AuthEnvRefs, "", "")
	if envErr != nil {
		return Response{}, providerError("CODEX_AUTH", envErr.Error(), false, false, "", envErr)
	}
	result, processErr := c.Process(ctx, ProcessSpec{Path: path, Args: args, Dir: req.RepoRoot, Env: env, Stdin: req.Prompt, MaxOutputBytes: req.MaxOutputBytes})
	threadID, text, reportedModel, reportedEffort, parseErr := parseCodexOutput(result.Stdout, req.Role, callbacks)
	usage := usageFromJSONLines(result.Stdout, true)
	response := Response{SessionID: threadID, Text: text, ReportedModel: reportedModel, ReportedEffort: reportedEffort, Usage: usage}
	known := threadID != ""
	if req.Resume && threadID == "" {
		threadID = req.SessionID
		known = true
		response.SessionID = threadID
	}
	if req.Resume && known && threadID != req.SessionID {
		return response, providerError("CODEX_SESSION_MISMATCH", "codex resumed a different session", false, true, threadID, nil)
	}
	if processErr != nil {
		return response, providerProcessError("codex", processErr, known, threadID)
	}
	if parseErr != nil {
		return response, providerError("CODEX_OUTPUT", parseErr.Error(), known, known, threadID, parseErr)
	}
	if !req.Resume && threadID == "" {
		return response, providerError("CODEX_NO_THREAD", "fresh codex turn did not emit thread.started", false, false, "", ErrMissingSession)
	}
	if strings.TrimSpace(text) == "" {
		return response, providerError("CODEX_NO_ENVELOPE", "codex produced no structured response", known, known, threadID, ErrInvalidEnvelope)
	}
	if reportedModel != "" && reportedModel != req.Model {
		return response, providerError("CODEX_MODEL_MISMATCH", "codex reported a different model than requested", false, known, threadID, nil)
	}
	if req.Effort != "" && reportedEffort != "" && reportedEffort != req.Effort {
		return response, providerError("CODEX_EFFORT_MISMATCH", "codex reported a different reasoning effort than requested", false, known, threadID, nil)
	}
	envelope, err := DecodeEnvelope([]byte(text), req.Role)
	if err != nil {
		return response, providerError("CODEX_ENVELOPE", err.Error(), known, known, threadID, err)
	}
	response.Envelope, response.Raw = &envelope, result.Stdout
	return response, nil
}

func (c *Codex) childEnv(req Request) []string {
	env := c.Env
	if len(env) == 0 {
		env = SanitizedEnv(os.Environ())
	}
	// CODEX_HOME is intentionally retained for auth/session state; no value is
	// copied into argv or durable state.
	return env
}

func BuildCodexArgs(req Request, schemaPath string) ([]string, error) {
	if req.RepoRoot == "" || schemaPath == "" {
		return nil, errors.New("codex repository and schema path are required")
	}
	mode := req.Sandbox
	if mode == "" {
		mode = "read-only"
	}
	args := []string{"-C", req.RepoRoot, "-s", mode, "-a", "never", "--search", "exec"}
	if req.Resume {
		args = append(args, "resume")
	}
	args = append(args, "--ignore-user-config", "--ignore-rules", "--json")
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	args = append(args, "--output-schema", schemaPath)
	routing, err := CodexConfigOverrides(req.Runtime)
	if err != nil {
		return nil, providerError("CODEX_ROUTING", err.Error(), false, false, "", err)
	}
	for i := 0; i+1 < len(routing); i += 2 {
		if req.Effort != "" && strings.HasPrefix(routing[i+1], "model_reasoning_effort=") {
			continue
		}
		args = append(args, routing[i], routing[i+1])
	}
	if req.Effort != "" {
		args = append(args, "--config", "model_reasoning_effort="+req.Effort)
	}
	if req.Resume {
		args = append(args, req.SessionID)
	}
	args = append(args, "-")
	return args, nil
}

func createSchemaFile(schema string) ([]byte, string, error) {
	f, err := os.CreateTemp("", "rolemux-codex-schema-*.json")
	if err != nil {
		return nil, "", err
	}
	name := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if err = f.Chmod(0o600); err == nil {
		_, err = f.WriteString(schema)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, name, err
	}
	return []byte(schema), name, nil
}

func parseCodexOutput(data []byte, role Role, callbacks Callbacks) (sessionID, text, model, effort string, parseErr error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	// JSON events can contain large structured envelopes; keep this separate
	// from the process byte limit while retaining a per-line bound.
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var lastRaw json.RawMessage
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return sessionID, text, model, effort, fmt.Errorf("invalid codex JSON event: %w", err)
		}
		typ, _ := event["type"].(string)
		if typ == "thread.started" {
			previous := sessionID
			if value, ok := event["thread_id"].(string); ok {
				sessionID = strings.TrimSpace(value)
			}
			if sessionID == "" {
				if value, ok := event["threadId"].(string); ok {
					sessionID = strings.TrimSpace(value)
				}
			}
			if previous != "" && sessionID != previous {
				return sessionID, text, model, effort, errors.New("codex emitted conflicting thread IDs")
			}
			if previous == "" && sessionID != "" && callbacks.SessionStarted != nil {
				if err := callbacks.SessionStarted(sessionID); err != nil {
					return sessionID, text, model, effort, err
				}
			}
		}
		if value, ok := event["model"].(string); ok {
			model = value
		}
		if value, ok := event["effort"].(string); ok {
			effort = value
		}
		if callbacks.Event != nil {
			raw, _ := json.Marshal(event)
			if err := callbacks.Event(Event{Type: typ, SessionID: sessionID, Model: model, Effort: effort, Raw: raw}); err != nil {
				return sessionID, text, model, effort, err
			}
		}
		if typ == "item.completed" {
			if item, ok := event["item"].(map[string]any); ok {
				if value, ok := item["text"].(string); ok {
					text = value
				}
				if value, ok := item["content"].(string); ok {
					text = value
				}
			}
		}
		if value, ok := event["structured_output"].(map[string]any); ok {
			lastRaw, _ = json.Marshal(value)
		}
		if typ == "response.completed" || typ == "turn.completed" {
			if value, ok := event["text"].(string); ok {
				text = value
			}
		}
		if _, ok := event["role"]; ok {
			lastRaw, _ = json.Marshal(event)
		}
	}
	if err := scanner.Err(); err != nil {
		return sessionID, text, model, effort, err
	}
	if strings.TrimSpace(text) == "" && len(lastRaw) > 0 {
		text = string(lastRaw)
	}
	return sessionID, text, model, effort, nil
}

func (c *Codex) Version(ctx context.Context) (string, error) {
	result, err := c.Process(ctx, ProcessSpec{Path: c.Path, Args: []string{"--version"}, Env: c.Env, MaxOutputBytes: 64 << 10})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

func (c *Codex) Auth(ctx context.Context) (AuthStatus, error) {
	version, err := c.Version(ctx)
	if err != nil {
		return AuthStatus{}, err
	}
	result, statusErr := c.Process(ctx, ProcessSpec{Path: c.Path, Args: []string{"login", "status"}, Env: c.Env, MaxOutputBytes: 256 << 10})
	message := strings.TrimSpace(string(result.Stdout))
	if message == "" {
		message = strings.TrimSpace(string(result.Stderr))
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "not logged in") || strings.Contains(lower, "not authenticated") {
		return AuthStatus{Version: version, Authenticated: false, Message: "run codex login"}, nil
	}
	if statusErr != nil {
		return AuthStatus{Version: version, Authenticated: false, Message: "run codex login"}, providerError("CODEX_AUTH", "Codex authentication status is unavailable", false, false, "", statusErr)
	}
	if !strings.Contains(lower, "logged in") {
		return AuthStatus{Version: version, Authenticated: false, Message: "unrecognized codex login status"}, providerError("CODEX_AUTH", "Codex returned an unrecognized login status", false, false, "", nil)
	}
	return AuthStatus{Version: version, Authenticated: true}, nil
}

// PaginateModelPages follows every non-empty cursor and detects cursor loops.
// The function is used by live Codex discovery and is independently testable.
func PaginateModelPages(ctx context.Context, first func(context.Context, string) (ModelPage, error)) ([]ModelInfo, string, error) {
	var all []ModelInfo
	cursor := ""
	seen := map[string]bool{}
	account, endpoint := "", ""
	for {
		if cursor != "" {
			if seen[cursor] {
				return nil, "", errors.New("codex model pagination cursor loop")
			}
			seen[cursor] = true
		}
		page, err := first(ctx, cursor)
		if err != nil {
			return nil, "", err
		}
		all = append(all, page.Models...)
		if page.Account != "" {
			account = page.Account
		}
		if page.Endpoint != "" {
			endpoint = page.Endpoint
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for i := range all {
		if all[i].Provider == "" {
			all[i].Provider = "codex"
		}
		if all[i].Origin == "" {
			all[i].Origin = "live"
		}
		if all[i].Availability == "" {
			all[i].Availability = "available"
		}
	}
	return all, account + "\x00" + endpoint, nil
}

func (c *Codex) ListModels(ctx context.Context, req ModelListRequest) (ModelPage, error) {
	if c.ModelPages != nil {
		models, account, endpoint, err := c.ModelPages(ctx, req.Runtime)
		return ModelPage{Models: models, Account: account, Endpoint: endpoint}, err
	}
	return c.listModelsAppServer(ctx, req.Runtime)
}

var _ Adapter = (*Codex)(nil)
