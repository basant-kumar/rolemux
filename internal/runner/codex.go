package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/basant-kumar/rolemux/internal/task"
)

type Codex struct {
	Path               string
	Process            ProcessFunc
	InteractiveProcess InteractiveProcessFunc
	Env                []string
	// PXPipePath is discovered once for the adapter, then checked again at
	// launch time so installing/removing the optional helper between turns is
	// safe. TaskLauncher is an injectable lifecycle boundary for tests.
	PXPipePath   string
	TaskLauncher CodexTaskLauncher
	AuthProbe    CodexAuthProbe
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
	env := SanitizedEnv(os.Environ())
	return &Codex{Path: resolved, Process: RunProcess, InteractiveProcess: RunInteractiveProcess, Env: env, PXPipePath: DetectPXPipePath(env)}, nil
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
	if err := ctx.Err(); err != nil {
		return Response{}, err
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
	var callbackMu sync.Mutex
	persistedSession := ""
	notifySession := func(id string) error {
		callbackMu.Lock()
		defer callbackMu.Unlock()
		if id == "" || id == persistedSession {
			return nil
		}
		if persistedSession != "" && persistedSession != id {
			return errors.New("codex emitted conflicting thread IDs")
		}
		if callbacks.SessionStarted != nil {
			if err := callbacks.SessionStarted(id); err != nil {
				return err
			}
		}
		persistedSession = id
		return nil
	}
	providerSpec := ProcessSpec{
		Path: path, Args: args, Dir: req.RepoRoot, Env: env, Stdin: req.Prompt, MaxOutputBytes: req.MaxOutputBytes,
		StdoutLine: func(line []byte) error {
			id, err := codexSessionFromLine(line)
			if err != nil {
				return err
			}
			if id != "" {
				if err := notifySession(id); err != nil {
					return err
				}
			}
			if callbacks.Event != nil {
				for _, event := range EventsFromLine("codex", line) {
					if err := callbacks.Event(event); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	result, processErr := c.runCodexTask(ctx, req, providerSpec, callbacks, schemaName)
	parseCallbacks := callbacks
	parseCallbacks.SessionStarted = notifySession
	parseCallbacks.Event = nil
	threadID, text, reportedModel, reportedEffort, parseErr := parseCodexOutput(result.Stdout, req.Role, parseCallbacks)
	usage, usageReported, terminalUsage := codexUsageFromJSONLines(result.Stdout)
	response := Response{SessionID: threadID, Text: text, ReportedModel: reportedModel, ReportedEffort: reportedEffort, Usage: usage, UsageStatus: usageStatus(usageReported, terminalUsage)}
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
	if selectionErr := VerifyReportedSelection("codex", req, response); selectionErr != nil {
		return response, selectionErr
	}
	envelope, err := DecodeEnvelope([]byte(text), req.Role)
	if err != nil {
		return response, providerError("CODEX_ENVELOPE", err.Error(), known, known, threadID, err)
	}
	response.Envelope, response.Raw = &envelope, result.Stdout
	return response, nil
}

func codexUsageFromJSONLines(data []byte) (usage TokenUsage, reported, terminal bool) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&event); err != nil {
			continue
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			continue
		}
		lineUsage, present := UsageFromMapWithPresence(event, true)
		if !present {
			continue
		}
		usage, reported = lineUsage, true
		typ, _ := event["type"].(string)
		if typ == "turn.completed" {
			terminal = true
		}
	}
	return usage, reported, terminal
}

func codexSessionFromLine(line []byte) (string, error) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return "", nil
	}
	var event struct {
		Type          string `json:"type"`
		ThreadID      string `json:"thread_id"`
		ThreadIDCamel string `json:"threadId"`
	}
	if err := json.Unmarshal(line, &event); err != nil {
		return "", nil // the full parser reports malformed provider output
	}
	if event.Type != "thread.started" {
		return "", nil
	}
	if event.ThreadID != "" {
		return strings.TrimSpace(event.ThreadID), nil
	}
	return strings.TrimSpace(event.ThreadIDCamel), nil
}

