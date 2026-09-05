// Package config implements RoleMux's layered, non-secret TOML
// configuration. Credentials remain in provider stores or are reacquired from
// validated environment-variable references; they are never serialized.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	providerid "github.com/basant-kumar/rolemux/internal/provider"
)

var (
	ErrUnknownRole      = errors.New("unknown role")
	ErrConfigConflict   = errors.New("configuration changed while it was being edited")
	ErrUnsafeConfig     = errors.New("unsafe configuration")
	ErrUnknownProvider  = errors.New("unknown provider")
	ErrMissingModel     = errors.New("profile has no model")
	ErrUnavailableModel = errors.New("configured model is unavailable")
	envNamePattern      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	modelIDPattern      = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)
	providerNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

const (
	RolePlanner      = "planner"
	RoleImplementer  = "implementer"
	RoleReviewer     = "reviewer"
	RolePlanReviewer = "plan_reviewer"
	RoleCodeReviewer = "code_reviewer"
)

var roleAliases = map[string]string{
	"planner": RolePlanner, "implementer": RoleImplementer, "reviewer": RoleReviewer,
	"plan-reviewer": RolePlanReviewer, "plan_reviewer": RolePlanReviewer,
	"code-reviewer": RoleCodeReviewer, "code_reviewer": RoleCodeReviewer,
}

type Profile struct {
	Provider string `toml:"provider" json:"provider"`
	Model    string `toml:"model" json:"model"`
	Effort   string `toml:"effort,omitempty" json:"effort,omitempty"`
	Speed    string `toml:"speed,omitempty" json:"speed,omitempty"`
}

type AuthCommand struct {
	Command           string   `toml:"command,omitempty" json:"command,omitempty"`
	Args              []string `toml:"args,omitempty" json:"args,omitempty"`
	TimeoutMS         int      `toml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
	RefreshIntervalMS int      `toml:"refresh_interval_ms,omitempty" json:"refresh_interval_ms,omitempty"`
}

// Provider contains only fixed, documented routing fields. The *Env fields
// are names, not values. Any map is validated before being sent to a provider.
type Provider struct {
	Name      string `toml:"name,omitempty" json:"name,omitempty"`
	Type      string `toml:"type,omitempty" json:"type,omitempty"`
	WireAPI   string `toml:"wire_api,omitempty" json:"wire_api,omitempty"`
	Transport string `toml:"transport,omitempty" json:"transport,omitempty"`
	CLIPath   string `toml:"cli_path,omitempty" json:"cli_path,omitempty"`

	BaseURL                     string            `toml:"base_url,omitempty" json:"base_url,omitempty"`
	GatewayURL                  string            `toml:"gateway_url,omitempty" json:"gateway_url,omitempty"`
	Headers                     map[string]string `toml:"headers,omitempty" json:"headers,omitempty"`
	ModelID                     string            `toml:"model_id,omitempty" json:"model_id,omitempty"`
	WireModel                   string            `toml:"wire_model,omitempty" json:"wire_model,omitempty"`
	EnvKey                      string            `toml:"env_key,omitempty" json:"env_key,omitempty"`
	BearerTokenEnv              string            `toml:"bearer_token_env,omitempty" json:"bearer_token_env,omitempty"`
	EnvHTTPHeaders              map[string]string `toml:"env_http_headers,omitempty" json:"env_http_headers,omitempty"`
	QueryParams                 map[string]string `toml:"query_params,omitempty" json:"query_params,omitempty"`
	RequestMaxRetries           int               `toml:"request_max_retries,omitempty" json:"request_max_retries,omitempty"`
	StreamMaxRetries            int               `toml:"stream_max_retries,omitempty" json:"stream_max_retries,omitempty"`
	StreamIdleTimeoutMS         int               `toml:"stream_idle_timeout_ms,omitempty" json:"stream_idle_timeout_ms,omitempty"`
	SupportsStandaloneWebSearch bool              `toml:"supports_standalone_web_search,omitempty" json:"supports_standalone_web_search,omitempty"`
	RequiresOpenAIAuth          bool              `toml:"requires_openai_auth,omitempty" json:"requires_openai_auth,omitempty"`
	Auth                        AuthCommand       `toml:"auth,omitempty" json:"auth,omitempty"`

	// Claude's recognized gateway/BYOK references.
	APIKeyEnv          string            `toml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	AuthTokenEnv       string            `toml:"auth_token_env,omitempty" json:"auth_token_env,omitempty"`
	BedrockProfileEnv  string            `toml:"bedrock_profile_env,omitempty" json:"bedrock_profile_env,omitempty"`
	BedrockRegionEnv   string            `toml:"bedrock_region_env,omitempty" json:"bedrock_region_env,omitempty"`
	VertexProjectEnv   string            `toml:"vertex_project_env,omitempty" json:"vertex_project_env,omitempty"`
	VertexRegionEnv    string            `toml:"vertex_region_env,omitempty" json:"vertex_region_env,omitempty"`
	FoundryEndpointEnv string            `toml:"foundry_endpoint_env,omitempty" json:"foundry_endpoint_env,omitempty"`
	FoundryAPIKeyEnv   string            `toml:"foundry_api_key_env,omitempty" json:"foundry_api_key_env,omitempty"`
	EnvRefs            map[string]string `toml:"env_refs,omitempty" json:"env_refs,omitempty"`

	// Copilot SDK-safe settings. Static bearer/API values are rejected.
	MaxPromptTokens int               `toml:"max_prompt_tokens,omitempty" json:"max_prompt_tokens,omitempty"`
	MaxOutputTokens int               `toml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	APIVersion      string            `toml:"api_version,omitempty" json:"api_version,omitempty"`
	SDKSettings     map[string]string `toml:"sdk_settings,omitempty" json:"sdk_settings,omitempty"`
}

type CustomModel struct {
	ID            string   `toml:"id" json:"id"`
	Label         string   `toml:"label,omitempty" json:"label,omitempty"`
	Aliases       []string `toml:"aliases,omitempty" json:"aliases,omitempty"`
	Efforts       []string `toml:"efforts,omitempty" json:"efforts,omitempty"`
	DefaultEffort string   `toml:"default_effort,omitempty" json:"default_effort,omitempty"`
	Availability  string   `toml:"availability" json:"availability"`

	// Codex custom routing fields.
	Name                        string            `toml:"name,omitempty" json:"name,omitempty"`
	BaseURL                     string            `toml:"base_url,omitempty" json:"base_url,omitempty"`
	WireAPI                     string            `toml:"wire_api,omitempty" json:"wire_api,omitempty"`
	EnvKey                      string            `toml:"env_key,omitempty" json:"env_key,omitempty"`
	EnvHTTPHeaders              map[string]string `toml:"env_http_headers,omitempty" json:"env_http_headers,omitempty"`
	QueryParams                 map[string]string `toml:"query_params,omitempty" json:"query_params,omitempty"`
	RequestMaxRetries           int               `toml:"request_max_retries,omitempty" json:"request_max_retries,omitempty"`
	StreamMaxRetries            int               `toml:"stream_max_retries,omitempty" json:"stream_max_retries,omitempty"`
	StreamIdleTimeoutMS         int               `toml:"stream_idle_timeout_ms,omitempty" json:"stream_idle_timeout_ms,omitempty"`
	SupportsStandaloneWebSearch bool              `toml:"supports_standalone_web_search,omitempty" json:"supports_standalone_web_search,omitempty"`
	RequiresOpenAIAuth          bool              `toml:"requires_openai_auth,omitempty" json:"requires_openai_auth,omitempty"`
	Auth                        AuthCommand       `toml:"auth,omitempty" json:"auth,omitempty"`

	// Copilot named-provider model fields. Statically configured Copilot
	// models remain fail-closed until live SDK discovery verifies them.
	Provider               string         `toml:"provider,omitempty" json:"provider,omitempty"`
	WireModel              string         `toml:"wire_model,omitempty" json:"wire_model,omitempty"`
	ModelID                string         `toml:"model_id,omitempty" json:"model_id,omitempty"`
	MaxPromptTokens        int            `toml:"max_prompt_tokens,omitempty" json:"max_prompt_tokens,omitempty"`
	MaxContextWindowTokens int            `toml:"max_context_window_tokens,omitempty" json:"max_context_window_tokens,omitempty"`
	MaxOutputTokens        int            `toml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	Capabilities           map[string]any `toml:"capabilities,omitempty" json:"capabilities,omitempty"`
}

