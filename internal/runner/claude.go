package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/basant-kumar/rolemux/internal/task"
	"github.com/google/uuid"
)

var claudeReadTools = []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch", "Skill"}

const (
	claudeAPIHost    = "api.anthropic.com"
	claudeAPIBaseURL = "https://" + claudeAPIHost
)

type Claude struct {
	Path               string
	Process            ProcessFunc
	InteractiveProcess InteractiveProcessFunc
	Env                []string
	Custom             []ModelInfo
	PXPipePath         string
	TaskLauncher       PXPipeLauncher
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
	env := SanitizedEnv(os.Environ())
	return &Claude{Path: resolved, Process: RunProcess, InteractiveProcess: RunInteractiveProcess, Env: env, PXPipePath: DetectPXPipePath(env)}, nil
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
	spec := ProcessSpec{Path: path, Args: args, Dir: req.RepoRoot, Env: env, Stdin: req.Prompt, MaxOutputBytes: req.MaxOutputBytes}
	result, processErr := c.runClaudeTask(ctx, req, spec, callbacks)
	outerID, nested, model, effort, parseErr := parseClaudeResult(result.Stdout, req.SessionID, req.Role)
	usage := usageFromJSONDocument(result.Stdout, false)
	response := Response{SessionID: outerID, Text: string(nested), ReportedModel: model, ReportedEffort: effort, Usage: usage}
	// A fresh Claude session ID is assigned and persisted before launch. Once
	// the OS accepts the task process, an error must retry that exact ID rather
	// than risk replaying the turn in a new conversation.
	known := req.Resume || outerID != "" || result.ProcessStarted
	if processErr != nil {
		return response, providerProcessError("claude", processErr, known, outerIDOr(outerID, req.SessionID))
	}
	if parseErr != nil {
		return response, providerError("CLAUDE_OUTPUT", parseErr.Error(), known, known, outerIDOr(outerID, req.SessionID), parseErr)
	}
	if outerID == "" || outerID != req.SessionID {
		return response, providerError("CLAUDE_SESSION_MISMATCH", "claude result session_id did not match requested session", false, known, outerID, nil)
	}
	if selectionErr := VerifyReportedSelection("claude", req, response); selectionErr != nil {
		return response, selectionErr
	}
	response.Raw, response.Envelope = result.Stdout, envelopePtr(nested, req.Role)
	return response, nil
}

func (c *Claude) runClaudeTask(ctx context.Context, req Request, providerSpec ProcessSpec, callbacks Callbacks) (ProcessResult, error) {
	if err := ctx.Err(); err != nil {
		return ProcessResult{}, err
	}
	launcher := c.claudeTaskLauncher(providerSpec.Env)
	if launcher == nil {
		if callbacks.Diagnostic != nil && !req.Resume {
			callbacks.Diagnostic(missingPXPipeDiagnostic("Claude"))
		}
		return c.Process(ctx, providerSpec)
	}
	if !ClaudeFirstPartyRouteSupported(req.Runtime, providerSpec.Env) {
		callbacks.Diagnostic = notifyDiagnostic(callbacks.Diagnostic, "pxpipe: Claude route is not the supported first-party Anthropic route; running Claude directly")
		return c.Process(ctx, providerSpec)
	}
	launchSpec := PXPipeLaunchSpec{
		PXPipePath:         c.pxpipePath(providerSpec.Env),
		Provider:           providerSpec,
		ProviderName:       "Claude",
		ServerEnv:          ClaudePXPipeServerEnvironment(providerSpec.Env),
		EventsFile:         pxpipeEventsFile(providerSpec.Env),
		RoutePrefix:        pxpipeClaudeRoutePattern,
		TaskStartsOnLaunch: true,
		Diagnostic:         callbacks.Diagnostic,
	}
	result, launchErr := launcher.Launch(ctx, launchSpec)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var helperErr *PXPipeLaunchError
	if errors.As(launchErr, &helperErr) && helperErr.BeforeTask {
		callbacks.Diagnostic = notifyDiagnostic(callbacks.Diagnostic, "pxpipe: private helper launch failed before Claude started ("+boundedDiagnostic(helperErr.Error())+"); running Claude directly")
		return c.Process(ctx, providerSpec)
	}
	return result, launchErr
}

