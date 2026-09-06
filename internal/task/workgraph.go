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
	ComplexityTrivial = "trivial"
	ComplexitySmall   = "small"
	ComplexityMedium  = "medium"
	ComplexityLarge   = "large"
	ComplexitySystem  = "system"
)

// WorkUnit is an execution-ready node in a planner-produced dependency graph.
// Scope is the node's exclusive write scope; the execution packet must carry
// all context the implementer needs without repeating broad repository research.
type WorkUnit struct {
	ID                 string   `json:"id"`
	Objective          string   `json:"objective"`
	Scope              string   `json:"scope"`
	DependsOn          []string `json:"depends_on"`
	ContextGroup       string   `json:"context_group"`
	ContextFiles       []string `json:"context_files"`
	AffectedSymbols    []string `json:"affected_symbols"`
	EstimatedMinutes   int      `json:"estimated_minutes"`
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
		result[i].ContextGroup = strings.TrimSpace(unit.ContextGroup)
		if result[i].ContextGroup == "" {
			result[i].ContextGroup = result[i].ID
		}
		result[i].ContextFiles = cleanStrings(unit.ContextFiles)
		if len(result[i].ContextFiles) == 0 {
			result[i].ContextFiles = ScopePatterns(unit.Scope)
		}
		result[i].AffectedSymbols = cleanStrings(unit.AffectedSymbols)
		if result[i].EstimatedMinutes == 0 {
			result[i].EstimatedMinutes = 5
		}
		result[i].DependsOn = cleanStrings(unit.DependsOn)
		result[i].AcceptanceCriteria = cleanStrings(unit.AcceptanceCriteria)
		result[i].ValidationCommands = cleanStrings(unit.ValidationCommands)
		if err := validatePlannerScope(unit.Scope); err != nil {
			return nil, fmt.Errorf("work unit %s scope: %w", result[i].ID, err)
		}
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

// ValidateComplexity keeps planner sizing machine-readable and stable across
// providers. Empty remains valid for durable tasks created before sizing was
// introduced.
func ValidateComplexity(complexity string) error {
	switch strings.ToLower(strings.TrimSpace(complexity)) {
	case "", ComplexityTrivial, ComplexitySmall, ComplexityMedium, ComplexityLarge, ComplexitySystem:
		return nil
	default:
		return fmt.Errorf("invalid task complexity %q", complexity)
	}
}

func NormalizeComplexity(complexity string) string {
	return strings.ToLower(strings.TrimSpace(complexity))
}

// ValidateWorkUnitsForComplexity prevents a planner from turning a local change into
// a miniature program. Large plans remain free to use the graph they need.
func ValidateWorkUnitsForComplexity(complexity string, units []WorkUnit) error {
	complexity = NormalizeComplexity(complexity)
	if err := ValidateComplexity(complexity); err != nil {
		return err
	}
	limit := 0
	switch complexity {
	case ComplexityTrivial:
		limit = 1
	case ComplexitySmall:
		limit = 2
	case ComplexityMedium:
		limit = 6
	}
	if limit > 0 && len(units) > limit {
		return fmt.Errorf("task complexity %s allows at most %d work unit(s), got %d", complexity, limit, len(units))
	}
	return nil
}

// Planner scopes are data, not prose. CanonicalScope intentionally accepts
// legitimate paths containing spaces; this narrower check rejects common
// model annotations that silently destroy manifest coverage.
func validatePlannerScope(raw string) error {
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		part = strings.TrimSpace(part)
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, "write ") || strings.HasPrefix(lower, "and ") ||
			strings.HasSuffix(lower, " (new)") || strings.HasSuffix(part, ".") {
			return fmt.Errorf("scope entries must be bare repository paths or globs, got %q", part)
		}
	}
	return nil
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
		if !validWorkUnitID(unit.ContextGroup) {
			return fmt.Errorf("work unit %s has invalid context group %q", unit.ID, unit.ContextGroup)
		}
		if unit.EstimatedMinutes < 1 || unit.EstimatedMinutes > 240 {
			return fmt.Errorf("work unit %s estimated minutes must be between 1 and 240", unit.ID)
		}
		if len(unit.ContextFiles) == 0 {
			return fmt.Errorf("work unit %s lacks authoritative context files", unit.ID)
		}
		for _, path := range unit.ContextFiles {
			if err := validatePlannerScope(path); err != nil {
				return fmt.Errorf("work unit %s context file: %w", unit.ID, err)
			}
			if _, err := CanonicalScope(path); err != nil {
				return fmt.Errorf("work unit %s context file: %w", unit.ID, err)
			}
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
			if a.ContextGroup == b.ContextGroup && !workUnitDependsTransitively(byID, a.ID, b.ID) && !workUnitDependsTransitively(byID, b.ID, a.ID) {
				return fmt.Errorf("context-sharing work units %s and %s must be dependency-ordered", a.ID, b.ID)
			}
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

// WorkUnitCriticalPath returns the deterministic longest dependency path by
// planner-supplied focused minutes. It is scheduling guidance, not a promise.
func WorkUnitCriticalPath(units []WorkUnit) ([]string, int, error) {
	if _, err := WorkUnitWaves(units); err != nil {
		return nil, 0, err
	}
	byID := make(map[string]WorkUnit, len(units))
	for _, unit := range units {
		byID[unit.ID] = unit
	}
	type pathCost struct {
		path []string
		cost int
	}
	memo := map[string]pathCost{}
	var visit func(string) pathCost
	visit = func(id string) pathCost {
		if value, ok := memo[id]; ok {
			return value
		}
		unit := byID[id]
		best := pathCost{}
		for _, dependency := range unit.DependsOn {
			candidate := visit(dependency)
			if candidate.cost > best.cost || candidate.cost == best.cost && strings.Join(candidate.path, "\x00") < strings.Join(best.path, "\x00") {
				best = candidate
			}
		}
		minutes := unit.EstimatedMinutes
		if minutes <= 0 {
			minutes = 5
		}
		result := pathCost{path: append(append([]string(nil), best.path...), id), cost: best.cost + minutes}
		memo[id] = result
		return result
	}
	best := pathCost{}
	ids := make([]string, 0, len(units))
	for _, unit := range units {
		ids = append(ids, unit.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		candidate := visit(id)
		if candidate.cost > best.cost || candidate.cost == best.cost && strings.Join(candidate.path, "\x00") < strings.Join(best.path, "\x00") {
			best = candidate
		}
	}
	return best.path, best.cost, nil
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
