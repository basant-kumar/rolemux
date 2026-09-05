package picker

import (
	"bytes"
	"context"
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

	// Search + three choices + footer is five lines. Since the cursor remains
	// on the fifth line, a redraw must move up four lines, never five.
	if got := bytes.Count(output.Bytes(), []byte("\x1b[4A")); got != 2 {
		t.Fatalf("fixed-region redraws=%d, output=%q", got, output.Bytes())
	}
	if bytes.Contains(output.Bytes(), []byte("\x1b[5A")) {
		t.Fatalf("redraw drifted above its region: %q", output.Bytes())
	}
	if got := bytes.Count(output.Bytes(), []byte("\x1b[?2026h")); got != 3 {
		t.Fatalf("synchronized frames=%d, output=%q", got, output.Bytes())
	}
}

func TestModelAndEffortOptionsExposeUnknowns(t *testing.T) {
	model := runner.ModelInfo{ID: "gpt-test", Availability: "unknown", Efforts: []string{"low", "max"}, DefaultEffort: "max"}
	if warning := UnknownAvailabilityWarning(model); warning == "" {
		t.Fatal("unknown model had no warning")
	}
	efforts := EffortOptions(model)
	if len(efforts) != 2 || efforts[1].ID != "max" {
		t.Fatalf("efforts=%#v", efforts)
	}
	unknown := EffortOptions(runner.ModelInfo{ID: "unknown"})
	if len(unknown) != 1 || unknown[0].ID != "" {
		t.Fatalf("unknown effort options=%#v", unknown)
	}
}
