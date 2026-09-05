package runner

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CapabilityProber is implemented by adapters whose security contract relies
// on a particular installed CLI flag surface.
type CapabilityProber interface {
	Probe(context.Context) error
}

func (c *Codex) Probe(ctx context.Context) error {
	version, err := c.Version(ctx)
	if err != nil {
		return fmt.Errorf("read Codex version: %w", err)
	}
	if !versionAtLeast(version, 0, 153, 3) {
		return fmt.Errorf("Codex %s is older than required 0.153.3", version)
	}
	checks := []struct {
		args     []string
		required []string
	}{
		{[]string{"--help"}, []string{"--cd", "--sandbox", "--ask-for-approval", "--search"}},
		{[]string{"exec", "--help"}, []string{"--ignore-user-config", "--ignore-rules", "--output-schema", "--json"}},
		{[]string{"exec", "resume", "--help"}, []string{"--ignore-user-config", "--ignore-rules", "--output-schema", "--json", "SESSION_ID"}},
	}
	for _, check := range checks {
		result, runErr := c.Process(ctx, ProcessSpec{Path: c.Path, Args: check.args, Env: c.Env, MaxOutputBytes: 2 << 20})
		if runErr != nil {
			return fmt.Errorf("Codex %s capability probe failed", strings.Join(check.args, " "))
		}
		text := string(result.Stdout)
		for _, required := range check.required {
			if !strings.Contains(text, required) {
				return fmt.Errorf("Codex %s lacks required %s capability", version, required)
			}
		}
	}
	return nil
}

func (c *Claude) Probe(ctx context.Context) error {
	version, err := c.Version(ctx)
	if err != nil {
		return fmt.Errorf("read Claude version: %w", err)
	}
	if !versionAtLeast(version, 2, 1, 260) {
		return fmt.Errorf("Claude %s is older than required 2.1.260", version)
	}
	result, runErr := c.Process(ctx, ProcessSpec{Path: c.Path, Args: []string{"--help"}, Env: c.Env, MaxOutputBytes: 2 << 20})
	if runErr != nil {
		return errorsWithoutOutput("Claude capability probe failed", runErr)
	}
	text := string(result.Stdout)
	for _, required := range []string{"--safe-mode", "--restricted", "--permission-mode", "--permission-prompts", "--tools", "--allowed-tools", "--strict-mcp-config", "--mcp-config", "--session-id", "--resume", "--json-schema", "--effort"} {
		if !strings.Contains(text, required) {
			return fmt.Errorf("Claude %s lacks required %s capability", version, required)
		}
	}
	return nil
}

func errorsWithoutOutput(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", message)
}

var semanticVersion = regexp.MustCompile(`(?:^|[^0-9])(\d+)\.(\d+)\.(\d+)(?:[^0-9]|$)`)

func versionAtLeast(value string, major, minor, patch int) bool {
	match := semanticVersion.FindStringSubmatch(value)
	if len(match) != 4 {
		return false
	}
	got := [3]int{}
	for i := range got {
		got[i], _ = strconv.Atoi(match[i+1])
	}
	want := [3]int{major, minor, patch}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

var _ CapabilityProber = (*Codex)(nil)
var _ CapabilityProber = (*Claude)(nil)
