package task

import (
	"strings"
	"testing"
)

func TestWorkUnitGraphBuildsParallelWaves(t *testing.T) {
	units, err := NormalizeWorkUnits([]WorkUnit{
		{ID: "T1", Objective: "API", Scope: "internal/api", ExecutionPacket: "Implement API", AcceptanceCriteria: []string{"API works"}, ValidationCommands: []string{"go test ./internal/api"}},
		{ID: "T2", Objective: "CLI", Scope: "cmd/tool", ExecutionPacket: "Implement CLI", AcceptanceCriteria: []string{"CLI works"}, ValidationCommands: []string{"go test ./cmd/tool"}},
		{ID: "T3", Objective: "Wire", Scope: "internal/wire", DependsOn: []string{"T1", "T2"}, ExecutionPacket: "Wire both", AcceptanceCriteria: []string{"Integrated"}, ValidationCommands: []string{"go test ./..."}},
	}, "plan")
	if err != nil {
		t.Fatal(err)
	}
	waves, err := WorkUnitWaves(units)
	if err != nil || len(waves) != 2 || strings.Join(waves[0], ",") != "T1,T2" || strings.Join(waves[1], ",") != "T3" {
		t.Fatalf("waves=%v err=%v", waves, err)
	}
}

func TestWorkUnitGraphRejectsUnsafeParallelScopesAndCycles(t *testing.T) {
	base := func(id, scope string) WorkUnit {
		return WorkUnit{ID: id, Objective: id, Scope: scope, ExecutionPacket: id, AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test"}}
	}
	t1, t2 := base("T1", "internal/api"), base("T2", "internal/api/client.go")
	if _, err := NormalizeWorkUnits([]WorkUnit{t1, t2}, "plan"); err == nil || !strings.Contains(err.Error(), "overlapping scopes") {
		t.Fatalf("unsafe parallel scopes accepted: %v", err)
	}
	t1.DependsOn, t2.DependsOn = []string{"T2"}, []string{"T1"}
	if _, err := NormalizeWorkUnits([]WorkUnit{t1, t2}, "plan"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle accepted: %v", err)
	}
}

func TestWorkUnitFallbackAndDerivedIDs(t *testing.T) {
	units, err := NormalizeWorkUnits(nil, "legacy plan")
	if err != nil || len(units) != 1 || units[0].ExecutionPacket != "legacy plan" || units[0].Scope != "**" {
		t.Fatalf("units=%#v err=%v", units, err)
	}
	longParent := strings.Repeat("a", 128)
	for _, id := range []string{WorkTaskID(longParent, "T1"), IntegrationTaskID(longParent)} {
		if len(id) > 128 || !validID(id) {
			t.Fatalf("invalid derived id %q", id)
		}
	}
}
