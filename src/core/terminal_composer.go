package core

import (
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
)

// terminalComposer stores its cursor as a UTF-8 byte offset. Every public
// mutator keeps that offset on an extended grapheme-cluster boundary, so a
// combining sequence or emoji ZWJ sequence is always edited as one visible
// character.
type terminalComposer struct {
	text       string
	cursorByte int

	// preferredCell is the content-relative display column retained across a
	// sequence of visual up/down movements. Horizontal movement and edits reset
	// it, matching the usual text-editor goal-column behavior.
	preferredCell    int
	preferredCellSet bool
}

// terminalComposerLayout is the plain-text result of laying out a composer.
// Rows contain the prompt/continuation indentation but no ANSI styling and no
// gray-band leading space. CursorRow is relative to Rows. CursorColumn is
// one-based and already accounts for the gray band's implicit leading space.
// FirstRow and TotalRows let callers indicate that a tall composer is scrolled.
type terminalComposerLayout struct {
	Rows         []string
	CursorRow    int
	CursorColumn int
	FirstRow     int
	TotalRows    int
}

type terminalComposerGrapheme struct {
	text    string
	start   int
	end     int
	width   int
	newline bool
	word    bool
}

type terminalComposerBoundary struct {
	byteOffset int
	cell       int
}

type terminalComposerVisualRow struct {
	text       string
	width      int
	boundaries []terminalComposerBoundary
}

type terminalComposerVisualLayout struct {
	rows       []terminalComposerVisualRow
	cursorRow  int
	cursorCell int
}

const terminalComposerTabStop = 4

func newTerminalComposer(text string) terminalComposer {
	return newTerminalComposerAt(text, len(text))
}

func newTerminalComposerAt(text string, cursorByte int) terminalComposer {
	c := terminalComposer{text: text}
	c.cursorByte = terminalComposerBoundaryAtOrBefore(text, cursorByte)
	return c
}

func (c *terminalComposer) Text() string {
	if c == nil {
		return ""
	}
	return c.text
}

func (c *terminalComposer) CursorByte() int {
	if c == nil {
		return 0
	}
	return c.cursorByte
}

// Reset replaces the complete draft and positions the cursor at a UTF-8 byte
// offset. An offset inside a grapheme is moved to that grapheme's start.
func (c *terminalComposer) Reset(text string, cursorByte int) {
	if c == nil {
		return
	}
	c.text = text
	c.cursorByte = terminalComposerBoundaryAtOrBefore(text, cursorByte)
	c.clearPreferredCell()
}

func (c *terminalComposer) SetCursorByte(cursorByte int) bool {
	if c == nil {
		return false
	}
	target := terminalComposerBoundaryAtOrBefore(c.text, cursorByte)
	if target == c.cursorByte {
		return false
	}
	c.cursorByte = target
	c.clearPreferredCell()
	return true
}

// Insert adds text at the cursor. Callers may pass one decoded character, a
// bracketed-paste payload, or a multi-line string; the cursor lands after the
// inserted UTF-8 bytes.
func (c *terminalComposer) Insert(text string) bool {
	if c == nil || text == "" {
		return false
	}
	c.normalizeCursor()
	c.text = c.text[:c.cursorByte] + text + c.text[c.cursorByte:]
	c.cursorByte = terminalComposerBoundaryAtOrAfter(c.text, c.cursorByte+len(text))
	c.clearPreferredCell()
	return true
}

func (c *terminalComposer) MoveGraphemeLeft() bool {
	if c == nil {
		return false
	}
	c.normalizeCursor()
	if c.cursorByte == 0 {
		return false
	}
	target := 0
	for _, grapheme := range terminalComposerGraphemes(c.text) {
		if grapheme.end >= c.cursorByte {
			target = grapheme.start
			break
		}
		target = grapheme.end
	}
	c.cursorByte = target
	c.clearPreferredCell()
	return true
}

func (c *terminalComposer) MoveGraphemeRight() bool {
	if c == nil {
		return false
	}
	c.normalizeCursor()
	if c.cursorByte >= len(c.text) {
		return false
	}
	for _, grapheme := range terminalComposerGraphemes(c.text) {
		if grapheme.end > c.cursorByte {
			c.cursorByte = grapheme.end
			c.clearPreferredCell()
			return true
		}
	}
	return false
}

func (c *terminalComposer) BackspaceGrapheme() bool {
	if c == nil {
		return false
	}
	c.normalizeCursor()
	if c.cursorByte == 0 {
		return false
	}
	end := c.cursorByte
	start := end
	for _, grapheme := range terminalComposerGraphemes(c.text) {
		if grapheme.end >= end {
			start = grapheme.start
			break
		}
		start = grapheme.end
	}
	c.text = c.text[:start] + c.text[end:]
	c.cursorByte = terminalComposerBoundaryAtOrAfter(c.text, start)
	c.clearPreferredCell()
	return true
}

