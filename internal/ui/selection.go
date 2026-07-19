package ui

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
)

const copyToastDuration = 2 * time.Second

type selectionPoint struct {
	row int
	col int
}

type selectionState struct {
	active   bool
	dragging bool
	moved    bool
	anchor   selectionPoint
	focus    selectionPoint
	lines    []selectionLine
}

type selectionLine struct {
	text           string
	hardBreakAfter bool
}

type clipboardResultMsg struct {
	sequence uint64
	chars    int
	err      error
}

type copyToastExpiredMsg struct{ sequence uint64 }

func defaultClipboardWrite(value string) error { return clipboard.WriteAll(value) }

func (m *Model) handleMouse(message tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.screen != screenChat || m.approval != nil {
		return m, nil
	}
	event := message.Mouse()
	viewportTop := m.layout().viewportTop
	localY := event.Y - viewportTop
	inside := localY >= 0 && localY < m.viewport.Height()
	if _, wheel := message.(tea.MouseWheelMsg); wheel {
		if !inside {
			return m, nil
		}
		updated, command := m.viewport.Update(message)
		m.viewport = updated
		if m.selection.dragging {
			m.selection.focus = m.selectionPointAt(localY, event.X, false)
			m.selection.moved = true
		}
		m.followOutput = false
		return m, command
	}
	switch message.(type) {
	case tea.MouseClickMsg:
		if event.Button != tea.MouseLeft || !inside {
			m.clearSelection()
			return m, nil
		}
		lines := selectionLines(m.viewport.GetContent(), m.viewport.Width())
		point := clampSelectionPoint(lines, m.viewport.YOffset()+localY, event.X-m.viewportLeftInset())
		m.selection = selectionState{active: true, dragging: true, anchor: point, focus: point, lines: lines}
		m.followOutput = false
		return m, nil
	case tea.MouseMotionMsg:
		if !m.selection.dragging || event.Button != tea.MouseLeft {
			return m, nil
		}
		m.selection.focus = m.selectionPointAt(localY, event.X, true)
		m.selection.moved = true
		return m, nil
	case tea.MouseReleaseMsg:
		if !m.selection.dragging {
			return m, nil
		}
		m.selection.dragging = false
		m.selection.focus = m.selectionPointAt(localY, event.X, true)
		if !m.selection.moved && m.selection.anchor == m.selection.focus {
			m.selection = selectionState{}
			return m, nil
		}
		selected := selectedText(m.selection.lines, m.selection.anchor, m.selection.focus)
		if selected == "" {
			m.selection = selectionState{}
			return m, nil
		}
		m.copyToastSequence++
		sequence := m.copyToastSequence
		writer := m.clipboardWrite
		return m, func() tea.Msg {
			err := writer(selected)
			return clipboardResultMsg{sequence: sequence, chars: utf8.RuneCountInString(selected), err: err}
		}
	}
	return m, nil
}

func (m *Model) selectionPointAt(localY, column int, scroll bool) selectionPoint {
	height := m.viewport.Height()
	if height <= 0 {
		return selectionPoint{}
	}
	if scroll {
		switch {
		case localY < 0:
			m.viewport.ScrollUp(min(3, -localY))
		case localY >= height:
			m.viewport.ScrollDown(min(3, localY-height+1))
		}
	}
	visibleRow := min(max(0, localY), height-1)
	return clampSelectionPoint(m.selection.lines, m.viewport.YOffset()+visibleRow, column-m.viewportLeftInset())
}

func (m *Model) renderViewport() string {
	view := m.viewport.View()
	if !m.selection.active {
		return indentBlock(view, m.viewportLeftInset())
	}
	lines := strings.Split(view, "\n")
	start, end := normalizedSelection(m.selection.anchor, m.selection.focus)
	for row := range lines {
		globalRow := m.viewport.YOffset() + row
		if globalRow < start.row || globalRow > end.row {
			continue
		}
		left, right := selectionColumns(m.selection.lines, start, end, globalRow)
		if right <= left {
			continue
		}
		width := ansi.StringWidth(lines[row])
		left = min(max(0, left), width)
		right = min(max(left, right), width)
		prefix := ansi.Cut(lines[row], 0, left)
		middle := ansi.Strip(ansi.Cut(lines[row], left, right))
		suffix := ansi.Cut(lines[row], right, width)
		lines[row] = prefix + m.styles.Selection.Render(middle) + suffix
	}
	return indentBlock(strings.Join(lines, "\n"), m.viewportLeftInset())
}

