package cli

import (
	"testing"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
)

func TestWizardViewUsesStructuredRoleAndStepPresentation(t *testing.T) {
	drafts := map[string]*profileDraft{
		config.RolePlanner: {
			provider: "claude",
			model:    runner.ModelInfo{ID: "model-id", Label: ""},
			effort:   "high",
			speed:    "fast",
		},
	}

	tests := []struct {
		name       string
		screen     wizardScreen
		step       string
		activeRole string
		context    string
		subtitle   string
	}{
		{
			name:     "role",
			screen:   wizardScreen{kind: wizardRole},
			step:     "Role",
			subtitle: "Choose which role to configure",
		},
		{
			name:     "review safety limit",
			screen:   wizardScreen{kind: wizardReviewMaxRounds},
			step:     "Review safety limit",
			context:  "Workflow settings",
			subtitle: "Choose the review safety limit; use --review-max-rounds N for an arbitrary value",
		},
		{
			name:       "provider",
			screen:     wizardScreen{kind: wizardProvider, role: config.RolePlanner},
			step:       "Provider",
			activeRole: "Planner",
			subtitle:   "Select provider",
		},
		{
			name:       "model",
			screen:     wizardScreen{kind: wizardModel, role: config.RolePlanner},
			step:       "Model",
			activeRole: "Planner",
			context:    "Provider: Claude Code",
			subtitle:   "Select model",
		},
		{
			name:       "verify model",
			screen:     wizardScreen{kind: wizardVerifyModel, role: config.RolePlanner},
			step:       "Verify model",
			activeRole: "Planner",
			context:    "Provider: Claude Code · Model: model-id",
			subtitle:   "model-id is not provider-verified. Select it anyway?",
		},
		{
			name:       "reasoning effort",
			screen:     wizardScreen{kind: wizardEffort, role: config.RolePlanner},
			step:       "Reasoning effort",
			activeRole: "Planner",
			context:    "Provider: Claude Code · Model: model-id",
			subtitle:   "Select reasoning effort",
		},
		{
			name:       "speed",
			screen:     wizardScreen{kind: wizardSpeed, role: config.RolePlanner},
			step:       "Speed",
			activeRole: "Planner",
			context:    "Provider: Claude Code · Model: model-id · Effort: high",
			subtitle:   "Select speed",
		},
		{
			name:       "plan reviewer setup",
			screen:     wizardScreen{kind: wizardSplitPlanReview},
			step:       "Reviewer setup",
			activeRole: "Plan reviewer",
			subtitle:   "Use a separate model for plan review?",
		},
		{
			name:       "code reviewer setup",
			screen:     wizardScreen{kind: wizardSplitCodeReview},
			step:       "Reviewer setup",
			activeRole: "Code reviewer",
			subtitle:   "Use a separate model for code review?",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view := wizardView(test.screen, true, "", drafts)
			if view.Title != "Configure RoleMux" || view.Step != test.step || view.ActiveRole != test.activeRole {
				t.Fatalf("view header=%#v", view)
			}
			if view.Context != test.context || view.Subtitle != test.subtitle {
				t.Fatalf("view details=%#v", view)
			}
		})
	}
}

func TestWizardViewKeepsNoticeSeparateAndDoesNotReuseProviderDraftDetails(t *testing.T) {
	drafts := map[string]*profileDraft{
		config.RolePlanner: {
			provider: "claude",
			model:    runner.ModelInfo{ID: "old-model", Label: "Old model"},
			effort:   "high",
			speed:    "fast",
		},
	}

	provider := wizardView(wizardScreen{kind: wizardProvider, role: config.RolePlanner}, true, "Provider failed", drafts)
	if provider.Subtitle != "Select provider" || provider.Notice != "Provider failed" || provider.Context != "" {
		t.Fatalf("provider view=%#v", provider)
	}

	for _, kind := range []wizardScreenKind{wizardModel, wizardVerifyModel, wizardEffort, wizardSpeed} {
		view := wizardView(wizardScreen{kind: kind, role: config.RoleImplementer}, true, "notice", drafts)
		if view.ActiveRole != "Implementer" || view.Notice != "notice" {
			t.Fatalf("kind=%v view=%#v", kind, view)
		}
	}

	for _, kind := range []wizardScreenKind{wizardModel, wizardVerifyModel, wizardEffort, wizardSpeed} {
		view := wizardView(wizardScreen{kind: kind, role: config.RolePlanner}, true, "", nil)
		if view.ActiveRole != "Planner" {
			t.Fatalf("kind=%v view=%#v", kind, view)
		}
	}
}

func TestWizardProviderStatusUsesSelectedProvider(t *testing.T) {
	drafts := map[string]*profileDraft{
		config.RolePlanner: {provider: "claude", model: runner.ModelInfo{ID: "old-model"}},
	}
	view := wizardProviderStatusView(wizardScreen{kind: wizardProvider, role: config.RolePlanner}, true, "", drafts, "codex")
	if view.ActiveRole != "Planner" || view.Step != "Provider" || view.Context != "Provider: Codex" {
		t.Fatalf("status view=%#v", view)
	}
}
