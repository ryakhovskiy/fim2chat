package fim

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Seeds also run as ordinary tests; `go test -fuzz=FuzzParse ./internal/fim`
// explores beyond them.

func FuzzParse(f *testing.F) {
	for _, format := range fimFormats {
		f.Add(build(format, "", "left", "right"), "")
		f.Add(build(format, "<|repo_name|>r<|file_sep|>a.go\n", "x := ", "\n"), "")
	}
	f.Add("plain code", "suffix")
	f.Add("", "")
	f.Add("<fim_prefix><fim_prefix><fim_suffix><fim_suffix><fim_middle><fim_middle>", "")
	f.Add("[SUFFIX][PREFIX][SUFFIX][PREFIX]", "x")
	f.Add("<PRE> <SUF> <MID>", "")
	f.Add("<｜fim▁begin｜>\xff\xfe<｜fim▁hole｜>", "")

	f.Fuzz(func(t *testing.T, prompt, explicitSuffix string) {
		p := Parse(prompt, explicitSuffix)

		if p.Format == "" {
			t.Fatal("empty format")
		}
		// Parsing only ever cuts the prompt: the sections are pieces of it
		// (the suffix of "suffix-field" comes from the request field instead).
		if !strings.Contains(prompt, p.Prefix) || (p.Format != "suffix-field" && !strings.Contains(prompt, p.Suffix)) {
			t.Fatalf("prefix or suffix is not part of the prompt: %+v", p)
		}
		switch p.Format {
		case "plain":
			if p.Prefix != prompt || p.Suffix != "" || p.Context != "" || explicitSuffix != "" {
				t.Fatalf("plain must be the whole prompt: %+v", p)
			}
		case "suffix-field":
			if p.Prefix != prompt || p.Suffix != explicitSuffix || explicitSuffix == "" {
				t.Fatalf("suffix-field must be prompt + explicit suffix: %+v", p)
			}
		default:
			// A recognised format never loses code: prefix, suffix and context
			// plus the markers account for the prompt (a little less for
			// code_llama's spaces and the context's token rewriting).
			if len(p.Prefix)+len(p.Suffix) > len(prompt) {
				t.Fatalf("sections longer than the prompt: %+v", p)
			}
			// ...and the same prompt always parses the same way.
			if q := Parse(prompt, explicitSuffix); q != p {
				t.Fatalf("not deterministic: %+v vs %+v", p, q)
			}
		}
		// What the model is sent is always well-formed.
		if msg := p.UserMessage(); !strings.Contains(msg, p.Prefix+cursorMarker+p.Suffix) {
			t.Fatalf("user message lost the prefix/cursor/suffix adjacency: %q", msg)
		}
	})
}

func FuzzClean(f *testing.F) {
	f.Add("x := ", "\n", "1", "")
	f.Add("foo(", ")", "a, b)", "")
	f.Add("s := \"ab", "\"", "cd\"", "")
	f.Add("\treturn ", "\n}", "```go\n\treturn a + b\n```", "\n\n")
	f.Add("", "", "<think>x</think>y", "y")
	f.Add("f(g(", "))", "a)", ")")
	f.Add("字", "字符\n", "字符串", "字")
	f.Add("a\r\nb(", ")\r\nc", "```\r\nx)\r\n```", "")
	f.Add(strings.Repeat("(", 100), strings.Repeat(")", 100), strings.Repeat(")", 50), "")
	f.Add("x", "\xff\xfe", "\xff", "")

	f.Fuzz(func(t *testing.T, prefix, suffix, model, stop string) {
		p := Prompt{Prefix: prefix, Suffix: suffix}
		out := p.Clean(model, []string{stop})

		// Clean only removes text: it never grows or invents anything.
		if len(out) > len(model) {
			t.Fatalf("output longer than input: %q -> %q", model, out)
		}
		// Valid in, valid out: cutting never splits a character.
		if utf8.ValidString(prefix) && utf8.ValidString(suffix) && utf8.ValidString(model) && utf8.ValidString(stop) && !utf8.ValidString(out) {
			t.Fatalf("produced invalid UTF-8 from valid input: %q -> %q", model, out)
		}
		// A stop sequence never survives in the output.
		if stop != "" && strings.Index(applyStops(out, []string{stop}), stop) >= 0 {
			t.Fatalf("stop %q survived in %q", stop, out)
		}
		// The marker we asked the model to use never leaks, unless the file has it.
		if !strings.Contains(prefix+suffix, cursorMarker) && strings.Contains(out, cursorMarker) {
			t.Fatalf("cursor marker leaked: %q", out)
		}
		// Deterministic.
		if again := p.Clean(model, []string{stop}); again != out {
			t.Fatalf("not deterministic: %q vs %q", out, again)
		}
	})
}

// overlaps must agree with the obvious quadratic definition.
func FuzzOverlaps(f *testing.F) {
	f.Add("abcabc", "abcabcd")
	f.Add("", "x")
	f.Add("aaaa", "aaaaaaa")
	f.Add("xy", "yz")
	f.Fuzz(func(t *testing.T, text, suffix string) {
		var want []int
		for k := min(len(text), len(suffix), maxOverlap); k > 0; k-- {
			if strings.HasSuffix(text, suffix[:k]) {
				want = append(want, k)
			}
		}
		got := overlaps(text, suffix)
		if len(got) != len(want) {
			t.Fatalf("overlaps(%q, %q) = %v, want %v", text, suffix, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("overlaps(%q, %q) = %v, want %v", text, suffix, got, want)
			}
		}
	})
}
