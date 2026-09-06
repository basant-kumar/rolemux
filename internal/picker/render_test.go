package picker

import (
	"strings"
	"testing"
)

func TestRenderContextualHeaderKeepsRoleBadgeSeparate(t *testing.T) {
	view := View{
		Title:      "Configure RoleMux",
		Step:       "Step 2 of 5",
		ActiveRole: "code-reviewer",
		Context:    "Project configuration",
		Subtitle:   "Choose a model",
		Notice:     "The model has not been verified",
		Search:     true,
	}
	lines := renderView(view, []Option{{ID: "model", Label: "Model"}}, "claude", 0, 80, 24)
	if len(lines) < 7 {
		t.Fatalf("contextual lines=%#v", lines)
	}
	if lines[0] != "Configure RoleMux · Step 2 of 5" {
		t.Fatalf("title/step line=%q", lines[0])
	}
	if lines[1] != roleBadgeStart+"Role: Code reviewer"+styleReset {
		t.Fatalf("role badge=%q", lines[1])
	}
	if !strings.Contains(strings.Join(lines, "\n"), "Project configuration") || !strings.Contains(strings.Join(lines, "\n"), "The model has not been verified") {
		t.Fatalf("contextual details=%#v", lines)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "Search: claude") {
		t.Fatalf("search query missing=%#v", lines)
	}
}

func TestRenderContextualHeightPreservesSelectionAndHeaderCore(t *testing.T) {
	options := []Option{
		{ID: "one", Label: "One", Description: "A long detail that can be elided"},
		{ID: "two", Label: "Two", Description: "Another long detail that can be elided"},
		{ID: "three", Label: "Three", Description: "More detail"},
		{ID: "four", Label: "Four", Description: "More detail"},
		{ID: "five", Label: "Five", Description: "More detail"},
		{ID: "six", Label: "Six", Description: "More detail"},
	}
	view := View{
		Title:      "Configure RoleMux",
		Step:       "Step 4 of 5",
		ActiveRole: "implementer",
		Context:    strings.Repeat("long project context ", 20),
		Subtitle:   "Choose a model",
		Notice:     strings.Repeat("long warning ", 20),
		Search:     true,
		FullScreen: true,
	}
	lines := renderView(view, options, "", 4, 32, 8)
	if len(lines) > 8 {
		t.Fatalf("height overflow=%d lines=%#v", len(lines), lines)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Configure RoleMux · Step 4 of 5", roleBadgeStart + "Role: Implementer" + styleReset, "> Five"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("required %q missing from %#v", want, lines)
		}
	}
}

func TestRenderStyledRowsWrapWithIndependentResets(t *testing.T) {
	view := View{Title: "Configure", ActiveRole: "a-very-long-role-name", FullScreen: true}
	lines := renderView(view, nil, "", 0, 24, 20)
	styled := []string{}
	for _, line := range lines {
		if strings.Contains(line, roleBadgeStart) {
			styled = append(styled, line)
		}
	}
	if len(styled) < 2 {
		t.Fatalf("badge did not wrap: %#v", lines)
	}
	for _, line := range styled {
		if !strings.HasPrefix(line, roleBadgeStart) || !strings.HasSuffix(line, styleReset) {
			t.Fatalf("styled physical row=%q", line)
		}
	}
}

func TestRenderStatusReusesContextualHeader(t *testing.T) {
	view := View{Title: "Configure RoleMux", Step: "Step 3 of 5", ActiveRole: "planner", Subtitle: "Discovering models"}
	frame := renderStatusFrame(view, "Checking provider sign-in…", 80, 24)
	if !strings.Contains(frame, "Configure RoleMux · Step 3 of 5") || !strings.Contains(frame, roleBadgeStart+"Role: Planner"+styleReset) || !strings.Contains(frame, "Checking provider sign-in…") {
		t.Fatalf("status frame=%q", frame)
	}
	if strings.Contains(frame, "enter select") {
		t.Fatalf("status frame unexpectedly included picker footer=%q", frame)
	}
}
