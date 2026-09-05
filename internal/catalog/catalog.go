// Package catalog implements provider model discovery with a small,
// account-and-endpoint-scoped last-good cache and non-blocking snapshot reads.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
)

type Catalog struct {
	Adapters  map[string]runner.Adapter
	Config    config.Config
	CachePath string
	Now       func() time.Time
}

var cachePathLocks sync.Map

type cacheEntry struct {
	Provider     string             `json:"provider"`
	IdentityHash string             `json:"identity_hash"`
	EndpointHash string             `json:"endpoint_hash,omitempty"`
	SavedAt      time.Time          `json:"saved_at"`
	Models       []runner.ModelInfo `json:"models"`
}

type cacheFile struct {
	Entries []cacheEntry `json:"entries"`
}

func New(adapters map[string]runner.Adapter, cfg config.Config, cachePath string) *Catalog {
	return &Catalog{Adapters: adapters, Config: cfg, CachePath: cachePath, Now: time.Now}
}

// DefaultCachePath derives the platform cache location from the caller's
// environment. Keeping this explicit prevents embedded callers and tests from
// accidentally reading or replacing another user's model catalog.
func DefaultCachePath(environ []string) string {
	values := map[string]string{}
	for _, item := range environ {
		if key, value, ok := strings.Cut(item, "="); ok {
			values[key] = value
		}
	}
	if runtime.GOOS == "windows" {
		if root := values["LocalAppData"]; filepath.IsAbs(root) {
			return filepath.Join(root, "rolemux", "models.json")
		}
		return ""
	}
	home := values["HOME"]
	if !filepath.IsAbs(home) {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches", "rolemux", "models.json")
	}
	if root := values["XDG_CACHE_HOME"]; filepath.IsAbs(root) {
		return filepath.Join(root, "rolemux", "models.json")
	}
	return filepath.Join(home, ".cache", "rolemux", "models.json")
}

func (c *Catalog) Models(ctx context.Context, provider string, refresh bool, runtime runner.ModelListRequest) ([]runner.ModelInfo, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil, errors.New("model provider is required")
	}
	adapter := c.Adapters[provider]
	if adapter == nil {
		return nil, fmt.Errorf("no adapter for provider %s", provider)
	}
	account := ""
	// A forced live refresh does not need a separate authentication probe:
	// ListModels is itself provider/account scoped and returns the account when
	// the CLI exposes it. This avoids starting the same CLI twice in configure.
	if !refresh {
		if auth, authErr := adapter.Auth(ctx); authErr == nil && auth.Authenticated {
			account = auth.Account
		}
	}
	if !refresh {
		if models, ok := c.cached(provider, account, runtime, false, false); ok {
			return models, nil
		}
	}
	page, err := adapter.ListModels(ctx, runtime)
	if err == nil {
		models := normalizeLive(provider, page.Models)
		models = appendCustom(models, c.Config, provider)
		if page.Account == "" {
			if account == "" {
				if auth, authErr := adapter.Auth(ctx); authErr == nil && auth.Authenticated {
					account = auth.Account
				}
			}
			page.Account = account
		}
		identity := identityHash(provider, page.Account, endpoint(page.Endpoint, runtime.Runtime.Endpoint))
		_ = c.save(cacheEntry{Provider: provider, IdentityHash: identity, EndpointHash: hashString(endpoint(page.Endpoint, runtime.Runtime.Endpoint)), SavedAt: c.now(), Models: models})
		return models, nil
	}
	if page.Account != "" {
		account = page.Account
	}
	if refresh && account == "" {
		if auth, authErr := adapter.Auth(ctx); authErr == nil && auth.Authenticated {
			account = auth.Account
		}
	}
	if models, ok := c.cached(provider, account, runtime, true, false); ok {
		return models, nil
	}
	custom := appendCustom(nil, c.Config, provider)
	if len(custom) > 0 {
		return custom, nil
	}
	return nil, fmt.Errorf("live model discovery failed for %s and no last-good cache is available: %w", provider, err)
}

// CachedModels returns an account-scoped snapshot without contacting the
// provider. It preserves the last verified availability so an interactive
// picker can open immediately; Origin and AgeSeconds still identify stale data.
func (c *Catalog) CachedModels(provider, account string, runtime runner.ModelListRequest) ([]runner.ModelInfo, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil, false
	}
	if models, ok := c.cached(provider, account, runtime, true, true); ok {
		return models, true
	}
	if strings.TrimSpace(account) != "" {
		return nil, false
	}
	return c.latestCachedForEndpoint(provider, runtime.Runtime.Endpoint)
}