// Config is the merged representation. Raw preserves unknown, unrelated TOML
// keys during an atomic profile update; it is never sent to a provider.
type Config struct {
	Profiles                   map[string]Profile                `toml:"profiles" json:"profiles"`
	Providers                  map[string]Provider               `toml:"providers" json:"providers"`
	Models                     map[string]map[string]CustomModel `toml:"models" json:"models"`
	CatalogTTLSeconds          int                               `toml:"catalog_ttl_seconds,omitempty" json:"catalog_ttl_seconds,omitempty"`
	ProviderTurnTimeoutSeconds int                               `toml:"provider_turn_timeout_seconds,omitempty" json:"provider_turn_timeout_seconds,omitempty"`
	Raw                        map[string]any                    `toml:"-" json:"-"`
}

func Default() Config {
	return Config{
		Profiles: map[string]Profile{
			RolePlanner:     {Provider: "codex"},
			RoleReviewer:    {Provider: "claude"},
			RoleImplementer: {Provider: "codex"},
		},
		Providers: map[string]Provider{}, Models: map[string]map[string]CustomModel{},
		CatalogTTLSeconds: 86400, ProviderTurnTimeoutSeconds: 900,
	}
}

func CanonicalRole(role string) (string, error) {
	canonical, ok := roleAliases[strings.ToLower(strings.TrimSpace(role))]
	if !ok {
		return "", fmt.Errorf("%w %q", ErrUnknownRole, role)
	}
	return canonical, nil
}

func ProfileRoles() []string {
	return []string{RolePlanner, RoleImplementer, RoleReviewer, RolePlanReviewer, RoleCodeReviewer}
}

