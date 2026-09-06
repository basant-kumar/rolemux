package picker

import (
	"fmt"
	"strings"
)

const (
	roleBadgeStart = "\x1b[1;7m"
	styleReset     = "\x1b[0m"
)

type lineStyle uint8

const (
	linePlain lineStyle = iota
	lineRoleBadge
)

// renderLine is kept free of terminal escape sequences until after wrapping.
// That keeps physical-row accounting independent of the badge styling.
type renderLine struct {
	text      string
	style     lineStyle
	priority  int
	protected bool
}

const (
	prioritySeparator    = 0
	priorityDetail       = 10
	priorityOptionDetail = 20
	priorityOptionLabel  = 50
	priorityInstruction  = 70
	priorityControls     = 80
	priorityCore         = 100
)

// renderFrame is a pure frame builder. width and height are explicit so the
// layout can be exercised without a real terminal.
func renderFrame(view View, options []Option, query string, cursor, width, height, previousLines int) (string, int) {
	lines := renderView(view, options, query, cursor, width, height)
	return buildFrame(lines, previousLines, view.FullScreen), len(lines)
}

func renderStatusFrame(view View, message string, width, height int) string {
	return buildFrame(renderStatusLines(view, message, width, height), 0, true)
}

func buildFrame(lines []string, previousLines int, fullScreen bool) string {
	var frame strings.Builder
	frame.WriteString("\x1b[?2026h")
	if previousLines == 0 && fullScreen {
		frame.WriteString("\x1b[2J\x1b[H")
	} else if previousLines > 0 {
		frame.WriteByte('\r')
		if previousLines > 1 {
			_, _ = fmt.Fprintf(&frame, "\x1b[%dA", previousLines-1)
		}
	}
	for i, line := range lines {
		_, _ = fmt.Fprintf(&frame, "\r\x1b[2K%s", line)
		if i < len(lines)-1 {
			frame.WriteByte('\n')
		}
	}
	for i := len(lines); i < previousLines; i++ {
		frame.WriteString("\n\r\x1b[2K")
	}
	if previousLines > len(lines) {
		_, _ = fmt.Fprintf(&frame, "\x1b[%dA", previousLines-len(lines))
	}
	frame.WriteString("\x1b[?2026l")
	return frame.String()
}

func renderView(view View, options []Option, query string, cursor, width, height int) []string {
	cursor = clampCursor(cursor, len(options))
	if !hasContext(view) {
		return materializeLines(renderStandaloneLines(view, options, query, cursor, width))
	}

	lines := contextualLines(view, options, query, cursor, width, height)
	if view.FullScreen && height > 0 {
		lines = fitContextualLines(lines, width, height)
	}
	return materializeLines(lines)
}

func renderStatusLines(view View, message string, width, height int) []string {
	if hasContext(view) {
		lines := contextualHeaderLines(view, width, height)
		if message != "" {
			lines = appendWrappedLine(lines, message, width, linePlain, priorityCore, true)
		}
		if height > 0 {
			lines = fitContextualLines(lines, width, height)
		}
		return materializeLines(lines)
	}

	lines := standaloneHeaderLines(view, width)
	// Keep the compatibility status shape: title/header, a blank line, then
	// the status message. ShowStatus reaches this path.
	lines = appendWrappedLine(lines, message, width, linePlain, priorityCore, true)
	return materializeLines(lines)
}

func renderStandaloneLines(view View, options []Option, query string, cursor, width int) []renderLine {
	lines := standaloneHeaderLines(view, width)
	if view.Search {
		lines = appendWrappedLine(lines, "Search: "+query, width, linePlain, priorityControls, false)
	}
	lines = appendOptionLines(lines, options, cursor, width)
	if len(options) == 0 {
		lines = appendWrappedLine(lines, "  No matches", width, linePlain, priorityCore, true)
	}
	lines = append(lines, renderLine{text: "", priority: prioritySeparator})
	lines = appendWrappedLine(lines, footerText(view), width, linePlain, priorityControls, false)
	return lines
}

