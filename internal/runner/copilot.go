package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

const CopilotSDKVersion = "v1.0.13-preview.4"

type Copilot struct {
	Path               string
	BaseDirectory      string
	InteractiveProcess InteractiveProcessFunc
	Env                []string
	// Discover is a deterministic seam for catalog tests. Production uses the
	// pinned SDK's ListModels method.
	Discover func(context.Context) ([]copilot.ModelInfo, error)
}

func NewCopilot(path string) (*Copilot, error) {
	var resolved string
	var err error
	if path != "" {
		resolved, err = resolveExplicitExecutable(path)
	} else {
		resolved, err = ResolveExecutable("COPILOT_CLI_PATH", "copilot", os.Environ())
	}
	if err != nil {
		return nil, err
	}
	cache, cacheErr := os.UserCacheDir()
	if cacheErr != nil {
		return nil, cacheErr
	}
	base := filepath.Join(cache, "rolemux", "copilot")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	return &Copilot{Path: resolved, BaseDirectory: base, InteractiveProcess: RunInteractiveProcess, Env: SanitizedEnv(os.Environ())}, nil
}

func CopilotReadOnlyTools() []string {
	return copilot.NewToolSet().AddBuiltIn("view", "grep", "web_fetch").ToSlice()
}

func CopilotPermissionHandler(repoRoot string) copilot.PermissionHandlerFunc {
	return func(request copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		reject := func(message string) (rpc.PermissionDecision, error) {
			return &rpc.PermissionDecisionReject{Feedback: &message}, nil
		}
		switch r := request.(type) {
		case copilot.PermissionRequestRead:
			return decideCopilotRead(repoRoot, r.Path, r.RequiresManagedApproval(), boolValue(r.RequestSandboxBypass), reject)
		case *copilot.PermissionRequestRead:
			if r == nil {
				return reject("nil read permission request")
			}
			return decideCopilotRead(repoRoot, r.Path, r.RequiresManagedApproval(), boolValue(r.RequestSandboxBypass), reject)
		case copilot.PermissionRequestURL:
			return decideCopilotURL(r.URL, r.RequiresManagedApproval(), boolValue(r.RequestSandboxBypass), reject)
		case *copilot.PermissionRequestURL:
			if r == nil {
				return reject("nil URL permission request")
			}
			return decideCopilotURL(r.URL, r.RequiresManagedApproval(), boolValue(r.RequestSandboxBypass), reject)
		default:
			return reject("permission request kind is not in RoleMux's allowlist")
		}
	}
}

func decideCopilotRead(repoRoot, candidate string, managed, bypass bool, reject func(string) (rpc.PermissionDecision, error)) (rpc.PermissionDecision, error) {
	if managed || bypass {
		return reject("managed approval or sandbox bypass is not auto-approved")
	}
	if _, err := PathWithinRepo(repoRoot, candidate); err != nil {
		return reject("read path is outside the repository")
	}
	return &rpc.PermissionDecisionApproveOnce{}, nil
}

func decideCopilotURL(raw string, managed, bypass bool, reject func(string) (rpc.PermissionDecision, error)) (rpc.PermissionDecision, error) {
	if managed || bypass {
		return reject("managed approval or sandbox bypass is not auto-approved")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return reject("only userinfo-free HTTP(S) URLs are allowed")
	}
	return &rpc.PermissionDecisionApproveOnce{}, nil
}

func (c *Copilot) clientOptions(req Request) *copilot.ClientOptions {
	base := c.BaseDirectory
	if base == "" {
		base = filepath.Join(os.TempDir(), "rolemux-copilot")
	}
	path := c.Path
	if req.Runtime.CLIPath != "" {
		path, _ = executableForRequest(path, req.Runtime.CLIPath)
	}
	return &copilot.ClientOptions{
		Connection:       copilot.StdioConnection{Path: path, Env: c.Env},
		WorkingDirectory: req.RepoRoot,
		BaseDirectory:    base,
		Mode:             copilot.ModeEmpty,
		UseLoggedInUser:  copilot.Bool(true),
	}
}

