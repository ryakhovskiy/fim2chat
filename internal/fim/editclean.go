package fim

import "strings"

// Clean removes model wrappers and normalises a rewrite's final newlines.
func (e Edit) Clean(text string, stops []string) (reply string, usable bool) {
	text = stripFences(stripThinking(text))
	markers := []string{seedCursor, zetaCursor}
	if !e.literalCursor && !strings.Contains(e.Region, cursorMarker) {
		markers = append(markers, cursorMarker)
	}
	// Removing one marker can join two fragments into another marker.
	for {
		previous := text
		for _, marker := range markers {
			text = strings.ReplaceAll(text, marker, "")
		}
		if text == previous {
			break
		}
	}

	var builtInStops []string
	switch e.Format {
	case EditZeta:
		if at := strings.Index(text, zetaStart); at >= 0 {
			text = strings.TrimPrefix(text[at+len(zetaStart):], "\n")
		}
		if at := strings.Index(text, zetaEnd); at >= 0 {
			text = strings.TrimSuffix(text[:at], "\n")
		}
		builtInStops = []string{zetaEnd}
	case EditZeta2:
		text = strings.TrimPrefix(text, seedCurrent)
		if at := strings.Index(text, seedUpdated); at >= 0 {
			text = text[:at]
		}
	case EditSweep:
		builtInStops = []string{sweepSeparator, "</s>"}
	}
	text = applyStops(applyStops(text, stops), builtInStops)
	if strings.TrimSpace(text) == "" {
		return e.NoChange(), false
	}
	// Copy the original line endings verbatim, including CRLF when present.
	ending := e.Region[len(strings.TrimRight(e.Region, "\r\n")):]
	return strings.TrimRight(text, "\r\n") + ending, true
}

// NoChange returns the format's safe response when no edit is available.
func (e Edit) NoChange() string {
	if e.Format == EditZeta {
		return e.Region
	}
	return ""
}
