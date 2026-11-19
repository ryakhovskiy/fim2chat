package fim

import (
	"strings"
	"unicode/utf8"
)

// minEchoChars is how many visible characters the cursor line needs before a
// matching start of the model output is treated as a re-typed copy of it.
// Shorter lines ("a", "(") are too ambiguous: the completion may legitimately
// begin with the same characters.
const minEchoChars = 3

// maxOverlap bounds how much of the suffix is compared with the end of the
// answer. It only limits memory; the comparison itself is linear.
const maxOverlap = 64 << 10

func (p Prompt) Clean(text string, stops []string) string {
	text = stripThinking(text)
	text = stripFences(text)
	// <CURSOR> is only ours to remove when the file itself never mentions it.
	if !strings.Contains(p.Prefix, cursorMarker) && !strings.Contains(p.Suffix, cursorMarker) {
		text = strings.ReplaceAll(text, cursorMarker, "")
	}
	text = trimPrefixEcho(text, p.Prefix)
	text = applyStops(text, stops)
	text = trimSuffixOverlap(text, p.Prefix, p.Suffix)
	return text
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// stripThinking removes reasoning blocks, which a model emits before its
// answer. Only blocks at the very start are reasoning: the answer is code, and
// code may contain these tags (this very file does). A block that was opened
// but never closed (output cut off by the token limit) leaves nothing usable.
func stripThinking(s string) string {
	for {
		t := strings.TrimLeft(s, " \t\r\n")
		if !strings.HasPrefix(t, thinkOpen) {
			return s
		}
		end := strings.Index(t, thinkClose)
		if end < 0 {
			return ""
		}
		s = strings.TrimLeft(t[end+len(thinkClose):], " \t\r\n")
	}
}

// stripFences removes a markdown code fence the model wrapped its answer in,
// and any prose after the closing fence. The fence may be longer than three
// backticks (used when the content has fences of its own), and the closing
// fence must be at least as long as the opening one.
func stripFences(s string) string {
	t := strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(t, "```") {
		return s
	}
	t = strings.TrimRight(t, " \t\r\n")
	n := len(t) - len(strings.TrimLeft(t, "`")) // length of the opening fence
	rest := t[n:]

	nl := strings.IndexByte(rest, '\n')
	if nl < 0 {
		// One line: either "```code```" or just "```lang".
		if inner, ok := cutClosingRun(rest, n); ok {
			return inner
		}
		return ""
	}
	body := rest[nl+1:] // the opening line (fence and language tag) is dropped

	lines := strings.Split(body, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if isFenceLine(lines[i], n) {
			return strings.TrimSuffix(strings.Join(lines[:i], "\n"), "\r")
		}
	}
	// No closing fence on a line of its own: the answer was cut off, or the
	// fence trails the last line of code.
	if inner, ok := cutClosingRun(body, n); ok {
		body = inner
	}
	return strings.TrimSuffix(strings.TrimSuffix(body, "\n"), "\r")
}

// isFenceLine reports whether line consists of n or more backticks only.
func isFenceLine(line string, n int) bool {
	t := strings.TrimSpace(line)
	return len(t) >= n && strings.Trim(t, "`") == ""
}

// cutClosingRun removes a trailing run of at least n backticks.
func cutClosingRun(s string, n int) (string, bool) {
	trimmed := strings.TrimRight(s, "`")
	if len(s)-len(trimmed) >= n {
		return trimmed, true
	}
	return s, false
}

// Chat models often re-type the line the cursor is on. Remove that echo.
func trimPrefixEcho(text, prefix string) string {
	line := prefix[strings.LastIndexByte(prefix, '\n')+1:]
	if line == "" {
		return text
	}
	// Counted in characters, not bytes: one CJK character or emoji is 3-4 bytes.
	if visible := strings.TrimSpace(line); visible != "" && utf8.RuneCountInString(visible) < minEchoChars {
		return text
	}
	if strings.HasPrefix(text, line) {
		return text[len(line):]
	}
	if trimmedLine := strings.TrimLeft(line, " \t"); trimmedLine != "" {
		t := strings.TrimLeft(text, " \t")
		if strings.HasPrefix(t, trimmedLine) {
			return t[len(trimmedLine):]
		}
	}
	return text
}

func applyStops(text string, stops []string) string {
	cut := len(text)
	for _, s := range stops {
		if s == "" {
			continue
		}
		if i := strings.Index(text, s); i >= 0 && i < cut {
			cut = i
		}
	}
	return text[:cut]
}

// Chat models often repeat the beginning of the suffix (closing brackets,
// the rest of the line...). Remove the longest such overlap.
func trimSuffixOverlap(text, prefix, suffix string) string {
	for _, k := range overlaps(text, suffix) {
		// Never cut through a multi-byte character.
		if !utf8.RuneStart(text[len(text)-k]) || (k < len(suffix) && !utf8.RuneStart(suffix[k])) {
			continue
		}
		overlap := suffix[:k]
		significant := strings.TrimSpace(overlap)
		if significant == "" {
			continue
		}
		switch {
		case onlyClosers(significant):
			// Closing brackets (alone or across lines, as in "}\n") are a
			// duplicate only if the suffix's own brackets already find their
			// openers; otherwise they close something the answer opened and
			// removing them would unbalance the code.
			if closersAreNeeded(prefix, text, suffix) {
				continue
			}
		case utf8.RuneCountInString(significant) < 2 && !strings.Contains(overlap, "\n") &&
			!loneDuplicate(prefix, text, suffix, significant[0]):
			// A lone character usually is not a duplicate (it may be
			// legitimate), except for a quote the editor inserted itself.
			continue
		}
		// Anything else that spans a line break or has 2+ visible characters
		// is almost surely a duplicate.
		return text[:len(text)-k]
	}
	return text
}

// overlaps returns, longest first, every k > 0 for which text ends with
// suffix[:k]. It walks the failure links of the Knuth-Morris-Pratt automaton,
// so it is linear however long the overlap is.
func overlaps(text, suffix string) []int {
	m := min(len(text), len(suffix), maxOverlap)
	if m == 0 {
		return nil
	}
	s, tail := suffix[:m], text[len(text)-m:]
	fail := make([]int32, m) // fail[i]: longest proper border of s[:i+1]
	for i, j := 1, 0; i < m; i++ {
		for j > 0 && s[i] != s[j] {
			j = int(fail[j-1])
		}
		if s[i] == s[j] {
			j++
		}
		fail[i] = int32(j)
	}
	j := 0 // how much of s the tail currently ends with
	for i := 0; i < m; i++ {
		for j > 0 && (j == m || tail[i] != s[j]) {
			j = int(fail[j-1])
		}
		if tail[i] == s[j] {
			j++
		}
	}
	var ks []int
	for ; j > 0; j = int(fail[j-1]) {
		ks = append(ks, j)
	}
	return ks
}

// loneDuplicate decides whether the one visible non-bracket character c, which
// ends the answer and also starts the suffix, repeats a quote the editor
// already inserted after the cursor. Brackets go through closersAreNeeded.
func loneDuplicate(prefix, text, suffix string, c byte) bool {
	return isQuote(c) && closesEarlierString(prefix, text, c)
}

var openerFor = map[byte]byte{')': '(', ']': '[', '}': '{'}

func isCloser(c byte) bool {
	_, ok := openerFor[c]
	return ok
}

// onlyClosers reports whether s is non-empty and made of closing brackets only.
func onlyClosers(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isCloser(s[i]) {
			return false
		}
	}
	return s != ""
}