func (c *Copilot) sessionConfig(req Request) *copilot.SessionConfig {
	return &copilot.SessionConfig{
		SessionID:                          req.SessionID,
		Model:                              req.Model,
		ReasoningEffort:                    req.Effort,
		WorkingDirectory:                   req.RepoRoot,
		AvailableTools:                     CopilotReadOnlyTools(),
		OnPermissionRequest:                CopilotPermissionHandler(req.RepoRoot),
		EnableConfigDiscovery:              copilot.Bool(false),
		EnableSkills:                       copilot.Bool(false),
		EnableFileHooks:                    copilot.Bool(false),
		EnableOnDemandInstructionDiscovery: copilot.Bool(false),
		EnableHostGitOperations:            copilot.Bool(false),
		EnableSessionStore:                 copilot.Bool(false),
		EnableSessionTelemetry:             copilot.Bool(false),
		SkipCustomInstructions:             copilot.Bool(true),
		CustomAgentsLocalOnly:              copilot.Bool(true),
		MCPServers:                         map[string]copilot.MCPServerConfig{},
		Provider:                           copilotProviderConfig(req),
	}
}

func (c *Copilot) resumeConfig(req Request) *copilot.ResumeSessionConfig {
	return &copilot.ResumeSessionConfig{
		Model:                              req.Model,
		ReasoningEffort:                    req.Effort,
		WorkingDirectory:                   req.RepoRoot,
		AvailableTools:                     CopilotReadOnlyTools(),
		OnPermissionRequest:                CopilotPermissionHandler(req.RepoRoot),
		EnableConfigDiscovery:              copilot.Bool(false),
		EnableSkills:                       copilot.Bool(false),
		EnableFileHooks:                    copilot.Bool(false),
		EnableOnDemandInstructionDiscovery: copilot.Bool(false),
		EnableHostGitOperations:            copilot.Bool(false),
		EnableSessionStore:                 copilot.Bool(false),
		EnableSessionTelemetry:             copilot.Bool(false),
		SkipCustomInstructions:             copilot.Bool(true),
		CustomAgentsLocalOnly:              copilot.Bool(true),
		MCPServers:                         map[string]copilot.MCPServerConfig{},
		Provider:                           copilotProviderConfigResume(req),
	}
}

// The SDK config is reconstructed from non-secret runtime metadata each turn.
// Credential callbacks are deliberately absent unless the host supplies one;
// a missing credential therefore fails in the SDK instead of falling back to a
// broader user configuration.
func copilotProviderConfig(req Request) *copilot.ProviderConfig {
	settings := req.Runtime.SDKSettings
	if len(settings) == 0 {
		return nil
	}
	p := &copilot.ProviderConfig{}
	p.Type = stringSetting(settings, "type")
	p.WireAPI = stringSetting(settings, "wire_api")
	p.Transport = stringSetting(settings, "transport")
	p.BaseURL = stringSetting(settings, "base_url")
	p.ModelID = stringSetting(settings, "model_id")
	p.WireModel = stringSetting(settings, "wire_model")
	p.MaxPromptTokens = intSetting(settings, "max_prompt_tokens")
	p.MaxOutputTokens = intSetting(settings, "max_output_tokens")
	p.Headers = stringMapSetting(settings, "headers")
	if envRef := stringSetting(settings, "bearer_token_env"); envRef != "" {
		p.BearerTokenProvider = func(copilot.ProviderTokenArgs) (string, error) {
			value := os.Getenv(envRef)
			if value == "" {
				return "", fmt.Errorf("missing credential environment %s", envRef)
			}
			return value, nil
		}
	}
	return p
}

