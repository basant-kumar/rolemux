package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Antigravity adapts Google's `agy` CLI. The CLI owns authentication,
// conversations, model discovery, sandboxing, and effort validation.
type Antigravity struct {
	Path               string
	Process            ProcessFunc
	InteractiveProcess InteractiveProcessFunc
	Env                []string
}

func NewAntigravity(path string) (*Antigravity, error) {
	var resolved string
	var err error
	if path != "" {
		resolved, err = resolveExplicitExecutable(path)
	} else {
		resolved, err = ResolveExecutable("ANTIGRAVITY_CLI_PATH", "agy", os.Environ())
	}
	if err != nil {
		return nil, err
	}
	return &Antigravity{Path: resolved, Process: RunProcess, InteractiveProcess: RunInteractiveProcess, Env: SanitizedEnv(os.Environ())}, nil
}

func (a *Antigravity) SupportsRole(Role) error { return nil }

func BuildAntigravityArgs(req Request) ([]string, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("Antigravity model is required")
	}
	if req.Resume && strings.TrimSpace(req.SessionID) == "" {
		return nil, errors.New("Antigravity resume conversation is required")
	}
	if req.Effort != "" && req.Effort != "low" && req.Effort != "medium" && req.Effort != "high" {
		return nil, fmt.Errorf("unsupported Antigravity effort %q", req.Effort)
	}
	if req.Speed != "" && req.Speed != "standard" {
		return nil, fmt.Errorf("Antigravity model %q does not advertise speed modes", req.Model)
	}
	mode := "plan"
	if req.Role == RoleImplementer {
		mode = "accept-edits"
	}
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--json-schema", NativeSchema(req.Role), "--model", req.Model, "--mode", mode, "--sandbox"}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.Resume {
		args = append(args, "--conversation", req.SessionID)
	}
	return args, nil
}

func (a *Antigravity) Run(ctx context.Context, req Request, callbacks Callbacks) (Response, error) {
	if a == nil || a.Path == "" || a.Process == nil {
		return Response{}, providerError("ANTIGRAVITY_UNAVAILABLE", "Antigravity executable is not configured", false, false, "", nil)
	}
	if req.RepoRoot == "" {
		return Response{}, providerError("ANTIGRAVITY_REPO", "repository root is required", false, false, "", nil)
	}
	path, err := executableForRequest(a.Path, req.Runtime.CLIPath)
	if err != nil {
		return Response{}, providerError("ANTIGRAVITY_UNAVAILABLE", err.Error(), false, false, "", err)
	}
	args, err := BuildAntigravityArgs(req)
	if err != nil {
		return Response{}, providerError("ANTIGRAVITY_SELECTION", err.Error(), false, req.Resume, req.SessionID, err)
	}
	env, err := runtimeEnvironment(a.Env, req.Runtime.AuthEnvRefs, "", "")
	if err != nil {
		return Response{}, providerError("ANTIGRAVITY_AUTH", err.Error(), false, req.Resume, req.SessionID, err)
	}
	var callbackMu sync.Mutex
	persisted := ""
	notify := func(id string) error {
		callbackMu.Lock()
		defer callbackMu.Unlock()
		id = strings.TrimSpace(id)
		if id == "" || id == persisted {
			return nil
		}
		if persisted != "" && persisted != id {
			return errors.New("Antigravity emitted conflicting conversation IDs")
		}
		if callbacks.SessionStarted != nil {
			if err := callbacks.SessionStarted(id); err != nil {
				return err
			}
		}
		persisted = id
		return nil
	}
	result, processErr := a.Process(ctx, ProcessSpec{
		Path: path, Args: args, Dir: req.RepoRoot, Env: env, Stdin: antigravityInput(req.Prompt), MaxOutputBytes: req.MaxOutputBytes,
		StdoutLine: func(line []byte) error {
			id, _ := antigravitySessionFromLine(line)
			return notify(id)
		},
	})
	parseCallbacks := callbacks
	parseCallbacks.SessionStarted = notify
	response, parseErr := parseAntigravityOutput(result.Stdout, req.Role, parseCallbacks)
	known := response.SessionID != ""
	if req.Resume && response.SessionID == "" {
		response.SessionID, known = req.SessionID, true
	}
	if req.Resume && response.SessionID != req.SessionID {
		return response, providerError("ANTIGRAVITY_SESSION_MISMATCH", "Antigravity resumed a different conversation", false, true, response.SessionID, nil)
	}
	if processErr != nil {
		if parseErr != nil {
			processErr = parseErr
		}
		return response, providerProcessError("antigravity", processErr, known, response.SessionID)
	}
	if parseErr != nil {
		return response, providerError("ANTIGRAVITY_OUTPUT", parseErr.Error(), known, known, response.SessionID, parseErr)
	}
	if !req.Resume && response.SessionID == "" {
		return response, providerError("ANTIGRAVITY_NO_CONVERSATION", "fresh Antigravity turn did not emit a conversation ID", false, false, "", ErrMissingSession)
	}
	if err := VerifyReportedSelection("antigravity", req, response); err != nil {
		return response, err
	}
	envelope, err := DecodeEnvelope([]byte(response.Text), req.Role)
	if err != nil {
		return response, providerError("ANTIGRAVITY_ENVELOPE", err.Error(), known, known, response.SessionID, err)
	}
	response.Envelope, response.Raw = &envelope, result.Stdout
	return response, nil
}