// EffectiveProfiles expands the shared reviewer profile into the two review
// roles, returning independent values suitable for task snapshots.
func (c Config) EffectiveProfiles() (map[string]Profile, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	result := make(map[string]Profile, 4)
	for _, role := range []string{RolePlanner, RoleImplementer} {
		result[role] = c.Profiles[role]
	}
	reviewer := c.Profiles[RoleReviewer]
	result[RolePlanReviewer] = reviewer
	result[RoleCodeReviewer] = reviewer
	if p, ok := c.Profiles[RolePlanReviewer]; ok {
		result[RolePlanReviewer] = p
	}
	if p, ok := c.Profiles[RoleCodeReviewer]; ok {
		result[RoleCodeReviewer] = p
	}
	for role, profile := range result {
		if err := c.validateEffectiveProfile(role, profile); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (c Config) validateEffectiveProfile(role string, profile Profile) error {
	if err := ValidateProfile(profile); err != nil {
		return fmt.Errorf("profile %s: %w", role, err)
	}
	for name, model := range c.Models[profile.Provider] {
		if profile.Model != name && profile.Model != model.ID && !contains(model.Aliases, profile.Model) {
			continue
		}
		if model.Availability == "unavailable" {
			return fmt.Errorf("profile %s: %w %q", role, ErrUnavailableModel, profile.Model)
		}
		if profile.Effort != "" && len(model.Efforts) > 0 && !contains(model.Efforts, profile.Effort) {
			return fmt.Errorf("profile %s: model %q does not support effort %q", role, profile.Model, profile.Effort)
		}
		return nil
	}
	return nil
}

func (c Config) Profile(role string) (Profile, error) {
	canonical, err := CanonicalRole(role)
	if err != nil {
		return Profile{}, err
	}
	profiles, err := c.EffectiveProfiles()
	if err != nil {
		return Profile{}, err
	}
	p, ok := profiles[canonical]
	if !ok {
		return Profile{}, fmt.Errorf("%w %q", ErrUnknownRole, role)
	}
	return p, nil
}

func (c Config) Provider(name string) Provider { return c.Providers[name] }

// ResolveProfile canonicalizes a configured custom-model alias and overlays
// that model's safe route onto its provider route for task snapshotting.
func (c Config) ResolveProfile(profile Profile) (Profile, Provider) {
	provider := c.Provider(profile.Provider)
	for name, model := range c.Models[profile.Provider] {
		if profile.Model != name && profile.Model != model.ID && !contains(model.Aliases, profile.Model) {
			continue
		}
		profile.Model = model.ID
		if profile.Model == "" {
			profile.Model = name
		}
		if profile.Provider == "codex" {
			if model.Name != "" {
				provider.Name = model.Name
			}
			if model.BaseURL != "" {
				provider.BaseURL, provider.GatewayURL = model.BaseURL, ""
			}
			if model.WireAPI != "" {
				provider.WireAPI = model.WireAPI
			}
			if model.EnvKey != "" {
				provider.EnvKey = model.EnvKey
			}
			if model.EnvHTTPHeaders != nil {
				provider.EnvHTTPHeaders = cloneStringMap(model.EnvHTTPHeaders)
			}
			if model.QueryParams != nil {
				provider.QueryParams = cloneStringMap(model.QueryParams)
			}
			if model.RequestMaxRetries != 0 {
				provider.RequestMaxRetries = model.RequestMaxRetries
			}
			if model.StreamMaxRetries != 0 {
				provider.StreamMaxRetries = model.StreamMaxRetries
			}
			if model.StreamIdleTimeoutMS != 0 {
				provider.StreamIdleTimeoutMS = model.StreamIdleTimeoutMS
			}
			if model.SupportsStandaloneWebSearch {
				provider.SupportsStandaloneWebSearch = true
			}
			if model.RequiresOpenAIAuth {
				provider.RequiresOpenAIAuth = true
			}
			if model.Auth.Command != "" {
				provider.Auth = model.Auth
			}
		}
		break
	}
	return profile, provider
}

func ConfigPaths(repoRoot string, environ []string) (global, project string) {
	env := envMap(environ)
	if home := strings.TrimSpace(env["HOME"]); home != "" {
		global = filepath.Join(home, ".rolemux", "config.toml")
	}
	if repoRoot != "" {
		project = filepath.Join(repoRoot, ".rolemux.toml")
	}
	return global, project
}

func Load(repoRoot string) (Config, error) { return LoadWithEnv(repoRoot, os.Environ()) }

func LoadWithEnv(repoRoot string, environ []string) (Config, error) {
	cfg := Default()
	env := envMap(environ)
	paths := []string{}
	if explicit := strings.TrimSpace(env["ROLEMUX_CONFIG"]); explicit != "" {
		paths = append(paths, explicit)
	} else {
		global, project := ConfigPaths(repoRoot, environ)
		paths = append(paths, global, project)
	}
	for _, name := range paths {
		if name == "" {
			continue
		}
		if err := mergeFile(&cfg, name); err != nil {
			return Config{}, err
		}
	}
	applyEnvironment(&cfg, env)
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func mergeFile(cfg *Config, name string) error {
	b, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config %s: %w", name, err)
	}
	var layer Config
	if _, err := toml.Decode(string(b), &layer); err != nil {
		return fmt.Errorf("parse config %s: %w", name, err)
	}
	var raw map[string]any
	if _, err := toml.Decode(string(b), &raw); err != nil {
		return fmt.Errorf("parse config %s: %w", name, err)
	}
	merge(cfg, layer)
	if cfg.Raw == nil {
		cfg.Raw = map[string]any{}
	}
	deepMerge(cfg.Raw, raw)
	return nil
}

func merge(dst *Config, src Config) {
	if dst.Profiles == nil {
		dst.Profiles = map[string]Profile{}
	}
	for role, p := range src.Profiles {
		old := dst.Profiles[role]
		if p.Provider != "" {
			old.Provider = p.Provider
		}
		if p.Model != "" {
			old.Model = p.Model
		}
		if p.Effort != "" {
			old.Effort = p.Effort
		}
		if p.Speed != "" {
			old.Speed = p.Speed
		}
		dst.Profiles[role] = old
	}
	if dst.Providers == nil {
		dst.Providers = map[string]Provider{}
	}
	for name, p := range src.Providers {
		old := dst.Providers[name]
		mergeProvider(&old, p)
		dst.Providers[name] = old
	}
	if dst.Models == nil {
		dst.Models = map[string]map[string]CustomModel{}
	}
	for provider, models := range src.Models {
		if dst.Models[provider] == nil {
			dst.Models[provider] = map[string]CustomModel{}
		}
		for name, model := range models {
			dst.Models[provider][name] = model
		}
	}
	if src.CatalogTTLSeconds != 0 {
		dst.CatalogTTLSeconds = src.CatalogTTLSeconds
	}
	if src.ProviderTurnTimeoutSeconds != 0 {
		dst.ProviderTurnTimeoutSeconds = src.ProviderTurnTimeoutSeconds
	}
}

func mergeProvider(dst *Provider, src Provider) {
	if src.Name != "" {
		dst.Name = src.Name
	}
	if src.Type != "" {
		dst.Type = src.Type
	}
	if src.WireAPI != "" {
		dst.WireAPI = src.WireAPI
	}
	if src.Transport != "" {
		dst.Transport = src.Transport
	}
	if src.CLIPath != "" {
		dst.CLIPath = src.CLIPath
	}
	if src.BaseURL != "" {
		dst.BaseURL = src.BaseURL
	}
	if src.GatewayURL != "" {
		dst.GatewayURL = src.GatewayURL
	}
	if src.Headers != nil {
		dst.Headers = cloneStringMap(src.Headers)
	}
	if src.ModelID != "" {
		dst.ModelID = src.ModelID
	}
	if src.WireModel != "" {
		dst.WireModel = src.WireModel
	}
	if src.EnvKey != "" {
		dst.EnvKey = src.EnvKey
	}
	if src.BearerTokenEnv != "" {
		dst.BearerTokenEnv = src.BearerTokenEnv
	}
	if src.EnvHTTPHeaders != nil {
		dst.EnvHTTPHeaders = cloneStringMap(src.EnvHTTPHeaders)
	}
	if src.QueryParams != nil {
		dst.QueryParams = cloneStringMap(src.QueryParams)
	}
	if src.RequestMaxRetries != 0 {
		dst.RequestMaxRetries = src.RequestMaxRetries
	}
	if src.StreamMaxRetries != 0 {
		dst.StreamMaxRetries = src.StreamMaxRetries
	}
	if src.StreamIdleTimeoutMS != 0 {
		dst.StreamIdleTimeoutMS = src.StreamIdleTimeoutMS
	}
	if src.SupportsStandaloneWebSearch {
		dst.SupportsStandaloneWebSearch = true
	}
	if src.RequiresOpenAIAuth {
		dst.RequiresOpenAIAuth = true
	}
	if src.Auth.Command != "" {
		dst.Auth = src.Auth
	}
	if src.APIKeyEnv != "" {
		dst.APIKeyEnv = src.APIKeyEnv
	}
	if src.AuthTokenEnv != "" {
		dst.AuthTokenEnv = src.AuthTokenEnv
	}
	if src.BedrockProfileEnv != "" {
		dst.BedrockProfileEnv = src.BedrockProfileEnv
	}
	if src.BedrockRegionEnv != "" {
		dst.BedrockRegionEnv = src.BedrockRegionEnv
	}
	if src.VertexProjectEnv != "" {
		dst.VertexProjectEnv = src.VertexProjectEnv
	}
	if src.VertexRegionEnv != "" {
		dst.VertexRegionEnv = src.VertexRegionEnv
	}
	if src.FoundryEndpointEnv != "" {
		dst.FoundryEndpointEnv = src.FoundryEndpointEnv
	}
	if src.FoundryAPIKeyEnv != "" {
		dst.FoundryAPIKeyEnv = src.FoundryAPIKeyEnv
	}
	if src.EnvRefs != nil {
		dst.EnvRefs = cloneStringMap(src.EnvRefs)
	}
	if src.MaxPromptTokens != 0 {
		dst.MaxPromptTokens = src.MaxPromptTokens
	}
	if src.MaxOutputTokens != 0 {
		dst.MaxOutputTokens = src.MaxOutputTokens
	}
	if src.APIVersion != "" {
		dst.APIVersion = src.APIVersion
	}
	if src.SDKSettings != nil {
		dst.SDKSettings = cloneStringMap(src.SDKSettings)
	}
}

func cloneStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func applyEnvironment(cfg *Config, env map[string]string) {
	if cfg.Providers == nil {
		cfg.Providers = map[string]Provider{}
	}
	for provider, variable := range map[string]string{"codex": "CODEX_CLI_PATH", "claude": "CLAUDE_CLI_PATH", "copilot": "COPILOT_CLI_PATH"} {
		if value := strings.TrimSpace(env[variable]); value != "" {
			p := cfg.Providers[provider]
			p.CLIPath = value
			cfg.Providers[provider] = p
		}
	}
	for provider, variable := range map[string]string{"codex": "CODEX_GATEWAY_URL", "claude": "CLAUDE_GATEWAY_URL", "copilot": "COPILOT_GATEWAY_URL"} {
		if value := strings.TrimSpace(env[variable]); value != "" {
			p := cfg.Providers[provider]
			p.GatewayURL = value
			cfg.Providers[provider] = p
		}
	}
}

func envMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, item := range environ {
		if k, v, ok := strings.Cut(item, "="); ok {
			out[k] = v
		}
	}
	return out
}

func Validate(c Config) error {
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	for role, p := range c.Profiles {
		if _, err := CanonicalRole(role); err != nil {
			return err
		}
		if p.Provider != "" && !providerid.Known(p.Provider) {
			return fmt.Errorf("%w %q", ErrUnknownProvider, p.Provider)
		}
		if p.Model != "" && !modelIDPattern.MatchString(p.Model) {
			return fmt.Errorf("profile %s has invalid model ID", role)
		}
		if p.Effort != "" && !validEffort(p.Effort) {
			return fmt.Errorf("profile %s has invalid effort %q", role, p.Effort)
		}
		if p.Speed != "" && !modelIDPattern.MatchString(p.Speed) {
			return fmt.Errorf("profile %s has invalid speed %q", role, p.Speed)
		}
		if credentialLike(p.Model) || credentialLike(p.Provider) {
			return fmt.Errorf("%w: profile %s contains credential-like data", ErrUnsafeConfig, role)
		}
	}
	if c.CatalogTTLSeconds < 0 {
		return errors.New("catalog_ttl_seconds must not be negative")
	}
	if c.ProviderTurnTimeoutSeconds != 0 && (c.ProviderTurnTimeoutSeconds < 30 || c.ProviderTurnTimeoutSeconds > 7200) {
		return errors.New("provider_turn_timeout_seconds must be between 30 and 7200")
	}
	for name, p := range c.Providers {
		if !providerNamePattern.MatchString(name) {
			return fmt.Errorf("invalid provider ID %q", name)
		}
		if err := ValidateProvider(name, p); err != nil {
			return err
		}
	}
	for provider, models := range c.Models {
		if !providerid.Known(provider) {
			return fmt.Errorf("%w %q", ErrUnknownProvider, provider)
		}
		selectors := map[string]string{}
		for name, model := range models {
			if err := ValidateCustomModel(provider, name, model); err != nil {
				return err
			}
			for _, selector := range append([]string{name, model.ID}, model.Aliases...) {
				if owner, exists := selectors[selector]; exists && owner != name {
					return fmt.Errorf("custom %s model selector %q is shared by %q and %q", provider, selector, owner, name)
				}
				selectors[selector] = name
			}
		}
	}
	return nil
}

func credentialLike(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "bearer ") || strings.Contains(lower, "sk-") ||
		strings.Contains(lower, "token=") || strings.Contains(lower, "token:") ||
		strings.Contains(lower, "api_key=") || strings.Contains(lower, "apikey=") ||
		strings.Contains(lower, "secret") || strings.Contains(lower, "-----begin ")
}

