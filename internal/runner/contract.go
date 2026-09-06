package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/basant-kumar/rolemux/internal/task"
)

// Envelope is the only model output accepted by the workflow. Fields are
// intentionally small: natural-language prose cannot substitute for status or
// verdict, and strict decoding rejects additions that could be misinterpreted.
type Envelope struct {
	Role       string          `json:"role"`
	Status     string          `json:"status,omitempty"`
	Verdict    string          `json:"verdict,omitempty"`
	Plan       string          `json:"plan,omitempty"`
	Question   string          `json:"question,omitempty"`
	Complexity string          `json:"complexity,omitempty"`
	WorkUnits  []task.WorkUnit `json:"work_units,omitempty"`
	Findings   []task.Finding  `json:"findings,omitempty"`
}

func DecodeEnvelope(data []byte, expected Role) (Envelope, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.HasPrefix(data, []byte("```")) {
		return Envelope{}, fmt.Errorf("%w: empty/prose/fenced response", ErrInvalidEnvelope)
	}
	var env Envelope
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrInvalidEnvelope, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Envelope{}, fmt.Errorf("%w: multiple JSON values", ErrInvalidEnvelope)
		}
		return Envelope{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidEnvelope, err)
	}
	if err := ValidateEnvelope(env, expected); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

func ValidateEnvelope(env Envelope, expected Role) error {
	if strings.TrimSpace(env.Role) == "" || env.Role != string(expected) {
		return fmt.Errorf("%w: expected role %q, got %q", ErrInvalidEnvelope, expected, env.Role)
	}
	switch expected {
	case RolePlanner, RoleImplementer:
		if env.Verdict != "" {
			return fmt.Errorf("%w: worker envelope has verdict", ErrInvalidEnvelope)
		}
		if env.Status != "ready" && env.Status != "needs_input" {
			return fmt.Errorf("%w: unsupported worker status", ErrInvalidEnvelope)
		}
		if env.Status == "ready" && expected == RolePlanner && strings.TrimSpace(env.Plan) == "" {
			return fmt.Errorf("%w: planner ready response has no plan", ErrInvalidEnvelope)
		}
		if expected == RolePlanner && env.Status == "ready" {
			if err := task.ValidateComplexity(env.Complexity); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidEnvelope, err)
			}
		}
		if env.Status == "ready" && env.Question != "" {
			return fmt.Errorf("%w: ready response has question", ErrInvalidEnvelope)
		}
		if env.Status == "needs_input" && strings.TrimSpace(env.Question) == "" {
			return fmt.Errorf("%w: question is empty", ErrInvalidEnvelope)
		}
		if expected == RolePlanner && env.Status == "ready" && len(env.WorkUnits) > 0 {
			units, err := task.NormalizeWorkUnits(env.WorkUnits, env.Plan)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidEnvelope, err)
			}
			if err := task.ValidateWorkUnitsForComplexity(env.Complexity, units); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidEnvelope, err)
			}
		}
	case RolePlanReviewer, RoleCodeReviewer:
		if env.Status != "" || env.Plan != "" || env.Question != "" {
			return fmt.Errorf("%w: reviewer envelope has worker fields", ErrInvalidEnvelope)
		}
		if env.Verdict != "approved" && env.Verdict != "changes_requested" {
			return fmt.Errorf("%w: unsupported reviewer verdict", ErrInvalidEnvelope)
		}
		if env.Findings == nil {
			return fmt.Errorf("%w: reviewer findings field is required", ErrInvalidEnvelope)
		}
		if env.Verdict == "approved" && len(env.Findings) != 0 {
			return fmt.Errorf("%w: approved review has findings", ErrInvalidEnvelope)
		}
		if env.Verdict == "changes_requested" && len(env.Findings) == 0 {
			return fmt.Errorf("%w: changes_requested has no findings", ErrInvalidEnvelope)
		}
	default:
		return fmt.Errorf("%w: unknown role", ErrInvalidEnvelope)
	}
	for _, finding := range env.Findings {
		if strings.TrimSpace(finding.Message) == "" {
			return fmt.Errorf("%w: finding message is empty", ErrInvalidEnvelope)
		}
		if finding.Line < 0 {
			return fmt.Errorf("%w: finding line is negative", ErrInvalidEnvelope)
		}
	}
	return nil
}

// NativeSchema returns the inline schema supplied to Codex/Claude. The
// schema is deliberately generated locally and contains no provider data.
func NativeSchema(role Role) string {
	var required []string
	roleValue, _ := json.Marshal(string(role))
	properties := fmt.Sprintf(`"role":{"type":"string","enum":[%s]}`, roleValue)
	if role == RolePlanner || role == RoleImplementer {
		// Strict structured-output APIs require every declared property to be
		// listed in required. Unused plan/question values are empty strings and
		// semantic validation below still enforces the role/status combination.
		required = []string{"role", "status", "plan", "question"}
		properties += `,"status":{"type":"string","enum":["ready","needs_input"]},"plan":{"type":"string"},"question":{"type":"string"}`
		if role == RolePlanner {
			required = append(required, "complexity", "work_units")
			properties += `,"complexity":{"type":"string","enum":["trivial","small","medium","large","system"]},"work_units":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","objective","scope","depends_on","context_group","context_files","affected_symbols","estimated_minutes","execution_packet","acceptance_criteria","validation_commands"],"properties":{"id":{"type":"string"},"objective":{"type":"string"},"scope":{"type":"string"},"depends_on":{"type":"array","items":{"type":"string"}},"context_group":{"type":"string"},"context_files":{"type":"array","items":{"type":"string"}},"affected_symbols":{"type":"array","items":{"type":"string"}},"estimated_minutes":{"type":"integer","minimum":1,"maximum":240},"execution_packet":{"type":"string"},"acceptance_criteria":{"type":"array","items":{"type":"string"}},"validation_commands":{"type":"array","items":{"type":"string"}}}}}`
		}
	} else {
		required = []string{"role", "verdict", "findings"}
		properties += `,"verdict":{"type":"string","enum":["approved","changes_requested"]},"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["severity","path","line","message"],"properties":{"severity":{"type":"string"},"path":{"type":"string"},"line":{"type":"integer","minimum":0},"message":{"type":"string"}}}}`
	}
	b, _ := json.Marshal(required)
	return fmt.Sprintf(`{"type":"object","additionalProperties":false,"required":%s,"properties":{%s}}`, b, properties)
}

func CopilotEnvelopePrompt(role Role) string {
	return fmt.Sprintf("Respond with exactly one JSON object and no markdown, prose, or additional values. It must validate against this role envelope: %s. Your role is %s.", NativeSchema(role), role)
}
