package fim

import (
	"errors"
	"strings"
)

// EditFormat identifies a supported Zed rewrite prompt layout.
type EditFormat string

const (
	// EditZeta rewrites the editable region in Zed's original Zeta layout.
	EditZeta EditFormat = "zeta"
	// EditZeta2 rewrites the current region in Zed's Seed-Coder layout.
	EditZeta2 EditFormat = "zeta2"
	// EditSweep rewrites the full current window in Zed's Sweep layout.
	EditSweep EditFormat = "sweep"
)

// ErrUnsupportedEdit marks a recognised edit layout that cannot be served.
var ErrUnsupportedEdit = errors.New("unsupported edit prompt format")

// Edit describes the code region or window a rewrite replaces.
type Edit struct {
	Format EditFormat
	Region string // Original replacement text with the Zed cursor marker removed.

	path          string
	context       string
	before        string
	after         string
	original      string
	cursor        int
	literalCursor bool
}

// ParseEdit recognises and parses Zed edit prompts before FIM parsing.
func ParseEdit(prompt string) (e Edit, ok bool, err error) {
	switch {
	case strings.HasPrefix(prompt, zetaInstruction) && strings.HasSuffix(prompt, zetaResponse):
		e, err = parseZeta(prompt)
	case strings.HasPrefix(prompt, seedSuffix) && strings.HasSuffix(prompt, seedMiddle):
		e, err = parseZeta2(prompt)
	case strings.HasPrefix(prompt, sweepSeparator) && strings.HasSuffix(prompt, "\n"):
		e, ok = parseSweep(prompt)
		if !ok {
			return Edit{}, false, nil
		}
	default:
		return Edit{}, false, nil
	}
	if err != nil {
		return Edit{}, false, err
	}
	e.literalCursor = strings.Contains(prompt, cursorMarker)
	return e, true, nil
}

const (
	zetaInstruction = "### Instruction:\nYou are a code completion assistant"
	zetaResponse    = "\n\n### Response:\n"
	zetaExcerpt     = "\n\n### User Excerpt:\n\n"
	zetaEdits       = "\n\n### User Edits:\n\n"
	zetaStart       = "<|editable_region_start|>"
	zetaEnd         = "<|editable_region_end|>"
	zetaCursor      = "<|user_cursor_is_here|>"
	seedSuffix      = "<[fim-suffix]>"
	seedPrefix      = "<[fim-prefix]>"
	seedMiddle      = "<[fim-middle]>"
	seedCurrent     = "<<<<<<< CURRENT\n"
	seedDivider     = "=======\n"
	seedUpdated     = ">>>>>>> UPDATED"
	seedFilename    = "<filename>"
	seedCursor      = "<|user_cursor|>"
	sweepSeparator  = "<|file_sep|>"
)

func parseZeta(prompt string) (Edit, error) {
	startAt := strings.Index(prompt, zetaStart+"\n")
	if startAt < 0 {
		return Edit{}, ErrUnsupportedEdit
	}
	// A heading quoted in the region or following code is not a layout boundary.
	excerptAt := strings.LastIndex(prompt[:startAt], zetaExcerpt)
	if excerptAt < 0 {
		return Edit{}, ErrUnsupportedEdit
	}
	excerpt := prompt[excerptAt+len(zetaExcerpt) : len(prompt)-len(zetaResponse)]
	if !strings.HasPrefix(excerpt, "```") || !strings.HasSuffix(excerpt, "\n```") {
		return Edit{}, ErrUnsupportedEdit
	}
	path, code, ok := strings.Cut(excerpt[3:len(excerpt)-len("\n```")], "\n")
	if !ok {
		return Edit{}, ErrUnsupportedEdit
	}
	codeAt := excerptAt + len(zetaExcerpt) + 3 + len(path) + 1
	start := startAt - codeAt
	if start < 0 || start+len(zetaStart)+1 > len(code) {
		return Edit{}, ErrUnsupportedEdit
	}
	regionAt := start + len(zetaStart) + 1
	end := strings.Index(code[regionAt:], "\n"+zetaEnd)
	if end < 0 {
		return Edit{}, ErrUnsupportedEdit
	}
	end += regionAt
	e := Edit{
		Format: EditZeta, path: path,
		before: strings.TrimPrefix(code[:start], "<|start_of_file|>\n"),
		after:  code[end+1+len(zetaEnd):],
	}
	if editsAt := strings.Index(prompt[:excerptAt], zetaEdits); editsAt >= 0 {
		e.context = prompt[editsAt+len(zetaEdits) : excerptAt]
	}
	e.setRegion(code[regionAt:end], zetaCursor)
	return e, nil
}