func copilotProviderConfigResume(req Request) *copilot.ProviderConfig {
	return copilotProviderConfig(req)
}
func stringSetting(m map[string]any, key string) string {
	if value, ok := m[key].(string); ok {
		return value
	}
	return ""
}
func intSetting(m map[string]any, key string) int {
	switch value := m[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	}
	return 0
}
func stringMapSetting(m map[string]any, key string) map[string]string {
	result := map[string]string{}
	if values, ok := m[key].(map[string]string); ok {
		for k, v := range values {
			result[k] = v
		}
		return result
	}
	if values, ok := m[key].(map[string]any); ok {
		for k, v := range values {
			if s, ok := v.(string); ok {
				result[k] = s
			}
		}
	}
	return result
}

func (c *Copilot) Run(ctx context.Context, req Request, callbacks Callbacks) (Response, error) {
	if req.Role == RoleImplementer {
		return Response{}, providerError("UNSUPPORTED_PROVIDER", "Copilot implementation is fail-closed until write isolation is proven", false, false, "", ErrUnsupportedProvider)
	}
	if c == nil || c.Path == "" {
		return Response{}, providerError("COPILOT_UNAVAILABLE", "copilot executable is not configured", false, false, "", nil)
	}
	if _, err := executableForRequest(c.Path, req.Runtime.CLIPath); err != nil {
		return Response{}, providerError("COPILOT_UNAVAILABLE", err.Error(), false, false, "", err)
	}
	client := copilot.NewClient(c.clientOptions(req))
	if err := client.Start(ctx); err != nil {
		return Response{}, providerProcessError("copilot", err, false, "")
	}
	defer client.Stop()
	var session *copilot.Session
	var err error
	if req.Resume {
		if req.SessionID == "" {
			return Response{}, providerError("COPILOT_SESSION", "resume requires a session ID", false, false, "", ErrMissingSession)
		}
		session, err = client.ResumeSession(ctx, req.SessionID, c.resumeConfig(req))
	} else {
		session, err = client.CreateSession(ctx, c.sessionConfig(req))
	}
	if err != nil {
		return Response{}, providerProcessError("copilot", err, req.Resume, req.SessionID)
	}
	if session == nil || strings.TrimSpace(session.SessionID) == "" {
		return Response{}, providerError("COPILOT_NO_SESSION", "Copilot did not return a durable session ID", false, false, "", ErrMissingSession)
	}
	if callbacks.SessionStarted != nil {
		if err := callbacks.SessionStarted(session.SessionID); err != nil {
			return Response{}, providerError("COPILOT_SESSION", "cannot persist session ID", false, true, session.SessionID, err)
		}
	}
	var usageMu sync.Mutex
	var usage TokenUsage
	unsubscribe := session.On(func(event copilot.SessionEvent) {
		data, ok := event.Data.(*copilot.AssistantUsageData)
		if !ok || data == nil {
			return
		}
		turn := TokenUsage{InputTokens: int64Value(data.InputTokens), CachedInputTokens: int64Value(data.CacheReadTokens), CacheWriteTokens: int64Value(data.CacheWriteTokens), OutputTokens: int64Value(data.OutputTokens), ReasoningTokens: int64Value(data.ReasoningTokens)}
		turn.TotalTokens = turn.InputTokens + turn.OutputTokens
		usageMu.Lock()
		usage.Add(turn)
		usageMu.Unlock()
	})
	defer unsubscribe()
	usageSnapshot := func() TokenUsage {
		usageMu.Lock()
		defer usageMu.Unlock()
		return usage
	}
	result, err := session.SendAndWait(ctx, copilot.MessageOptions{Prompt: CopilotEnvelopePrompt(req.Role) + "\n\n" + req.Prompt})
	if err != nil {
		return Response{SessionID: session.SessionID, Usage: usageSnapshot()}, providerProcessError("copilot", err, true, session.SessionID)
	}
	if result == nil {
		return Response{SessionID: session.SessionID, Usage: usageSnapshot()}, providerError("COPILOT_OUTPUT", "Copilot produced no terminal assistant message", true, true, session.SessionID, ErrInvalidEnvelope)
	}
	data, ok := result.Data.(*copilot.AssistantMessageData)
	if !ok || data == nil {
		return Response{SessionID: session.SessionID, Usage: usageSnapshot()}, providerError("COPILOT_OUTPUT", "Copilot terminal event was not an assistant message", true, true, session.SessionID, ErrInvalidEnvelope)
	}
	env, err := DecodeEnvelope([]byte(data.Content), req.Role)
	if err != nil {
		return Response{SessionID: session.SessionID, Text: data.Content, Usage: usageSnapshot()}, providerError("COPILOT_ENVELOPE", err.Error(), true, true, session.SessionID, err)
	}
	response := Response{SessionID: session.SessionID, Text: data.Content, Envelope: &env, Usage: usageSnapshot()}
	if data.Model != nil {
		response.ReportedModel = *data.Model
	}
	return response, nil
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (c *Copilot) ListModels(ctx context.Context, req ModelListRequest) (ModelPage, error) {
	if _, err := executableForRequest(c.Path, req.Runtime.CLIPath); err != nil {
		return ModelPage{}, providerError("COPILOT_UNAVAILABLE", err.Error(), false, false, "", err)
	}
	var models []copilot.ModelInfo
	var err error
	if c.Discover != nil {
		models, err = c.Discover(ctx)
	} else {
		client := copilot.NewClient(c.clientOptions(Request{RepoRoot: "."}))
		if err = client.Start(ctx); err == nil {
			models, err = client.ListModels(ctx)
			_ = client.Stop()
		}
	}
	if err != nil {
		return ModelPage{}, err
	}
	page := ModelPage{Models: make([]ModelInfo, 0, len(models))}
	for _, model := range models {
		info := ModelInfo{ID: model.ID, Label: model.Name, Provider: "copilot", Origin: "live", Availability: "available", Efforts: append([]string(nil), model.SupportedReasoningEfforts...), DefaultEffort: model.DefaultReasoningEffort}
		page.Models = append(page.Models, info)
	}
	return page, nil
}

func (c *Copilot) Version(ctx context.Context) (string, error) {
	if c == nil || c.Path == "" {
		return "", errors.New("copilot executable is not configured")
	}
	result, err := RunProcess(ctx, ProcessSpec{Path: c.Path, Args: []string{"--version"}, Env: c.Env, MaxOutputBytes: 64 << 10})
	if err != nil {
		return "", err
	}
	cliVersion := strings.TrimSpace(string(result.Stdout))
	if cliVersion == "" {
		cliVersion = "unknown CLI version"
	}
	return cliVersion + "; sdk " + CopilotSDKVersion, nil
}
func (c *Copilot) Auth(ctx context.Context) (AuthStatus, error) {
	version, versionErr := c.Version(ctx)
	if versionErr != nil {
		return AuthStatus{}, versionErr
	}
	client := copilot.NewClient(c.clientOptions(Request{RepoRoot: "."}))
	if err := client.Start(ctx); err != nil {
		return AuthStatus{}, err
	}
	defer client.Stop()
	status, err := client.GetAuthStatus(ctx)
	if err != nil {
		return AuthStatus{}, err
	}
	account := deref(status.Login)
	if host := deref(status.Host); host != "" {
		account += "@" + host
	}
	return AuthStatus{Authenticated: status.IsAuthenticated, Version: version, Message: deref(status.StatusMessage), Account: account}, nil
}
func (c *Copilot) Login(ctx context.Context, req LoginRequest) error {
	if c == nil || c.Path == "" {
		return errors.New("copilot executable is not configured")
	}
	run := c.InteractiveProcess
	if run == nil {
		run = RunInteractiveProcess
	}
	return run(ctx, c.Path, []string{"login"}, req.RepoRoot, c.Env, req.Stdin, req.Stdout, req.Stderr)
}
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

var _ Adapter = (*Copilot)(nil)
var _ Authenticator = (*Copilot)(nil)
var _ = errors.Is