func (c *Codex) runCodexTask(ctx context.Context, req Request, providerSpec ProcessSpec, callbacks Callbacks, schemaPath string) (ProcessResult, error) {
	if err := ctx.Err(); err != nil {
		return ProcessResult{}, err
	}
	launcher := c.codexTaskLauncher(providerSpec.Env)
	if launcher == nil {
		if callbacks.Diagnostic != nil && !req.Resume {
			callbacks.Diagnostic(missingPXPipeDiagnostic("Codex"))
		}
		return c.Process(ctx, providerSpec)
	}
	evidence, probeErr := c.codexAuthProbe(ctx, providerSpec.Path, providerSpec.Env)
	if err := ctx.Err(); err != nil {
		return ProcessResult{}, err
	}
	if probeErr != nil || evidence.Mode != CodexAuthChatGPT {
		reason := evidence.Reason
		if reason == "" {
			reason = codexLaunchReason(evidence, false)
		}
		callbacks.Diagnostic = notifyDiagnostic(callbacks.Diagnostic, "pxpipe: "+reason+"; running Codex directly")
		return c.Process(ctx, providerSpec)
	}
	if !CodexChatGPTRouteSupported(req.Runtime, providerSpec.Env) {
		reason := codexLaunchReason(evidence, false)
		if reason == "" {
			reason = "Codex route is not the supported ChatGPT route"
		}
		callbacks.Diagnostic = notifyDiagnostic(callbacks.Diagnostic, "pxpipe: "+reason+"; running Codex directly")
		return c.Process(ctx, providerSpec)
	}
	overlayRequest := req
	overlayRequest.Runtime = CodexChatGPTRuntimeOverlay(req.Runtime)
	overlayArgs, err := BuildCodexArgs(overlayRequest, schemaPath)
	if err != nil {
		callbacks.Diagnostic = notifyDiagnostic(callbacks.Diagnostic, "pxpipe: transport overlay unavailable; running Codex directly")
		return c.Process(ctx, providerSpec)
	}
	wrappedSpec := providerSpec
	wrappedSpec.Args = overlayArgs
	launchSpec := PXPipeLaunchSpec{
		PXPipePath:   c.pxpipePath(providerSpec.Env),
		Provider:     wrappedSpec,
		ProviderName: "Codex",
		ServerEnv:    PXPipeServerEnvironment(providerSpec.Env),
		EventsFile:   pxpipeEventsFile(providerSpec.Env),
		RoutePrefix:  pxpipeRoutePattern,
		Diagnostic:   callbacks.Diagnostic,
	}
	result, launchErr := launcher.Launch(ctx, launchSpec)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var helperErr *PXPipeLaunchError
	if errors.As(launchErr, &helperErr) && helperErr.BeforeTask {
		callbacks.Diagnostic = notifyDiagnostic(callbacks.Diagnostic, "pxpipe: private helper launch failed before a durable Codex thread ("+boundedDiagnostic(helperErr.Error())+"); running Codex directly")
		return c.Process(ctx, providerSpec)
	}
	return result, launchErr
}

func boundedDiagnostic(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return "unknown error"
	}
	// pxpipe prints route/CA setup before the actionable child-process error,
	// so retain both ends instead of truncating away the cause.
	const head, tail = 120, 600
	if len(message) > head+tail {
		return message[:head] + "... " + message[len(message)-tail:]
	}
	return message
}

func notifyDiagnostic(callback func(string), message string) func(string) {
	if callback != nil {
		callback(message)
	}
	return callback
}

