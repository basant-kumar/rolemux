package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/basant-kumar/rolemux/internal/task"
)

var (
	codexProviderID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	codexEnvName    = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// CodexConfigOverrides returns deterministic, non-secret `-c key=value`
// arguments for the exact safe routing snapshot. Runtime.SDKSettings is a
// typed-at-the-workflow boundary represented as JSON-compatible values so it
// can survive a restart without retaining credentials.
func CodexConfigOverrides(runtime task.RuntimeSnapshot) ([]string, error) {
	overrides := map[string]string{}
	if runtime.ProviderID != "" {
		if !codexProviderID.MatchString(runtime.ProviderID) {
			return nil, fmt.Errorf("unsafe Codex provider id %q", runtime.ProviderID)
		}
		overrides["model_provider"] = quoteTOML(runtime.ProviderID)
	}
	if runtime.Endpoint != "" {
		key := "openai_base_url"
		if runtime.ProviderID != "" {
			key = "model_providers." + runtime.ProviderID + ".base_url"
		}
		overrides[key] = quoteTOML(runtime.Endpoint)
	}
	if runtime.WireAPI != "" {
		if runtime.ProviderID == "" {
			return nil, errors.New("Codex wire_api requires a named provider")
		}
		overrides["model_providers."+runtime.ProviderID+".wire_api"] = quoteTOML(runtime.WireAPI)
	}
	for key, value := range runtime.SDKSettings {
		full, err := codexRoutingKey(runtime.ProviderID, key)
		if err != nil {
			return nil, fmt.Errorf("unsafe/unknown Codex routing field %q", key)
		}
		if err := validateCodexField(full, value); err != nil {
			return nil, fmt.Errorf("Codex routing field %s: %w", key, err)
		}
		encoded, err := codexValue(value)
		if err != nil {
			return nil, fmt.Errorf("Codex routing field %s: %w", key, err)
		}
		overrides[full] = encoded
	}
	for key, value := range runtime.Auth {
		if runtime.ProviderID == "" {
			return nil, errors.New("Codex auth metadata requires a named provider")
		}
		full := key
		if !strings.Contains(key, ".") {
			full = "model_providers." + runtime.ProviderID + ".auth." + key
		}
		if !isSafeCodexKey(full) || !strings.Contains(full, ".auth.") {
			return nil, fmt.Errorf("unsafe/unknown Codex auth field %q", key)
		}
		if err := validateCodexField(full, value); err != nil {
			return nil, fmt.Errorf("Codex auth field %s: %w", key, err)
		}
		encoded, err := codexValue(value)
		if err != nil {
			return nil, err
		}
		overrides[full] = encoded
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		args = append(args, "--config", key+"="+overrides[key])
	}
	return args, nil
}

func isSafeCodexKey(key string) bool {
	if key == "model_reasoning_effort" || key == "model_provider" || key == "openai_base_url" {
		return true
	}
	parts := strings.Split(key, ".")
	if len(parts) < 3 || parts[0] != "model_providers" || !codexProviderID.MatchString(parts[1]) {
		return false
	}
	if len(parts) == 3 {
		return safeProviderField(parts[2])
	}
	if len(parts) == 4 && parts[2] == "auth" {
		return safeAuthField(parts[3])
	}
	return false
}

func safeProviderField(value string) bool {
	switch value {
	case "name", "base_url", "wire_api", "env_key", "env_http_headers", "query_params", "request_max_retries", "stream_max_retries", "stream_idle_timeout_ms", "supports_standalone_web_search", "requires_openai_auth", "supports_websockets":
		return true
	default:
		return false
	}
}

func safeAuthField(value string) bool {
	switch value {
	case "command", "args", "timeout_ms", "refresh_interval_ms":
		return true
	default:
		return false
	}
}

func codexRoutingKey(providerID, key string) (string, error) {
	if isSafeCodexKey(key) {
		return key, nil
	}
	if providerID != "" && safeProviderField(key) {
		return "model_providers." + providerID + "." + key, nil
	}
	return "", errors.New("unknown field")
}

func validateCodexField(key string, value any) error {
	leaf := key[strings.LastIndex(key, ".")+1:]
	switch leaf {
	case "supports_standalone_web_search", "requires_openai_auth", "supports_websockets":
		if _, ok := value.(bool); !ok {
			return errors.New("field must be a boolean")
		}
	case "request_max_retries", "stream_max_retries", "stream_idle_timeout_ms":
		switch value.(type) {
		case int, int64, float64, json.Number:
		default:
			return errors.New("field must be numeric")
		}
	case "env_key":
		name, ok := value.(string)
		if !ok || !codexEnvName.MatchString(name) {
			return errors.New("env_key must name an environment variable")
		}
	case "env_http_headers":
		for _, name := range stringValues(value) {
			if !codexEnvName.MatchString(name) {
				return errors.New("env_http_headers values must name environment variables")
			}
		}
	case "command":
		command, ok := value.(string)
		if !ok || !safeCommand(command) {
			return errors.New("auth command must be a bare executable or absolute path")
		}
	case "args":
		for _, arg := range stringValues(value) {
			if strings.ContainsAny(arg, "\x00\r\n;&|`$<>") || credentialLiteral(arg) {
				return errors.New("unsafe auth argument")
			}
		}
	}
	return nil
}

func stringValues(value any) []string {
	var result []string
	switch values := value.(type) {
	case map[string]string:
		for _, value := range values {
			result = append(result, value)
		}
	case map[string]any:
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
	case []string:
		result = append(result, values...)
	case []any:
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
	}
	return result
}

func safeCommand(command string) bool {
	if command == "" || strings.ContainsAny(command, "\x00\r\n;&|`$<> ") {
		return false
	}
	return filepath.IsAbs(command) || filepath.Base(command) == command
}

func codexValue(value any) (string, error) {
	switch value := value.(type) {
	case string:
		if strings.ContainsAny(value, "\x00\r\n") || credentialLiteral(value) {
			return "", errors.New("literal credential or unsafe string")
		}
		return quoteTOML(value), nil
	case bool:
		return strconv.FormatBool(value), nil
	case int:
		return strconv.Itoa(value), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	case []string:
		items := make([]string, len(value))
		for i, item := range value {
			encoded, err := codexValue(item)
			if err != nil {
				return "", err
			}
			items[i] = encoded
		}
		return "[" + strings.Join(items, ", ") + "]", nil
	case []any:
		items := make([]string, len(value))
		for i, item := range value {
			encoded, err := codexValue(item)
			if err != nil {
				return "", err
			}
			items[i] = encoded
		}
		return "[" + strings.Join(items, ", ") + "]", nil
	case map[string]string:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			encoded, err := codexValue(value[key])
			if err != nil {
				return "", err
			}
			parts = append(parts, quoteTOML(key)+" = "+encoded)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			encoded, err := codexValue(value[key])
			if err != nil {
				return "", err
			}
			parts = append(parts, quoteTOML(key)+" = "+encoded)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	default:
		b, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("unsupported routing value %s", b)
	}
}

func quoteTOML(value string) string { b, _ := json.Marshal(value); return string(b) }
func credentialLiteral(value string) bool {
	// Uppercase identifiers are environment-variable references, not values.
	// Their contents are reacquired from the child environment at runtime.
	if codexEnvName.MatchString(value) {
		return false
	}
	lower := strings.ToLower(value)
	return strings.Contains(lower, "bearer ") || strings.Contains(lower, "api_key") || strings.Contains(lower, "token=") || strings.Contains(lower, "secret") || strings.HasPrefix(value, "sk-")
}