func antigravityInput(prompt string) string {
	data, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": prompt}})
	return string(data) + "\n"
}

func antigravitySessionFromLine(line []byte) (string, error) {
	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		return "", err
	}
	return firstString(event, "conversation_id", "conversationId", "session_id", "sessionId"), nil
}

func parseAntigravityOutput(data []byte, role Role, callbacks Callbacks) (Response, error) {
	response := Response{UsageCumulative: true}
	var providerMessage string
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return response, fmt.Errorf("invalid Antigravity JSON event: %w", err)
		}
		typ := firstString(event, "event", "type")
		payload := event
		if nested, ok := event[typ].(map[string]any); ok {
			payload = nested
		}
		id := firstString(event, "conversation_id", "conversationId", "session_id", "sessionId")
		if id == "" {
			id = firstString(payload, "conversation_id", "conversationId", "session_id", "sessionId")
		}
		if id != "" {
			if response.SessionID != "" && response.SessionID != id {
				return response, errors.New("Antigravity emitted conflicting conversation IDs")
			}
			if response.SessionID == "" && callbacks.SessionStarted != nil {
				if err := callbacks.SessionStarted(id); err != nil {
					return response, err
				}
			}
			response.SessionID = id
		}
		model := firstString(event, "model")
		if init, ok := event["init"].(map[string]any); ok && model == "" {
			model = firstString(init, "model")
		}
		if model != "" {
			response.ReportedModel = model
		}
		effort := firstString(event, "effort")
		if effort == "" {
			effort = firstString(payload, "effort")
		}
		if effort != "" {
			response.ReportedEffort = effort
		}
		if status := strings.ToLower(firstString(payload, "status")); status != "" && status != "success" {
			providerMessage = firstString(payload, "error", "message")
			if providerMessage == "" {
				providerMessage = "Antigravity reported status " + status
			}
		}
		if text := antigravityEnvelopeText(payload, role); text != "" {
			response.Text = text
		}
		if usageMap, ok := payload["usage"].(map[string]any); ok {
			response.Usage = antigravityUsage(usageMap)
		}
		if callbacks.Event != nil {
			encoded, _ := json.Marshal(event)
			if err := callbacks.Event(Event{Type: typ, SessionID: response.SessionID, Model: response.ReportedModel, Effort: response.ReportedEffort, Raw: encoded}); err != nil {
				return response, err
			}
		}
	}
	if providerMessage != "" {
		return response, errors.New(providerMessage)
	}
	if strings.TrimSpace(response.Text) == "" {
		return response, errors.New("Antigravity produced no structured response")
	}
	envelope, err := DecodeEnvelope([]byte(response.Text), role)
	if err != nil {
		return response, err
	}
	response.Envelope = &envelope
	return response, nil
}