func credentialField(name string) bool {
	n := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "-", "_"), " ", "_"))
	return strings.Contains(n, "authorization") || strings.Contains(n, "api_key") ||
		strings.Contains(n, "apikey") || strings.Contains(n, "token") ||
		strings.Contains(n, "secret") || strings.Contains(n, "cookie") || strings.Contains(n, "password")
}

func validEffort(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func ValidateEnvRef(name string) error {
	if name == "" || !envNamePattern.MatchString(name) {
		return fmt.Errorf("%w: invalid environment reference", ErrUnsafeConfig)
	}
	return nil
}

func ValidateProvider(name string, p Provider) error {
	if !providerid.Known(name) {
		return fmt.Errorf("%w %q", ErrUnknownProvider, name)
	}
	for _, ref := range []string{p.EnvKey, p.BearerTokenEnv, p.APIKeyEnv, p.AuthTokenEnv, p.BedrockProfileEnv, p.BedrockRegionEnv, p.VertexProjectEnv, p.VertexRegionEnv, p.FoundryEndpointEnv, p.FoundryAPIKeyEnv} {
		if ref != "" {
			if err := ValidateEnvRef(ref); err != nil {
				return fmt.Errorf("provider %s: %w", name, err)
			}
		}
	}
	for key, ref := range p.EnvRefs {
		if err := ValidateEnvRef(key); err != nil {
			return fmt.Errorf("provider %s env_refs target %q: %w", name, key, err)
		}
		if err := ValidateEnvRef(ref); err != nil {
			return fmt.Errorf("provider %s env_refs source for %s: %w", name, key, err)
		}
	}
	for key, value := range p.Headers {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") || credentialField(key) || credentialLike(value) || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: provider %s headers", ErrUnsafeConfig, name)
		}
	}
	for header, ref := range p.EnvHTTPHeaders {
		if strings.TrimSpace(header) == "" || strings.ContainsAny(header, "\r\n") {
			return fmt.Errorf("%w: provider %s env_http_headers", ErrUnsafeConfig, name)
		}
		if err := ValidateEnvRef(ref); err != nil {
			return fmt.Errorf("provider %s env_http_headers %s: %w", name, header, err)
		}
	}
	for key, value := range p.QueryParams {
		if key == "" || credentialField(key) || credentialLike(value) || strings.ContainsAny(key+value, "\r\n") {
			return fmt.Errorf("%w: provider %s query_params", ErrUnsafeConfig, name)
		}
	}
	for _, endpoint := range []string{p.BaseURL, p.GatewayURL} {
		if endpoint != "" {
			if err := validateEndpoint(endpoint); err != nil {
				return fmt.Errorf("provider %s: %w", name, err)
			}
		}
	}
	if p.CLIPath != "" && (!filepath.IsAbs(p.CLIPath) || strings.ContainsAny(p.CLIPath, "\x00\r\n")) {
		return fmt.Errorf("provider %s: cli_path must be an absolute path", name)
	}
	if err := validateProviderFields(name, p); err != nil {
		return err
	}
	if err := ValidateAuthCommand(p.Auth); err != nil {
		return fmt.Errorf("provider %s: %w", name, err)
	}
	if p.RequestMaxRetries < 0 || p.StreamMaxRetries < 0 || p.StreamIdleTimeoutMS < 0 || p.MaxPromptTokens < 0 || p.MaxOutputTokens < 0 {
		return fmt.Errorf("provider %s has negative limit", name)
	}
	return nil
}

