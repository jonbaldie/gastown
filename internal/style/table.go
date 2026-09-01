// Package style provides consistent terminal styling using Lipgloss.
package style

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Column defines a table column with name and width.
type Column struct {
	Name  string
	Width int
	Align Alignment
	Style lipgloss.Style
}

// Alignment specifies column text alignment.
type Alignment int

const (
	AlignLeft Alignment = iota
	AlignRight
	AlignCenter
)

// Table provides styled table rendering.
type Table struct {
	columns     []Column
	rows        [][]string
	headerSep   bool
	indent      string
	headerStyle lipgloss.Style
}

// NewTable creates a new table with the given columns.
func NewTable(columns ...Column) *Table {
	return &Table{
		columns:     columns,
		headerSep:   true,
		indent:      "  ",
		headerStyle: Bold,
	}
}

// SetIndent sets the left indent for the table.
func (t *Table) SetIndent(indent string) *Table {
	t.indent = indent
	return t
}

// SetHeaderSeparator enables/disables the header separator line.
func (t *Table) SetHeaderSeparator(enabled bool) *Table {
	t.headerSep = enabled
	return t
}

// AddRow adds a row of values to the table.
func (t *Table) AddRow(values ...string) *Table {
	// Pad with empty strings if needed
	for missing := len(t.columns) - len(values); missing > 0; missing-- {
		values = append(values, "")
	}
	t.rows = append(t.rows, values)
	return t
}

// Render returns the formatted table string.
func (t *Table) Render() string {
	if len(t.columns) == 0 {
		return ""
	}

	var sb strings.Builder
	t.renderHeader(&sb)
	if t.headerSep {
		t.renderSeparator(&sb)
	}
	t.renderRows(&sb)
	return sb.String()
}

func (t *Table) renderHeader(sb *strings.Builder) {
	sb.WriteString(t.indent)
	for i, col := range t.columns {
		text := t.headerStyle.Render(col.Name)
		sb.WriteString(t.pad(text, col.Name, col.Width, col.Align))
		if i < len(t.columns)-1 {
			sb.WriteString(" ")
		}
	}
	sb.WriteString("\n")
}

func (t *Table) renderSeparator(sb *strings.Builder) {
	sb.WriteString(t.indent)
	totalWidth := 0
	for i, col := range t.columns {
		totalWidth += col.Width
		if i < len(t.columns)-1 {
			totalWidth++ // space between columns
		}
	}
	sb.WriteString(Dim.Render(strings.Repeat("─", totalWidth)))
	sb.WriteString("\n")
}

func (t *Table) renderRows(sb *strings.Builder) {
	for _, row := range t.rows {
		t.renderRow(sb, row)
	}
}

func (t *Table) renderRow(sb *strings.Builder, row []string) {
	sb.WriteString(t.indent)
	for i, col := range t.columns {
		val, plainVal := renderTableCell(row, i, col)
		sb.WriteString(t.pad(val, plainVal, col.Width, col.Align))
		if i < len(t.columns)-1 {
			sb.WriteString(" ")
		}
	}
	sb.WriteString("\n")
}

func renderTableCell(row []string, index int, col Column) (string, string) {
	val := ""
	if index < len(row) {
		val = row[index]
	}

	plainVal := stripAnsi(val)
	if len(plainVal) > col.Width {
		val = plainVal[:col.Width-3] + "..."
	}
	if col.Style.Value() != "" {
		val = col.Style.Render(val)
	}
	return val, plainVal
}

// pad pads text to width, accounting for ANSI escape sequences.
// styledText is the text with ANSI codes, plainText is without.
func (t *Table) pad(styledText, plainText string, width int, align Alignment) string {
	plainLen := len(plainText)
	if plainLen >= width {
		return styledText
	}

	padding := width - plainLen

	switch align {
	case AlignRight:
		return strings.Repeat(" ", padding) + styledText
	case AlignCenter:
		left := padding / 2
		right := padding - left
		return strings.Repeat(" ", left) + styledText + strings.Repeat(" ", right)
	default: // AlignLeft
		return styledText + strings.Repeat(" ", padding)
	}
}

// ansiRegex matches CSI escape sequences: ESC [ <params> <final byte>
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// stripAnsi removes ANSI escape sequences from a string.
func stripAnsi(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}