func antigravityEnvelopeText(payload map[string]any, role Role) string {
	if structured, ok := payload["structured_output"]; ok && structured != nil {
		if encoded := canonicalAntigravityEnvelope(structured, role); encoded != "" {
			return encoded
		}
	}
	response := firstString(payload, "response")
	if response == "" {
		return ""
	}
	if encoded := canonicalAntigravityEnvelope(json.RawMessage(response), role); encoded != "" {
		return encoded
	}
	// Some resumed Antigravity conversations return a schema-valid object in
	// a Markdown fence instead of populating structured_output. Extract only a
	// strictly valid role envelope; surrounding prose is never accepted.
	for offset := 0; offset < len(response); {
		start := strings.IndexByte(response[offset:], '{')
		if start < 0 {
			break
		}
		start += offset
		decoder := json.NewDecoder(strings.NewReader(response[start:]))
		var value any
		if err := decoder.Decode(&value); err == nil {
			if encoded := canonicalAntigravityEnvelope(value, role); encoded != "" {
				return encoded
			}
		}
		offset = start + 1
	}
	return response
}

func canonicalAntigravityEnvelope(value any, role Role) string {
	var object map[string]any
	switch typed := value.(type) {
	case map[string]any:
		object = typed
	case json.RawMessage:
		if json.Unmarshal(typed, &object) != nil {
			return ""
		}
	default:
		encoded, err := json.Marshal(value)
		if err != nil || json.Unmarshal(encoded, &object) != nil {
			return ""
		}
	}
	if marker, ok := object["role"].(string); ok && strings.EqualFold(strings.TrimSpace(marker), string(role)) {
		// Antigravity may title-case enum strings in structured output.
		// Normalize only the case-equivalent marker; semantic mismatches fail.
		object["role"] = string(role)
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return ""
	}
	if _, err := DecodeEnvelope(encoded, role); err != nil {
		return ""
	}
	return string(encoded)
}