func (c *Claude) claudeTaskLauncher(env []string) PXPipeLauncher {
	if c == nil {
		return nil
	}
	if c.TaskLauncher != nil {
		if launcher, ok := c.TaskLauncher.(*PXPipeTaskLauncher); ok {
			if launcher.Path == "" {
				copy := *launcher
				copy.Path = c.pxpipePath(env)
				if copy.Path == "" && copy.ServerFactory == nil {
					return nil
				}
				if copy.ServerFactory == nil && !executableFile(copy.Path) {
					return nil
				}
				return &copy
			}
			if launcher.ServerFactory == nil && !executableFile(launcher.Path) {
				return nil
			}
		}
		return c.TaskLauncher
	}
	path := c.pxpipePath(env)
	if path == "" || !executableFile(path) {
		return nil
	}
	return &PXPipeTaskLauncher{Path: path, Process: c.Process}
}

func (c *Claude) pxpipePath(env []string) string {
	if c != nil && strings.TrimSpace(c.PXPipePath) != "" && executableFile(c.PXPipePath) {
		return c.PXPipePath
	}
	return DetectPXPipePath(env)
}

// ClaudeFirstPartyRouteSupported prevents pxpipe from silently replacing a
// configured gateway, Bedrock, Vertex, or Foundry transport.
func ClaudeFirstPartyRouteSupported(runtime task.RuntimeSnapshot, environ []string) bool {
	if runtime.ProviderType != "" && !strings.EqualFold(runtime.ProviderType, "claude") {
		return false
	}
	if runtime.Endpoint != "" && !equivalentAnthropicRoute(runtime.Endpoint) {
		return false
	}
	if runtime.ProviderID != "" || runtime.WireAPI != "" || len(runtime.Auth) > 0 {
		return false
	}
	for target := range stringMapSetting(runtime.SDKSettings, "env_map") {
		switch strings.ToUpper(target) {
		case "AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "CLOUD_ML_PROJECT_ID", "CLOUD_ML_REGION", "FOUNDRY_ENDPOINT", "FOUNDRY_API_KEY":
			return false
		}
	}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_URL"} {
		value, set, conflict := environmentRouteValue(environ, key)
		if conflict || set && value != "" && !equivalentAnthropicRoute(value) {
			return false
		}
	}
	for _, key := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		if enabledEnvironmentFlag(environ, key) {
			return false
		}
	}
	return true
}

func equivalentAnthropicRoute(value string) bool {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), claudeAPIHost) || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return strings.TrimRight(u.EscapedPath(), "/") == ""
}

