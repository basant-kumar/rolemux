package task

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	defaultWorkUnitID = "T1"
	maxWorkUnitIDLen  = 32
)

// WorkUnit is an execution-ready node in a planner-produced dependency graph.
// Scope is the node's exclusive write scope; the execution packet must carry
// all context the implementer needs without repeating broad repository research.
type WorkUnit struct {
	ID                 string   `json:"id"`
	Objective          string   `json:"objective"`
	Scope              string   `json:"scope"`
	DependsOn          []string `json:"depends_on"`
	ExecutionPacket    string   `json:"execution_packet"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	ValidationCommands []string `json:"validation_commands"`
}

// NormalizeWorkUnits canonicalizes scopes and validates graph safety. The
// single-node fallback preserves old provider sessions created before planner
// envelopes carried a graph.
func NormalizeWorkUnits(units []WorkUnit, plan string) ([]WorkUnit, error) {
	if len(units) == 0 {
		units = []WorkUnit{{
			ID: defaultWorkUnitID, Objective: "Execute the approved plan", Scope: "**",
			ExecutionPacket: plan, AcceptanceCriteria: []string{"The approved plan is implemented"},
			ValidationCommands: []string{"Run the task-relevant validation named in the approved plan"},
		}}
	}
	result := make([]WorkUnit, len(units))
	for i, unit := range units {
		result[i] = unit
		result[i].ID = strings.TrimSpace(unit.ID)
		result[i].Objective = strings.TrimSpace(unit.Objective)
		result[i].ExecutionPacket = strings.TrimSpace(unit.ExecutionPacket)
		result[i].DependsOn = cleanStrings(unit.DependsOn)
		result[i].AcceptanceCriteria = cleanStrings(unit.AcceptanceCriteria)
		result[i].ValidationCommands = cleanStrings(unit.ValidationCommands)
		scope, err := CanonicalScope(unit.Scope)
		if err != nil {
			return nil, fmt.Errorf("work unit %s scope: %w", result[i].ID, err)
		}
		result[i].Scope = scope
	}
	if err := ValidateWorkUnits(result); err != nil {
		return nil, err
	}
	return result, nil
}

func ValidateWorkUnits(units []WorkUnit) error {
	if len(units) == 0 {
		return errors.New("plan has no work units")
	}
	byID := make(map[string]WorkUnit, len(units))
	caseFoldedIDs := make(map[string]string, len(units))
	for _, unit := range units {
		if !validWorkUnitID(unit.ID) {
			return fmt.Errorf("invalid work unit id %q", unit.ID)
		}
		if _, exists := byID[unit.ID]; exists {
			return fmt.Errorf("duplicate work unit id %q", unit.ID)
		}
		folded := strings.ToLower(unit.ID)
		if previous, exists := caseFoldedIDs[folded]; exists {
			return fmt.Errorf("work unit ids %q and %q collide on a case-insensitive filesystem", previous, unit.ID)
		}
		if unit.Objective == "" || unit.ExecutionPacket == "" || unit.Scope == "" {
			return fmt.Errorf("work unit %s lacks objective, scope, or execution packet", unit.ID)
		}
		if len(unit.AcceptanceCriteria) == 0 || len(unit.ValidationCommands) == 0 {
			return fmt.Errorf("work unit %s lacks acceptance criteria or validation commands", unit.ID)
		}
		byID[unit.ID] = unit
		caseFoldedIDs[folded] = unit.ID
	}
	for _, unit := range units {
		seen := map[string]bool{}
		for _, dependency := range unit.DependsOn {
			if dependency == unit.ID {
				return fmt.Errorf("work unit %s depends on itself", unit.ID)
			}
			if _, exists := byID[dependency]; !exists {
				return fmt.Errorf("work unit %s depends on unknown unit %s", unit.ID, dependency)
			}
			if seen[dependency] {
				return fmt.Errorf("work unit %s repeats dependency %s", unit.ID, dependency)
			}
			seen[dependency] = true
		}
	}
	waves, err := WorkUnitWaves(units)
	if err != nil || len(waves) == 0 {
		if err == nil {
			err = errors.New("work-unit graph is empty")
		}
		return err
	}
	for left := 0; left < len(units); left++ {
		for right := left + 1; right < len(units); right++ {
			a, b := units[left], units[right]
			if !ScopesOverlap(a.Scope, b.Scope) {
				continue
			}
			if !workUnitDependsTransitively(byID, a.ID, b.ID) && !workUnitDependsTransitively(byID, b.ID, a.ID) {
				return fmt.Errorf("parallel work units %s and %s have overlapping scopes", a.ID, b.ID)
			}
		}
	}
	return nil
}

// WorkUnitWaves returns deterministic topological layers. Every unit in one
// layer can be scheduled concurrently after all earlier layers are approved.
func WorkUnitWaves(units []WorkUnit) ([][]string, error) {
	indegree := map[string]int{}
	children := map[string][]string{}
	for _, unit := range units {
		if _, exists := indegree[unit.ID]; exists {
			return nil, fmt.Errorf("duplicate work unit id %q", unit.ID)
		}
		indegree[unit.ID] = len(unit.DependsOn)
		for _, dependency := range unit.DependsOn {
			children[dependency] = append(children[dependency], unit.ID)
		}
	}
	var waves [][]string
	processed := 0
	for processed < len(units) {
		var wave []string
		for id, degree := range indegree {
			if degree == 0 {
				wave = append(wave, id)
			}
		}
		if len(wave) == 0 {
			return nil, errors.New("work-unit dependency graph contains a cycle")
		}
		sort.Strings(wave)
		waves = append(waves, wave)
		for _, id := range wave {
			delete(indegree, id)
			processed++
			for _, child := range children[id] {
				indegree[child]--
			}
		}
	}
	return waves, nil
}

func WorkTaskID(parentID, unitID string) string {
	return derivedTaskID(parentID, unitID)
}

func IntegrationTaskID(parentID string) string {
	return derivedTaskID(parentID, "integration")
}

func derivedTaskID(parentID, suffix string) string {
	candidate := parentID + "--" + suffix
	if len(candidate) <= 128 && validID(candidate) {
		return candidate
	}
	digest := digestBytes([]byte(candidate))[:12]
	maxPrefix := 128 - len(digest) - 2
	if maxPrefix > len(parentID) {
		maxPrefix = len(parentID)
	}
	return parentID[:maxPrefix] + "--" + digest
}

func cleanStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func validWorkUnitID(id string) bool {
	if id == "" || len(id) > maxWorkUnitIDLen {
		return false
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func workUnitDependsTransitively(byID map[string]WorkUnit, from, target string) bool {
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if seen[id] {
			return false
		}
		seen[id] = true
		for _, dependency := range byID[id].DependsOn {
			if dependency == target || visit(dependency) {
				return true
			}
		}
		return false
	}
	return visit(from)
}