func (c *terminalComposer) DeleteGrapheme() bool {
	if c == nil {
		return false
	}
	c.normalizeCursor()
	if c.cursorByte >= len(c.text) {
		return false
	}
	end := c.cursorByte
	for _, grapheme := range terminalComposerGraphemes(c.text) {
		if grapheme.end > c.cursorByte {
			end = grapheme.end
			break
		}
	}
	c.text = c.text[:c.cursorByte] + c.text[end:]
	c.cursorByte = terminalComposerBoundaryAtOrAfter(c.text, c.cursorByte)
	c.clearPreferredCell()
	return true
}

// MoveLogicalHome moves to the start of the explicit logical line. Soft wraps
// introduced by Layout do not affect Home/End behavior.
func (c *terminalComposer) MoveLogicalHome() bool {
	if c == nil {
		return false
	}
	c.normalizeCursor()
	target := 0
	for _, grapheme := range terminalComposerGraphemes(c.text) {
		if grapheme.start >= c.cursorByte {
			break
		}
		if grapheme.newline && grapheme.end <= c.cursorByte {
			target = grapheme.end
		}
	}
	if target == c.cursorByte {
		return false
	}
	c.cursorByte = target
	c.clearPreferredCell()
	return true
}

// MoveLogicalEnd moves to just before the next explicit newline, or to the end
// of the draft when the current logical line is the last one.
func (c *terminalComposer) MoveLogicalEnd() bool {
	if c == nil {
		return false
	}
	c.normalizeCursor()
	target := len(c.text)
	for _, grapheme := range terminalComposerGraphemes(c.text) {
		if grapheme.newline && grapheme.start >= c.cursorByte {
			target = grapheme.start
			break
		}
	}
	if target == c.cursorByte {
		return false
	}
	c.cursorByte = target
	c.clearPreferredCell()
	return true
}

// MoveWordLeft uses Unicode grapheme boundaries and Unicode letter/number/mark
// classes. It first skips separators to the left and then moves to the start of
// the preceding word.
func (c *terminalComposer) MoveWordLeft() bool {
	if c == nil {
		return false
	}
	c.normalizeCursor()
	target := terminalComposerWordLeft(c.text, c.cursorByte)
	if target == c.cursorByte {
		return false
	}
	c.cursorByte = target
	c.clearPreferredCell()
	return true
}

// MoveWordRight moves to the start of the next Unicode word, or the end of the
// draft when no later word exists.
func (c *terminalComposer) MoveWordRight() bool {
	if c == nil {
		return false
	}
	c.normalizeCursor()
	target := terminalComposerWordRight(c.text, c.cursorByte)
	if target == c.cursorByte {
		return false
	}
	c.cursorByte = target
	c.clearPreferredCell()
	return true
}

func (c *terminalComposer) DeleteWordBackward() bool {
	if c == nil {
		return false
	}
	c.normalizeCursor()
	start := terminalComposerWordLeft(c.text, c.cursorByte)
	if start == c.cursorByte {
		return false
	}
	c.text = c.text[:start] + c.text[c.cursorByte:]
	c.cursorByte = terminalComposerBoundaryAtOrAfter(c.text, start)
	c.clearPreferredCell()
	return true
}

func (c *terminalComposer) MoveVisualUp(contentWidth int) bool {
	return c.moveVisual(-1, contentWidth)
}

func (c *terminalComposer) MoveVisualDown(contentWidth int) bool {
	return c.moveVisual(1, contentWidth)
}

// Layout hard-wraps the draft at contentWidth display cells. The prompt is
// present only on the first visual row; every later row receives an equal-width
// plain-space indent. contentWidth is the cell budget after that prompt/indent,
// not the terminal width. maxRows bounds only content rows (the band's top and
// bottom padding are owned by the caller); maxRows <= 0 keeps all rows.
func (c *terminalComposer) Layout(prompt string, contentWidth, maxRows int) terminalComposerLayout {
	if c == nil {
		c = &terminalComposer{}
	}
	c.normalizeCursor()
	visual := terminalComposerBuildVisualLayout(c.text, c.cursorByte, contentWidth)
	promptWidth := terminalDisplayWidth(prompt)
	indent := strings.Repeat(" ", promptWidth)

	firstRow := 0
	if maxRows > 0 && len(visual.rows) > maxRows && visual.cursorRow >= maxRows {
		firstRow = visual.cursorRow - maxRows + 1
	}
	lastRow := len(visual.rows)
	if maxRows > 0 && lastRow-firstRow > maxRows {
		lastRow = firstRow + maxRows
	}

	rows := make([]string, 0, lastRow-firstRow)
	for index := firstRow; index < lastRow; index++ {
		prefix := indent
		if index == 0 {
			prefix = prompt
		}
		rows = append(rows, prefix+visual.rows[index].text)
	}

	return terminalComposerLayout{
		Rows:         rows,
		CursorRow:    visual.cursorRow - firstRow,
		CursorColumn: promptWidth + visual.cursorCell + 2,
		FirstRow:     firstRow,
		TotalRows:    len(visual.rows),
	}
}