func (c *Catalog) latestCachedForEndpoint(provider, endpoint string) ([]runner.ModelInfo, bool) {
	entries, err := c.readCache()
	if err != nil {
		return nil, false
	}
	wantedEndpoint := hashString(endpoint)
	var newest *cacheEntry
	for index := range entries {
		entry := &entries[index]
		if entry.Provider != provider || entry.EndpointHash != wantedEndpoint || newest != nil && !entry.SavedAt.After(newest.SavedAt) {
			continue
		}
		newest = entry
	}
	if newest == nil {
		return nil, false
	}
	age := int64(c.now().Sub(newest.SavedAt).Seconds())
	return markCache(newest.Models, age, true), true
}

func normalizeLive(provider string, models []runner.ModelInfo) []runner.ModelInfo {
	result := make([]runner.ModelInfo, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		if model.ID == "" || seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		model.Provider = provider
		if model.Origin == "" {
			model.Origin = "live"
		}
		if model.Availability == "" {
			model.Availability = "unknown"
		}
		// Some adapters expose safe local aliases when their CLI has no live
		// catalog API. Preserve that provenance instead of presenting a custom
		// fallback as a provider-verified live model.
		model.Custom = model.Custom || model.Origin == "custom"
		model.AgeSeconds = 0
		model.Efforts = uniqueSorted(model.Efforts)
		model.Aliases = uniqueSorted(model.Aliases)
		result = append(result, model)
	}
	return result
}

func appendCustom(models []runner.ModelInfo, cfg config.Config, provider string) []runner.ModelInfo {
	seen := map[string]bool{}
	for _, model := range models {
		seen[model.ID] = true
	}
	customModels := make([]runner.ModelInfo, 0, len(cfg.Models[provider]))
	for name, custom := range cfg.Models[provider] {
		id := custom.ID
		if id == "" {
			id = name
		}
		if seen[id] {
			continue
		}
		aliases := uniqueSorted(custom.Aliases)
		customModels = append(customModels, runner.ModelInfo{ID: id, Label: custom.Label, Provider: provider, Origin: "custom", Availability: "unknown", Efforts: uniqueSorted(custom.Efforts), DefaultEffort: custom.DefaultEffort, Aliases: aliases, Custom: true})
		seen[id] = true
	}
	sort.Slice(customModels, func(i, j int) bool { return customModels[i].ID < customModels[j].ID })
	return append(models, customModels...)
}

func markCache(models []runner.ModelInfo, age int64, preserveAvailability bool) []runner.ModelInfo {
	result := append([]runner.ModelInfo(nil), models...)
	for i := range result {
		result[i].Origin = "cache"
		if !preserveAvailability {
			result[i].Availability = "unknown"
		}
		result[i].AgeSeconds = age
	}
	return result
}

func (c *Catalog) cached(provider, account string, req runner.ModelListRequest, allowExpired, preserveAvailability bool) ([]runner.ModelInfo, bool) {
	entries, err := c.readCache()
	if err != nil {
		return nil, false
	}
	identity := identityHash(provider, account, req.Runtime.Endpoint)
	for _, entry := range entries {
		if entry.Provider == provider && entry.IdentityHash == identity {
			age := int64(c.now().Sub(entry.SavedAt).Seconds())
			if !allowExpired && c.Config.CatalogTTLSeconds > 0 && age > int64(c.Config.CatalogTTLSeconds) {
				return nil, false
			}
			return markCache(entry.Models, age, preserveAvailability), true
		}
	}
	return nil, false
}

func (c *Catalog) readCache() ([]cacheEntry, error) {
	lock := cachePathLock(c.CachePath)
	lock.Lock()
	defer lock.Unlock()
	return c.readCacheUnlocked()
}

func (c *Catalog) readCacheUnlocked() ([]cacheEntry, error) {
	if c.CachePath == "" {
		return nil, errors.New("cache disabled")
	}
	b, err := os.ReadFile(c.CachePath)
	if err != nil {
		return nil, err
	}
	var file cacheFile
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, err
	}
	return file.Entries, nil
}

func (c *Catalog) save(entry cacheEntry) error {
	if c.CachePath == "" {
		return nil
	}
	lock := cachePathLock(c.CachePath)
	lock.Lock()
	defer lock.Unlock()
	entries, _ := c.readCacheUnlocked()
	replaced := false
	for i := range entries {
		if entries[i].Provider == entry.Provider && entries[i].IdentityHash == entry.IdentityHash {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Provider == entries[j].Provider {
			return entries[i].IdentityHash < entries[j].IdentityHash
		}
		return entries[i].Provider < entries[j].Provider
	})
	b, err := json.MarshalIndent(cacheFile{Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(c.CachePath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.CachePath), ".models-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(b)
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
	return os.Rename(name, c.CachePath)
}

func cachePathLock(path string) *sync.Mutex {
	value, _ := cachePathLocks.LoadOrStore(path, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (c *Catalog) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
func identityHash(provider, account, endpoint string) string {
	return hashString(strings.Join([]string{provider, account, endpoint}, "\x00"))
}
func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func endpoint(page, fallback string) string {
	if page != "" {
		return page
	}
	return fallback
}
func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	sort.Strings(result)
	return result
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