func antigravityUsage(values map[string]any) TokenUsage {
	usage := TokenUsage{
		InputTokens:       number(values, "input_tokens", "input"),
		CachedInputTokens: number(values, "cached_input_tokens", "cache_read_tokens", "cache_read"),
		OutputTokens:      number(values, "output_tokens", "output"),
		ReasoningTokens:   number(values, "reasoning_tokens", "thinking_tokens", "thinking"),
		TotalTokens:       number(values, "total_tokens", "total"),
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (a *Antigravity) Version(ctx context.Context) (string, error) {
	result, err := a.Process(ctx, ProcessSpec{Path: a.Path, Args: []string{"--version"}, Env: a.Env, MaxOutputBytes: 64 << 10})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

func (a *Antigravity) Auth(ctx context.Context) (AuthStatus, error) {
	version, err := a.Version(ctx)
	if err != nil {
		return AuthStatus{}, err
	}
	models, err := a.ListModels(ctx, ModelListRequest{})
	if err != nil {
		return AuthStatus{Version: version, Message: "run agy to sign in"}, err
	}
	if len(models.Models) == 0 {
		return AuthStatus{Version: version, Message: "run agy to sign in"}, errors.New("Antigravity authentication status is unavailable")
	}
	return AuthStatus{Authenticated: true, Version: version, Account: models.Account}, nil
}

func (a *Antigravity) LocalAuthHint() AuthStatus {
	identity := antigravityCredentialIdentity(a.Env)
	if identity == "" {
		return AuthStatus{Message: "run agy to sign in"}
	}
	return AuthStatus{Authenticated: true, Account: identity, Message: "local credentials found; live validation runs in the background"}
}

func (a *Antigravity) Login(ctx context.Context, req LoginRequest) error {
	run := a.InteractiveProcess
	if run == nil {
		run = RunInteractiveProcess
	}
	return run(ctx, a.Path, nil, req.RepoRoot, a.Env, req.Stdin, req.Stdout, req.Stderr)
}

var (
	antigravityBullet = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)])\s*`)
	antigravitySlug   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

func (a *Antigravity) ListModels(ctx context.Context, req ModelListRequest) (ModelPage, error) {
	path, err := executableForRequest(a.Path, req.Runtime.CLIPath)
	if err != nil {
		return ModelPage{}, err
	}
	result, err := a.Process(ctx, ProcessSpec{Path: path, Args: []string{"models"}, Env: a.Env, MaxOutputBytes: 2 << 20})
	if err != nil {
		return ModelPage{}, providerProcessError("antigravity", err, false, "")
	}
	models := []ModelInfo{}
	for _, raw := range strings.Split(string(result.Stdout), "\n") {
		line := strings.TrimSpace(antigravityBullet.ReplaceAllString(raw, ""))
		if line == "" || strings.HasPrefix(strings.ToLower(line), "available model") {
			continue
		}
		id, label := line, line
		if fields := strings.SplitN(line, "\t", 2); len(fields) == 2 {
			id, label = strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		} else if fields := strings.Fields(line); len(fields) > 1 && strings.Contains(fields[0], "-") && antigravitySlug.MatchString(fields[0]) {
			id, label = fields[0], strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		}
		if id == "" {
			continue
		}
		effort := effortFromAntigravityLabel(label)
		info := ModelInfo{ID: id, Label: label, Provider: "antigravity", Origin: "live", Availability: "available"}
		if effort != "" {
			// Antigravity model slugs encode the supported effort. Passing a
			// conflicting effort fails before the turn, while models such as
			// Claude Thinking reject --effort entirely.
			info.Efforts = []string{effort}
			info.EffortOptions = []ModelOption{{ID: effort, Label: strings.ToUpper(effort[:1]) + effort[1:], Description: "Fixed by this Antigravity model variant"}}
			info.DefaultEffort = effort
		}
		models = append(models, info)
	}
	if len(models) == 0 {
		return ModelPage{}, errors.New("Antigravity returned no models")
	}
	return ModelPage{Models: models, Account: antigravityCredentialIdentity(a.Env)}, nil
}

// Antigravity does not expose an account ID in its model/config commands.
// Scope caches to credential-file metadata without reading or persisting the
// credential itself. A login/account switch rewrites this file.
func antigravityCredentialIdentity(environ []string) string {
	home := ""
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok && key == "HOME" {
			home = value
			break
		}
	}
	if home == "" {
		return ""
	}
	info, err := os.Stat(filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("credential-metadata:%d:%d", info.Size(), info.ModTime().UnixNano())
}

func effortFromAntigravityLabel(label string) string {
	lower := strings.ToLower(label)
	for _, effort := range []string{"high", "medium", "low"} {
		if strings.Contains(lower, "("+effort+")") {
			return effort
		}
	}
	return ""
}

func (a *Antigravity) Probe(ctx context.Context) error {
	result, err := a.Process(ctx, ProcessSpec{Path: a.Path, Args: []string{"--help"}, Env: a.Env, MaxOutputBytes: 2 << 20})
	if err != nil {
		return err
	}
	help := string(result.Stdout) + string(result.Stderr)
	for _, flag := range []string{"--output-format", "--json-schema", "--model", "--effort", "--conversation", "--mode", "--sandbox"} {
		if !strings.Contains(help, flag) {
			return fmt.Errorf("installed Antigravity CLI does not support required flag %s", flag)
		}
	}
	return nil
}
