package runner

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var bypassEnvironmentKeys = map[string]bool{
	"CODEX_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX": true,
	"CODEX_APPROVAL_MODE":                            true,
	"CLAUDE_ALLOW_DANGEROUS":                         true,
	"CLAUDE_BYPASS_PERMISSIONS":                      true,
	"COPILOT_ALLOW_ALL":                              true,
	"COPILOT_YOLO":                                   true,
}

// SanitizedEnv removes known provider bypass/auto-approval controls. It does
// not log or otherwise expose the remaining values.
func SanitizedEnv(environ []string) []string {
	result := make([]string, 0, len(environ))
	for _, item := range environ {
		key, _, ok := strings.Cut(item, "=")
		if !ok || bypassEnvironmentKeys[key] || strings.Contains(strings.ToLower(key), "bypass") && strings.Contains(strings.ToLower(key), "approval") {
			continue
		}
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}

// runtimeEnvironment keeps provider credentials out of argv while reusing
// only references captured in the task's runtime snapshot. Endpoint is safe
// routing metadata and is optionally exported under the provider's documented
// gateway environment variable.
func runtimeEnvironment(base []string, refs []string, endpointKey, endpoint string) ([]string, error) {
	return runtimeEnvironmentMapped(base, refs, nil, endpointKey, endpoint)
}

func runtimeEnvironmentMapped(base []string, refs []string, mappings map[string]string, endpointKey, endpoint string) ([]string, error) {
	if len(base) == 0 {
		base = os.Environ()
	}
	base = SanitizedEnv(base)
	values := map[string]string{}
	for _, item := range base {
		if key, value, ok := strings.Cut(item, "="); ok {
			values[key] = value
		}
	}
	for _, ref := range refs {
		if _, ok := values[ref]; !ok {
			return nil, fmt.Errorf("missing credential environment %s", ref)
		}
	}
	for target, source := range mappings {
		if target == "" || source == "" {
			return nil, errors.New("invalid runtime environment mapping")
		}
		value, ok := values[source]
		if !ok {
			return nil, fmt.Errorf("missing credential environment %s", source)
		}
		values[target] = value
	}
	if endpoint != "" && endpointKey != "" {
		values[endpointKey] = endpoint
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result, nil
}

func ResolveExecutable(envName, fallback string, environ []string) (string, error) {
	env := map[string]string{}
	for _, item := range environ {
		if k, v, ok := strings.Cut(item, "="); ok {
			env[k] = v
		}
	}
	if value := strings.TrimSpace(env[envName]); value != "" {
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("%s must be an absolute executable path", envName)
		}
		info, err := os.Stat(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", envName, err)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%s is not executable", envName)
		}
		return filepath.Clean(value), nil
	}
	path, err := exec.LookPath(fallback)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", fallback, err)
	}
	return path, nil
}

func resolveExplicitExecutable(value string) (string, error) {
	if !filepath.IsAbs(value) {
		return "", errors.New("configured CLI path must be absolute")
	}
	clean := filepath.Clean(value)
	info, err := os.Stat(clean)
	if err != nil {
		return "", err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", errors.New("configured CLI path is not executable")
	}
	return clean, nil
}

func executableForRequest(fallback string, runtimePath string) (string, error) {
	if runtimePath == "" {
		return fallback, nil
	}
	return resolveExplicitExecutable(runtimePath)
}

// PathWithinRepo canonicalizes both the repository and candidate path and
// rejects missing paths, symlink escapes, and arbitrary host reads.
func PathWithinRepo(repoRoot, candidate string) (string, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	full, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("path escapes repository")
	}
	return resolved, nil
}

// WritePathWithinRepo validates a file-write target, including a not-yet-created
// file. Every existing path component is resolved so a symlink cannot redirect
// the write outside the repository. The returned relative path uses slashes for
// matching against RoleMux's repository-relative scope syntax.
func WritePathWithinRepo(repoRoot, candidate string) (string, string, error) {
	logicalRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", "", err
	}
	root, err := filepath.EvalSymlinks(logicalRoot)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(candidate) == "" {
		return "", "", errors.New("write path is required")
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(logicalRoot, candidate)
	}
	full, err := filepath.Abs(candidate)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(logicalRoot, full)
	if err != nil || pathEscapesRoot(rel) {
		// macOS commonly exposes /var and its resolved /private/var path
		// interchangeably. Accept either spelling of the same repository root.
		rel, err = filepath.Rel(root, full)
	}
	if err != nil || rel == "." || pathEscapesRoot(rel) {
		return "", "", errors.New("write path escapes repository")
	}
	full = filepath.Join(root, rel)
	probe := full
	for {
		if _, statErr := os.Lstat(probe); statErr == nil {
			break
		} else if !os.IsNotExist(statErr) {
			return "", "", statErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", "", errors.New("write path has no existing parent")
		}
		probe = parent
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", "", err
	}
	resolvedRel, err := filepath.Rel(root, resolved)
	if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) || filepath.IsAbs(resolvedRel) {
		return "", "", errors.New("write path escapes repository through a symlink")
	}
	return full, filepath.ToSlash(rel), nil
}

func pathEscapesRoot(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative)
}

func SafeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func boolValue(value *bool) bool { return value != nil && *value }
