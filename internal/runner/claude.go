package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
)

var claudeReadTools = []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch"}

type Claude struct {
	Path    string
	Process ProcessFunc
	Env     []string
	Custom  []ModelInfo
}

func NewClaude(path string) (*Claude, error) {
	var resolved string
	var err error
	if path != "" {
		resolved, err = resolveExplicitExecutable(path)
	} else {
		resolved, err = ResolveExecutable("CLAUDE_CLI_PATH", "claude", os.Environ())
	}
	if err != nil {
		return nil, err
	}
	return &Claude{Path: resolved, Process: RunProcess, Env: SanitizedEnv(os.Environ())}, nil
}

func (c *Claude) Run(ctx context.Context, req Request, callbacks Callbacks) (Response, error) {
	if c == nil || c.Path == "" || c.Process == nil {
		return Response{}, providerError("CLAUDE_UNAVAILABLE", "claude executable is not configured", false, false, "", nil)
	}
	if req.RepoRoot == "" {
		return Response{}, providerError("CLAUDE_REPO", "repository root is required", false, false, "", nil)
	}
	if req.Resume && req.SessionID == "" {
		return Response{}, providerError("CLAUDE_SESSION", "resume requires a session ID", false, false, "", ErrMissingSession)
	}
	if !req.Resume && req.SessionID == "" {
		req.SessionID = newSessionID()
	}
	path, pathErr := executableForRequest(c.Path, req.Runtime.CLIPath)
	if pathErr != nil {
		return Response{}, providerError("CLAUDE_UNAVAILABLE", pathErr.Error(), false, false, req.SessionID, pathErr)
	}
	if !req.Resume && callbacks.SessionStarted != nil {
		// Claude's --session-id is preassigned. Persist it before starting so an
		// interruption can retry the exact same conversation.
		if err := callbacks.SessionStarted(req.SessionID); err != nil {
			return Response{}, providerError("CLAUDE_SESSION", "cannot persist preassigned session", false, false, req.SessionID, err)
		}
	}
	args, err := BuildClaudeArgs(req)
	if err != nil {
		return Response{}, err
	}
	env, envErr := runtimeEnvironmentMapped(c.Env, req.Runtime.AuthEnvRefs, stringMapSetting(req.Runtime.SDKSettings, "env_map"), "ANTHROPIC_BASE_URL", req.Runtime.Endpoint)
	if envErr != nil {
		return Response{}, providerError("CLAUDE_AUTH", envErr.Error(), false, false, req.SessionID, envErr)
	}
	result, processErr := c.Process(ctx, ProcessSpec{Path: path, Args: args, Dir: req.RepoRoot, Env: env, Stdin: req.Prompt, MaxOutputBytes: req.MaxOutputBytes})
	outerID, nested, model, effort, parseErr := parseClaudeResult(result.Stdout, req.SessionID, req.Role)
	usage := usageFromJSONDocument(result.Stdout, false)
	response := Response{SessionID: outerID, Text: string(nested), ReportedModel: model, ReportedEffort: effort, Usage: usage}
	known := req.Resume || outerID != ""
	if processErr != nil {
		return response, providerProcessError("claude", processErr, known, outerIDOr(outerID, req.SessionID))
	}
	if parseErr != nil {
		return response, providerError("CLAUDE_OUTPUT", parseErr.Error(), known, known, outerIDOr(outerID, req.SessionID), parseErr)
	}
	if outerID == "" || outerID != req.SessionID {
		return response, providerError("CLAUDE_SESSION_MISMATCH", "claude result session_id did not match requested session", false, known, outerID, nil)
	}
	response.Raw, response.Envelope = result.Stdout, envelopePtr(nested, req.Role)
	return response, nil
}

func envelopePtr(nested []byte, role Role) *Envelope {
	env, err := DecodeEnvelope(nested, role)
	if err != nil {
		return nil
	}
	return &env
}