func validateEndpoint(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("invalid http(s) endpoint")
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%w: endpoint contains userinfo or fragment", ErrUnsafeConfig)
	}
	for key, values := range u.Query() {
		if credentialField(key) {
			return fmt.Errorf("%w: endpoint contains credential query", ErrUnsafeConfig)
		}
		for _, value := range values {
			if credentialLike(value) {
				return fmt.Errorf("%w: endpoint contains credential query", ErrUnsafeConfig)
			}
		}
	}
	return nil
}

func validateProviderFields(name string, p Provider) error {
	unsupported := func(condition bool, field string) error {
		if condition {
			return fmt.Errorf("provider %s: unsupported field %s", name, field)
		}
		return nil
	}
	check := func(fields ...struct {
		set  bool
		name string
	}) error {
		for _, field := range fields {
			if err := unsupported(field.set, field.name); err != nil {
				return err
			}
		}
		return nil
	}
	field := func(set bool, name string) struct {
		set  bool
		name string
	} {
		return struct {
			set  bool
			name string
		}{set, name}
	}

	switch name {
	case "codex":
		if err := check(
			field(p.Type != "", "type"), field(p.Transport != "", "transport"),
			field(len(p.Headers) > 0, "headers"), field(p.ModelID != "", "model_id"),
			field(p.WireModel != "", "wire_model"), field(p.BearerTokenEnv != "", "bearer_token_env"),
			field(p.APIKeyEnv != "", "api_key_env"), field(p.AuthTokenEnv != "", "auth_token_env"),
			field(p.BedrockProfileEnv != "" || p.BedrockRegionEnv != "" || p.VertexProjectEnv != "" || p.VertexRegionEnv != "" || p.FoundryEndpointEnv != "" || p.FoundryAPIKeyEnv != "", "claude environment references"),
			field(len(p.EnvRefs) > 0, "env_refs"), field(p.MaxPromptTokens != 0 || p.MaxOutputTokens != 0 || p.APIVersion != "" || len(p.SDKSettings) > 0, "copilot SDK settings"),
		); err != nil {
			return err
		}
	case "claude":
		if err := check(
			field(p.Name != "", "name"),
			field(p.Type != "" || p.WireAPI != "" || p.Transport != "", "wire routing"),
			field(p.BaseURL != "", "base_url"), field(len(p.Headers) > 0, "headers"),
			field(p.ModelID != "" || p.WireModel != "", "model routing"),
			field(p.EnvKey != "" || p.BearerTokenEnv != "" || len(p.EnvHTTPHeaders) > 0 || len(p.QueryParams) > 0, "codex/copilot auth routing"),
			field(p.RequestMaxRetries != 0 || p.StreamMaxRetries != 0 || p.StreamIdleTimeoutMS != 0 || p.SupportsStandaloneWebSearch || p.RequiresOpenAIAuth || p.Auth.Command != "" || len(p.Auth.Args) > 0 || p.Auth.TimeoutMS != 0 || p.Auth.RefreshIntervalMS != 0, "codex routing"),
			field(p.MaxPromptTokens != 0 || p.MaxOutputTokens != 0 || p.APIVersion != "" || len(p.SDKSettings) > 0, "copilot SDK settings"),
		); err != nil {
			return err
		}
	case "copilot":
		if err := check(
			field(p.Name != "", "name"),
			field(p.EnvKey != "" || len(p.EnvHTTPHeaders) > 0 || len(p.QueryParams) > 0, "codex environment routing"),
			field(p.RequestMaxRetries != 0 || p.StreamMaxRetries != 0 || p.StreamIdleTimeoutMS != 0 || p.SupportsStandaloneWebSearch || p.RequiresOpenAIAuth || p.Auth.Command != "" || len(p.Auth.Args) > 0 || p.Auth.TimeoutMS != 0 || p.Auth.RefreshIntervalMS != 0, "codex routing"),
			field(p.APIKeyEnv != "" || p.AuthTokenEnv != "" || p.BedrockProfileEnv != "" || p.BedrockRegionEnv != "" || p.VertexProjectEnv != "" || p.VertexRegionEnv != "" || p.FoundryEndpointEnv != "" || p.FoundryAPIKeyEnv != "" || len(p.EnvRefs) > 0, "claude environment references"),
			field(len(p.SDKSettings) > 0, "sdk_settings"),
		); err != nil {
			return err
		}
		if p.Type != "" && p.Type != "openai" && p.Type != "azure" && p.Type != "anthropic" {
			return fmt.Errorf("provider copilot: unsupported type %q", p.Type)
		}
		if p.WireAPI != "" && p.WireAPI != "completions" && p.WireAPI != "responses" {
			return fmt.Errorf("provider copilot: unsupported wire_api %q", p.WireAPI)
		}
		if p.Transport != "" && p.Transport != "http" && p.Transport != "websockets" {
			return fmt.Errorf("provider copilot: unsupported transport %q", p.Transport)
		}
		if p.APIVersion != "" && p.Type != "azure" {
			return fmt.Errorf("provider copilot: api_version requires type azure")
		}
		if p.BaseURL != "" && p.GatewayURL != "" {
			return fmt.Errorf("provider copilot: base_url and gateway_url are mutually exclusive")
		}
	}
	return nil
}