func (c *terminalComposer) moveVisual(delta, contentWidth int) bool {
	if c == nil || delta == 0 {
		return false
	}
	c.normalizeCursor()
	visual := terminalComposerBuildVisualLayout(c.text, c.cursorByte, contentWidth)
	targetRow := visual.cursorRow + delta
	if targetRow < 0 || targetRow >= len(visual.rows) {
		return false
	}
	if !c.preferredCellSet {
		c.preferredCell = visual.cursorCell
		c.preferredCellSet = true
	}
	target := terminalComposerClosestBoundary(visual.rows[targetRow].boundaries, c.preferredCell)
	if target == c.cursorByte {
		return false
	}
	c.cursorByte = target
	return true
}

func (c *terminalComposer) normalizeCursor() {
	if c == nil {
		return
	}
	c.cursorByte = terminalComposerBoundaryAtOrBefore(c.text, c.cursorByte)
}

func (c *terminalComposer) clearPreferredCell() {
	if c == nil {
		return
	}
	c.preferredCell = 0
	c.preferredCellSet = false
}

// terminalDisplayWidth returns the number of monospace terminal cells occupied
// by plain text. Callers must remove ANSI control sequences before using it.
func terminalDisplayWidth(text string) int {
	return uniseg.StringWidth(text)
}

// terminalFitCells returns the longest grapheme-safe prefix of plain text that
// fits within width display cells. It does not add an ellipsis or ANSI styling.
func terminalFitCells(text string, width int) string {
	if text == "" || width <= 0 {
		return ""
	}
	graphemes := uniseg.NewGraphemes(text)
	used := 0
	end := 0
	for graphemes.Next() {
		cluster := graphemes.Str()
		if terminalComposerIsNewline(cluster) {
			break
		}
		clusterWidth := graphemes.Width()
		if cluster == "\t" {
			clusterWidth = terminalComposerTabAdvance(used)
		}
		if clusterWidth < 0 || used+clusterWidth > width {
			break
		}
		_, end = graphemes.Positions()
		used += clusterWidth
	}
	return text[:end]
}

func terminalTailCells(text string, width int) string {
	if text == "" || width <= 0 {
		return ""
	}
	type cluster struct {
		start int
		width int
	}
	clusters := make([]cluster, 0, uniseg.GraphemeClusterCount(text))
	graphemes := uniseg.NewGraphemes(text)
	cell := 0
	for graphemes.Next() {
		start, _ := graphemes.Positions()
		clusterWidth := graphemes.Width()
		if clusterWidth < 0 {
			clusterWidth = 0
		}
		if graphemes.Str() == "\t" {
			clusterWidth = terminalComposerTabAdvance(cell)
		}
		clusters = append(clusters, cluster{start: start, width: clusterWidth})
		cell += clusterWidth
	}
	used := 0
	start := len(text)
	for index := len(clusters) - 1; index >= 0; index-- {
		if used+clusters[index].width > width {
			break
		}
		used += clusters[index].width
		start = clusters[index].start
	}
	return text[start:]
}

func terminalComposerBoundaryAtOrBefore(text string, byteOffset int) int {
	if byteOffset <= 0 || text == "" {
		return 0
	}
	if byteOffset >= len(text) {
		return len(text)
	}
	boundary := 0
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		start, end := graphemes.Positions()
		if byteOffset < end {
			return start
		}
		boundary = end
	}
	return boundary
}

func terminalComposerBoundaryAtOrAfter(text string, byteOffset int) int {
	if byteOffset <= 0 || text == "" {
		return 0
	}
	if byteOffset >= len(text) {
		return len(text)
	}
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		start, end := graphemes.Positions()
		if byteOffset <= start {
			return start
		}
		if byteOffset <= end {
			return end
		}
	}
	return len(text)
}

func terminalComposerGraphemes(text string) []terminalComposerGrapheme {
	if text == "" {
		return nil
	}
	clusters := make([]terminalComposerGrapheme, 0, uniseg.GraphemeClusterCount(text))
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		start, end := graphemes.Positions()
		cluster := graphemes.Str()
		width := graphemes.Width()
		if width < 0 {
			width = 0
		}
		clusters = append(clusters, terminalComposerGrapheme{
			text:    cluster,
			start:   start,
			end:     end,
			width:   width,
			newline: terminalComposerIsNewline(cluster),
			word:    terminalComposerIsWord(cluster),
		})
	}
	return clusters
}

