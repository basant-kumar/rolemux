// Package picker provides a tiny searchable terminal picker. The stateful
// filtering/navigation logic is independent of terminal setup and is used by
// configure tests without a model call.
package picker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/basant-kumar/rolemux/internal/runner"
)

type Option struct{ ID, Label string }

type Action int

const (
	ActionSelected Action = iota
	ActionBack
	ActionCancel
)

// View controls one wizard screen. FullScreen clears the alternate buffer
// before the first frame; CanBack makes Escape return to the previous screen.
type View struct {
	Title      string
	Subtitle   string
	Search     bool
	CanBack    bool
	FullScreen bool
}

// Screen owns the alternate terminal buffer used by the configure wizard.
// Leaving it restores the caller's original terminal contents.
type Screen struct {
	out    io.Writer
	active bool
}

func NewScreen(out io.Writer) *Screen { return &Screen{out: out} }

func (s *Screen) Enter() {
	if s == nil || s.out == nil || s.active {
		return
	}
	_, _ = io.WriteString(s.out, "\x1b[?1049h\x1b[2J\x1b[H")
	s.active = true
}

func (s *Screen) Leave() {
	if s == nil || s.out == nil || !s.active {
		return
	}
	_, _ = io.WriteString(s.out, "\x1b[?2026l\x1b[?25h\x1b[?1049l")
	s.active = false
}

type State struct {
	Options []Option
	Query   string
	Cursor  int
}

func New(options []Option) *State { return &State{Options: append([]Option(nil), options...)} }

func (s *State) Filtered() []Option {
	query := strings.ToLower(strings.TrimSpace(s.Query))
	result := []Option{}
	for _, option := range s.Options {
		if query == "" || strings.Contains(strings.ToLower(option.ID), query) || strings.Contains(strings.ToLower(option.Label), query) {
			result = append(result, option)
		}
	}
	if len(result) == 0 {
		s.Cursor = 0
	} else if s.Cursor >= len(result) {
		s.Cursor = len(result) - 1
	}
	return result
}

func (s *State) Input(r rune) { s.Query += string(r); s.Cursor = 0 }
func (s *State) Backspace() {
	if s.Query != "" {
		_, size := utf8.DecodeLastRuneInString(s.Query)
		s.Query = s.Query[:len(s.Query)-size]
		s.Cursor = 0
	}
}
func (s *State) Move(delta int) {
	options := s.Filtered()
	if len(options) == 0 {
		return
	}
	s.Cursor = (s.Cursor + delta) % len(options)
	if s.Cursor < 0 {
		s.Cursor += len(options)
	}
}
func (s *State) Selected() (Option, bool) {
	options := s.Filtered()
	if len(options) == 0 {
		return Option{}, false
	}
	return options[s.Cursor], true
}

type Key int

const (
	KeyRune Key = iota
	KeyUp
	KeyDown
	KeyEnter
	KeyEscape
	KeyBackspace
)

func (s *State) Handle(key Key, r rune) (selected *Option, cancelled bool) {
	switch key {
	case KeyRune:
		s.Input(r)
	case KeyBackspace:
		s.Backspace()
	case KeyUp:
		s.Move(-1)
	case KeyDown:
		s.Move(1)
	case KeyEnter:
		if value, ok := s.Selected(); ok {
			return &value, false
		}
	case KeyEscape:
		return nil, true
	}
	return nil, false
}

func handleRune(state *State, r rune, searchable bool) *Option {
	if searchable {
		state.Handle(KeyRune, r)
		return nil
	}
	want := strings.ToLower(string(r))
	for _, option := range state.Options {
		first, _ := utf8.DecodeRuneInString(strings.TrimSpace(option.ID))
		if strings.ToLower(string(first)) == want {
			choice := option
			return &choice
		}
	}
	return nil
}

// Pick is the small standalone searchable picker API. Escape and Ctrl+C both
// report cancellation; multi-screen callers should use Select to distinguish
// back navigation from cancellation.
func Pick(ctx context.Context, in io.Reader, out io.Writer, options []Option) (Option, bool, error) {
	choice, action, err := Select(ctx, in, out, options, View{Search: true})
	return choice, action != ActionSelected, err
}