func (c *Codex) codexTaskLauncher(env []string) CodexTaskLauncher {
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

func (c *Codex) pxpipePath(env []string) string {
	if c != nil && strings.TrimSpace(c.PXPipePath) != "" {
		if executableFile(c.PXPipePath) {
			return c.PXPipePath
		}
	}
	return DetectPXPipePath(env)
}

func (c *Codex) codexAuthProbe(ctx context.Context, path string, env []string) (CodexAuthEvidence, error) {
	if c != nil && c.AuthProbe != nil {
		return c.AuthProbe(ctx, path, append([]string(nil), env...))
	}
	return c.codexAuthEvidence(ctx, path, env)
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
	if req.Speed != "" && req.Speed != "standard" {
		args = append(args, "--config", "service_tier="+req.Speed)
	}
	if instructions := codexDeveloperInstructions(req.Role); instructions != "" {
		args = append(args, "--config", "developer_instructions="+quoteTOML(instructions))
	}
	if req.Resume {
		args = append(args, req.SessionID)
	}
	args = append(args, "-")
	return args, nil
}

const (
	codexImplementerDeveloperInstructions  = `RoleMux implementer discipline: before editing, use at most six underlying read/search operations across at most two provider tool turns and at most 16 KiB aggregate discovery output; nested calls count separately. Prefer packet-named files and symbols. Do not run git status/diff/log, repository-wide searches, repository-wide surveys, or a post-green survey. Consolidate related source and test edits into at most two cohesive edit calls before validation, then one correction batch when practical. Run focused validation in one batched call; never run the full repository suite. Return needs_input immediately for host-only evidence instead of retrying a blocked command. Wait at least 30 seconds for a background test or build instead of one-second polling, and stop immediately when focused validation passes.`
	codexReviewerDeveloperInstructions     = `RoleMux reviewer discipline: treat the supplied delta and evidence as authoritative. Inspect only changed files and their direct blast radius. Use no git commands and never run the full repository suite. Return the validated verdict promptly.`
	codexPlanReviewerDeveloperInstructions = `RoleMux plan-reviewer discipline: validate the supplied task, plan, and work graph against the acceptance criteria without redoing repository research. Reject full-repository or combined browser validation inside individual work units; reserve it for integration. Inspect only the supplied planning evidence and return the validated verdict promptly.`
)

func codexDeveloperInstructions(role Role) string {
	switch role {
	case RoleImplementer:
		return codexImplementerDeveloperInstructions
	case RoleCodeReviewer:
		return codexReviewerDeveloperInstructions
	case RolePlanReviewer:
		return codexPlanReviewerDeveloperInstructions
	default:
		return ""
	}
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
	status := append(append([]byte(nil), result.Stdout...), '\n')
	status = append(status, result.Stderr...)
	authenticated, message, parseErr := ParseCodexLoginStatus(status)
	if parseErr == nil {
		if authenticated && statusErr != nil {
			return AuthStatus{Version: version, Message: "Codex authentication status is unavailable"}, providerError("CODEX_AUTH", "Codex authentication status is unavailable", false, false, "", statusErr)
		}
		return AuthStatus{Version: version, Authenticated: authenticated, Message: message}, nil
	}
	if statusErr != nil {
		return AuthStatus{Version: version, Authenticated: false, Message: "run codex login"}, providerError("CODEX_AUTH", "Codex authentication status is unavailable", false, false, "", statusErr)
	}
	return AuthStatus{Version: version, Authenticated: false, Message: "unrecognized codex login status"}, providerError("CODEX_AUTH", "Codex returned an unrecognized login status", false, false, "", parseErr)
}

func (c *Codex) Login(ctx context.Context, req LoginRequest) error {
	if c == nil || c.Path == "" {
		return errors.New("codex executable is not configured")
	}
	run := c.InteractiveProcess
	if run == nil {
		run = RunInteractiveProcess
	}
	return run(ctx, c.Path, []string{"login"}, req.RepoRoot, c.Env, req.Stdin, req.Stdout, req.Stderr)
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
var _ Authenticator = (*Codex)(nil)
