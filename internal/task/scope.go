package task

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// CanonicalScope normalizes comma/newline separated repository-relative
// paths/globs. The default scope is ** and excludes all RoleMux/Git state. An
// explicit scope may name only the tracked project artifacts under
// .rolemux/plans; tracking is enforced by the manifest observer.
func CanonicalScope(raw string) (string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.ContainsRune(part, '\\') {
			return "", errors.New("scope may not contain backslashes")
		}
		if strings.ContainsRune(part, '\x00') || strings.HasPrefix(part, "/") || strings.HasPrefix(part, "~") {
			return "", errors.New("scope must contain repository-relative paths")
		}
		part = path.Clean(strings.TrimPrefix(part, "./"))
		if part == "." || part == ".." || strings.HasPrefix(part, "../") || strings.Contains(part, "/../") {
			return "", errors.New("scope may not escape the repository")
		}
		if part == ".git" || strings.HasPrefix(part, ".git/") {
			return "", errors.New("scope may not include private git state")
		}
		if part == ".rolemux" || (strings.HasPrefix(part, ".rolemux/") && part != ".rolemux/plans" && !strings.HasPrefix(part, ".rolemux/plans/")) {
			return "", errors.New("scope may only include RoleMux project plans")
		}
		seen[part] = true
	}
	if len(seen) == 0 {
		return "**", nil
	}
	result := make([]string, 0, len(seen))
	for part := range seen {
		result = append(result, part)
	}
	sort.Strings(result)
	return strings.Join(result, ","), nil
}

func ScopeSpecHash(scope string) string { return digestBytes([]byte(scope)) }

func ScopePatterns(scope string) []string {
	if strings.TrimSpace(scope) == "" {
		scope = "**"
	}
	parts := strings.Split(scope, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.Trim(strings.TrimSpace(part), "/"); part != "" {
			result = append(result, part)
		}
	}
	return result
}

// ScopeMatches uses slash-separated paths and supports * (one segment) and
// ** (zero or more segments). A literal directory matches descendants.
func ScopeMatches(scope, repoPath string) bool {
	if strings.ContainsRune(repoPath, '\\') {
		return false
	}
	repoPath = strings.Trim(repoPath, "/")
	if repoPath == "" || repoPath == ".git" || strings.HasPrefix(repoPath, ".git/") {
		return false
	}
	for _, pattern := range ScopePatterns(scope) {
		pattern = strings.Trim(pattern, "/")
		if repoPath == ".rolemux" || strings.HasPrefix(repoPath, ".rolemux/") {
			if pattern == "**" || (pattern != ".rolemux/plans" && !strings.HasPrefix(pattern, ".rolemux/plans/")) {
				continue
			}
		}
		if pattern == "**" || pattern == repoPath || strings.HasPrefix(repoPath, pattern+"/") {
			return true
		}
		if globMatch(pattern, repoPath) {
			return true
		}
	}
	return false
}

func globMatch(pattern, value string) bool {
	pp, vv := strings.Split(pattern, "/"), strings.Split(value, "/")
	var match func(int, int) bool
	match = func(i, j int) bool {
		if i == len(pp) {
			return j == len(vv)
		}
		if pp[i] == "**" {
			return match(i+1, j) || (j < len(vv) && match(i, j+1))
		}
		if j == len(vv) {
			return false
		}
		ok, err := path.Match(pp[i], vv[j])
		return err == nil && ok && match(i+1, j+1)
	}
	return match(0, 0)
}

func ScopeEntries(entries []FileEntry, scope string) []FileEntry {
	result := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		if ScopeMatches(scope, entry.Path) {
			result = append(result, entry)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func ManifestChanged(before, after []FileEntry) bool {
	return HashManifest(before) != HashManifest(after)
}

type ManifestDeltaResult struct {
	Added, Removed, Changed []FileEntry
}

func ManifestDelta(before, after []FileEntry) ManifestDeltaResult {
	old := make(map[string]FileEntry, len(before))
	now := make(map[string]FileEntry, len(after))
	for _, entry := range before {
		old[entry.Path] = entry
	}
	for _, entry := range after {
		now[entry.Path] = entry
	}
	var result ManifestDeltaResult
	for p, entry := range now {
		previous, ok := old[p]
		if !ok {
			result.Added = append(result.Added, entry)
		} else if HashManifest([]FileEntry{previous}) != HashManifest([]FileEntry{entry}) {
			result.Changed = append(result.Changed, entry)
		}
	}
	for p, entry := range old {
		if _, ok := now[p]; !ok {
			result.Removed = append(result.Removed, entry)
		}
	}
	sort.Slice(result.Added, func(i, j int) bool { return result.Added[i].Path < result.Added[j].Path })
	sort.Slice(result.Changed, func(i, j int) bool { return result.Changed[i].Path < result.Changed[j].Path })
	sort.Slice(result.Removed, func(i, j int) bool { return result.Removed[i].Path < result.Removed[j].Path })
	return result
}

func (d ManifestDeltaResult) Paths() []string {
	seen := map[string]bool{}
	for _, entries := range [][]FileEntry{d.Added, d.Removed, d.Changed} {
		for _, entry := range entries {
			seen[entry.Path] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func UnmatchedScopePatterns(entries []FileEntry, scope string) []string {
	result := []string{}
	for _, pattern := range ScopePatterns(scope) {
		matched := false
		for _, entry := range entries {
			if ScopeMatches(pattern, entry.Path) {
				matched = true
				break
			}
		}
		if !matched {
			result = append(result, pattern)
		}
	}
	return result
}

// ScopesOverlap is conservative and advisory only. It never rejects a
// different task ID or serializes a provider call.
func ScopesOverlap(a, b string) bool {
	for _, left := range ScopePatterns(a) {
		for _, right := range ScopePatterns(b) {
			if left == "**" || right == "**" || left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/") {
				return true
			}
			if scopesMayIntersect(left, right) {
				return true
			}
		}
	}
	return false
}

// scopesMayIntersect returns false only when two patterns are provably rooted
// in distinct literal subtrees. Glob ambiguity deliberately becomes an
// advisory overlap instead of a false assurance of isolation.
func scopesMayIntersect(a, b string) bool {
	left, right := strings.Split(a, "/"), strings.Split(b, "/")
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		if strings.ContainsAny(left[i], "*?[") || strings.ContainsAny(right[i], "*?[") {
			return true
		}
		if left[i] != right[i] {
			return false
		}
	}
	// A shared literal prefix means one pattern can select the prefix itself or
	// descendants selected by the other pattern.
	return true
}

func wildcardPrefix(pattern string) string {
	if index := strings.IndexAny(pattern, "*?["); index >= 0 {
		prefix := strings.TrimSuffix(pattern[:index], "/")
		if prefix == "" {
			return "*"
		}
		return prefix
	}
	return ""
}

func ValidateScope(scope string) error {
	canonical, err := CanonicalScope(scope)
	if err != nil {
		return err
	}
	if canonical == "" {
		return fmt.Errorf("empty canonical scope")
	}
	return nil
}
