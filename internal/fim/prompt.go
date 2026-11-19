package fim

import "strings"

const cursorMarker = "<CURSOR>"

const DefaultSystemPrompt = `You are a code completion engine embedded in a text editor.
You receive the current file with the cursor position marked as ` + cursorMarker + `.
Reply with ONLY the exact text that should be inserted at the cursor.

Rules:
- Never repeat text that already exists before or after the cursor.
- No markdown, no code fences, no explanations, no comments about what you did.
- Keep the insertion short and focused: usually finish the current line, statement or small block.
- Match the indentation, naming and style of the surrounding code.
- If nothing sensible should be inserted, reply with an empty message.`

type fimFormat struct {
	name       string
	prefix     string
	suffix     string
	middle     string
	trimSpaces bool // CodeLlama puts spaces around its markers
}

// The formats Zed can emit for open_ai_compatible_api (prompt_format setting).
var fimFormats = []fimFormat{
	{name: "deepseek_coder", prefix: "<｜fim▁begin｜>", suffix: "<｜fim▁hole｜>", middle: "<｜fim▁end｜>"},
	{name: "deepseek_coder(ascii)", prefix: "<|fim▁begin|>", suffix: "<|fim▁hole|>", middle: "<|fim▁end|>"},
	{name: "qwen/codegemma", prefix: "<|fim_prefix|>", suffix: "<|fim_suffix|>", middle: "<|fim_middle|>"},
	{name: "star_coder", prefix: "<fim_prefix>", suffix: "<fim_suffix>", middle: "<fim_middle>"},
	{name: "code_llama", prefix: "<PRE>", suffix: "<SUF>", middle: "<MID>", trimSpaces: true},
	{name: "codestral", prefix: "[PREFIX]", suffix: "[SUFFIX]"},
	{name: "glm", prefix: "<|code_prefix|>", suffix: "<|code_suffix|>", middle: "<|code_middle|>"},
}

// Special tokens that may appear in the context part (repo name, file separators...).
var contextTokenCleaner = strings.NewReplacer(
	"<|repo_name|>", "Repository: ",
	"<|file_sep|>", "\n--- file: ",
	"<repo_name>", "Repository: ",
	"<file_sep>", "\n--- file: ",
	"<filename>", "\n--- file: ",
	"<|endoftext|>", "",
)

type Prompt struct {
	Format  string
	Context string
	Prefix  string
	Suffix  string
}

// Parse splits a pre-formatted FIM prompt into context, prefix and suffix.
//
// The code being edited may itself contain marker text (for example this
// project's own source), so the real markers are located by structure rather
// than by "first match anywhere": a format with a middle marker is recognised
// by the prompt ending with it, and the sections are cut at the real marker
// positions instead of at any marker-looking text.
func Parse(prompt, explicitSuffix string) Prompt {
	if f, ok := detectFormat(prompt); ok {
		if p, ok := split(prompt, f); ok {
			return p
		}
	}
	if explicitSuffix != "" {
		return Prompt{Format: "suffix-field", Prefix: prompt, Suffix: explicitSuffix}
	}
	return Prompt{Format: "plain", Prefix: prompt}
}

func hasMarkers(prompt string, f fimFormat) bool {
	return strings.Contains(prompt, f.prefix) && strings.Contains(prompt, f.suffix)
}

func detectFormat(prompt string) (fimFormat, bool) {
	// The middle marker is the last thing in a real prompt, so a format that
	// ends with its own marker cannot be a different format's code.
	for _, f := range fimFormats {
		if f.middle != "" && strings.HasSuffix(prompt, f.middle) && hasMarkers(prompt, f) {
			return f, true
		}
	}
	for _, f := range fimFormats {
		if hasMarkers(prompt, f) {
			return f, true
		}
	}
	return fimFormat{}, false
}

func split(prompt string, f fimFormat) (Prompt, bool) {
	var context, prefix, suffix string
	if f.middle != "" {
		body := prompt
		hasMiddle := strings.HasSuffix(body, f.middle)
		if hasMiddle {
			body = body[:len(body)-len(f.middle)]
		}
		// Usual order: <prefix>code<suffix>code<middle>. The suffix marker is
		// searched after the prefix marker, so marker-like text in the context
		// cannot be mistaken for it. If there is none, try suffix-first order.
		var ok bool
		last := &suffix // the section that runs to the end of the prompt
		if context, prefix, suffix, ok = splitOrdered(body, f.prefix, f.suffix); !ok {
			if context, suffix, prefix, ok = splitOrdered(body, f.suffix, f.prefix); !ok {
				return Prompt{}, false
			}
			last = &prefix
		}
		if !hasMiddle {
			// The middle marker is not at the very end; stop at it, if present.
			if i := strings.Index(*last, f.middle); i >= 0 {
				*last = (*last)[:i]
			}
		}
	} else {
		// No middle marker; the sections may come in either order.
		pi, si := strings.Index(prompt, f.prefix), strings.Index(prompt, f.suffix)
		if pi < 0 || si < 0 {
			return Prompt{}, false
		}
		if pi < si {
			context, prefix, suffix = prompt[:pi], prompt[pi+len(f.prefix):si], prompt[si+len(f.suffix):]
		} else {
			context, suffix, prefix = prompt[:si], prompt[si+len(f.suffix):pi], prompt[pi+len(f.prefix):]
		}
	}
	if f.trimSpaces {
		prefix = strings.TrimPrefix(prefix, " ")
		prefix = strings.TrimSuffix(prefix, " ")
		suffix = strings.TrimSuffix(suffix, " ")
	}
	return Prompt{
		Format:  f.name,
		Context: strings.TrimSpace(contextTokenCleaner.Replace(context)),
		Prefix:  prefix,
		Suffix:  suffix,
	}, true
}

// splitOrdered cuts body at the first occurrence of first and the first
// occurrence of second after it, returning the text before, between and after.
func splitOrdered(body, first, second string) (before, between, after string, ok bool) {
	i := strings.Index(body, first)
	if i < 0 {
		return "", "", "", false
	}
	start := i + len(first)
	rel := strings.Index(body[start:], second)
	if rel < 0 {
		return "", "", "", false
	}
	j := start + rel
	return body[:i], body[start:j], body[j+len(second):], true
}

func (p Prompt) UserMessage() string {
	var b strings.Builder
	if p.Context != "" {
		b.WriteString("Additional context (repository / other files):\n<context>\n")
		b.WriteString(p.Context)
		b.WriteString("\n</context>\n\n")
	}
	b.WriteString("Current file, cursor marked as " + cursorMarker + ":\n<file>\n")
	b.WriteString(p.Prefix)
	b.WriteString(cursorMarker)
	b.WriteString(p.Suffix)
	b.WriteString("\n</file>\n\nReply with only the exact text to insert at " + cursorMarker + ".")
	return b.String()
}