func ValidateAuthCommand(auth AuthCommand) error {
	if auth.Command == "" {
		if len(auth.Args) > 0 || auth.TimeoutMS != 0 || auth.RefreshIntervalMS != 0 {
			return errors.New("auth command settings require command")
		}
		return nil
	}
	if strings.ContainsAny(auth.Command, ";&|<>$`\x00\n\r") || strings.ContainsAny(filepath.Base(auth.Command), " \t") {
		return fmt.Errorf("%w: auth command must be one executable", ErrUnsafeConfig)
	}
	if auth.Command == "." || auth.Command == ".." || (!filepath.IsAbs(auth.Command) && filepath.Base(auth.Command) != auth.Command) {
		return fmt.Errorf("%w: auth command must be a bare executable or absolute path", ErrUnsafeConfig)
	}
	switch strings.ToLower(filepath.Base(auth.Command)) {
	case "sh", "bash", "dash", "zsh", "fish", "csh", "tcsh", "ksh", "env":
		return fmt.Errorf("%w: auth command may not invoke a shell", ErrUnsafeConfig)
	}
	for _, arg := range auth.Args {
		if strings.ContainsAny(arg, ";&|<>$`\x00\n\r") || credentialLike(arg) {
			return fmt.Errorf("%w: auth command args may not contain shell syntax or secrets", ErrUnsafeConfig)
		}
	}
	if auth.TimeoutMS < 0 || auth.RefreshIntervalMS < 0 {
		return errors.New("auth command timeouts must not be negative")
	}
	return nil
}

func ValidateCustomModel(provider, name string, m CustomModel) error {
	if !providerNamePattern.MatchString(name) || m.ID == "" || !modelIDPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid custom %s model %q", provider, name)
	}
	if m.Availability != "available" && m.Availability != "unknown" && m.Availability != "unavailable" {
		return fmt.Errorf("custom model %s has invalid availability", name)
	}
	if provider == "copilot" {
		return fmt.Errorf("copilot custom models require live SDK discovery")
	}
	seen := map[string]bool{}
	for _, alias := range m.Aliases {
		if alias == "" || !modelIDPattern.MatchString(alias) || seen[alias] || alias == m.ID {
			return fmt.Errorf("custom model %s has duplicate/empty aliases", name)
		}
		seen[alias] = true
	}
	if m.DefaultEffort != "" && !contains(m.Efforts, m.DefaultEffort) {
		return fmt.Errorf("custom model %s default effort is not supported", name)
	}
	for i, effort := range m.Efforts {
		if !validEffort(effort) || contains(m.Efforts[:i], effort) {
			return fmt.Errorf("custom model %s has invalid/duplicate effort %q", name, effort)
		}
	}
	if m.RequestMaxRetries < 0 || m.StreamMaxRetries < 0 || m.StreamIdleTimeoutMS < 0 || m.MaxPromptTokens < 0 || m.MaxContextWindowTokens < 0 || m.MaxOutputTokens < 0 {
		return fmt.Errorf("custom model %s has negative limit", name)
	}
	if provider == "claude" && hasCustomRouting(m) {
		return fmt.Errorf("claude custom model %s contains codex routing", name)
	}
	if provider == "codex" {
		if m.Provider != "" || m.WireModel != "" || m.ModelID != "" || m.MaxPromptTokens != 0 || m.MaxContextWindowTokens != 0 || m.MaxOutputTokens != 0 || len(m.Capabilities) > 0 {
			return fmt.Errorf("codex custom model %s contains copilot routing", name)
		}
		if err := ValidateEnvRefOptional(m.EnvKey); err != nil {
			return err
		}
		for header, ref := range m.EnvHTTPHeaders {
			if strings.TrimSpace(header) == "" || strings.ContainsAny(header, "\r\n") {
				return fmt.Errorf("custom model %s has invalid env_http_headers", name)
			}
			if err := ValidateEnvRef(ref); err != nil {
				return fmt.Errorf("custom model %s env_http_headers %s: %w", name, header, err)
			}
		}
		for key, value := range m.QueryParams {
			if key == "" || credentialField(key) || credentialLike(value) {
				return fmt.Errorf("%w: custom model %s query_params", ErrUnsafeConfig, name)
			}
		}
		if m.BaseURL != "" {
			if err := validateEndpoint(m.BaseURL); err != nil {
				return fmt.Errorf("custom model %s: %w", name, err)
			}
		}
		if err := ValidateAuthCommand(m.Auth); err != nil {
			return err
		}
	}
	return nil
}

func hasCustomRouting(m CustomModel) bool {
	return m.Name != "" || m.BaseURL != "" || m.WireAPI != "" || m.EnvKey != "" ||
		len(m.EnvHTTPHeaders) > 0 || len(m.QueryParams) > 0 || m.RequestMaxRetries != 0 ||
		m.StreamMaxRetries != 0 || m.StreamIdleTimeoutMS != 0 || m.SupportsStandaloneWebSearch ||
		m.RequiresOpenAIAuth || m.Auth.Command != "" || len(m.Auth.Args) > 0 ||
		m.Auth.TimeoutMS != 0 || m.Auth.RefreshIntervalMS != 0 || m.Provider != "" ||
		m.WireModel != "" || m.ModelID != "" || m.MaxPromptTokens != 0 ||
		m.MaxContextWindowTokens != 0 || m.MaxOutputTokens != 0 || len(m.Capabilities) > 0
}

