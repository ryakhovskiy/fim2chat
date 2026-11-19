package fim

import (
	"strings"
	"testing"
)

// clean runs the whole post-processing pipeline.
func clean(prefix, suffix, model string, stops ...string) string {
	return Prompt{Prefix: prefix, Suffix: suffix}.Clean(model, stops)
}

type cleanCase struct {
	name, prefix, suffix, model string
	stops                       []string
	want                        string
}

func runClean(t *testing.T, cases []cleanCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clean(tc.prefix, tc.suffix, tc.model, tc.stops...); got != tc.want {
				t.Errorf("Clean(%q)\n got %q\nwant %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestStripThinking(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"no thinking", "42", "42"},
		{"closed block", "<think>hmm</think>42", "42"},
		{"newlines after the block are dropped", "<think>hmm</think>\n\n42", "42"},
		{"multi-line reasoning", "<think>line1\nline2\n</think>\n42", "42"},
		{"leading whitespace before the block", "  \n<think>x</think>42", "42"},
		{"two blocks in a row", "<think>a</think>\n<think>b</think>\n42", "42"},
		{"empty block", "<think></think>42", "42"},
		{"unterminated block: nothing usable", "<think>still thinking about", ""},
		{"unterminated after whitespace", "\n <think>still", ""},
		{"only an opening tag", "<think>", ""},
		{"other tag names are not reasoning", "<thinking>x</thinking>42", "<thinking>x</thinking>42"},
		{"tag names are case-sensitive", "<THINK>x</THINK>42", "<THINK>x</THINK>42"},
		// The model's answer is code, and code may contain these tags. Only
		// reasoning at the very start of the output is reasoning.
		{"tags in the middle belong to the code", "html := `<think>a</think>` + x", "html := `<think>a</think>` + x"},
		{"this package's own regexp literal", "regexp.MustCompile(`(?s)<think>.*?</think>\\s*`)", "regexp.MustCompile(`(?s)<think>.*?</think>\\s*`)"},
		{"unterminated tag in the middle belongs to the code", "x := \"<think>\"", "x := \"<think>\""},
		{"closing tag alone belongs to the code", "</think>42", "</think>42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripThinking(tc.in); got != tc.want {
				t.Errorf("stripThinking(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripFences(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"not fenced", "42", "42"},
		{"not fenced keeps leading whitespace", "  \t42\n", "  \t42\n"},
		{"language tag", "```go\n42\n```", "42"},
		{"no language tag", "```\n42\n```", "42"},
		{"multi-line body keeps inner newlines", "```python\nx = 1\ny = 2\n```", "x = 1\ny = 2"},
		{"body indentation is kept", "```go\n\tif x {\n\t\ty()\n\t}\n```", "\tif x {\n\t\ty()\n\t}"},
		{"newline after the closing fence", "```go\n42\n```\n", "42"},
		{"blank lines and spaces before the fence", "\n\n  ```go\n42\n```", "42"},
		{"prose after the closing fence is dropped", "```\n42\n```\nHope this helps!", "42"},
		{"no closing fence (cut off by the token limit)", "```go\n42\nmore", "42\nmore"},
		{"no closing fence, trailing newline", "```go\n42\n", "42"},
		{"closing fence on the last line of the body", "```go\n42```", "42"},
		{"only the opening line", "```go", ""},
		{"only backticks", "```", ""},
		{"language tag with trailing spaces", "```go   \n42\n```", "42"},
		{"empty fenced block", "```go\n\n```", ""},
		{"empty fenced block, no blank line", "```go\n```", ""},
		{"CRLF line endings", "```go\r\n42\r\n```\r\n", "42"},
		{"CRLF multi-line", "```go\r\na\r\nb\r\n```", "a\r\nb"},
		{"one-line fence", "```x := 1```", "x := 1"},
		{"fence only in the middle is prose, not ours", "Here:\n```go\n42\n```", "Here:\n```go\n42\n```"},
		{"inline code is not a fence", "`42`", "`42`"},
		{"two backticks are not a fence", "``42``", "``42``"},
		// A markdown file being edited: the answer holds fenced blocks of its own.
		{"inner fences stay (4-backtick outer fence)", "````md\n```go\nx\n```\n````", "```go\nx\n```"},
		{"inner fences stay (3-backtick, last fence closes)", "```md\n```go\nx\n```\ntext\n```", "```go\nx\n```\ntext"},
		{"longer closing fence", "```go\n42\n`````", "42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripFences(tc.in); got != tc.want {
				t.Errorf("stripFences(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTrimPrefixEcho(t *testing.T) {
	for _, tc := range []struct{ name, prefix, text, want string }{
		{"no echo", "x := ", "foo()", "foo()"},
		{"whole line retyped", "x := ", "x := foo()", "foo()"},
		{"tab indent retyped", "\treturn ", "\treturn a + b", "a + b"},
		{"indent dropped by the model", "\treturn ", "return a + b", "a + b"},
		{"indentation differs but the line is the same", "\treturn ", "    return a", "a"},
		{"only the current line counts", "a := 1\nb := ", "a := 1\nb := 2", "a := 1\nb := 2"},
		{"line retyped, multi-line completion", "func f() {\n\treturn ", "return 1\n}", "1\n}"},
		{"indentation only: retyped", "    ", "    return x", "return x"},
		{"indentation only: different width", "    ", "  return x", "  return x"},
		{"empty prefix line (cursor at line start)", "a\n", "b := 1", "b := 1"},
		{"empty prefix", "", "b := 1", "b := 1"},
		{"text equals the line", "x := ", "x := ", ""},
		{"text shorter than the line", "x := foo(", "x :", "x :"},
		{"echo only matches at the start", "x := ", "y; x := 1", "y; x := 1"},
		{"partial word is a legit completion", "fmt.Pri", "ntln(x)", "ntln(x)"},
		{"partial word retyped from the start", "fmt.Pri", "fmt.Println(x)", "ntln(x)"},
		{"CRLF in earlier lines", "a\r\nb := ", "b := 1", "1"},

		// Very short lines are ambiguous: the completion may start the same way.
		{"one char is not an echo", "a", "abc", "abc"},
		{"two chars are not an echo", "ab", "abab", "abab"},
		{"bracket is not an echo", "x\n[", "[1, 2]", "[1, 2]"},
		{"three chars are an echo", "abc", "abcdef", "def"},
		{"indented short token: indent does not count", "\t(", "\t(1)", "\t(1)"},

		// Length is counted in characters, not bytes: one CJK or emoji character
		// is 3-4 bytes but still a single visible character.
		{"one CJK character is not an echo", "字", "字符", "字符"},
		{"one emoji is not an echo", "🎉", "🎉🎉", "🎉🎉"},
		{"two CJK characters are not an echo", "字符", "字符串", "字符串"},
		{"three CJK characters are an echo", "字符串", "字符串列表", "列表"},
		{"accented two-char word is not an echo", "éé", "ééé", "ééé"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimPrefixEcho(tc.text, tc.prefix); got != tc.want {
				t.Errorf("trimPrefixEcho(%q, prefix %q)\n got %q\nwant %q", tc.text, tc.prefix, got, tc.want)
			}
		})
	}
}

func TestApplyStops(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		stops []string
		want  string
	}{
		{"no stops", "abc", nil, "abc"},
		{"stop not present", "abc", []string{"x"}, "abc"},
		{"stop in the middle", "abcdef", []string{"cd"}, "ab"},
		{"stop at the start empties the text", "abc", []string{"a"}, ""},
		{"stop at the very end", "abc", []string{"c"}, "ab"},
		{"earliest of several stops wins", "abcdef", []string{"e", "c", "d"}, "ab"},
		{"order of stops is irrelevant", "abcdef", []string{"c", "e"}, "ab"},
		{"first occurrence is used", "a;b;c", []string{";"}, "a"},
		{"empty stop is ignored", "abc", []string{"", "c"}, "ab"},
		{"only empty stops", "abc", []string{""}, "abc"},
		{"multi-line stop", "a\n\nb", []string{"\n\n"}, "a"},
		{"newline stop", "line1\nline2", []string{"\n"}, "line1"},
		{"stop longer than the text", "ab", []string{"abc"}, "ab"},
		{"unicode stop", "ab字cd", []string{"字"}, "ab"},
		{"stop is case-sensitive", "abC", []string{"c"}, "abC"},
		{"empty text", "", []string{"x"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyStops(tc.text, tc.stops); got != tc.want {
				t.Errorf("applyStops(%q, %q) = %q, want %q", tc.text, tc.stops, got, tc.want)
			}
		})
	}
}

func TestTrimSuffixOverlap(t *testing.T) {
	for _, tc := range []struct{ name, prefix, suffix, text, want string }{
		// --- degenerate input
		{"empty text", "f(", ")", "", ""},
		{"empty suffix", "f(", "", "a)", "a)"},
		{"both empty", "", "", "", ""},
		{"no overlap", "x := ", "\n}", "foo()", "foo()"},

		// --- multi-character overlaps are always duplicates
		{"closing pair", "f(g(", "))", "a))", "a"},
		{"closer and newline", "if x {\n", "\n}", "y()\n}", "y()"},
		{"rest of the line", "x := foo(", "bar)\nnext", "a, bar)", "a, "},
		{"whole text is the overlap", "", "}\n}", "}\n", ""},
		{"text equals the start of the suffix", "", "ab\ncd", "ab", ""},
		{"overlap includes leading whitespace", "x", "  foo", "y  foo", "y"},
		{"longest overlap wins", "f(", "))\n", "a))\n", "a"},

		// --- closing brackets across lines: a duplicate only if nothing of the answer's own needs them
		{"own closing brace before the suffix's, kept", "func f() {\n\tfor i := 0; i < 3; i++ {\n", "}\n", "\t\tfmt.Println(i)\n\t}\n", "\t\tfmt.Println(i)\n\t}\n"},
		{"own closing paren and newline, kept", "foo(\n\tbar(\n", ")\n", "\t\tx,\n\t)\n", "\t\tx,\n\t)\n"},
		{"answer also closes the suffix's brace, trimmed", "if a {\n\tif b {\n", "}\n", "\t\tx()\n\t}\n}\n", "\t\tx()\n\t}\n"},
		{"closing brace of the cursor's own block, trimmed", "if x {\n", "}\n", "y()\n}\n", "y()\n"},

		// --- a lone character is ambiguous
		{"lone non-bracket is kept", "x := 1", ";", "+ 2;", "+ 2;"},
		{"lone letter is kept", "a + ", "x\n", "x", "x"},
		{"lone quote is kept", "s := ", "\"", "\"abc\"", "\"abc\""},
		{"lone comma is kept", "f(a", ", b)", "x,", "x,"},

		// --- a lone closing bracket: duplicate, or does it close something of its own?
		{"paren duplicate", "foo(", ")", "a, b)", "a, b"},
		{"paren: only the bracket", "foo(", ")", ")", ""},
		{"paren: closes a nested call, kept", "foo(bar(", ")", "x)", "x)"},
		{"bracket duplicate", "x := []int{1, 2", "}", ", 3}", ", 3"},
		{"square bracket duplicate", "a[", "]", "i]", "i"},
		{"square: nested index", "a[b[", "]", "i]", "i]"},
		{"brace: struct literal", "f(T{", "}", "a: 1}", "a: 1"},
		{"brace: closes own, kept", "f(T{U{", "}", "a: 1}", "a: 1}"},
		{"text closes its own bracket, suffix bracket has no opener", "foo(bar[", "]", "1]", "1"},
		{"mismatched kind: paren in text, bracket in suffix", "a[", "]", "f(x)]", "f(x)"},
		{"whitespace before the lone bracket", "foo(", "  )", "a  )", "a"},
		{"two suffix closers, one in text: it is a duplicate", "f(g(", "))", "a)", "a"},
		{"two suffix closers, text closes none", "f(g(", "))", "a", "a"},
		{"two suffix closers but only one opener", "g(", "))", "a)", "a"},
		{"bracket not at the end of the text", "foo(", ")", "a) + b", "a) + b"},
		{"suffix bracket has nothing to close, kept", "x := 1", ")", "+ 2)", "+ 2"},

		// --- brackets inside strings and character literals are not brackets
		{"paren inside a string", "foo(\"(\", ", ")", "\"a\")", "\"a\""},
		// In each of these a naive scanner that does not skip strings pops the
		// wrong opener and would call the bracket a duplicate.
		{"closer inside a string does not count", "f(g(\")\", ", ")", "x)", "x)"},
		{"closer in an earlier string, then a call", "foo(\")\", bar(", ")", "x)", "x)"},
		{"escaped quote does not end the string", "foo(\"a\\\"(\", bar(", ")", "x)", "x)"},
		{"closer inside a char literal does not count", "f(g(c == ')', ", ")", "x)", "x)"},
		{"char literal with a closer", "if c == ')' && foo(", ")", "x)", "x"},
		{"char literal with an opener", "if c == '(' && foo(", ")", "x)", "x"},
		{"escaped char literal", "if c == '\\'' && foo(", ")", "x)", "x"},
		{"unterminated string ends at the line break", "s := \"abc\nfoo(", ")", "x)", "x"},
		// Known limitation: comments are not understood, so the "(" in the comment
		// counts as open and the bracket is conservatively kept.
		{"comments are not understood", "// don't (\nfoo(", ")", "x)", "x)"},

		// --- unicode: one visible character is one character, however many bytes
		{"lone CJK character is kept", "x", "字\n", "y字", "y字"},
		{"lone CJK bracket-like character is kept", "x", "）", "y）", "y）"},
		{"two CJK characters are trimmed", "x", "字符\n", "y字符", "y"},
		{"CJK with a newline is trimmed", "x", "\n字", "y\n字", "y"},
		{"emoji overlap of two characters", "x", "🎉🎉\n", "y🎉🎉", "y"},

		// --- limits
		// An answer that repeats a long stretch of the suffix is removed entirely,
		// however long it is; it used to be cut at 2 KiB and leave a long tail.
		{"long duplicate", "", strings.Repeat("ab\n", 5000), strings.Repeat("ab\n", 4000), ""},
		{"long duplicate with a real beginning", "", strings.Repeat("ab\n", 5000), "real\n" + strings.Repeat("ab\n", 4000), "real\n"},
		{"very long overlap, still linear", "", strings.Repeat("a", 60000) + "\n", strings.Repeat("a", 60000), ""},

		// --- a lone quote: duplicate only if it closes a string opened before the cursor
		{"quote closes the string opened before the cursor", "s := \"ab", "\"", "cd\"", "cd"},
		{"quote opens and closes inside the text, kept", "s := ", "\"", "\"abc\"", "\"abc\""},
		{"escaped quote does not close", "s := \"ab", "\"", "cd\\\"", "cd\\\""},
		{"second string in the text, kept", "s := \"ab", "\"", "c\" + \"d\"", "c\" + \"d\""},
		{"different quote kind, kept", "s := \"ab", "'", "c'", "c'"},
		{"single quotes", "x = 'ab", "'", "cd'", "cd"},
		{"python f-string prefix", "x = f'a", "'", "b'", "b"},
		{"python raw string prefix", "x = r'a", "'", "b'", "b"},
		{"two-letter string prefix", "x = rb'a", "'", "b'", "b"},
		{"backtick", "x := `ab", "`", "cd`", "cd"},
		{"apostrophe in a word is not a string", "// don't", "'", "t'", "t'"},
		{"quote followed by a bracket is a two-character overlap", "print('a", "')", "b')", "b"},
		{"whitespace-only overlap is never stripped", "x", "  y", "a  ", "a  "},
		{"whitespace overlap with a line break is whitespace too", "x", "\n\ny", "a\n\n", "a\n\n"},
		{"a string closed and reopened on the line", "s := \"a\" + \"b", "\"", "c\"", "c"},
		{"string opened on an earlier line does not count", "s := \"ab\ncd", "\"", "e\"", "e\""},
		{"an escaped quote inside the text does not close the string", "s := \"ab", "\"", "c\\\"d\"", "c\\\"d"},
		{"a line break inside the text ends the question", "s := \"ab", "\"", "c\nd\"", "c\nd\""},
		{"escaped quote earlier on the line keeps the string open", "s := \"a\\\"b", "\"", "c\"", "c"},
		{"escaped backslash before the closing quote", "s := \"a\\\\", "\"", "\"", ""},

		// --- invalid UTF-8 (a model can emit a truncated character): never cut inside a character
		{"stray continuation byte is not an overlap boundary", "x", "\x96b", "a\x96", "a\x96"},
		{"overlap that would end inside a character", "x", "\xe4\xb8\x96y", "a\xe4\xb8", "a\xe4\xb8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimSuffixOverlap(tc.text, tc.prefix, tc.suffix); got != tc.want {
				t.Errorf("trimSuffixOverlap(text %q, prefix %q, suffix %q)\n got %q\nwant %q", tc.text, tc.prefix, tc.suffix, got, tc.want)
			}
		})
	}
}

func TestOpenBrackets(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"foo(", "("},
		{"foo(bar)", ""},
		{"a[b{c(", "[{("},
		{"a[b{c(d)e}", "["},
		{"(]", "("}, // a closer that does not match the top is ignored
		{")", ""},   // a closer with nothing open is ignored
		{`f("(", `, "("},
		{`f("(" + g(`, "(("},
		{`f("a\\")`, ""}, // an escaped backslash does not escape the closing quote
		{`f("a\"(`, "("}, // the escaped quote keeps the string open, so that "(" is string content
		{"f(')')", ""},
		{"f('(')", ""},
		{`f('\'')`, ""},
		{"s := \"unterminated\nf(", "("},
		// Escaped quote inside a character literal: both literals are skipped, so
		// the "(" in '(' does not count (it used to, leaving "{(").
		{"{'\\'','('", "{"},
		{"f('\\n', '(')", ""},
		{"x := '\\''; g(", "("},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := string(openBrackets(tc.in)); got != tc.want {
				t.Errorf("openBrackets(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCleanCursorMarker(t *testing.T) {
	runClean(t, []cleanCase{
		{name: "echoed marker is removed", prefix: "x := ", model: "4<CURSOR>2", want: "42"},
		{name: "marker alone", prefix: "x := ", model: "<CURSOR>", want: ""},
		{name: "several markers", prefix: "x := ", model: "<CURSOR>4<CURSOR>2<CURSOR>", want: "42"},
		{name: "marker the file really contains is kept", prefix: "const m = \"<CURSOR>\"; x := ", model: "a<CURSOR>b", want: "a<CURSOR>b"},
		{name: "marker in the suffix is also the file's", prefix: "x := ", suffix: "// <CURSOR>", model: "a<CURSOR>b", want: "a<CURSOR>b"},
		{name: "similar text is not the marker", prefix: "x := ", model: "<cursor> <CURSOR >", want: "<cursor> <CURSOR >"},
	})
}

// Realistic model output, run through the complete pipeline.
func TestCleanPipeline(t *testing.T) {
	runClean(t, []cleanCase{
		{name: "a good answer is untouched", prefix: "func add(a, b int) int {\n\treturn ", suffix: "\n}\n", model: "a + b", want: "a + b"},
		{name: "empty answer stays empty", prefix: "x := ", model: "", want: ""},
		{name: "whitespace-only answer is kept as is", prefix: "x := ", model: "\n", want: "\n"},
		{name: "multi-line answer keeps its indentation", prefix: "func f() {\n\t", suffix: "\n}", model: "a := 1\n\tb := 2", want: "a := 1\n\tb := 2"},
		{name: "answer ends with a newline", prefix: "x := 1\n", suffix: "y := 2", model: "z := 3\n", want: "z := 3\n"},

		{name: "reasoning, then fence", prefix: "x := ", model: "<think>hmm</think>\n```go\n42\n```", want: "42"},
		{name: "fence, then echoed line", prefix: "\treturn ", model: "```go\n\treturn a + b\n```", want: "a + b"},
		{name: "echo, then closing bracket duplicate", prefix: "\tfoo(", suffix: ")\n", model: "\tfoo(a, b)", want: "a, b"},
		{name: "everything at once", prefix: "func f() int {\n\treturn g(", suffix: ")\n}\n",
			model: "<think>plan</think>\n```go\n\treturn g(x, y)\n}\n```", want: "x, y"},
		{name: "reasoning, fence, echo and cursor marker", prefix: "x := f(", suffix: ")",
			model: "<think>a</think>```\nx := f(1<CURSOR>)\n```", want: "1"},

		{name: "stop sequence cuts the answer", prefix: "x := ", model: "1\n\nfunc other() {}", stops: []string{"\n\n"}, want: "1"},
		{name: "stop applies after echo removal", prefix: "x := ", model: "x := 1; y := 2", stops: []string{"; "}, want: "1"},
		{name: "stop that appears in the echoed line only", prefix: "a; b := ", model: "a; b := 1", stops: []string{";"}, want: "1"},
		{name: "stop, then suffix overlap", prefix: "f(", suffix: ")", model: "a)\n\nrest", stops: []string{"\n\n"}, want: "a"},

		{name: "CRLF answer inside a CRLF file", prefix: "a\r\nb(", suffix: ")\r\nc", model: "x)", want: "x"},
		{name: "unicode answer", prefix: "s := \"", suffix: "\"", model: "héllo 世界\"", want: "héllo 世界"},
		{name: "answer identical to the suffix start is dropped", prefix: "if x {\n", suffix: "\n}\n", model: "\n}\n", want: ""},
	})
}

func TestCleanOnlyEverRemovesText(t *testing.T) {
	// A fixed table of nasty inputs: the output is never longer than the input.
	inputs := []string{"", " ", "\n", "```", "```\n```", "<think>", "</think>", "<CURSOR>", ")", "))))", "\x00", "字", "a\r\n", strings.Repeat("(", 4000)}
	prompts := []Prompt{{}, {Prefix: "f("}, {Suffix: ")"}, {Prefix: "f(", Suffix: "))"}, {Prefix: "字", Suffix: "字"}, {Prefix: strings.Repeat("(", 3000), Suffix: strings.Repeat(")", 3000)}}
	for _, p := range prompts {
		for _, in := range inputs {
			if out := p.Clean(in, []string{"\n", ""}); len(out) > len(in) {
				t.Errorf("Clean grew %q into %q", in, out)
			}
		}
	}
}

func BenchmarkClean(b *testing.B) {
	p := Prompt{Prefix: strings.Repeat("foo(bar(", 50000), Suffix: strings.Repeat("))\n", 50000)}
	text := "```go\nx, y)\n```"
	for b.Loop() {
		p.Clean(text, nil)
	}
}

func TestClosesEarlierString(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, text string
		q                  byte
		want               bool
	}{
		{"closes the open string", `x := "ab`, `c"`, '"', true},
		{"empty text", `x := "ab`, ``, '"', false},
		{"text does not end with the quote", `x := "ab`, `c`, '"', false},
		{"no string is open", `x := `, `c"`, '"', false},
		{"another kind of string is open", `x := 'ab`, `c"`, '"', false},
		{"a quote earlier in the text closes it first", `x := "ab`, `c" + "d"`, '"', false},
		{"text ends with an escaped quote", `x := "ab`, `c\"`, '"', false},
		{"the text crosses a line break", `x := "ab`, "c\nd\"", '"', false},
		{"only the last line of the prefix counts", "x := \"\nfoo(\"a", `b"`, '"', true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := closesEarlierString(tc.prefix, tc.text, tc.q); got != tc.want {
				t.Errorf("closesEarlierString(%q, %q, %q) = %v, want %v", tc.prefix, tc.text, tc.q, got, tc.want)
			}
		})
	}
}

func TestOpenQuoteAtEnd(t *testing.T) {
	for _, tc := range []struct {
		line string
		want byte
	}{
		{``, 0},
		{`x := 1`, 0},
		{`x := "ab`, '"'},
		{`x := "ab"`, 0},
		{`x := "ab" + "c`, '"'},
		{`x := "a\"b`, '"'}, // escaped quote does not close
		{`x := "a\\"`, 0},   // escaped backslash does
		{`x := 'ab`, '\''},
		{`x := "it's`, '"'}, // an apostrophe inside a string is just text
		{`// don't`, 0},     // an apostrophe in a word is not a string
		{`x = f'a`, '\''},   // string prefix letter
		{`x = rb'a`, '\''},  // two prefix letters: "rb" looks like a word, accepted as apostrophe (limitation)
		{"x := `ab", '`'},
		{`say "hi" and 'bye`, '\''},
	} {
		if got := openQuoteAtEnd(tc.line); got != tc.want {
			t.Errorf("openQuoteAtEnd(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}