// Select reads one raw-key wizard screen without requiring a paid provider. A
// lone Escape is recognized after a short sequence window and therefore does
// not wait for a second byte forever on pipes or terminals.
func Select(ctx context.Context, in io.Reader, out io.Writer, options []Option, view View) (Option, Action, error) {
	restore, err := enterRawMode(in)
	if err != nil {
		return Option{}, ActionCancel, err
	}
	defer restore()
	state := New(options)
	renderedLines := 0
	nextByte := func(wait time.Duration) (byte, bool, error) {
		if wait > 0 {
			ready, readyErr := inputReady(in, wait)
			if readyErr != nil || !ready {
				return 0, false, readyErr
			}
		}
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		buffer := []byte{0}
		n, readErr := in.Read(buffer)
		if n == 1 {
			return buffer[0], true, nil
		}
		return 0, false, readErr
	}
	render := func() {
		if out == nil {
			return
		}
		lines := []string{}
		if view.Title != "" {
			lines = append(lines, view.Title)
		}
		if view.Subtitle != "" {
			lines = append(lines, view.Subtitle)
		}
		if view.Title != "" || view.Subtitle != "" {
			lines = append(lines, "")
		}
		if view.Search {
			lines = append(lines, "Search: "+state.Query)
		}
		filtered := state.Filtered()
		start := 0
		const visible = 10
		if state.Cursor >= visible {
			start = state.Cursor - visible + 1
		}
		end := start + visible
		if end > len(filtered) {
			end = len(filtered)
		}
		for i := start; i < end; i++ {
			marker := "  "
			if i == state.Cursor {
				marker = "> "
			}
			label := filtered[i].Label
			if label == "" || label == filtered[i].ID {
				lines = append(lines, marker+filtered[i].ID)
			} else {
				lines = append(lines, fmt.Sprintf("%s%s  %s", marker, filtered[i].ID, label))
			}
		}
		if len(filtered) == 0 {
			lines = append(lines, "  No matches")
		}
		footer := "↑/↓ move  enter select  esc/ctrl+c cancel"
		if view.CanBack {
			footer = "↑/↓ move  enter select  esc back  ctrl+c cancel"
		}
		lines = append(lines, "", footer)

		// Build and write one synchronized frame. The cursor is left on the
		// previous frame's final line, so reaching its first line requires
		// renderedLines-1 upward moves. Using renderedLines here makes every
		// keypress drift upward and leaves stale footer lines behind.
		var frame strings.Builder
		frame.WriteString("\x1b[?2026h")
		if renderedLines == 0 && view.FullScreen {
			frame.WriteString("\x1b[2J\x1b[H")
		} else if renderedLines > 0 {
			frame.WriteByte('\r')
			if renderedLines > 1 {
				_, _ = fmt.Fprintf(&frame, "\x1b[%dA", renderedLines-1)
			}
		}
		for i, line := range lines {
			_, _ = fmt.Fprintf(&frame, "\r\x1b[2K%s", line)
			if i < len(lines)-1 {
				frame.WriteByte('\n')
			}
		}
		for i := len(lines); i < renderedLines; i++ {
			frame.WriteString("\n\r\x1b[2K")
		}
		if renderedLines > len(lines) {
			_, _ = fmt.Fprintf(&frame, "\x1b[%dA", renderedLines-len(lines))
		}
		frame.WriteString("\x1b[?2026l")
		_, _ = io.WriteString(out, frame.String())
		renderedLines = len(lines)
	}
	if out != nil {
		_, _ = io.WriteString(out, "\x1b[?25l")
		defer func() { _, _ = io.WriteString(out, "\x1b[?25h") }()
	}
	render()
	for {
		b, ok, err := nextByte(0)
		if err != nil {
			if err == io.EOF {
				return Option{}, ActionCancel, nil
			}
			return Option{}, ActionCancel, err
		}
		if !ok {
			continue
		}
		if b == 0x03 {
			return Option{}, ActionCancel, nil
		}
		if b == 0x1b {
			second, present, sequenceErr := nextByte(25 * time.Millisecond)
			if sequenceErr != nil && sequenceErr != io.EOF {
				return Option{}, ActionCancel, sequenceErr
			}
			if !present {
				if view.CanBack {
					return Option{}, ActionBack, nil
				}
				return Option{}, ActionCancel, nil
			}
			if second != '[' {
				if view.CanBack {
					return Option{}, ActionBack, nil
				}
				return Option{}, ActionCancel, nil
			}
			third, present, sequenceErr := nextByte(25 * time.Millisecond)
			if sequenceErr != nil && sequenceErr != io.EOF {
				return Option{}, ActionCancel, sequenceErr
			}
			if !present {
				if view.CanBack {
					return Option{}, ActionBack, nil
				}
				return Option{}, ActionCancel, nil
			}
			switch third {
			case 'A', 'D':
				state.Handle(KeyUp, 0)
			case 'B', 'C':
				state.Handle(KeyDown, 0)
			}
			render()
			continue
		}
		var selected *Option
		switch b {
		case '\n', '\r':
			selected, _ = state.Handle(KeyEnter, 0)
		case 0x7f, 0x08:
			state.Handle(KeyBackspace, 0)
		default:
			if b >= utf8.RuneSelf {
				// Model IDs are normally ASCII; preserve a valid UTF-8 search
				// query when labels are not.
				sequence := []byte{b}
				for !utf8.FullRune(sequence) && len(sequence) < utf8.UTFMax {
					next, present, readErr := nextByte(25 * time.Millisecond)
					if readErr != nil || !present {
						break
					}
					sequence = append(sequence, next)
				}
				r, _ := utf8.DecodeRune(sequence)
				selected = handleRune(state, r, view.Search)
			} else if b >= 0x20 {
				selected = handleRune(state, rune(b), view.Search)
			}
		}
		if selected != nil {
			return *selected, ActionSelected, nil
		}
		render()
	}
}

func ModelOptions(models []runner.ModelInfo) []Option {
	result := make([]Option, 0, len(models))
	for _, model := range models {
		label := model.Label
		if label == "" {
			label = model.ID
		}
		result = append(result, Option{ID: model.ID, Label: label})
	}
	return result
}

func EffortOptions(model runner.ModelInfo) []Option {
	if len(model.Efforts) == 0 {
		return []Option{{ID: "", Label: "(optional effort; provider default/unknown)"}}
	}
	result := make([]Option, 0, len(model.Efforts))
	for _, effort := range model.Efforts {
		label := effort
		if effort == model.DefaultEffort {
			label += " (default)"
		}
		result = append(result, Option{ID: effort, Label: label})
	}
	return result
}

func UnknownAvailabilityWarning(model runner.ModelInfo) string {
	if model.Availability == "available" {
		return ""
	}
	return fmt.Sprintf("model %q has unknown availability; verify before selecting", model.ID)
}