func ValidateEnvRefOptional(value string) error {
	if value == "" {
		return nil
	}
	return ValidateEnvRef(value)
}
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// FileHash returns the hash used for read/check/write conflict detection.
// Missing files have an empty hash; existence alone is not a conflict.
func FileHash(name string) (string, error) {
	b, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// ConfigureProfile applies one named profile while preserving unrelated raw
// TOML tables. beforeHash must be the hash observed before the caller read the
// file. The compare is performed immediately before the atomic rename.
func ConfigureProfile(name, role string, profile Profile, beforeHash string) error {
	canonical, err := CanonicalRole(role)
	if err != nil {
		return err
	}
	if err := ValidateProfile(profile); err != nil {
		return err
	}
	cfg, raw, err := readForUpdate(name)
	if err != nil {
		return err
	}
	if current, err := FileHash(name); err != nil {
		return err
	} else if current != beforeHash {
		return ErrConfigConflict
	}
	if raw == nil {
		raw = map[string]any{}
	}
	profiles := mapFrom(raw["profiles"])
	profiles[canonical] = map[string]any{"provider": profile.Provider, "model": profile.Model}
	if profile.Effort != "" {
		profiles[canonical].(map[string]any)["effort"] = profile.Effort
	}
	if profile.Speed != "" {
		profiles[canonical].(map[string]any)["speed"] = profile.Speed
	}
	raw["profiles"] = profiles
	_ = cfg
	return WriteRawAtomic(name, raw, beforeHash)
}

// ConfigureProfilesAtomic updates a set of profiles in one compare-and-swap
// write while preserving all unrelated TOML keys.
func ConfigureProfilesAtomic(name string, profilesToSet map[string]Profile, beforeHash string) error {
	if len(profilesToSet) == 0 {
		return errors.New("at least one profile is required")
	}
	_, raw, err := readForUpdate(name)
	if err != nil {
		return err
	}
	if raw == nil {
		raw = map[string]any{}
	}
	profiles := mapFrom(raw["profiles"])
	for role, profile := range profilesToSet {
		canonical, err := CanonicalRole(role)
		if err != nil {
			return err
		}
		if err := ValidateProfile(profile); err != nil {
			return fmt.Errorf("profile %s: %w", canonical, err)
		}
		value := map[string]any{"provider": profile.Provider, "model": profile.Model}
		if profile.Effort != "" {
			value["effort"] = profile.Effort
		}
		if profile.Speed != "" {
			value["speed"] = profile.Speed
		}
		profiles[canonical] = value
	}
	raw["profiles"] = profiles
	return WriteRawAtomic(name, raw, beforeHash)
}

// ImportConfigAtomic imports only RoleMux-owned tables from a TOML fragment.
// Unrelated destination keys are preserved, while unknown keys nested inside
// RoleMux-owned tables are discarded by re-encoding the validated typed form.
func ImportConfigAtomic(name string, data []byte, beforeHash string) error {
	var fragment Config
	if _, err := toml.Decode(string(data), &fragment); err != nil {
		return fmt.Errorf("parse imported config: %w", err)
	}
	var source map[string]any
	if _, err := toml.Decode(string(data), &source); err != nil {
		return fmt.Errorf("parse imported config: %w", err)
	}
	if err := Validate(fragment); err != nil {
		return err
	}
	_, destination, err := readForUpdate(name)
	if err != nil {
		return err
	}
	if destination == nil {
		destination = map[string]any{}
	}
	if _, ok := source["profiles"]; ok {
		destination["profiles"] = profilesRaw(fragment.Profiles)
	}
	if _, ok := source["providers"]; ok {
		destination["providers"] = providersRaw(fragment.Providers)
	}
	if _, ok := source["models"]; ok {
		destination["models"] = modelsRaw(fragment.Models)
	}
	if _, ok := source["catalog_ttl_seconds"]; ok {
		destination["catalog_ttl_seconds"] = fragment.CatalogTTLSeconds
	}
	if _, ok := source["provider_turn_timeout_seconds"]; ok {
		destination["provider_turn_timeout_seconds"] = fragment.ProviderTurnTimeoutSeconds
	}
	return WriteRawAtomic(name, destination, beforeHash)
}

func ValidateProfile(p Profile) error {
	if !providerid.Known(p.Provider) {
		return fmt.Errorf("%w %q", ErrUnknownProvider, p.Provider)
	}
	if p.Model == "" {
		return ErrMissingModel
	}
	if !modelIDPattern.MatchString(p.Model) {
		return errors.New("invalid model ID")
	}
	if p.Effort != "" && !validEffort(p.Effort) {
		return errors.New("invalid effort")
	}
	if p.Speed != "" && !modelIDPattern.MatchString(p.Speed) {
		return errors.New("invalid speed")
	}
	return nil
}

// WriteConfigAtomic serializes known config deterministically. Raw keys are
// preserved if present and the compare-and-swap happens before rename.
func WriteConfigAtomic(name string, cfg Config, beforeHash string) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	raw := cloneMap(cfg.Raw)
	if raw == nil {
		raw = map[string]any{}
	}
	raw["profiles"] = profilesRaw(cfg.Profiles)
	raw["providers"] = providersRaw(cfg.Providers)
	if len(cfg.Models) > 0 {
		raw["models"] = modelsRaw(cfg.Models)
	}
	if cfg.CatalogTTLSeconds != 0 {
		raw["catalog_ttl_seconds"] = cfg.CatalogTTLSeconds
	}
	if cfg.ProviderTurnTimeoutSeconds != 0 {
		raw["provider_turn_timeout_seconds"] = cfg.ProviderTurnTimeoutSeconds
	}
	return WriteRawAtomic(name, raw, beforeHash)
}

func WriteRawAtomic(name string, raw map[string]any, beforeHash string) error {
	data, err := marshalRaw(raw)
	if err != nil {
		return err
	}
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Recheck after the complete temp file is durable and immediately before
	// publishing it. Existing files are expected; only byte-hash drift is a
	// conflict.
	current, err := FileHash(name)
	if err != nil {
		return err
	}
	if current != beforeHash {
		return ErrConfigConflict
	}
	if err := os.Rename(tmpName, name); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func readForUpdate(name string) (Config, map[string]any, error) {
	b, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		c := Default()
		return c, map[string]any{}, nil
	}
	if err != nil {
		return Config{}, nil, err
	}
	var c Config
	if _, err := toml.Decode(string(b), &c); err != nil {
		return Config{}, nil, err
	}
	var raw map[string]any
	if _, err := toml.Decode(string(b), &raw); err != nil {
		return Config{}, nil, err
	}
	return c, raw, nil
}

func marshalRaw(raw map[string]any) ([]byte, error) {
	// BurntSushi emits maps in sorted order. Normalize nested maps first so
	// output is deterministic across Go map iteration.
	normalized := normalize(raw)
	var b bytes.Buffer
	enc := toml.NewEncoder(&b)
	if err := enc.Encode(normalized); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func normalize(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = normalize(x)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = x
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = normalize(x)
		}
		return out
	default:
		return value
	}
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func mapFrom(value any) map[string]any {
	result := map[string]any{}
	switch v := value.(type) {
	case map[string]any:
		for k, x := range v {
			result[k] = x
		}
	case map[string]map[string]any:
		for k, x := range v {
			result[k] = x
		}
	}
	return result
}

func profilesRaw(profiles map[string]Profile) map[string]any {
	out := map[string]any{}
	keys := make([]string, 0, len(profiles))
	for k := range profiles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := profiles[k]
		m := map[string]any{"provider": p.Provider}
		if p.Model != "" {
			m["model"] = p.Model
		}
		if p.Effort != "" {
			m["effort"] = p.Effort
		}
		if p.Speed != "" {
			m["speed"] = p.Speed
		}
		out[k] = m
	}
	return out
}

