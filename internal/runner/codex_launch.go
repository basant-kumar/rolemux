package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/basant-kumar/rolemux/internal/task"
)

const (
	// These values describe the only Codex transport that RoleMux can safely
	// overlay. They are routing metadata, not model policy.
	codexChatGPTProviderID = "rolemux_chatgpt"
	codexChatGPTHost       = "chatgpt.com"
	codexChatGPTOrigin     = "https://" + codexChatGPTHost
	codexChatGPTBaseURL    = codexChatGPTOrigin + "/backend-api/codex"

	CodexAuthChatGPT         CodexAuthMode = "chatgpt"
	CodexAuthAPIKey          CodexAuthMode = "api_key"
	CodexAuthUnauthenticated CodexAuthMode = "unauthenticated"
	CodexAuthUnknown         CodexAuthMode = "unknown"

	codexAuthProbeTimeout = 5 * time.Second
)

// CodexAuthMode is transient evidence from the selected Codex executable's
// own login-status command. It is deliberately not part of RuntimeSnapshot.
type CodexAuthMode string

type CodexAuthEvidence struct {
	Mode   CodexAuthMode
	Reason string
}

// CodexAuthProbe is injectable so the task-launch gate can be tested without
// invoking a real provider CLI. Implementations must not return credentials.
type CodexAuthProbe func(context.Context, string, []string) (CodexAuthEvidence, error)

// ParseCodexAuthStatus recognizes the supported Codex 0.153.x status output
// without retaining account identifiers or token material. Unknown and
// contradictory output is intentionally rejected so callers can fall back to
// the untouched provider invocation.
func ParseCodexAuthStatus(data []byte) (CodexAuthMode, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return CodexAuthUnknown, errors.New("empty Codex authentication status")
	}
	if json.Valid(data) {
		if mode, err := parseCodexAuthJSON(data); err == nil {
			return mode, nil
		}
	}
	return parseCodexAuthText(string(data))
}

// ParseCodexLoginStatus answers the broader question used by doctor and
// configure: can this CLI run authenticated turns? It intentionally accepts
// Codex auth modes that are not safe for the ChatGPT-only pxpipe overlay.
// ParseCodexAuthStatus remains the stricter routing gate.
func ParseCodexLoginStatus(data []byte) (bool, string, error) {
	if mode, err := ParseCodexAuthStatus(data); err == nil {
		switch mode {
		case CodexAuthChatGPT:
			return true, "authenticated with ChatGPT", nil
		case CodexAuthAPIKey:
			return true, "authenticated with API key", nil
		case CodexAuthUnauthenticated:
			return false, "run codex login", nil
		}
	}
	lower := strings.ToLower(strings.TrimSpace(string(data)))
	if strings.Contains(lower, "not logged in") || strings.Contains(lower, "not authenticated") {
		return false, "run codex login", nil
	}
	for _, marker := range []string{
		"logged in using access token",
		"logged in using personal access token",
		"logged in using workload identity",
		"logged in using amazon bedrock api key",
		"logged in using amazon bedrock aws access keys",
	} {
		if strings.Contains(lower, marker) {
			return true, "authenticated with Codex CLI", nil
		}
	}
	return false, "", errors.New("unrecognized Codex authentication status")
}

func parseCodexAuthText(value string) (CodexAuthMode, error) {
	lower := strings.ToLower(value)
	chatGPT := strings.Contains(lower, "logged in using chatgpt") ||
		strings.Contains(lower, "authenticated using chatgpt") ||
		strings.Contains(lower, "login method: chatgpt")
	apiKey := strings.Contains(lower, "logged in using an api key") ||
		strings.Contains(lower, "logged in using api key") ||
		strings.Contains(lower, "authenticated using an api key") ||
		strings.Contains(lower, "authenticated using api key") ||
		strings.Contains(lower, "login method: api key")
	unauthenticated := strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "not authenticated") ||
		strings.Contains(lower, "not signed in") ||
		strings.Contains(lower, "no active login")
	loggedIn := !unauthenticated && (strings.Contains(lower, "logged in") || strings.Contains(lower, "authenticated"))

	if (chatGPT && apiKey) || (unauthenticated && (chatGPT || apiKey || loggedIn)) {
		return CodexAuthUnknown, errors.New("contradictory Codex authentication status")
	}
	switch {
	case chatGPT:
		return CodexAuthChatGPT, nil
	case apiKey:
		return CodexAuthAPIKey, nil
	case unauthenticated:
		return CodexAuthUnauthenticated, nil
	default:
		return CodexAuthUnknown, errors.New("unrecognized Codex authentication status")
	}
}

