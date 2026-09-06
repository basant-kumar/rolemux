package cli

import (
	"strings"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/picker"
	"github.com/basant-kumar/rolemux/internal/runner"
)

func wizardView(screen wizardScreen, canBack bool, notice string, drafts map[string]*profileDraft) picker.View {
	view := picker.View{
		Title:      "Configure RoleMux",
		Step:       wizardStep(screen.kind),
		ActiveRole: wizardActiveRole(screen),
		CanBack:    canBack,
		FullScreen: true,
		Notice:     notice,
	}

	switch screen.kind {
	case wizardRole:
		view.Subtitle = "Choose which role to configure"
	case wizardReviewMaxRounds:
		view.Context = "Workflow settings"
		view.Subtitle = "Choose the review safety limit; use --review-max-rounds N for an arbitrary value"
	case wizardProvider:
		view.Subtitle, view.Search = "Select provider", true
	case wizardModel:
		view.Subtitle, view.Search = "Select model", true
		if draft := profileDraftFor(drafts, screen.role); draft != nil {
			view.Context = providerContext(draft.provider)
		}
	case wizardVerifyModel:
		view.Subtitle = unverifiedModelInstruction(profileDraftFor(drafts, screen.role))
		view.Context = modelSelectionContext(profileDraftFor(drafts, screen.role), false)
	case wizardEffort:
		view.Subtitle = "Select reasoning effort"
		view.Context = modelSelectionContext(profileDraftFor(drafts, screen.role), false)
	case wizardSpeed:
		view.Subtitle = "Select speed"
		view.Context = modelSelectionContext(profileDraftFor(drafts, screen.role), true)
	case wizardSplitPlanReview:
		view.Subtitle = "Use a separate model for plan review?"
	case wizardSplitCodeReview:
		view.Subtitle = "Use a separate model for code review?"
	}
	return view
}

func wizardProviderStatusView(screen wizardScreen, canBack bool, notice string, drafts map[string]*profileDraft, provider string) picker.View {
	view := wizardView(screen, canBack, notice, drafts)
	view.Context = providerContext(provider)
	return view
}

func wizardStep(kind wizardScreenKind) string {
	switch kind {
	case wizardRole:
		return "Role"
	case wizardReviewMaxRounds:
		return "Review safety limit"
	case wizardProvider:
		return "Provider"
	case wizardModel:
		return "Model"
	case wizardVerifyModel:
		return "Verify model"
	case wizardEffort:
		return "Reasoning effort"
	case wizardSpeed:
		return "Speed"
	case wizardSplitPlanReview, wizardSplitCodeReview:
		return "Reviewer setup"
	default:
		return ""
	}
}

func wizardActiveRole(screen wizardScreen) string {
	switch screen.kind {
	case wizardRole, wizardReviewMaxRounds:
		return ""
	case wizardSplitPlanReview:
		return roleDisplayName(config.RolePlanReviewer)
	case wizardSplitCodeReview:
		return roleDisplayName(config.RoleCodeReviewer)
	default:
		if strings.TrimSpace(screen.role) == "" {
			return ""
		}
		return roleDisplayName(screen.role)
	}
}

func roleDisplayName(role string) string {
	switch role {
	case config.RolePlanner:
		return "Planner"
	case config.RoleImplementer:
		return "Implementer"
	case config.RoleReviewer:
		return "Shared reviewer"
	case config.RolePlanReviewer:
		return "Plan reviewer"
	case config.RoleCodeReviewer:
		return "Code reviewer"
	}
	label := strings.NewReplacer("_", " ", "-", " ").Replace(strings.TrimSpace(role))
	if label == "" {
		return "Review roles"
	}
	runes := []rune(label)
	return strings.ToUpper(string(runes[0])) + string(runes[1:])
}

func profileDraftFor(drafts map[string]*profileDraft, role string) *profileDraft {
	if drafts == nil || strings.TrimSpace(role) == "" {
		return nil
	}
	return drafts[role]
}

func providerContext(provider string) string {
	if strings.TrimSpace(provider) == "" {
		return ""
	}
	return "Provider: " + providerDisplayName(provider)
}

func modelSelectionContext(draft *profileDraft, includeEffort bool) string {
	if draft == nil {
		return ""
	}
	parts := []string{providerContext(draft.provider)}
	if model := modelDisplayLabel(draft.model); model != "" {
		parts = append(parts, "Model: "+model)
	}
	if includeEffort && strings.TrimSpace(draft.effort) != "" {
		parts = append(parts, "Effort: "+draft.effort)
	}
	return strings.Join(nonEmptyStrings(parts), " · ")
}

func modelDisplayLabel(model runner.ModelInfo) string {
	if strings.TrimSpace(model.Label) != "" {
		return model.Label
	}
	return model.ID
}

func unverifiedModelInstruction(draft *profileDraft) string {
	if draft == nil {
		return "This model is not provider-verified. Select it anyway?"
	}
	if model := modelDisplayLabel(draft.model); model != "" {
		return model + " is not provider-verified. Select it anyway?"
	}
	return "This model is not provider-verified. Select it anyway?"
}

func nonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}
