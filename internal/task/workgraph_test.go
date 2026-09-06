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

func TestPlannerScopesRejectProseAndAnnotations(t *testing.T) {
	base := WorkUnit{ID: "T1", Objective: "change", ExecutionPacket: "packet", AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test"}}
	for _, scope := range []string{"Write only internal/a.go", "internal/a.go (new)", "internal/a.go,and internal/b.go", "internal/a.go."} {
		unit := base
		unit.Scope = scope
		if _, err := NormalizeWorkUnits([]WorkUnit{unit}, "plan"); err == nil || !strings.Contains(err.Error(), "bare repository paths") {
			t.Fatalf("scope %q err=%v", scope, err)
		}
	}
}

func TestComplexityBoundsWorkGraphSize(t *testing.T) {
	unit := func(id string) WorkUnit {
		return WorkUnit{ID: id, Objective: id, Scope: id + ".go", ExecutionPacket: id, AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test"}}
	}
	for _, tc := range []struct {
		complexity string
		units      []WorkUnit
		wantError  bool
	}{
		{ComplexityTrivial, []WorkUnit{unit("T1")}, false},
		{ComplexityTrivial, []WorkUnit{unit("T1"), unit("T2")}, true},
		{ComplexitySmall, []WorkUnit{unit("T1"), unit("T2")}, false},
		{ComplexitySmall, []WorkUnit{unit("T1"), unit("T2"), unit("T3")}, true},
		{ComplexityLarge, []WorkUnit{unit("T1"), unit("T2"), unit("T3")}, false},
	} {
		err := ValidateWorkUnitsForComplexity(tc.complexity, tc.units)
		if (err != nil) != tc.wantError {
			t.Fatalf("complexity=%s units=%d err=%v", tc.complexity, len(tc.units), err)
		}
	}
}

func TestContextGroupsAreOrderedAndCriticalPathUsesEstimates(t *testing.T) {
	units, err := NormalizeWorkUnits([]WorkUnit{
		{ID: "T1", Objective: "core", Scope: "core.go", ContextGroup: "core", ContextFiles: []string{"core.go"}, EstimatedMinutes: 7, ExecutionPacket: "core", AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test core"}},
		{ID: "T2", Objective: "follow-up", Scope: "follow.go", ContextGroup: "core", ContextFiles: []string{"core.go", "follow.go"}, EstimatedMinutes: 4, DependsOn: []string{"T1"}, ExecutionPacket: "follow", AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test follow"}},
		{ID: "T3", Objective: "independent", Scope: "other.go", ContextGroup: "other", ContextFiles: []string{"other.go"}, EstimatedMinutes: 9, ExecutionPacket: "other", AcceptanceCriteria: []string{"done"}, ValidationCommands: []string{"test other"}},
	}, "plan")
	if err != nil {
		t.Fatal(err)
	}
	path, minutes, err := WorkUnitCriticalPath(units)
	if err != nil || strings.Join(path, ",") != "T1,T2" || minutes != 11 {
		t.Fatalf("path=%v minutes=%d err=%v", path, minutes, err)
	}
	bad := append([]WorkUnit(nil), units...)
	bad[1].DependsOn = nil
	if err := ValidateWorkUnits(bad); err == nil || !strings.Contains(err.Error(), "context-sharing") {
		t.Fatalf("unordered context group accepted: %v", err)
	}
}
