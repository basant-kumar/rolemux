// Package runner contains provider adapters. Every adapter exposes the same
// durable-session boundary so workflow code can be tested entirely with fakes.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/basant-kumar/rolemux/internal/task"
)

type TokenUsage = task.TokenUsage

type Role string

const (
	RolePlanner      Role = "planner"
	RolePlanReviewer Role = "plan_reviewer"
	RoleImplementer  Role = "implementer"
	RoleCodeReviewer Role = "code_reviewer"
)

type Request struct {
	Role      Role
	Operation string
	Prompt    string
	Model     string
	Effort    string
	RepoRoot  string
	Scope     string
	SessionID string
	Resume    bool
	Sandbox   string
	Runtime   task.RuntimeSnapshot

	// MaxOutputBytes is a hard process-output bound. Zero uses a safe default.
	MaxOutputBytes int64
}

type Callbacks struct {
	// SessionStarted must durably record an ID before a fresh call is
	// considered successful. Adapters invoke it as soon as the provider emits
	// its durable session/thread event.
	SessionStarted func(string) error
	Event          func(Event) error
}

type Event struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	Model     string          `json:"model,omitempty"`
	Effort    string          `json:"effort,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

type Response struct {
	Text           string          `json:"text,omitempty"`
	SessionID      string          `json:"session_id"`
	ReportedModel  string          `json:"reported_model,omitempty"`
	ReportedEffort string          `json:"reported_effort,omitempty"`
	Envelope       *Envelope       `json:"envelope,omitempty"`
	Raw            json.RawMessage `json:"raw,omitempty"`
	Usage          task.TokenUsage `json:"usage,omitempty"`
}

// UsageFromMap normalizes snake_case and camelCase usage payloads. For OpenAI
// and Copilot, cached tokens are a subset of input tokens; Anthropic reports
// cache read/write tokens separately, so callers select the correct total.
func UsageFromMap(values map[string]any, inputIncludesCache bool) task.TokenUsage {
	if nested, ok := values["usage"].(map[string]any); ok {
		values = nested
	}
	usage := task.TokenUsage{
		InputTokens:       number(values, "input_tokens", "inputTokens"),
		CachedInputTokens: number(values, "cached_input_tokens", "cache_read_input_tokens", "cacheReadTokens"),
		CacheWriteTokens:  number(values, "cache_creation_input_tokens", "cacheWriteTokens"),
		OutputTokens:      number(values, "output_tokens", "outputTokens"),
		ReasoningTokens:   number(values, "reasoning_tokens", "reasoningTokens"),
		TotalTokens:       number(values, "total_tokens", "totalTokens"),
	}
	if details, ok := values["input_tokens_details"].(map[string]any); ok && usage.CachedInputTokens == 0 {
		usage.CachedInputTokens = number(details, "cached_tokens")
	}
	if details, ok := values["output_tokens_details"].(map[string]any); ok && usage.ReasoningTokens == 0 {
		usage.ReasoningTokens = number(details, "reasoning_tokens")
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		if !inputIncludesCache {
			usage.TotalTokens += usage.CachedInputTokens + usage.CacheWriteTokens
		}
	}
	return usage
}

func number(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := values[key].(type) {
		case float64:
			return int64(value)
		case int64:
			return value
		case int:
			return int64(value)
		case json.Number:
			n, _ := value.Int64()
			return n
		}
	}
	return 0
}

func usageFromJSONDocument(data []byte, inputIncludesCache bool) TokenUsage {
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return TokenUsage{}
	}
	return UsageFromMap(values, inputIncludesCache)
}

func usageFromJSONLines(data []byte, inputIncludesCache bool) TokenUsage {
	var latest TokenUsage
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		usage := usageFromJSONDocument(line, inputIncludesCache)
		if !usage.Empty() {
			latest = usage
		}
	}
	return latest
}

type ModelInfo struct {
	ID            string   `json:"id"`
	Label         string   `json:"label,omitempty"`
	Provider      string   `json:"provider"`
	Origin        string   `json:"origin"`
	Availability  string   `json:"availability"`
	AgeSeconds    int64    `json:"age_seconds,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	Aliases       []string `json:"aliases,omitempty"`
	Custom        bool     `json:"custom"`
	Account       string   `json:"-"`
}

type ModelListRequest struct {
	Refresh bool
	Runtime task.RuntimeSnapshot
}

type ModelPage struct {
	Models     []ModelInfo
	NextCursor string
	Account    string
	Endpoint   string
}

type AuthStatus struct {
	Authenticated bool   `json:"authenticated"`
	Version       string `json:"version,omitempty"`
	Message       string `json:"message,omitempty"`
	// Account is used only as an in-memory cache discriminator. Catalog cache
	// files persist its hash, never the raw account identifier.
	Account string `json:"-"`
}

type Adapter interface {
	Run(context.Context, Request, Callbacks) (Response, error)
	ListModels(context.Context, ModelListRequest) (ModelPage, error)
	Version(context.Context) (string, error)
	Auth(context.Context) (AuthStatus, error)
}

var (
	ErrUnsupportedProvider = errors.New("unsupported provider operation")
	ErrOutputLimit         = errors.New("provider output exceeds configured limit")
	ErrMissingSession      = errors.New("provider did not emit a durable session ID")
	ErrInvalidEnvelope     = errors.New("provider returned an invalid JSON envelope")
)

type ProviderError struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable"`
	KnownSession bool   `json:"known_session"`
	SessionID    string `json:"session_id,omitempty"`
	Cause        error  `json:"-"`
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "provider error"
	}
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func providerError(code, message string, retryable, known bool, session string, cause error) error {
	return &ProviderError{Code: code, Message: message, Retryable: retryable, KnownSession: known, SessionID: session, Cause: cause}
}

func providerProcessError(provider string, cause error, known bool, session string) error {
	label := map[string]string{"codex": "Codex", "claude": "Claude", "copilot": "Copilot"}[strings.ToLower(provider)]
	if label == "" {
		label = "Provider"
	}
	code := strings.ToUpper(provider) + "_PROCESS"
	message := label + " invocation failed; run rolemux doctor --json"
	lower := strings.ToLower(errorString(cause))
	switch {
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		code, message = "INTERRUPTED", label+" invocation was interrupted"
	case errors.Is(cause, ErrOutputLimit):
		code, message = "OUTPUT_LIMIT", label+" output exceeded RoleMux's safety limit"
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "rate-limit"), strings.Contains(lower, "usage limit"), strings.Contains(lower, "quota"), strings.Contains(lower, " 429"):
		code, message = "RATE_LIMITED", label+" is rate-limited; let the orchestrator decide when to retry"
	case strings.Contains(lower, "not logged in"), strings.Contains(lower, "unauthorized"), strings.Contains(lower, "authentication"), strings.Contains(lower, " 401"):
		code, message = "AUTH_REQUIRED", "Log in with the "+label+" CLI, then retry"
	case strings.Contains(lower, "model") && (strings.Contains(lower, "not found") || strings.Contains(lower, "unavailable") || strings.Contains(lower, "unsupported")):
		code, message = "MODEL_UNAVAILABLE", "The selected "+label+" model is unavailable; let the orchestrator choose the next action"
	}
	return providerError(code, message, known, known, session, cause)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
