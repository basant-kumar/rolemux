// Package provider defines the provider IDs understood by this RoleMux build.
// Keeping this list independent from adapters and configuration prevents CLI
// commands from growing provider-specific switches.
package provider

import "sort"

const (
	Codex   = "codex"
	Claude  = "claude"
	Copilot = "copilot"
)

var builtins = map[string]struct{}{
	Codex: {}, Claude: {}, Copilot: {},
}

// Known reports whether an ID is supported by this build.
func Known(name string) bool {
	_, ok := builtins[name]
	return ok
}

// Names returns a stable copy suitable for command output and iteration.
func Names() []string {
	names := make([]string, 0, len(builtins))
	for name := range builtins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