func enabledEnvironmentFlag(environ []string, wanted string) bool {
	value := strings.ToLower(strings.TrimSpace(environmentValue(environ, wanted)))
	return value != "" && value != "0" && value != "false" && value != "no" && value != "off"
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
	if req.Speed != "" && req.Speed != "standard" && req.Speed != "fast" {
		return nil, fmt.Errorf("unsupported Claude speed %q", req.Speed)
	}
	tools := append([]string(nil), claudeReadTools...)
	if req.Role == RoleImplementer {
		tools = append(tools, "Edit", "Write")
	}
	toolList := strings.Join(tools, ",")
	args := []string{"--print", "--output-format", "json", "--input-format", "text", "--restricted", "--permission-mode", "dontAsk", "--permission-prompts", "none", "--tools", toolList, "--allowed-tools", toolList, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
	if req.Speed == "fast" {
		args = append(args, "--settings", `{"fastMode":true}`)
	} else if req.Speed == "standard" {
		args = append(args, "--settings", `{"fastMode":false}`)
	}
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

func (c *Claude) Login(ctx context.Context, req LoginRequest) error {
	if c == nil || c.Path == "" {
		return errors.New("claude executable is not configured")
	}
	run := c.InteractiveProcess
	if run == nil {
		run = RunInteractiveProcess
	}
	return run(ctx, c.Path, []string{"auth", "login"}, req.RepoRoot, c.Env, req.Stdin, req.Stdout, req.Stderr)
}

func (c *Claude) ListModels(ctx context.Context, req ModelListRequest) (ModelPage, error) {
	if c == nil || c.Path == "" || c.Process == nil {
		return ModelPage{}, providerError("CLAUDE_UNAVAILABLE", "claude executable is not configured", false, false, "", nil)
	}
	path, err := executableForRequest(c.Path, req.Runtime.CLIPath)
	if err != nil {
		return ModelPage{}, providerError("CLAUDE_UNAVAILABLE", err.Error(), false, false, "", err)
	}
	env, err := runtimeEnvironmentMapped(c.Env, req.Runtime.AuthEnvRefs, stringMapSetting(req.Runtime.SDKSettings, "env_map"), "ANTHROPIC_BASE_URL", req.Runtime.Endpoint)
	if err != nil {
		return ModelPage{}, providerError("CLAUDE_AUTH", err.Error(), false, false, "", err)
	}
	args := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--safe-mode", "--restricted", "--permission-mode", "plan", "--permission-prompts", "none", "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--setting-sources", "", "--no-session-persistence"}
	input := `{"type":"control_request","request_id":"rolemux-models","request":{"subtype":"initialize"}}` + "\n"
	discoveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, runErr := c.Process(discoveryCtx, ProcessSpec{Path: path, Args: args, Dir: ".", Env: env, Stdin: input, MaxOutputBytes: 8 << 20})
	if runErr != nil {
		return ModelPage{}, providerProcessError("claude", runErr, false, "")
	}
	page, parseErr := parseClaudeModelHandshake(result.Stdout)
	if parseErr != nil {
		return ModelPage{}, providerError("CLAUDE_MODEL_DISCOVERY", parseErr.Error(), false, false, "", parseErr)
	}
	page.Models = append(page.Models, c.Custom...)
	page.Endpoint = req.Runtime.Endpoint
	return page, nil
}

func parseClaudeModelHandshake(data []byte) (ModelPage, error) {
	type model struct {
		Value                 string   `json:"value"`
		ResolvedModel         string   `json:"resolvedModel"`
		DisplayName           string   `json:"displayName"`
		Description           string   `json:"description"`
		SupportsEffort        bool     `json:"supportsEffort"`
		SupportedEffortLevels []string `json:"supportedEffortLevels"`
		SupportsFastMode      bool     `json:"supportsFastMode"`
	}
	type payload struct {
		Models  []model `json:"models"`
		Account struct {
			Email        string `json:"email"`
			Organization string `json:"organization"`
		} `json:"account"`
	}
	type frame struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string  `json:"subtype"`
			RequestID string  `json:"request_id"`
			Response  payload `json:"response"`
			Error     string  `json:"error"`
		} `json:"response"`
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var message frame
		if json.Unmarshal([]byte(line), &message) != nil || message.Type != "control_response" || message.Response.RequestID != "rolemux-models" {
			continue
		}
		if message.Response.Subtype != "success" {
			return ModelPage{}, fmt.Errorf("Claude model handshake failed: %s", message.Response.Error)
		}
		models := make([]ModelInfo, 0, len(message.Response.Response.Models))
		for _, item := range message.Response.Response.Models {
			if item.Value == "" {
				continue
			}
			info := ModelInfo{ID: item.Value, Label: item.DisplayName, Description: item.Description, Provider: "claude", Origin: "live", Availability: "available", IsDefault: item.Value == "default"}
			if item.SupportsEffort {
				info.Efforts = append([]string(nil), item.SupportedEffortLevels...)
				for _, effort := range item.SupportedEffortLevels {
					info.EffortOptions = append(info.EffortOptions, ModelOption{ID: effort, Label: effort})
				}
			}
			if item.SupportsFastMode {
				info.SpeedOptions = []ModelOption{{ID: "fast", Label: "Fast", Description: "Provider fast mode; lower latency with higher usage"}}
				info.DefaultSpeed = "standard"
			}
			info.ContextWindowTokens = contextWindowFromModelValue(item.Value)
			if item.ResolvedModel != "" && item.ResolvedModel != item.Value {
				info.Aliases = []string{item.ResolvedModel}
			}
			models = append(models, info)
		}
		if len(models) == 0 {
			return ModelPage{}, errors.New("Claude model handshake returned no models")
		}
		account := message.Response.Response.Account.Email
		if account == "" {
			account = message.Response.Response.Account.Organization
		}
		return ModelPage{Models: models, Account: account}, nil
	}
	return ModelPage{}, errors.New("Claude model handshake returned no matching response")
}

func contextWindowFromModelValue(value string) int {
	value = strings.ToLower(strings.TrimSpace(value))
	start := strings.LastIndexByte(value, '[')
	if start < 0 || !strings.HasSuffix(value, "]") {
		return 0
	}
	amount := strings.TrimSuffix(value[start+1:], "]")
	if len(amount) < 2 {
		return 0
	}
	multiplier := 0
	switch amount[len(amount)-1] {
	case 'k':
		multiplier = 1_000
	case 'm':
		multiplier = 1_000_000
	default:
		return 0
	}
	number, err := strconv.Atoi(amount[:len(amount)-1])
	if err != nil || number <= 0 {
		return 0
	}
	return number * multiplier
}

var _ Adapter = (*Claude)(nil)
var _ Authenticator = (*Claude)(nil)