func parseCodexAuthJSON(data []byte) (CodexAuthMode, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return CodexAuthUnknown, err
	}
	modes := map[CodexAuthMode]bool{}
	var visit func(any)
	visit = func(current any) {
		switch item := current.(type) {
		case map[string]any:
			for key, child := range item {
				key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
				switch key {
				case "auth_mode", "authmethod", "auth_method", "login_method", "loginmethod", "method":
					if text, ok := child.(string); ok {
						if mode, ok := authModeMarker(text); ok {
							modes[mode] = true
						}
					}
				case "authenticated", "logged_in", "loggedin":
					if logged, ok := child.(bool); ok && !logged {
						modes[CodexAuthUnauthenticated] = true
					}
				}
				visit(child)
			}
		case []any:
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(value)
	if len(modes) != 1 {
		return CodexAuthUnknown, errors.New("unrecognized or contradictory Codex authentication status")
	}
	for mode := range modes {
		return mode, nil
	}
	return CodexAuthUnknown, errors.New("unrecognized Codex authentication status")
}

func authModeMarker(value string) (CodexAuthMode, bool) {
	lower := strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(lower, "chatgpt"):
		return CodexAuthChatGPT, true
	case strings.Contains(lower, "api_key"), strings.Contains(lower, "api key"), strings.Contains(lower, "apikey"):
		return CodexAuthAPIKey, true
	case strings.Contains(lower, "unauth"), strings.Contains(lower, "not logged"), strings.Contains(lower, "signed out"):
		return CodexAuthUnauthenticated, true
	default:
		return CodexAuthUnknown, false
	}
}

func (c *Codex) codexAuthEvidence(ctx context.Context, path string, env []string) (CodexAuthEvidence, error) {
	if c == nil || c.Process == nil {
		return CodexAuthEvidence{Mode: CodexAuthUnknown, Reason: "Codex authentication probe is unavailable"}, errors.New("Codex authentication probe is unavailable")
	}
	probeCtx, cancel := context.WithTimeout(ctx, codexAuthProbeTimeout)
	defer cancel()
	result, runErr := c.Process(probeCtx, ProcessSpec{
		Path: path, Args: []string{"login", "status"}, Env: append([]string(nil), env...), MaxOutputBytes: 256 << 10,
	})
	if ctx.Err() != nil {
		return CodexAuthEvidence{Mode: CodexAuthUnknown, Reason: "authentication probe cancelled"}, ctx.Err()
	}
	status := append(append([]byte(nil), result.Stdout...), '\n')
	status = append(status, result.Stderr...)
	mode, parseErr := ParseCodexAuthStatus(status)
	if parseErr == nil && (runErr == nil || mode == CodexAuthUnauthenticated) {
		return CodexAuthEvidence{Mode: mode}, nil
	}
	if runErr != nil {
		return CodexAuthEvidence{Mode: CodexAuthUnknown, Reason: "authentication status probe failed"}, errors.New("Codex authentication status probe failed")
	}
	return CodexAuthEvidence{Mode: CodexAuthUnknown, Reason: "authentication status format is unknown"}, nil
}