// standaloneHeaderLines intentionally mirrors the original standalone Pick
// layout. In particular, the new View fields do not affect it when empty.
func standaloneHeaderLines(view View, width int) []renderLine {
	lines := []renderLine{}
	if view.Title != "" {
		lines = appendWrappedLine(lines, view.Title, width, linePlain, priorityCore, true)
	}
	if view.Subtitle != "" {
		lines = appendWrappedLine(lines, view.Subtitle, width, linePlain, priorityInstruction, true)
	}
	if view.Title != "" || view.Subtitle != "" {
		lines = append(lines, renderLine{text: "", priority: prioritySeparator})
	}
	return lines
}

func contextualLines(view View, options []Option, query string, cursor, width, height int) []renderLine {
	lines := contextualHeaderLines(view, width, height)
	if view.Search {
		lines = appendWrappedLine(lines, "Search: "+query, width, linePlain, priorityControls, false)
	}
	lines = appendOptionLines(lines, options, cursor, width)
	if len(options) == 0 {
		lines = appendWrappedLine(lines, "  No matches", width, linePlain, priorityOptionLabel, true)
	}
	lines = append(lines, renderLine{text: "", priority: prioritySeparator})
	lines = appendWrappedLine(lines, footerText(view), width, linePlain, priorityControls, false)
	return lines
}

func contextualHeaderLines(view View, width, height int) []renderLine {
	lines := []renderLine{}
	if title := contextTitle(view); title != "" {
		lines = appendWrappedLine(lines, title, width, linePlain, priorityCore, true)
	}
	if role := strings.TrimSpace(view.ActiveRole); role != "" {
		badge := "Role: " + displayRoleName(role)
		lines = appendWrappedLine(lines, badge, width, lineRoleBadge, priorityCore, true)
	}
	lines = appendBoundedDetail(lines, view.Context, width, height)
	if view.Subtitle != "" {
		lines = appendWrappedLine(lines, view.Subtitle, width, linePlain, priorityInstruction, false)
	}
	lines = appendBoundedDetail(lines, view.Notice, width, height)
	if len(lines) > 0 {
		lines = append(lines, renderLine{text: "", priority: prioritySeparator})
	}
	return lines
}

func appendOptionLines(lines []renderLine, options []Option, cursor, width int) []renderLine {
	start, end := optionWindow(len(options), cursor)
	for i := start; i < end; i++ {
		marker := "  "
		priority := priorityOptionLabel
		protected := false
		if i == cursor {
			marker = "> "
			priority = priorityCore
			protected = true
		}
		label := options[i].Label
		if label == "" {
			label = options[i].ID
		}
		lines = appendWrappedLine(lines, marker+label, width, linePlain, priority, protected)
		if options[i].Description != "" {
			lines = appendWrappedDetail(lines, "    "+options[i].Description, width, priorityOptionDetail)
		}
		if options[i].Meta != "" {
			lines = appendWrappedDetail(lines, "    "+options[i].Meta, width, priorityOptionDetail)
		}
	}
	return lines
}

func appendWrappedDetail(lines []renderLine, text string, width, priority int) []renderLine {
	return appendWrappedLine(lines, text, width, linePlain, priority, false)
}

func appendBoundedDetail(lines []renderLine, text string, width, height int) []renderLine {
	if strings.TrimSpace(text) == "" {
		return lines
	}
	plain := wrapPlainText(text, width)
	truncated := false
	if width > 1 {
		for i := range plain {
			if len([]rune(plain[i])) > width-1 {
				plain[i] = ellipsize(plain[i], width)
				truncated = true
			}
		}
	}
	if height > 0 {
		limit := detailRowLimit(height)
		if len(plain) > limit {
			plain = plain[:limit]
			truncated = true
		}
	}
	if truncated && len(plain) > 0 {
		plain[len(plain)-1] = ellipsize(plain[len(plain)-1], width)
	}
	for _, line := range plain {
		lines = append(lines, renderLine{text: line, priority: priorityDetail})
	}
	return lines
}

func appendWrappedLine(lines []renderLine, text string, width int, style lineStyle, priority int, protected bool) []renderLine {
	for _, line := range wrapPlainText(text, width) {
		lines = append(lines, renderLine{text: line, style: style, priority: priority, protected: protected})
	}
	return lines
}

func wrapPlainText(text string, width int) []string {
	parts := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	return wrapLines(parts, width)
}

