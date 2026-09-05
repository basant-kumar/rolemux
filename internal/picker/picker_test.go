package picker

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/basant-kumar/rolemux/internal/runner"
)

func TestFilteringAndArrowNavigation(t *testing.T) {
	state := New([]Option{{ID: "gpt-5.6-sol", Label: "Sol"}, {ID: "gpt-5.6-luna", Label: "Luna"}, {ID: "claude-fable-5", Label: "Fable"}})
	for _, r := range "luna" {
		state.Handle(KeyRune, r)
	}
	filtered := state.Filtered()
	if len(filtered) != 1 || filtered[0].ID != "gpt-5.6-luna" {
		t.Fatalf("filtered=%#v", filtered)
	}
	state = New([]Option{{ID: "one"}, {ID: "two"}})
	state.Handle(KeyUp, 0)
	if selected, _ := state.Selected(); selected.ID != "two" {
		t.Fatalf("up did not wrap: %#v", selected)
	}
}

func TestPickHandlesArrowAndLoneEscapeWithoutBlocking(t *testing.T) {
	options := []Option{{ID: "one"}, {ID: "two"}}
	choice, cancelled, err := Pick(context.Background(), bytes.NewBufferString("\x1b[B\n"), &bytes.Buffer{}, options)
	if err != nil || cancelled || choice.ID != "two" {
		t.Fatalf("choice=%#v cancelled=%v err=%v", choice, cancelled, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, cancelled, err = Pick(ctx, bytes.NewBuffer([]byte{0x1b}), &bytes.Buffer{}, options)
	if err != nil || !cancelled {
		t.Fatalf("lone escape cancelled=%v err=%v", cancelled, err)
	}
}

func TestPickRedrawKeepsOneFixedTerminalRegion(t *testing.T) {
	options := []Option{{ID: "one"}, {ID: "two"}, {ID: "three"}}
	var output bytes.Buffer
	choice, cancelled, err := Pick(context.Background(), bytes.NewBufferString("\x1b[B\x1b[B\n"), &output, options)
	if err != nil || cancelled || choice.ID != "three" {
		t.Fatalf("choice=%#v cancelled=%v err=%v", choice, cancelled, err)
	}

	// Search + three choices + spacer + footer is six lines. Since the cursor
	// remains on the sixth line, a redraw must move up five lines, never six.
	if got := bytes.Count(output.Bytes(), []byte("\x1b[5A")); got != 2 {
		t.Fatalf("fixed-region redraws=%d, output=%q", got, output.Bytes())
	}
	if bytes.Contains(output.Bytes(), []byte("\x1b[6A")) {
		t.Fatalf("redraw drifted above its region: %q", output.Bytes())
	}
	if got := bytes.Count(output.Bytes(), []byte("\x1b[?2026h")); got != 3 {
		t.Fatalf("synchronized frames=%d, output=%q", got, output.Bytes())
	}
}

func TestSelectDistinguishesBackCancelAndConfirmationShortcut(t *testing.T) {
	options := []Option{{ID: "no", Label: "No"}, {ID: "yes", Label: "Yes"}}
	_, action, err := Select(context.Background(), bytes.NewBuffer([]byte{0x1b}), &bytes.Buffer{}, options, View{CanBack: true})
	if err != nil || action != ActionBack {
		t.Fatalf("escape action=%v err=%v", action, err)
	}
	_, action, err = Select(context.Background(), bytes.NewBuffer([]byte{0x03}), &bytes.Buffer{}, options, View{CanBack: true})
	if err != nil || action != ActionCancel {
		t.Fatalf("ctrl-c action=%v err=%v", action, err)
	}
	choice, action, err := Select(context.Background(), bytes.NewBufferString("y"), &bytes.Buffer{}, options, View{})
	if err != nil || action != ActionSelected || choice.ID != "yes" {
		t.Fatalf("shortcut choice=%#v action=%v err=%v", choice, action, err)
	}
}

func TestSelectStartsOnConfiguredOption(t *testing.T) {
	options := []Option{{ID: "one"}, {ID: "two"}}
	choice, action, err := Select(context.Background(), bytes.NewBufferString("\r"), &bytes.Buffer{}, options, View{InitialID: "two"})
	if err != nil || action != ActionSelected || choice.ID != "two" {
		t.Fatalf("choice=%#v action=%v err=%v", choice, action, err)
	}
}

func TestWrapLinesAccountsForTerminalRows(t *testing.T) {
	wrapped := wrapLines([]string{"    a provider description that is deliberately long"}, 24)
	if len(wrapped) < 2 {
		t.Fatalf("description did not wrap: %#v", wrapped)
	}
	for _, line := range wrapped {
		if len([]rune(line)) >= 24 || !strings.HasPrefix(line, "    ") {
			t.Fatalf("bad wrapped line %q", line)
		}
	}
}

func TestAlternateScreenEnterLeaveIsIdempotent(t *testing.T) {
	var output bytes.Buffer
	screen := NewScreen(&output)
	screen.Enter()
	screen.Enter()
	screen.Leave()
	screen.Leave()
	if got := bytes.Count(output.Bytes(), []byte("\x1b[?1049h")); got != 1 {
		t.Fatalf("entries=%d output=%q", got, output.Bytes())
	}
	if got := bytes.Count(output.Bytes(), []byte("\x1b[?1049l")); got != 1 {
		t.Fatalf("leaves=%d output=%q", got, output.Bytes())
	}
}

func TestModelAndEffortOptionsExposeUnknowns(t *testing.T) {
	model := runner.ModelInfo{ID: "gpt-test", Label: "GPT Test", Description: "Live details", Availability: "unknown", ContextWindowTokens: 272000, MaxContextWindowTokens: 872000, Efforts: []string{"low", "max"}, EffortOptions: []runner.ModelOption{{ID: "low", Description: "Quick"}, {ID: "max", Description: "Deep"}}, DefaultEffort: "max", SpeedOptions: []runner.ModelOption{{ID: "priority", Label: "Fast", Description: "2x speed"}}}
	if warning := UnknownAvailabilityWarning(model); warning == "" {
		t.Fatal("unknown model had no warning")
	}
	efforts := EffortOptions(model)
	if len(efforts) != 2 || efforts[1].ID != "max" {
		t.Fatalf("efforts=%#v", efforts)
	}
	models := ModelOptions([]runner.ModelInfo{model})
	if !strings.Contains(models[0].Meta, "272K context") || !strings.Contains(models[0].Meta, "872K max") || models[0].Description != "Live details" {
		t.Fatalf("model options=%#v", models)
	}
	speeds := SpeedOptions(model)
	if len(speeds) != 2 || speeds[0].ID != "standard" || speeds[1].ID != "priority" {
		t.Fatalf("speed options=%#v", speeds)
	}
	unknown := EffortOptions(runner.ModelInfo{ID: "unknown"})
	if len(unknown) != 1 || unknown[0].ID != "" {
		t.Fatalf("unknown effort options=%#v", unknown)
	}
}
