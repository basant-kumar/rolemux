// Package provider defines the provider IDs understood by this RoleMux build.
// Keeping this list independent from adapters and configuration prevents CLI
// commands from growing provider-specific switches.
package provider

import "sort"

const (
	Codex       = "codex"
	Claude      = "claude"
	Copilot     = "copilot"
	Antigravity = "antigravity"
)

var preferredOrder = []string{Claude, Codex, Antigravity, Copilot}

var builtins = map[string]struct{}{
	Codex: {}, Claude: {}, Copilot: {}, Antigravity: {},
}

// Known reports whether an ID is supported by this build.
func Known(name string) bool {
	_, ok := builtins[name]
	return ok
}

// Names returns a stable copy suitable for command output and iteration.
func Names() []string {
	return append([]string(nil), preferredOrder...)
}

// SortNames applies the product display order to built-ins and places future
// extension providers alphabetically after them.
func SortNames(names []string) {
	rank := map[string]int{}
	for index, name := range preferredOrder {
		rank[name] = index
	}
	sort.Slice(names, func(i, j int) bool {
		left, leftKnown := rank[names[i]]
		right, rightKnown := rank[names[j]]
		if leftKnown != rightKnown {
			return leftKnown
		}
		if leftKnown {
			return left < right
		}
		return names[i] < names[j]
	})
}
