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

	"github.com/basant/rolemux/internal/runner"
)

type Option struct{ ID, Label string }

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

// Pick reads bytes without requiring a paid provider. A lone Escape is
// recognized after a short sequence window and therefore does not wait for a
// second byte forever on pipes or terminals.
func Pick(ctx context.Context, in io.Reader, out io.Writer, options []Option) (Option, bool, error) {
	restore, err := enterRawMode(in)
	if err != nil {
		return Option{}, false, err
	}
	defer restore()
	state := New(options)
	type readResult struct {
		data []byte
		err  error
	}
	reads := make(chan readResult, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buffer := make([]byte, 64)
		for {
			n, err := in.Read(buffer)
			result := readResult{data: append([]byte(nil), buffer[:n]...), err: err}
			select {
			case reads <- result:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var pending []byte
	var terminalErr error
	renderedLines := 0
	nextByte := func(wait time.Duration) (byte, bool, error) {
		if len(pending) > 0 {
			b := pending[0]
			pending = pending[1:]
			return b, true, nil
		}
		var timer <-chan time.Time
		if wait > 0 {
			t := time.NewTimer(wait)
			defer t.Stop()
			timer = t.C
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-timer:
			return 0, false, nil
		case result := <-reads:
			pending = append(pending, result.data...)
			terminalErr = result.err
			if len(pending) == 0 {
				return 0, false, terminalErr
			}
			b := pending[0]
			pending = pending[1:]
			return b, true, nil
		}
	}
	render := func() {
		if out == nil {
			return
		}
		if renderedLines > 0 {
			_, _ = fmt.Fprintf(out, "\r\x1b[%dA", renderedLines)
		}
		lines := []string{"Search: " + state.Query}
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
		lines = append(lines, "↑/↓ move  enter select  esc cancel")
		for i, line := range lines {
			_, _ = fmt.Fprintf(out, "\r\x1b[2K%s", line)
			if i < len(lines)-1 {
				_, _ = io.WriteString(out, "\n")
			}
		}
		for i := len(lines); i < renderedLines; i++ {
			_, _ = io.WriteString(out, "\n\r\x1b[2K")
		}
		if renderedLines > len(lines) {
			_, _ = fmt.Fprintf(out, "\x1b[%dA", renderedLines-len(lines))
		}
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
				return Option{}, true, nil
			}
			return Option{}, false, err
		}
		if !ok {
			if terminalErr == io.EOF {
				return Option{}, true, nil
			}
			continue
		}
		if b == 0x1b {
			second, present, sequenceErr := nextByte(25 * time.Millisecond)
			if sequenceErr != nil && sequenceErr != io.EOF {
				return Option{}, false, sequenceErr
			}
			if !present {
				return Option{}, true, nil
			}
			if second != '[' {
				return Option{}, true, nil
			}
			third, present, sequenceErr := nextByte(25 * time.Millisecond)
			if sequenceErr != nil && sequenceErr != io.EOF {
				return Option{}, false, sequenceErr
			}
			if !present {
				return Option{}, true, nil
			}
			switch third {
			case 'A':
				state.Handle(KeyUp, 0)
			case 'B':
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
				state.Handle(KeyRune, r)
			} else if b >= 0x20 {
				state.Handle(KeyRune, rune(b))
			}
		}
		if selected != nil {
			return *selected, false, nil
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