func parseZeta2(prompt string) (Edit, error) {
	if !strings.HasSuffix(prompt, "\n"+seedDivider+seedMiddle) {
		return Edit{}, ErrUnsupportedEdit
	}
	end := len(prompt) - len(seedDivider) - len(seedMiddle)
	currentAt := strings.LastIndex(prompt[:end], seedCurrent)
	if currentAt < 0 {
		return Edit{}, ErrUnsupportedEdit
	}
	prefixAt := strings.Index(prompt[:currentAt], seedPrefix)
	if prefixAt < len(seedSuffix) {
		return Edit{}, ErrUnsupportedEdit
	}
	blockAt := strings.LastIndex(prompt[:currentAt], seedFilename)
	if blockAt < prefixAt+len(seedPrefix) {
		return Edit{}, ErrUnsupportedEdit
	}
	path, before, ok := strings.Cut(prompt[blockAt+len(seedFilename):currentAt], "\n")
	if !ok {
		return Edit{}, ErrUnsupportedEdit
	}
	e := Edit{
		Format: EditZeta2, path: path,
		context: contextTokenCleaner.Replace(prompt[prefixAt+len(seedPrefix) : blockAt]),
		before:  before, after: prompt[len(seedSuffix):prefixAt],
	}
	e.setRegion(prompt[currentAt+len(seedCurrent):end], seedCursor)
	return e, nil
}

func parseSweep(prompt string) (Edit, bool) {
	blocks := strings.Split(prompt, sweepSeparator)
	if len(blocks) < 4 {
		return Edit{}, false
	}
	n := len(blocks)
	originalHeader, original, okOriginal := strings.Cut(blocks[n-3], "\n")
	currentHeader, current, okCurrent := strings.Cut(blocks[n-2], "\n")
	updatedHeader, tail, okUpdated := strings.Cut(blocks[n-1], "\n")
	path, ok := strings.CutPrefix(originalHeader, "original/")
	if !ok || !okOriginal || !okCurrent || !okUpdated || tail != "" ||
		currentHeader != "current/"+path || updatedHeader != "updated/"+path {
		return Edit{}, false
	}
	return Edit{
		Format: EditSweep, path: path, cursor: -1,
		Region:   strings.TrimSuffix(current, "\n"),
		original: strings.TrimSuffix(original, "\n"),
		context:  contextTokenCleaner.Replace(strings.Join(blocks[:n-3], sweepSeparator)),
	}, true
}

func (e *Edit) setRegion(region, marker string) {
	e.cursor = strings.Index(region, marker)
	e.Region = strings.ReplaceAll(region, marker, "")
}

// SystemPrompt instructs the model to return a complete replacement.
func (e Edit) SystemPrompt() string {
	switch e.Format {
	case EditZeta:
		return editSystemRules + "\nReply with ONLY the full rewritten region. If no change is needed, return the region unchanged."
	case EditZeta2:
		return editSystemRules + "\nReply with ONLY the full rewritten region, including every unchanged line. If no change is needed, return the region unchanged."
	case EditSweep:
		return editSystemRules + "\nThere is no cursor. Predict the next edit from the difference between the original and current windows. Reply with ONLY the full rewritten current window. If no change is needed, return the current window unchanged."
	default:
		return ""
	}
}

const editSystemRules = `You are a code-editing engine embedded in a text editor.
You receive recent edits, surrounding context, and a region or window to rewrite.
Keep every unchanged line verbatim. Never abbreviate or elide code, and never use "rest unchanged" comments.
No markdown, no code fences, no explanations, no format markers, and no <CURSOR> in the reply.
Match the file's indentation, naming and style.`

// UserMessage supplies the edit history, context and replacement region.
func (e Edit) UserMessage() string {
	var b strings.Builder
	b.WriteString("File path: " + e.path + "\n\n")
	if strings.TrimSpace(e.context) != "" {
		label := "Related files and recent edits"
		if e.Format == EditZeta {
			label = "Recent user edits"
		}
		writeEditSection(&b, label, "context", e.context)
	}
	if e.Format == EditSweep {
		writeEditSection(&b, "Original window", "original", e.original)
		writeEditSection(&b, "Current window", "current", e.Region)
		b.WriteString("Reply with only the full updated current window.")
	} else {
		writeEditSection(&b, "Code before the region", "before", e.before)
		region := e.Region
		if e.cursor >= 0 && e.cursor <= len(region) {
			region = region[:e.cursor] + cursorMarker + region[e.cursor:]
		}
		writeEditSection(&b, "Region to rewrite", "region", region)
		writeEditSection(&b, "Code after the region", "after", e.after)
		b.WriteString("Reply with only the full rewritten region, without the cursor marker.")
	}
	return b.String()
}

func writeEditSection(b *strings.Builder, label, tag, text string) {
	b.WriteString(label + ":\n<" + tag + ">\n")
	b.WriteString(text)
	b.WriteString("\n</" + tag + ">\n\n")
}

// ContextBytes counts parsed context outside the region, excluding layout markers.
func (e Edit) ContextBytes() int {
	return len(e.context) + len(e.before) + len(e.after) + len(e.original)
}