// closersAreNeeded reports whether the run of closing brackets that starts the
// suffix still finds all its openers once prefix+text is in place. If it does,
// a bracket at the end of text closes something else and must be kept. If it
// does not, the editor's brackets are already enough and text repeats one.
func closersAreNeeded(prefix, text, suffix string) bool {
	stack := openBrackets(prefix + text)
	rest := strings.TrimLeft(suffix, " \t\r\n")
	for i := 0; i < len(rest) && isCloser(rest[i]); i++ {
		n := len(stack)
		if n == 0 || stack[n-1] != openerFor[rest[i]] {
			return false
		}
		stack = stack[:n-1]
	}
	return true
}

// openBrackets returns the brackets in s that are still open at its end. It
// skips "double quoted" strings and 'c' character literals, but it is only a
// heuristic: comments and multi-line strings are not understood.
func openBrackets(s string) []byte {
	var stack []byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '(', '[', '{':
			stack = append(stack, c)
		case ')', ']', '}':
			if n := len(stack); n > 0 && stack[n-1] == openerFor[c] {
				stack = stack[:n-1]
			}
		case '"':
			for i++; i < len(s) && s[i] != '"' && s[i] != '\n'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '\'':
			// A character literal: 'x' or an escaped one such as '\''. The escaped
			// form must be tried first, or '\'' would be read as the literal '\'
			// followed by a stray quote.
			if i+3 < len(s) && s[i+1] == '\\' && s[i+3] == '\'' {
				i += 3
			} else if i+2 < len(s) && s[i+2] == '\'' {
				i += 2
			}
		}
	}
	return stack
}

func isQuote(c byte) bool { return c == '"' || c == '\'' || c == '`' }

// closesEarlierString reports whether the cursor sits inside a string opened
// earlier on the same line, and the quote q that ends text is the one that
// closes it: then the editor's auto-inserted closing quote in the suffix makes
// text's quote a duplicate. A quote that merely closes a string opened and
// finished inside text is legitimate.
func closesEarlierString(prefix, text string, q byte) bool {
	if text == "" || text[len(text)-1] != q {
		return false
	}
	if openQuoteAtEnd(prefix[strings.LastIndexByte(prefix, '\n')+1:]) != q {
		return false
	}
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '\n':
			return false
		case q:
			return i == len(text)-1 // the first unescaped quote closes the string
		}
	}
	return false
}

// openQuoteAtEnd returns the quote character of the string that is still open
// at the end of line, or 0. An apostrophe inside a word ("don't") does not
// start a string, but a string prefix letter does (f'..', r'..', b'..').
func openQuoteAtEnd(line string) byte {
	var open byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if open != 0 {
			switch c {
			case '\\':
				i++
			case open:
				open = 0
			}
			continue
		}
		if !isQuote(c) || (c == '\'' && isApostrophe(line, i)) {
			continue
		}
		open = c
	}
	return open
}

// stringPrefixes are the letters that may directly precede a quote and belong
// to the string (Python and others: r'..', b'..', f'..', rb'..', fr'..').
var stringPrefixes = map[string]bool{"r": true, "b": true, "f": true, "u": true, "rb": true, "br": true, "fr": true, "rf": true}

// isApostrophe reports whether the single quote at line[i] sits inside a word
// ("don't") rather than starting a string. A word that is just a string prefix
// ("f", "rb") followed by the quote starts a string.
func isApostrophe(line string, i int) bool {
	if i == 0 || !isWordByte(line[i-1]) {
		return false
	}
	start := i
	for start > 0 && isWordByte(line[start-1]) {
		start--
	}
	return !stringPrefixes[strings.ToLower(line[start:i])]
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 0x80 || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