func BuildClaudeArgs(req Request) ([]string, error) {
	if req.Model == "" {
		return nil, errors.New("claude model is required")
	}
	if req.Resume && req.SessionID == "" {
		return nil, errors.New("claude resume session is required")
	}
	tools := append([]string(nil), claudeReadTools...)
	if req.Role == RoleImplementer {
		tools = append(tools, "Edit", "Write")
	}
	toolList := strings.Join(tools, ",")
	args := []string{"--print", "--output-format", "json", "--input-format", "text", "--safe-mode", "--restricted", "--permission-mode", "dontAsk", "--permission-prompts", "none", "--tools", toolList, "--allowed-tools", toolList, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
	if req.Resume {
		args = append(args, "--resume", req.SessionID)
	} else {
		id := req.SessionID
		if id == "" {
			id = newSessionID()
		}
		args = append(args, "--session-id", id)
	}
	args = append(args, "--json-schema", NativeSchema(req.Role), "--model", req.Model)
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	return args, nil
}

func parseClaudeResult(data []byte, expectedSession string, role Role) (session string, nested []byte, model, effort string, err error) {
	var outer map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if e := dec.Decode(&outer); e != nil {
		return "", nil, "", "", fmt.Errorf("%w: invalid Claude outer JSON: %v", ErrInvalidEnvelope, e)
	}
	var extra any
	if e := dec.Decode(&extra); e != nil && !errors.Is(e, io.EOF) {
		return "", nil, "", "", fmt.Errorf("%w: multiple Claude result values", ErrInvalidEnvelope)
	} else if e == nil {
		return "", nil, "", "", fmt.Errorf("%w: multiple Claude result values", ErrInvalidEnvelope)
	}
	if raw, ok := outer["session_id"]; ok {
		if e := json.Unmarshal(raw, &session); e != nil {
			return "", nil, "", "", fmt.Errorf("%w: bad session_id", ErrInvalidEnvelope)
		}
	}
	if raw, ok := outer["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	if raw, ok := outer["effort"]; ok {
		_ = json.Unmarshal(raw, &effort)
	}
	raw, ok := outer["structured_output"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return session, nil, model, effort, fmt.Errorf("%w: Claude outer result lacks structured_output", ErrInvalidEnvelope)
	}
	// Strictly validate the nested envelope; return the original canonical
	// bytes to preserve exactly what the provider supplied to workflow.
	if _, e := DecodeEnvelope(raw, role); e != nil {
		return session, nil, model, effort, e
	}
	return session, append([]byte(nil), raw...), model, effort, nil
}

func outerIDOr(got, fallback string) string {
	if got != "" {
		return got
	}
	return fallback
}

func newSessionID() string { return uuid.New().String() }

func (c *Claude) Version(ctx context.Context) (string, error) {
	r, err := c.Process(ctx, ProcessSpec{Path: c.Path, Args: []string{"--version"}, Env: c.Env, MaxOutputBytes: 64 << 10})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(r.Stdout)), nil
}
func (c *Claude) Auth(ctx context.Context) (AuthStatus, error) {
	v, err := c.Version(ctx)
	if err != nil {
		return AuthStatus{}, err
	}
	r, err := c.Process(ctx, ProcessSpec{Path: c.Path, Args: []string{"auth", "status"}, Env: c.Env, MaxOutputBytes: 256 << 10})
	var status struct {
		Authenticated *bool  `json:"authenticated"`
		LoggedIn      *bool  `json:"loggedIn"`
		AccountID     string `json:"accountId"`
		Email         string `json:"email"`
		Login         string `json:"login"`
	}
	decodeErr := json.Unmarshal(r.Stdout, &status)
	account := status.AccountID
	if account == "" {
		account = status.Email
	}
	if account == "" {
		account = status.Login
	}
	if status.Authenticated != nil {
		message := ""
		if !*status.Authenticated {
			message = "run claude auth login"
		}
		return AuthStatus{Version: v, Authenticated: *status.Authenticated, Account: account, Message: message}, nil
	}
	if status.LoggedIn != nil {
		message := ""
		if !*status.LoggedIn {
			message = "run claude auth login"
		}
		return AuthStatus{Version: v, Authenticated: *status.LoggedIn, Account: account, Message: message}, nil
	}
	if err != nil || decodeErr != nil {
		return AuthStatus{Version: v, Message: "run claude auth status"}, providerError("CLAUDE_AUTH", "Claude authentication status is unavailable", false, false, "", err)
	}
	return AuthStatus{Version: v, Authenticated: true, Account: account}, nil
}

func (c *Claude) ListModels(context.Context, ModelListRequest) (ModelPage, error) {
	models := []ModelInfo{
		{ID: "claude-sonnet", Label: "Claude Sonnet", Provider: "claude", Origin: "local", Availability: "unknown", Aliases: []string{"sonnet"}},
		{ID: "claude-opus", Label: "Claude Opus", Provider: "claude", Origin: "local", Availability: "unknown", Aliases: []string{"opus"}},
		{ID: "claude-haiku", Label: "Claude Haiku", Provider: "claude", Origin: "local", Availability: "unknown", Aliases: []string{"haiku"}},
		{ID: "claude-fable-5", Label: "Claude Fable 5", Provider: "claude", Origin: "custom", Availability: "unknown", Aliases: []string{"fable"}},
	}
	models = append(models, c.Custom...)
	return ModelPage{Models: models}, nil
}

var _ Adapter = (*Claude)(nil)