func terminalComposerIsNewline(cluster string) bool {
	return cluster == "\n" || cluster == "\r" || cluster == "\r\n"
}

func terminalComposerIsWord(cluster string) bool {
	for _, r := range cluster {
		if r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
			return true
		}
	}
	return false
}

func terminalComposerWordLeft(text string, cursorByte int) int {
	if cursorByte <= 0 {
		return 0
	}
	graphemes := terminalComposerGraphemes(text)
	index := len(graphemes) - 1
	for index >= 0 && graphemes[index].end > cursorByte {
		index--
	}
	for index >= 0 && !graphemes[index].word {
		index--
	}
	for index >= 0 && graphemes[index].word {
		index--
	}
	if index+1 < len(graphemes) {
		return graphemes[index+1].start
	}
	return 0
}

func terminalComposerWordRight(text string, cursorByte int) int {
	if cursorByte >= len(text) {
		return len(text)
	}
	graphemes := terminalComposerGraphemes(text)
	index := 0
	for index < len(graphemes) && graphemes[index].end <= cursorByte {
		index++
	}
	if index < len(graphemes) && graphemes[index].word {
		for index < len(graphemes) && graphemes[index].word {
			index++
		}
	}
	for index < len(graphemes) && !graphemes[index].word {
		index++
	}
	if index < len(graphemes) {
		return graphemes[index].start
	}
	return len(text)
}

func terminalComposerBuildVisualLayout(text string, cursorByte, contentWidth int) terminalComposerVisualLayout {
	if contentWidth <= 0 {
		contentWidth = 1
	}
	cursorByte = terminalComposerBoundaryAtOrBefore(text, cursorByte)
	rows := make([]terminalComposerVisualRow, 0, 1)
	current := terminalComposerVisualRow{
		boundaries: []terminalComposerBoundary{{byteOffset: 0, cell: 0}},
	}
	cursorRow, cursorCell := 0, 0

	finishRow := func() {
		rows = append(rows, current)
	}
	startRow := func(byteOffset int) {
		current = terminalComposerVisualRow{
			boundaries: []terminalComposerBoundary{{byteOffset: byteOffset, cell: 0}},
		}
	}

	for _, grapheme := range terminalComposerGraphemes(text) {
		if grapheme.newline {
			if cursorByte == grapheme.start {
				cursorRow = len(rows)
				cursorCell = current.width
			}
			finishRow()
			startRow(grapheme.end)
			if cursorByte == grapheme.end {
				cursorRow = len(rows)
				cursorCell = 0
			}
			continue
		}

		clusterText := grapheme.text
		clusterWidth := grapheme.width
		if clusterText == "\t" {
			clusterWidth = terminalComposerTabAdvance(current.width)
			clusterText = strings.Repeat(" ", clusterWidth)
		}
		if clusterWidth > 0 && current.width > 0 && current.width+clusterWidth > contentWidth {
			finishRow()
			startRow(grapheme.start)
		}
		// A soft-wrap boundary belongs to the following row. Assigning the
		// cursor here intentionally overrides the same byte boundary recorded
		// at the end of the previous row.
		if cursorByte == grapheme.start {
			cursorRow = len(rows)
			cursorCell = current.width
		}
		current.text += clusterText
		current.width += clusterWidth
		current.boundaries = append(current.boundaries, terminalComposerBoundary{
			byteOffset: grapheme.end,
			cell:       current.width,
		})
		if cursorByte == grapheme.end {
			cursorRow = len(rows)
			cursorCell = current.width
		}
	}
	finishRow()

	return terminalComposerVisualLayout{
		rows:       rows,
		cursorRow:  cursorRow,
		cursorCell: cursorCell,
	}
}

func terminalComposerTabAdvance(cell int) int {
	advance := terminalComposerTabStop - cell%terminalComposerTabStop
	if advance <= 0 {
		return terminalComposerTabStop
	}
	return advance
}

func terminalComposerClosestBoundary(boundaries []terminalComposerBoundary, goalCell int) int {
	if len(boundaries) == 0 {
		return 0
	}
	if goalCell <= boundaries[0].cell {
		return boundaries[0].byteOffset
	}
	best := boundaries[0]
	bestDistance := absInt(best.cell - goalCell)
	for _, boundary := range boundaries[1:] {
		distance := absInt(boundary.cell - goalCell)
		if distance < bestDistance {
			best = boundary
			bestDistance = distance
		}
	}
	return best.byteOffset
}
