// Package runner contains provider adapters. Every adapter exposes the same
// durable-session boundary so workflow code can be tested entirely with fakes.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Speed     string
	RepoRoot  string
	Scope     string
	// SkillDirectories are provider-native roots selected by RoleMux. Adapters
	// that expose an explicit skill-loading API may use them; others rely on
	// their native discovery while receiving the same bounded metadata note.
	SkillDirectories []string
	SessionID        string
	Resume           bool
	Sandbox          string
	Runtime          task.RuntimeSnapshot
	Budget           task.RoleBudget

	// MaxOutputBytes is a hard process-output bound. Zero uses a safe default.
	MaxOutputBytes int64
}

// ModelOption is a provider-advertised setting value. Descriptions are kept
// optional because not every CLI exposes them.
type ModelOption struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

type Callbacks struct {
	// SessionStarted must durably record an ID before a fresh call is
	// considered successful. Adapters invoke it as soon as the provider emits
	// its durable session/thread event.
	SessionStarted func(string) error
	Event          func(Event) error
	// Diagnostic receives concise, non-secret launch diagnostics. Adapters do
	// not persist these messages and callers may leave it nil.
	Diagnostic func(string)
}

type Event struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	Model     string          `json:"model,omitempty"`
	Effort    string          `json:"effort,omitempty"`
	AgentTurn bool            `json:"agent_turn,omitempty"`
	ToolCall  bool            `json:"tool_call,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

type UsageStatus string

const (
	UsageComplete   UsageStatus = "complete"
	UsageIncomplete UsageStatus = "incomplete"
	UsageUnreported UsageStatus = "unreported"

	// UsageStatus* aliases keep the field-oriented names convenient for
	// callers while Usage* follows the package's other typed-constant APIs.
	UsageStatusComplete   = UsageComplete
	UsageStatusIncomplete = UsageIncomplete
	UsageStatusUnreported = UsageUnreported
)

func usageStatus(reported, terminal bool) UsageStatus {
	if !reported {
		return UsageStatusUnreported
	}
	if terminal {
		return UsageStatusComplete
	}
	return UsageStatusIncomplete
}

type Response struct {
	Text           string          `json:"text,omitempty"`
	SessionID      string          `json:"session_id"`
	ReportedModel  string          `json:"reported_model,omitempty"`
	ReportedEffort string          `json:"reported_effort,omitempty"`
	Envelope       *Envelope       `json:"envelope,omitempty"`
	Raw            json.RawMessage `json:"raw,omitempty"`
	Usage          task.TokenUsage `json:"usage,omitempty"`
	UsageStatus    UsageStatus     `json:"usage_status,omitempty"`
	// UsageCumulative means token counters cover the whole resumed provider
	// conversation rather than only this invocation.
	UsageCumulative bool `json:"usage_cumulative,omitempty"`
}

var ErrBudgetExceeded = errors.New("role execution budget exceeded")

type BudgetError struct {
	Kind     string
	Limit    int64
	Observed int64
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%s: %s budget limit %d exceeded (observed %d)", ErrBudgetExceeded, e.Kind, e.Limit, e.Observed)
}

func (e *BudgetError) Unwrap() error { return ErrBudgetExceeded }

// UsageFromMap normalizes snake_case and camelCase usage payloads. For OpenAI
// and Copilot, cached tokens are a subset of input tokens; Anthropic reports
// cache read/write tokens separately, so callers select the correct total.
func UsageFromMap(values map[string]any, inputIncludesCache bool) task.TokenUsage {
	usage, _ := UsageFromMapWithPresence(values, inputIncludesCache)
	return usage
}

// UsageFromMapWithPresence is UsageFromMap plus whether values contained at
// least one recognized numeric usage counter. Presence is deliberately
// independent from the counter values so an explicitly reported zero remains
// distinguishable from an absent usage report.
func UsageFromMapWithPresence(values map[string]any, inputIncludesCache bool) (task.TokenUsage, bool) {
	if values == nil {
		return task.TokenUsage{}, false
	}
	if nested, ok := values["usage"].(map[string]any); ok {
		values = nested
	}
	var usage task.TokenUsage
	var reported bool
	var present bool
	if usage.InputTokens, present = numberWithPresence(values, "input_tokens", "inputTokens"); present {
		reported = true
	}
	if usage.CachedInputTokens, present = numberWithPresence(values, "cached_input_tokens", "cache_read_input_tokens", "cacheReadTokens"); present {
		reported = true
	}
	if usage.CacheWriteTokens, present = numberWithPresence(values, "cache_creation_input_tokens", "cache_write_tokens", "cacheWriteTokens"); present {
		reported = true
	}
	if usage.OutputTokens, present = numberWithPresence(values, "output_tokens", "outputTokens"); present {
		reported = true
	}
	if usage.ReasoningTokens, present = numberWithPresence(values, "reasoning_tokens", "reasoningTokens"); present {
		reported = true
	}
	if usage.TotalTokens, present = numberWithPresence(values, "total_tokens", "totalTokens"); present {
		reported = true
	}
	if details, ok := values["input_tokens_details"].(map[string]any); ok {
		if cached, detailsPresent := numberWithPresence(details, "cached_tokens", "cachedTokens"); detailsPresent && usage.CachedInputTokens == 0 {
			if _, explicit := numberWithPresence(values, "cached_input_tokens", "cache_read_input_tokens", "cacheReadTokens"); !explicit {
				usage.CachedInputTokens = cached
			}
			reported = true
		}
	}
	if details, ok := values["inputTokensDetails"].(map[string]any); ok {
		if cached, detailsPresent := numberWithPresence(details, "cached_tokens", "cachedTokens"); detailsPresent && usage.CachedInputTokens == 0 {
			if _, explicit := numberWithPresence(values, "cached_input_tokens", "cache_read_input_tokens", "cacheReadTokens"); !explicit {
				usage.CachedInputTokens = cached
			}
			reported = true
		}
	}
	if details, ok := values["output_tokens_details"].(map[string]any); ok {
		if reasoning, detailsPresent := numberWithPresence(details, "reasoning_tokens", "reasoningTokens"); detailsPresent && usage.ReasoningTokens == 0 {
			if _, explicit := numberWithPresence(values, "reasoning_tokens", "reasoningTokens"); !explicit {
				usage.ReasoningTokens = reasoning
			}
			reported = true
		}
	}
	if details, ok := values["outputTokensDetails"].(map[string]any); ok {
		if reasoning, detailsPresent := numberWithPresence(details, "reasoning_tokens", "reasoningTokens"); detailsPresent && usage.ReasoningTokens == 0 {
			if _, explicit := numberWithPresence(values, "reasoning_tokens", "reasoningTokens"); !explicit {
				usage.ReasoningTokens = reasoning
			}
			reported = true
		}
	}
	if !reported {
		return usage, false
	}
	if !hasNumericField(values, "total_tokens", "totalTokens") {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		if !inputIncludesCache {
			usage.TotalTokens += usage.CachedInputTokens + usage.CacheWriteTokens
		}
	}
	return usage, true
}

func number(values map[string]any, keys ...string) int64 {
	value, _ := numberWithPresence(values, keys...)
	return value
}

func numberWithPresence(values map[string]any, keys ...string) (int64, bool) {
	if values == nil {
		return 0, false
	}
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return int64(typed), true
		case float32:
			return int64(typed), true
		case int64:
			return typed, true
		case int:
			return int64(typed), true
		case int32:
			return int64(typed), true
		case int16:
			return int64(typed), true
		case int8:
			return int64(typed), true
		case uint:
			return int64(typed), true
		case uint64:
			return int64(typed), true
		case uint32:
			return int64(typed), true
		case uint16:
			return int64(typed), true
		case uint8:
			return int64(typed), true
		case json.Number:
			if n, err := typed.Int64(); err == nil {
				return n, true
			}
			if n, err := typed.Float64(); err == nil {
				return int64(n), true
			}
		}
	}
	return 0, false
}

func hasNumericField(values map[string]any, keys ...string) bool {
	_, ok := numberWithPresence(values, keys...)
	return ok
}

func usageFromJSONDocument(data []byte, inputIncludesCache bool) TokenUsage {
	usage, _ := usageFromJSONDocumentWithPresence(data, inputIncludesCache)
	return usage
}

func usageFromJSONDocumentWithPresence(data []byte, inputIncludesCache bool) (TokenUsage, bool) {
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return TokenUsage{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return TokenUsage{}, false
	}
	return UsageFromMapWithPresence(values, inputIncludesCache)
}

func usageFromJSONLines(data []byte, inputIncludesCache bool) TokenUsage {
	usage, _ := usageFromJSONLinesWithPresence(data, inputIncludesCache)
	return usage
}

func usageFromJSONLinesWithPresence(data []byte, inputIncludesCache bool) (TokenUsage, bool) {
	var latest TokenUsage
	present := false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		usage, linePresent := usageFromJSONDocumentWithPresence(line, inputIncludesCache)
		if linePresent {
			latest = usage
			present = true
		}
	}
	return latest, present
}

type ModelInfo struct {
	ID                     string        `json:"id"`
	Label                  string        `json:"label,omitempty"`
	Description            string        `json:"description,omitempty"`
	Provider               string        `json:"provider"`
	Origin                 string        `json:"origin"`
	Availability           string        `json:"availability"`
	AgeSeconds             int64         `json:"age_seconds,omitempty"`
	ContextWindowTokens    int           `json:"context_window_tokens,omitempty"`
	MaxContextWindowTokens int           `json:"max_context_window_tokens,omitempty"`
	MaxPromptTokens        int           `json:"max_prompt_tokens,omitempty"`
	MaxOutputTokens        int           `json:"max_output_tokens,omitempty"`
	IsDefault              bool          `json:"is_default,omitempty"`
	Efforts                []string      `json:"efforts,omitempty"`
	EffortOptions          []ModelOption `json:"effort_options,omitempty"`
	DefaultEffort          string        `json:"default_effort,omitempty"`
	SpeedOptions           []ModelOption `json:"speed_options,omitempty"`
	DefaultSpeed           string        `json:"default_speed,omitempty"`
	Aliases                []string      `json:"aliases,omitempty"`
	Custom                 bool          `json:"custom"`
	Account                string        `json:"-"`
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

type LoginRequest struct {
	RepoRoot string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
}

// Authenticator is optional so third-party adapters can remain read-only.
// Built-in CLI adapters implement it with their official interactive login.
type Authenticator interface {
	Login(context.Context, LoginRequest) error
}

// LocalAuthHinter lets an adapter report credential presence without starting
// the provider. Configure may use this to stay responsive; doctor and task
// execution still use the adapter's authoritative live checks.
type LocalAuthHinter interface {
	LocalAuthHint() AuthStatus
}

// RoleSupporter lets an adapter fail before model selection when it cannot
// safely perform a role. Adapters that omit it are assumed to support all
// roles.
type RoleSupporter interface {
	SupportsRole(Role) error
}

type Adapter interface {
	Run(context.Context, Request, Callbacks) (Response, error)
	ListModels(context.Context, ModelListRequest) (ModelPage, error)
	Version(context.Context) (string, error)
	Auth(context.Context) (AuthStatus, error)
}

// ValidateSelection checks a provider-advertised model/effort/speed tuple.
// Empty effort and standard speed are valid defaults; every non-default value
// must be explicitly advertised by the selected model.
func ValidateSelection(role Role, model, effort, speed string, models []ModelInfo, adapter Adapter) error {
	if supporter, ok := adapter.(RoleSupporter); ok {
		if err := supporter.SupportsRole(role); err != nil {
			return err
		}
	}
	var selected *ModelInfo
	for i := range models {
		if models[i].ID == model {
			selected = &models[i]
		} else {
			for _, alias := range models[i].Aliases {
				if alias == model {
					selected = &models[i]
					break
				}
			}
		}
		if selected != nil {
			break
		}
	}
	if selected == nil {
		return providerError("MODEL_UNAVAILABLE", fmt.Sprintf("model %q is not in the provider catalog", model), false, false, "", nil)
	}
	if selected.Availability == "unavailable" {
		return providerError("MODEL_UNAVAILABLE", fmt.Sprintf("model %q is unavailable", model), false, false, "", nil)
	}
	if effort != "" && !modelOptionContains(selected.EffortOptions, selected.Efforts, effort) {
		return fmt.Errorf("model %q does not advertise effort %q", model, effort)
	}
	if speed != "" && speed != "standard" && !modelOptionContains(selected.SpeedOptions, nil, speed) {
		return fmt.Errorf("model %q does not advertise speed %q", model, speed)
	}
	return nil
}

func modelOptionContains(options []ModelOption, legacy []string, want string) bool {
	for _, option := range options {
		if option.ID == want {
			return true
		}
	}
	for _, value := range legacy {
		if value == want {
			return true
		}
	}
	return false
}

func VerifyReportedSelection(provider string, req Request, response Response) error {
	known := response.SessionID != ""
	// Some providers expose an explicit automatic router as a selectable
	// model. Its terminal response identifies the concrete model chosen for
	// that turn, which is evidence that routing worked rather than drift.
	automaticModel := strings.EqualFold(strings.TrimSpace(req.Model), "auto")
	if response.ReportedModel != "" && response.ReportedModel != req.Model && !automaticModel {
		return providerError(strings.ToUpper(provider)+"_MODEL_MISMATCH", provider+" reported a different model than requested", false, known, response.SessionID, nil)
	}
	if req.Effort != "" && response.ReportedEffort != "" && response.ReportedEffort != req.Effort {
		return providerError(strings.ToUpper(provider)+"_EFFORT_MISMATCH", provider+" reported a different reasoning effort than requested", false, known, response.SessionID, nil)
	}
	return nil
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
	label := map[string]string{"codex": "Codex", "claude": "Claude", "copilot": "Copilot", "antigravity": "Antigravity"}[strings.ToLower(provider)]
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