func visibleSelectionLines(content string, width, height, offset int) []selectionLine {
	if width <= 0 || height <= 0 {
		return nil
	}
	wrapped := selectionLines(content, width)
	start := min(max(0, offset), len(wrapped))
	end := min(len(wrapped), start+height)
	visible := append([]selectionLine(nil), wrapped[start:end]...)
	for len(visible) < height {
		visible = append(visible, selectionLine{})
	}
	return visible
}

func selectionLines(content string, width int) []selectionLine {
	if width <= 0 {
		return nil
	}
	source := strings.Split(content, "\n")
	wrapped := make([]selectionLine, 0, len(source))
	for index, line := range source {
		lineWidth := ansi.StringWidth(line)
		if lineWidth == 0 {
			wrapped = append(wrapped, selectionLine{text: ansi.Strip(line), hardBreakAfter: index < len(source)-1})
			continue
		}
		for column := 0; column < lineWidth; column += width {
			last := column+width >= lineWidth
			wrapped = append(wrapped, selectionLine{
				text:           ansi.Strip(ansi.Cut(line, column, min(lineWidth, column+width))),
				hardBreakAfter: last && index < len(source)-1,
			})
		}
	}
	return wrapped
}

func clampSelectionPoint(lines []selectionLine, row, col int) selectionPoint {
	if len(lines) == 0 {
		return selectionPoint{}
	}
	row = min(max(0, row), len(lines)-1)
	width := ansi.StringWidth(lines[row].text)
	col = min(max(0, col), width)
	return selectionPoint{row: row, col: col}
}

func normalizedSelection(left, right selectionPoint) (selectionPoint, selectionPoint) {
	if left.row > right.row || (left.row == right.row && left.col > right.col) {
		return right, left
	}
	return left, right
}

func selectionColumns(lines []selectionLine, start, end selectionPoint, row int) (int, int) {
	if row < 0 || row >= len(lines) {
		return 0, 0
	}
	width := ansi.StringWidth(lines[row].text)
	if start.row == end.row {
		return selectionCellStart(lines[row].text, start.col), selectionCellEnd(lines[row].text, end.col)
	}
	if row == start.row {
		return selectionCellStart(lines[row].text, start.col), width
	}
	if row == end.row {
		return 0, selectionCellEnd(lines[row].text, end.col)
	}
	return 0, width
}

func selectionCellStart(value string, column int) int {
	start, _ := selectionCellBounds(value, column)
	return start
}

func selectionCellEnd(value string, column int) int {
	_, end := selectionCellBounds(value, column)
	return end
}

func selectionCellBounds(value string, column int) (int, int) {
	width := ansi.StringWidth(value)
	column = min(max(0, column), width)
	position := 0
	for _, character := range value {
		cellWidth := ansi.StringWidth(string(character))
		if cellWidth <= 0 {
			continue
		}
		if column < position+cellWidth {
			return position, position + cellWidth
		}
		position += cellWidth
	}
	return width, width
}

func selectedText(lines []selectionLine, anchor, focus selectionPoint) string {
	if len(lines) == 0 {
		return ""
	}
	start, end := normalizedSelection(anchor, focus)
	var selected strings.Builder
	for row := start.row; row <= end.row && row < len(lines); row++ {
		left, right := selectionColumns(lines, start, end, row)
		selected.WriteString(ansi.Cut(lines[row].text, left, right))
		if row < end.row && lines[row].hardBreakAfter {
			selected.WriteByte('\n')
		}
	}
	return strings.TrimRight(selected.String(), "\n")
}

func (m *Model) clearSelection() {
	m.selection = selectionState{}
}

func (m *Model) handleClipboardResult(message clipboardResultMsg) tea.Cmd {
	if message.sequence != m.copyToastSequence {
		return nil
	}
	if message.err != nil {
		m.copyToast = "Copy failed"
	} else {
		m.copyToast = pluralCount(message.chars, "char") + " copied"
	}
	sequence := message.sequence
	return m.clock.Tick(copyToastDuration, func(time.Time) tea.Msg {
		return copyToastExpiredMsg{sequence: sequence}
	})
}

func pluralCount(value int, singular string) string {
	label := singular
	if value != 1 {
		label += "s"
	}
	return strconv.Itoa(value) + " " + label
}