func materializeLines(lines []renderLine) []string {
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if line.style == lineRoleBadge {
			result = append(result, roleBadgeStart+line.text+styleReset)
			continue
		}
		result = append(result, line.text)
	}
	return result
}

func fitContextualLines(lines []renderLine, width, height int) []renderLine {
	if height <= 0 || len(lines) == 0 {
		return lines
	}
	result := append([]renderLine(nil), lines...)
	for physicalLineCount(result) > height {
		remove := -1
		removePriority := int(^uint(0) >> 1)
		for i := len(result) - 1; i >= 0; i-- {
			line := result[i]
			if line.protected || line.priority >= priorityCore {
				continue
			}
			if line.priority < removePriority {
				remove = i
				removePriority = line.priority
			}
		}
		if remove < 0 {
			break
		}
		result = append(result[:remove], result[remove+1:]...)
	}

	// A pathological width/height combination can leave only the protected
	// title, role, and selected label. Keep those rows intact and compact any
	// remaining protected control text only if it is still necessary.
	if physicalLineCount(result) > height {
		for i := range result {
			if result[i].protected && result[i].priority < priorityCore {
				result[i].text = ellipsize(result[i].text, width)
			}
		}
	}
	return result
}

func physicalLineCount(lines []renderLine) int { return len(lines) }

func optionWindow(optionCount, cursor int) (int, int) {
	if optionCount == 0 {
		return 0, 0
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= optionCount {
		cursor = optionCount - 1
	}
	const visible = 6
	start := 0
	if cursor >= visible {
		start = cursor - visible + 1
	}
	end := start + visible
	if end > optionCount {
		end = optionCount
	}
	return start, end
}

func clampCursor(cursor, optionCount int) int {
	if optionCount == 0 || cursor < 0 {
		return 0
	}
	if cursor >= optionCount {
		return optionCount - 1
	}
	return cursor
}

func footerText(view View) string {
	if view.CanBack {
		return "↑/↓ move  enter select  esc back  ctrl+c cancel"
	}
	return "↑/↓ move  enter select  esc/ctrl+c cancel"
}

func hasContext(view View) bool {
	return strings.TrimSpace(view.ActiveRole) != "" || strings.TrimSpace(view.Step) != "" || strings.TrimSpace(view.Context) != "" || strings.TrimSpace(view.Notice) != ""
}

func contextTitle(view View) string {
	title := strings.TrimSpace(view.Title)
	step := strings.TrimSpace(view.Step)
	switch {
	case title != "" && step != "":
		return title + " · " + step
	case title != "":
		return title
	default:
		return step
	}
}

func displayRoleName(role string) string {
	role = strings.TrimSpace(role)
	role = strings.NewReplacer("_", " ", "-", " ").Replace(role)
	if role == "" {
		return "Review roles"
	}
	if role == "all" {
		return "All roles"
	}
	runes := []rune(role)
	upper := strings.ToUpper(string(runes[0]))
	return upper + string(runes[1:])
}

func detailRowLimit(height int) int {
	limit := height / 4
	if limit < 1 {
		limit = 1
	}
	if limit > 4 {
		limit = 4
	}
	return limit
}

func ellipsize(text string, width int) string {
	if width <= 0 {
		return text
	}
	limit := width - 1
	if limit <= 0 {
		return "…"
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func wrapLines(lines []string, width int) []string {
	if width < 20 {
		return lines
	}
	// Avoid writing in the terminal's final column; terminals differ in when
	// they materialize an automatic wrap there.
	limit := width - 1
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		runes := []rune(line)
		indentLength := 0
		for indentLength < len(runes) && runes[indentLength] == ' ' {
			indentLength++
		}
		indent := string(runes[:indentLength])
		for len(runes) > limit {
			cut := limit
			for i := limit; i > indentLength+8; i-- {
				if runes[i] == ' ' {
					cut = i
					break
				}
			}
			result = append(result, strings.TrimRight(string(runes[:cut]), " "))
			remainder := strings.TrimLeft(string(runes[cut:]), " ")
			runes = []rune(indent + remainder)
		}
		result = append(result, string(runes))
	}
	return result
}