func providersRaw(providers map[string]Provider) map[string]any {
	out := map[string]any{}
	for name, p := range providers {
		m := map[string]any{}
		if p.Name != "" {
			m["name"] = p.Name
		}
		if p.Type != "" {
			m["type"] = p.Type
		}
		if p.WireAPI != "" {
			m["wire_api"] = p.WireAPI
		}
		if p.Transport != "" {
			m["transport"] = p.Transport
		}
		if p.CLIPath != "" {
			m["cli_path"] = p.CLIPath
		}
		if p.BaseURL != "" {
			m["base_url"] = p.BaseURL
		}
		if p.GatewayURL != "" {
			m["gateway_url"] = p.GatewayURL
		}
		if p.Headers != nil {
			m["headers"] = cloneStringMap(p.Headers)
		}
		if p.EnvKey != "" {
			m["env_key"] = p.EnvKey
		}
		if p.BearerTokenEnv != "" {
			m["bearer_token_env"] = p.BearerTokenEnv
		}
		if p.APIKeyEnv != "" {
			m["api_key_env"] = p.APIKeyEnv
		}
		if p.AuthTokenEnv != "" {
			m["auth_token_env"] = p.AuthTokenEnv
		}
		if p.BedrockProfileEnv != "" {
			m["bedrock_profile_env"] = p.BedrockProfileEnv
		}
		if p.BedrockRegionEnv != "" {
			m["bedrock_region_env"] = p.BedrockRegionEnv
		}
		if p.VertexProjectEnv != "" {
			m["vertex_project_env"] = p.VertexProjectEnv
		}
		if p.VertexRegionEnv != "" {
			m["vertex_region_env"] = p.VertexRegionEnv
		}
		if p.FoundryEndpointEnv != "" {
			m["foundry_endpoint_env"] = p.FoundryEndpointEnv
		}
		if p.FoundryAPIKeyEnv != "" {
			m["foundry_api_key_env"] = p.FoundryAPIKeyEnv
		}
		if p.EnvRefs != nil {
			m["env_refs"] = cloneStringMap(p.EnvRefs)
		}
		if p.EnvHTTPHeaders != nil {
			m["env_http_headers"] = cloneStringMap(p.EnvHTTPHeaders)
		}
		if p.QueryParams != nil {
			m["query_params"] = cloneStringMap(p.QueryParams)
		}
		if p.ModelID != "" {
			m["model_id"] = p.ModelID
		}
		if p.WireModel != "" {
			m["wire_model"] = p.WireModel
		}
		if p.RequestMaxRetries != 0 {
			m["request_max_retries"] = p.RequestMaxRetries
		}
		if p.StreamMaxRetries != 0 {
			m["stream_max_retries"] = p.StreamMaxRetries
		}
		if p.StreamIdleTimeoutMS != 0 {
			m["stream_idle_timeout_ms"] = p.StreamIdleTimeoutMS
		}
		if p.SupportsStandaloneWebSearch {
			m["supports_standalone_web_search"] = true
		}
		if p.RequiresOpenAIAuth {
			m["requires_openai_auth"] = true
		}
		if p.Auth.Command != "" {
			m["auth"] = authRaw(p.Auth)
		}
		if p.MaxPromptTokens != 0 {
			m["max_prompt_tokens"] = p.MaxPromptTokens
		}
		if p.MaxOutputTokens != 0 {
			m["max_output_tokens"] = p.MaxOutputTokens
		}
		if p.APIVersion != "" {
			m["api_version"] = p.APIVersion
		}
		if p.SDKSettings != nil {
			m["sdk_settings"] = cloneStringMap(p.SDKSettings)
		}
		out[name] = m
	}
	return out
}

func modelsRaw(models map[string]map[string]CustomModel) map[string]any {
	out := map[string]any{}
	for provider, set := range models {
		inner := map[string]any{}
		for name, m := range set {
			item := map[string]any{"id": m.ID, "availability": m.Availability}
			if m.Label != "" {
				item["label"] = m.Label
			}
			if len(m.Aliases) > 0 {
				item["aliases"] = m.Aliases
			}
			if len(m.Efforts) > 0 {
				item["efforts"] = m.Efforts
			}
			if m.DefaultEffort != "" {
				item["default_effort"] = m.DefaultEffort
			}
			if m.Name != "" {
				item["name"] = m.Name
			}
			if m.BaseURL != "" {
				item["base_url"] = m.BaseURL
			}
			if m.WireAPI != "" {
				item["wire_api"] = m.WireAPI
			}
			if m.EnvKey != "" {
				item["env_key"] = m.EnvKey
			}
			if m.EnvHTTPHeaders != nil {
				item["env_http_headers"] = cloneStringMap(m.EnvHTTPHeaders)
			}
			if m.QueryParams != nil {
				item["query_params"] = cloneStringMap(m.QueryParams)
			}
			if m.RequestMaxRetries != 0 {
				item["request_max_retries"] = m.RequestMaxRetries
			}
			if m.StreamMaxRetries != 0 {
				item["stream_max_retries"] = m.StreamMaxRetries
			}
			if m.StreamIdleTimeoutMS != 0 {
				item["stream_idle_timeout_ms"] = m.StreamIdleTimeoutMS
			}
			if m.SupportsStandaloneWebSearch {
				item["supports_standalone_web_search"] = true
			}
			if m.RequiresOpenAIAuth {
				item["requires_openai_auth"] = true
			}
			if m.Auth.Command != "" {
				item["auth"] = authRaw(m.Auth)
			}
			if m.Provider != "" {
				item["provider"] = m.Provider
			}
			if m.WireModel != "" {
				item["wire_model"] = m.WireModel
			}
			if m.ModelID != "" {
				item["model_id"] = m.ModelID
			}
			if m.MaxPromptTokens != 0 {
				item["max_prompt_tokens"] = m.MaxPromptTokens
			}
			if m.MaxContextWindowTokens != 0 {
				item["max_context_window_tokens"] = m.MaxContextWindowTokens
			}
			if m.MaxOutputTokens != 0 {
				item["max_output_tokens"] = m.MaxOutputTokens
			}
			if m.Capabilities != nil {
				item["capabilities"] = cloneMap(m.Capabilities)
			}
			inner[name] = item
		}
		out[provider] = inner
	}
	return out
}

func authRaw(auth AuthCommand) map[string]any {
	out := map[string]any{"command": auth.Command}
	if len(auth.Args) > 0 {
		out["args"] = append([]string(nil), auth.Args...)
	}
	if auth.TimeoutMS != 0 {
		out["timeout_ms"] = auth.TimeoutMS
	}
	if auth.RefreshIntervalMS != 0 {
		out["refresh_interval_ms"] = auth.RefreshIntervalMS
	}
	return out
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			dm, _ := dst[k].(map[string]any)
			if dm == nil {
				dm = map[string]any{}
				dst[k] = dm
			}
			deepMerge(dm, sm)
		} else {
			dst[k] = v
		}
	}
}

// SnapshotEnvironment returns only values for validated references. It is
// intentionally generic so runner packages can use it without persisting the
// returned values.
func SnapshotEnvironment(refs []string, environ []string) (map[string]string, error) {
	env := envMap(environ)
	out := map[string]string{}
	for _, ref := range refs {
		if err := ValidateEnvRef(ref); err != nil {
			return nil, err
		}
		if value, ok := env[ref]; ok {
			out[ref] = value
		}
	}
	return out, nil
}

func ReadAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }
