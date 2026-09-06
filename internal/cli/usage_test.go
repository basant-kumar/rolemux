package cli

import (
	"strings"
	"testing"

	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
)

func TestUsageAndStatusExposeAvailabilityMetadata(t *testing.T) {
	root := cliRepo(t)
	store := task.NewStore(root)
	state := task.State{
		ID: "usage-output", Phase: task.PhaseFailed,
		ProfilesSnapshot: map[string]task.ProfileSnapshot{
			string(runner.RolePlanner):     {Provider: "codex", Model: "planner"},
			string(runner.RoleImplementer): {Provider: "claude", Model: "implementer"},
		},
		Usage: map[string]task.TokenUsage{
			string(runner.RolePlanner): {
				Requests: 1, PromptBytes: 12, UnreportedRequests: 1,
			},
			string(runner.RoleImplementer): {
				Requests: 2, PromptBytes: 24, IncompleteRequests: 1,
				InputTokens: 20, CachedInputTokens: 5, OutputTokens: 7, TotalTokens: 27,
			},
		},
	}
	if err := store.Create(state); err != nil {
		t.Fatal(err)
	}

	code, output, stderr := runTestApp(t, root, "", "usage", state.ID, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("usage code=%d stderr=%q output=%s", code, stderr, output)
	}
	payload := decodeSingleObject(t, output)
	result, ok := payload["result"].(map[string]any)
	if !ok {
		t.Fatalf("result=%#v", payload["result"])
	}
	roles, ok := result["roles"].([]any)
	if !ok || len(roles) != 2 {
		t.Fatalf("roles=%#v", result["roles"])
	}
	if roles[0].(map[string]any)["role"] != string(runner.RoleImplementer) || roles[1].(map[string]any)["role"] != string(runner.RolePlanner) {
		t.Fatalf("roles are not sorted: %#v", roles)
	}
	planner := roles[1].(map[string]any)
	if planner["unreported_requests"] != float64(1) {
		t.Fatalf("planner availability=%#v", planner)
	}
	totals := result["totals"].(map[string]any)
	if totals["unreported_requests"] != float64(1) || totals["incomplete_requests"] != float64(1) || totals["requests"] != float64(3) {
		t.Fatalf("totals=%#v", totals)
	}
	if totals["uncached_input_tokens"] != float64(20) {
		t.Fatalf("uncached totals=%#v", totals)
	}

	code, output, stderr = runTestApp(t, root, "", "status", state.ID)
	if code != 0 || stderr != "" {
		t.Fatalf("status code=%d stderr=%q output=%s", code, stderr, output)
	}
	if !strings.Contains(string(output), "tokens are unreported") || !strings.Contains(string(output), "incomplete reported totals") {
		t.Fatalf("human status omitted availability labels: %s", output)
	}
	if !strings.Contains(string(output), "unreported=1") || !strings.Contains(string(output), "incomplete=1") {
		t.Fatalf("human status omitted availability counters: %s", output)
	}

	code, output, stderr = runTestApp(t, root, "", "status", state.ID, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("JSON status code=%d stderr=%q output=%s", code, stderr, output)
	}
	payload = decodeSingleObject(t, output)
	result = payload["result"].(map[string]any)
	statusUsage := result["usage"].(map[string]any)
	if statusUsage[string(runner.RolePlanner)].(map[string]any)["unreported_requests"] != float64(1) {
		t.Fatalf("status usage=%#v", statusUsage)
	}
}

func TestUsageHumanOutputNamesAllUnreportedTokens(t *testing.T) {
	root := cliRepo(t)
	store := task.NewStore(root)
	state := task.State{
		ID: "unreported-output", Phase: task.PhaseFailed,
		Usage: map[string]task.TokenUsage{
			string(runner.RolePlanner): {Requests: 1, PromptBytes: 8, UnreportedRequests: 1},
		},
	}
	if err := store.Create(state); err != nil {
		t.Fatal(err)
	}
	code, output, stderr := runTestApp(t, root, "", "usage", state.ID)
	if code != 0 || stderr != "" || !strings.Contains(string(output), "tokens are unreported") {
		t.Fatalf("code=%d stderr=%q output=%s", code, stderr, output)
	}
}

func TestVersionFlagAndUnknownCommand(t *testing.T) {
	root := cliRepo(t)
	code, output, stderr := runTestApp(t, root, "", "--version")
	if code != 0 || stderr != "" || string(output) != "vtest\n" {
		t.Fatalf("code=%d stderr=%q output=%q", code, stderr, output)
	}

	code, output, stderr = runTestApp(t, root, "", "version")
	if code == 0 || len(output) != 0 || !strings.Contains(string(stderr), `rolemux: USAGE: unknown command "version"`) {
		t.Fatalf("unknown command code=%d stderr=%q output=%q", code, stderr, output)
	}

	code, output, stderr = runTestApp(t, root, "", "--version", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("JSON version code=%d stderr=%q output=%s", code, stderr, output)
	}
	payload := decodeSingleObject(t, output)
	if payload["ok"] != true || payload["command"] != "version" {
		t.Fatalf("JSON version payload=%#v", payload)
	}
}

func TestNestedHelpFlagsReturnUsageWithoutRunningCommands(t *testing.T) {
	root := cliRepo(t)
	for _, args := range [][]string{
		{"plan", "start", "--help"},
		{"plan", "start", "-h"},
		{"quick", "start", "--help"},
		{"quick", "start", "-h"},
	} {
		code, output, stderr := runTestApp(t, root, "", args...)
		if code != 0 || stderr != "" {
			t.Fatalf("args=%v code=%d stderr=%q output=%s", args, code, stderr, output)
		}
		if !strings.Contains(string(output), "rolemux --version") {
			t.Fatalf("args=%v did not return usage: %s", args, output)
		}
		if strings.Contains(string(output), "rolemux version") {
			t.Fatalf("args=%v still exposed version alias: %s", args, output)
		}
	}
}
