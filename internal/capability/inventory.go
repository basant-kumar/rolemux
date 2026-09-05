// Package capability discovers bounded, non-secret metadata about skills and
// helper tools that a delegated provider session may use.
package capability

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxFrontmatterBytes = 16 << 10
	maxCandidates       = 256
	maxSkills           = 20
	maxDescriptionRunes = 240
	maxNoteBytes        = 6 << 10
)

type Skill struct {
	Name        string
	Description string
	Provider    string
	Scope       string
	Native      bool
	score       int
	order       int
}

type Helper struct {
	Name string
	Path string
}

type Inventory struct {
	Provider         string
	Skills           []Skill
	Helpers          []Helper
	Tools            []string
	SkillDirectories []string
}

type Options struct {
	Provider         string
	Role             string
	Task             string
	RepoRoot         string
	CodexAdminSkills string
	Environ          []string
}

type rootSpec struct {
	provider string
	scope    string
	path     string
	native   bool
	order    int
}

// Discover returns metadata only. Skill bodies, arbitrary frontmatter fields,
// environment values, and filesystem errors never enter the inventory.
func Discover(options Options) Inventory {
	provider := strings.ToLower(strings.TrimSpace(options.Provider))
	roots := discoveryRoots(provider, options.RepoRoot, options.CodexAdminSkills, options.Environ)
	result := Inventory{Provider: provider, Tools: harnessTools(provider, options.Role)}
	seenNative, seenHost := map[string]bool{}, map[string]bool{}
	candidates := 0
	for _, root := range roots {
		if candidates >= maxCandidates {
			break
		}
		directories, skills := readRoot(root, maxCandidates-candidates)
		candidates += len(skills)
		if root.native {
			result.SkillDirectories = append(result.SkillDirectories, directories...)
		}
		for _, skill := range skills {
			key := strings.ToLower(skill.Name)
			if root.native {
				// Codex intentionally exposes same-named skills from different
				// repository/user locations instead of choosing one winner.
				if provider != "codex" && seenNative[key] {
					continue
				}
				seenNative[key] = true
			} else {
				if seenNative[key] || seenHost[root.provider+"\x00"+key] {
					continue
				}
				seenHost[root.provider+"\x00"+key] = true
			}
			skill.Provider, skill.Scope, skill.Native, skill.order = root.provider, root.scope, root.native, root.order
			skill.score = relevance(options.Task, skill.Name+" "+skill.Description)
			result.Skills = append(result.Skills, skill)
		}
	}
	result.SkillDirectories = uniquePaths(result.SkillDirectories)
	if path := executable(options.Environ, "PXPIPE_CLI_PATH", "pxpipe"); path != "" {
		result.Helpers = append(result.Helpers, Helper{Name: "pxpipe", Path: path})
	}
	sort.SliceStable(result.Skills, func(i, j int) bool {
		a, b := result.Skills[i], result.Skills[j]
		if a.Native != b.Native {
			return a.Native
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.order != b.order {
			return a.order < b.order
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Name < b.Name
	})
	if len(result.Skills) > maxSkills {
		result.Skills = result.Skills[:maxSkills]
	}
	return result
}

// Note renders a bounded first-turn handoff. Metadata is advisory: the native
// provider harness remains the authority on whether a skill/tool can run.
func (inventory Inventory) Note(role string) string {
	var lines []string
	lines = append(lines,
		"Available capability metadata (skill bodies are not included):",
		"Use a relevant native skill/tool only if this provider session exposes it and the current sandbox permits it. Do not invoke the rolemux skill from inside a delegated role.",
	)
	for _, skill := range inventory.Skills {
		kind := "host-mediated"
		if skill.Native && !strings.EqualFold(skill.Name, "rolemux") {
			kind = "native-discoverable"
		}
		line := fmt.Sprintf("- skill %s [%s, %s/%s]: %s", skill.Name, kind, skill.Provider, skill.Scope, skill.Description)
		if !appendBounded(&lines, line) {
			break
		}
	}
	for _, helper := range inventory.Helpers {
		message := "installed helper"
		if helper.Name == "pxpipe" {
			message = "installed; RoleMux owns a private server for eligible Claude/Codex task turns and prints its temporary dashboard URL; pxpipe itself decides exact-model eligibility, pass-through, and measured savings"
		}
		if !appendBounded(&lines, fmt.Sprintf("- tool %s: %s", helper.Name, message)) {
			break
		}
	}
	if len(inventory.Tools) > 0 {
		appendBounded(&lines, "- provider tools: "+strings.Join(inventory.Tools, ", "))
	}
	if role == "plan_reviewer" || role == "code_reviewer" {
		appendBounded(&lines, "If essential evidence is unavailable here, report one precise finding naming the capability, operation/resource, required inputs, and expected evidence so the planner or implementer can request it from the orchestrator.")
	} else {
		appendBounded(&lines, "If an essential capability is host-mediated or unavailable here, return needs_input with one precise question naming the capability, operation/resource, required inputs, and expected evidence. The orchestrator will gather it and resume this same session.")
	}
	return strings.Join(lines, "\n")
}

func harnessTools(provider, role string) []string {
	write := role == "implementer"
	switch provider {
	case "claude":
		tools := []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch", "Skill"}
		if write {
			tools = append(tools, "Edit", "Write")
		}
		return tools
	case "codex":
		tools := []string{"repository read/search", "sandboxed shell", "web search", "native skills"}
		if write {
			tools = append(tools, "workspace edits")
		}
		return tools
	case "copilot":
		tools := []string{"view", "grep", "web_fetch", "explicit native skills"}
		if write {
			tools = append(tools, "scope-confined workspace edits")
		}
		return tools
	case "antigravity":
		return []string{"provider-native tools and skills exposed by the active sandbox"}
	default:
		return nil
	}
}

func appendBounded(lines *[]string, line string) bool {
	current := 0
	for _, existing := range *lines {
		current += len(existing) + 1
	}
	if current+len(line)+1 > maxNoteBytes {
		return false
	}
	*lines = append(*lines, line)
	return true
}

func discoveryRoots(provider, repo, codexAdminSkills string, environ []string) []rootSpec {
	home := env(environ, "HOME")
	join := func(base string, parts ...string) string {
		if strings.TrimSpace(base) == "" {
			return ""
		}
		return filepath.Join(append([]string{base}, parts...)...)
	}
	project := func(parts ...string) string { return join(repo, parts...) }
	personal := func(parts ...string) string { return join(home, parts...) }
	copilotHome := env(environ, "COPILOT_HOME")
	if copilotHome == "" {
		copilotHome = personal(".copilot")
	} else if !filepath.IsAbs(copilotHome) {
		copilotHome = ""
	}
	copilot := func(parts ...string) string { return join(copilotHome, parts...) }

	native := map[string][]rootSpec{
		// Claude gives personal skills precedence over project skills.
		"claude": {
			{provider: "claude", scope: "personal", path: personal(".claude", "skills")},
			{provider: "shared", scope: "personal", path: personal(".agents", "skills")},
			{provider: "claude", scope: "project", path: project(".claude", "skills")},
			{provider: "shared", scope: "project", path: project(".agents", "skills")},
		},
		// Copilot documents project roots before personal roots.
		"copilot": {
			{provider: "copilot", scope: "project", path: project(".github", "skills")},
			{provider: "shared", scope: "project", path: project(".agents", "skills")},
			{provider: "claude", scope: "project", path: project(".claude", "skills")},
			{provider: "copilot", scope: "personal", path: copilot("skills")},
			{provider: "shared", scope: "personal", path: personal(".agents", "skills")},
		},
		"codex": {
			{provider: "shared", scope: "project", path: project(".agents", "skills")},
			{provider: "shared", scope: "personal", path: personal(".agents", "skills")},
			{provider: "codex", scope: "admin", path: codexAdminSkills},
		},
		"antigravity": {
			{provider: "shared", scope: "project", path: project(".agents", "skills")},
			{provider: "antigravity", scope: "project", path: project(".gemini", "antigravity-cli", "skills")},
			{provider: "antigravity", scope: "personal", path: personal(".gemini", "antigravity-cli", "skills")},
		},
	}
	result := []rootSpec{}
	seenPath := map[string]bool{}
	add := func(root rootSpec, isNative bool) {
		if root.path == "" {
			return
		}
		clean := filepath.Clean(root.path)
		if seenPath[clean] {
			return
		}
		root.path, root.native, root.order = clean, isNative, len(result)
		result = append(result, root)
		seenPath[clean] = true
	}
	for _, root := range native[provider] {
		add(root, true)
	}

	pluginBases := []struct {
		provider string
		base     string
	}{
		{provider: "claude", base: personal(".claude", "plugins", "cache")},
		{provider: "codex", base: personal(".codex", "plugins", "cache")},
		{provider: "copilot", base: copilot("installed-plugins")},
		{provider: "antigravity", base: personal(".gemini", "antigravity-cli", "plugins")},
	}
	for _, plugins := range pluginBases {
		if plugins.provider != provider {
			continue
		}
		for _, path := range nestedSkillRoots(plugins.base) {
			add(rootSpec{provider: plugins.provider, scope: "plugin", path: path}, true)
		}
	}
	for _, path := range commaSeparatedAbsolutePaths(env(environ, "COPILOT_SKILLS_DIRS")) {
		if provider == "copilot" {
			add(rootSpec{provider: "copilot", scope: "custom", path: path}, true)
		}
	}

	all := []rootSpec{
		{provider: "claude", scope: "personal", path: personal(".claude", "skills")},
		{provider: "codex", scope: "personal", path: personal(".codex", "skills")},
		{provider: "copilot", scope: "personal", path: copilot("skills")},
		{provider: "antigravity", scope: "personal", path: personal(".gemini", "antigravity-cli", "skills")},
		{provider: "shared", scope: "personal", path: personal(".agents", "skills")},
		{provider: "copilot", scope: "project", path: project(".github", "skills")},
		{provider: "claude", scope: "project", path: project(".claude", "skills")},
		{provider: "codex", scope: "project", path: project(".codex", "skills")},
		{provider: "shared", scope: "project", path: project(".agents", "skills")},
	}
	for _, root := range all {
		add(root, false)
	}
	for _, plugins := range pluginBases {
		for _, path := range nestedSkillRoots(plugins.base) {
			add(rootSpec{provider: plugins.provider, scope: "plugin", path: path}, false)
		}
	}
	for _, path := range commaSeparatedAbsolutePaths(env(environ, "COPILOT_SKILLS_DIRS")) {
		add(rootSpec{provider: "copilot", scope: "custom", path: path}, false)
	}
	return result
}

func commaSeparatedAbsolutePaths(value string) []string {
	result := []string{}
	for _, item := range strings.Split(value, ",") {
		path := strings.TrimSpace(item)
		if filepath.IsAbs(path) {
			result = append(result, filepath.Clean(path))
		}
	}
	return uniquePaths(result)
}

func nestedSkillRoots(base string) []string {
	if base == "" {
		return nil
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	result := []string{}
	_ = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == base {
			return nil
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return filepath.SkipDir
		}
		depth := len(strings.Split(relative, string(filepath.Separator)))
		if entry.Type()&os.ModeSymlink != 0 || (entry.IsDir() && depth > 6) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() && entry.Name() == "skills" {
			result = append(result, path)
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(result)
	if len(result) > 64 {
		result = result[:64]
	}
	return result
}

func readRoot(root rootSpec, remaining int) ([]string, []Skill) {
	if root.path == "" || remaining <= 0 {
		return nil, nil
	}
	info, err := os.Lstat(root.path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil
	}
	result := []Skill{}
	_ = filepath.WalkDir(root.path, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == root.path {
			return nil
		}
		relative, err := filepath.Rel(root.path, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		depth := len(strings.Split(relative, string(filepath.Separator)))
		if entry.Type()&os.ModeSymlink != 0 || (entry.IsDir() && depth >= 3) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" || depth > 3 || len(result) >= remaining {
			return nil
		}
		fileInfo, err := os.Lstat(path)
		if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		name, description, ok := readMetadata(path)
		if ok {
			result = append(result, Skill{Name: name, Description: description})
		}
		return nil
	})
	if len(result) == 0 {
		return nil, nil
	}
	return []string{root.path}, result
}

func readMetadata(path string) (string, string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxFrontmatterBytes+1))
	if err != nil || len(data) > maxFrontmatterBytes || !utf8.Valid(data) {
		return "", "", false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false
	}
	values := map[string]string{}
	closed := false
	for index := 1; index < len(lines); index++ {
		line := strings.TrimSuffix(lines[index], "\r")
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		key, raw, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !ok || (key != "name" && key != "description") {
			continue
		}
		raw = strings.TrimSpace(raw)
		if raw == "|" || raw == ">" || raw == "|-" || raw == ">-" {
			parts := []string{}
			for index+1 < len(lines) {
				next := strings.TrimSuffix(lines[index+1], "\r")
				if next == "" || next[0] == ' ' || next[0] == '\t' {
					index++
					if strings.TrimSpace(next) != "" {
						parts = append(parts, strings.TrimSpace(next))
					}
					continue
				}
				break
			}
			values[key] = strings.Join(parts, " ")
			continue
		}
		values[key] = unquote(raw)
	}
	if !closed {
		return "", "", false
	}
	name := sanitize(values["name"], 64)
	description := sanitize(values["description"], maxDescriptionRunes)
	if name == "" || description == "" {
		return "", "", false
	}
	return name, description, true
}

func unquote(value string) string {
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	if decoded, err := strconv.Unquote(value); err == nil {
		return decoded
	}
	return value
}

func sanitize(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	words := strings.Fields(value)
	for i, word := range words {
		lower := strings.ToLower(strings.Trim(word, ".,;:()[]{}"))
		if (strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "ghp_") || strings.HasPrefix(lower, "xoxb-") || strings.HasPrefix(lower, "aiza")) && len(lower) > 12 {
			words[i] = "[redacted]"
		}
	}
	value = strings.Join(words, " ")
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit-1]) + "…"
	}
	return value
}

func relevance(task, value string) int {
	terms := map[string]bool{}
	for _, term := range strings.FieldsFunc(strings.ToLower(task), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(term) >= 3 {
			terms[term] = true
		}
	}
	score := 0
	value = strings.ToLower(value)
	for term := range terms {
		if strings.Contains(value, term) {
			score++
		}
	}
	return score
}

func executable(environ []string, override, name string) string {
	if path := env(environ, override); path != "" {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return path
		}
		return ""
	}
	for _, directory := range filepath.SplitList(env(environ, "PATH")) {
		if directory == "" {
			continue
		}
		path := filepath.Join(directory, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return path
		}
	}
	return ""
}

func env(environ []string, key string) string {
	for i := len(environ) - 1; i >= 0; i-- {
		if name, value, ok := strings.Cut(environ[i], "="); ok && name == key {
			return value
		}
	}
	return ""
}

func uniquePaths(paths []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, path := range paths {
		path = filepath.Clean(path)
		if path != "." && !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	return result
}