// CodexChatGPTRouteSupported reports whether the current routing snapshot and
// environment positively identify the supported first-party ChatGPT route.
// An empty snapshot is the default route only when authentication evidence has
// already established ChatGPT login; it is never authentication evidence by
// itself.
func CodexChatGPTRouteSupported(runtime task.RuntimeSnapshot, environ []string) bool {
	if runtime.ProviderType != "" && !strings.EqualFold(runtime.ProviderType, "codex") {
		return false
	}
	if len(runtime.Auth) > 0 || len(runtime.AuthEnvRefs) > 0 {
		return false
	}
	if runtime.Endpoint == "" {
		if runtime.ProviderID != "" || runtime.WireAPI != "" || len(runtime.SDKSettings) > 0 {
			return false
		}
	} else {
		if !equivalentChatGPTRoute(runtime.Endpoint) {
			return false
		}
		if runtime.WireAPI != "" && !strings.EqualFold(runtime.WireAPI, "responses") {
			return false
		}
		for key := range runtime.SDKSettings {
			switch key {
			case "requires_openai_auth", "supports_websockets", "name":
			default:
				return false
			}
		}
	}
	for _, key := range []string{"OPENAI_BASE_URL", "OPENAI_API_BASE", "CODEX_BASE_URL", "CODEX_API_BASE"} {
		value, set, conflict := environmentRouteValue(environ, key)
		if conflict || set && value != "" && !equivalentChatGPTRoute(value) {
			return false
		}
	}
	for _, key := range []string{"CODEX_API_KEY", "OPENAI_API_KEY"} {
		if nonEmptyEnvironmentValue(environ, key) {
			return false
		}
	}
	return true
}

func nonEmptyEnvironmentValue(environ []string, wanted string) bool {
	for _, item := range environ {
		key, value, ok := strings.Cut(item, "=")
		if ok && strings.EqualFold(key, wanted) && value != "" {
			return true
		}
	}
	return false
}

func environmentRouteValue(environ []string, wanted string) (value string, set, conflict bool) {
	for _, item := range environ {
		key, candidate, ok := strings.Cut(item, "=")
		if !ok || !strings.EqualFold(key, wanted) {
			continue
		}
		if !set {
			value, set = candidate, true
			continue
		}
		if candidate != value {
			return "", true, true
		}
	}
	return value, set, false
}

func equivalentChatGPTRoute(value string) bool {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), codexChatGPTHost) || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return strings.TrimRight(u.EscapedPath(), "/") == "/backend-api/codex"
}

// CodexChatGPTRuntimeOverlay returns an immutable, ephemeral routing copy for
// the verified ChatGPT transport. It intentionally discards custom auth and
// gateway fields while retaining the selected CLI path and credential refs.
func CodexChatGPTRuntimeOverlay(runtime task.RuntimeSnapshot) task.RuntimeSnapshot {
	overlay := cloneRuntimeSnapshot(runtime)
	overlay.ProviderID = codexChatGPTProviderID
	overlay.Endpoint = codexChatGPTBaseURL
	overlay.WireAPI = "responses"
	overlay.Auth = nil
	overlay.SDKSettings = map[string]any{
		"name":                 "RoleMux ChatGPT",
		"requires_openai_auth": true,
		"supports_websockets":  false,
	}
	return overlay
}

func cloneRuntimeSnapshot(runtime task.RuntimeSnapshot) task.RuntimeSnapshot {
	clone := runtime
	clone.AuthEnvRefs = append([]string(nil), runtime.AuthEnvRefs...)
	clone.Auth = cloneAnyMap(runtime.Auth)
	clone.SDKSettings = cloneAnyMap(runtime.SDKSettings)
	return clone
}

func cloneAnyMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = cloneAny(value)
	}
	return result
}

func cloneAny(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return cloneAnyMap(item)
	case map[string]string:
		result := make(map[string]string, len(item))
		for key, value := range item {
			result[key] = value
		}
		return result
	case []any:
		result := make([]any, len(item))
		for index, value := range item {
			result[index] = cloneAny(value)
		}
		return result
	case []string:
		return append([]string(nil), item...)
	default:
		return value
	}
}

func codexLaunchReason(evidence CodexAuthEvidence, route bool) string {
	if evidence.Reason != "" {
		return evidence.Reason
	}
	if evidence.Mode != CodexAuthChatGPT {
		return fmt.Sprintf("Codex authentication mode %q is not ChatGPT", evidence.Mode)
	}
	if !route {
		return "Codex route is not the supported ChatGPT route"
	}
	return ""
}
