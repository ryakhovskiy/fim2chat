package fim

import (
	"strings"
	"testing"
)

// build assembles a prompt the way the editor does for each format.
func build(f fimFormat, context, prefix, suffix string) string {
	switch f.name {
	case "code_llama": // <PRE> {prefix} <SUF>{suffix} <MID>
		return context + f.prefix + " " + prefix + " " + f.suffix + suffix + " " + f.middle
	case "codestral": // [SUFFIX]{suffix}[PREFIX]{prefix}
		return context + f.suffix + suffix + f.prefix + prefix
	default: // <prefix>{prefix}<suffix>{suffix}<middle>
		return context + f.prefix + prefix + f.suffix + suffix + f.middle
	}
}

// The code that goes in. Anything is legal inside a source file.
var samples = []struct{ name, prefix, suffix string }{
	{"simple", "left", "right"},
	{"empty prefix (cursor at file start)", "", "right"},
	{"empty suffix (cursor at file end)", "left", ""},
	{"both empty", "", ""},
	{"multi-line Go", "package main\n\nfunc main() {\n\tfmt.Println(", ")\n}\n"},
	{"CRLF line endings", "a\r\nb(", ")\r\nc\r\n"},
	{"tabs and leading spaces", "\t\tx := ", "\n\t\ty()"},
	{"unicode", "// héllo 世界 🎉\nx := \"", "\"\n// ünï"},
	{"trailing space in prefix", "x = ", "\n"},
	{"leading space in suffix", "x", " + y"},
	{"only whitespace", "   ", "\n\n"},
	{"angle brackets and brackets in code", "a < b && c > d; arr[", "]; f<T>()"},
	{"marker-like but not a marker", "<fim_>x", "<|fim|>"},
	{"very long line", strings.Repeat("x", 100000), strings.Repeat("y", 100000)},
}

func TestParseEveryFormat(t *testing.T) {
	for _, f := range fimFormats {
		for _, s := range samples {
			t.Run(f.name+"/"+s.name, func(t *testing.T) {
				got := Parse(build(f, "", s.prefix, s.suffix), "")
				want := Prompt{Format: f.name, Prefix: s.prefix, Suffix: s.suffix}
				if got != want {
					t.Errorf("Parse = %+v\nwant   %+v", short(got), short(want))
				}
			})
		}
	}
}

func TestParseEveryFormatWithContext(t *testing.T) {
	const ctx = "<|repo_name|>myrepo<|file_sep|>a.go\npackage a\n<|file_sep|>b.go\npackage b\n<|file_sep|>cur.go\n"
	const wantCtx = "Repository: myrepo\n--- file: a.go\npackage a\n\n--- file: b.go\npackage b\n\n--- file: cur.go"
	for _, f := range fimFormats {
		t.Run(f.name, func(t *testing.T) {
			got := Parse(build(f, ctx, "left", "right"), "")
			want := Prompt{Format: f.name, Context: wantCtx, Prefix: "left", Suffix: "right"}
			if got != want {
				t.Errorf("Parse = %+v\nwant   %+v", got, want)
			}
		})
	}
}

// short keeps failure output readable for the very long samples.
func short(p Prompt) Prompt {
	if len(p.Prefix) > 80 {
		p.Prefix = p.Prefix[:40] + "…" + p.Prefix[len(p.Prefix)-40:]
	}
	if len(p.Suffix) > 80 {
		p.Suffix = p.Suffix[:40] + "…" + p.Suffix[len(p.Suffix)-40:]
	}
	return p
}

// The table itself must be sane, or detection cannot be.
func TestFormatTableInvariants(t *testing.T) {
	names := map[string]bool{}
	var markers []string
	for _, f := range fimFormats {
		if names[f.name] {
			t.Errorf("duplicate format name %q", f.name)
		}
		names[f.name] = true
		if f.prefix == "" || f.suffix == "" || f.prefix == f.suffix {
			t.Errorf("%s: prefix and suffix markers must be non-empty and different", f.name)
		}
		if f.middle != "" && (f.middle == f.prefix || f.middle == f.suffix) {
			t.Errorf("%s: middle marker equals another marker", f.name)
		}
		markers = append(markers, f.prefix, f.suffix)
		if f.middle != "" {
			markers = append(markers, f.middle)
		}
	}
	// A marker inside another format's marker would be detected as the wrong format.
	for i, a := range markers {
		for j, b := range markers {
			if i != j && a != b && strings.Contains(a, b) {
				t.Errorf("marker %q contains marker %q", a, b)
			}
		}
	}
}

func TestParseWithoutMarkers(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, explicit string
		want                   Prompt
	}{
		{"plain", "left", "", Prompt{Format: "plain", Prefix: "left"}},
		{"empty prompt", "", "", Prompt{Format: "plain"}},
		{"explicit suffix field", "left", "right", Prompt{Format: "suffix-field", Prefix: "left", Suffix: "right"}},
		{"only a prefix marker", "<fim_prefix>left", "", Prompt{Format: "plain", Prefix: "<fim_prefix>left"}},
		{"only a suffix marker", "left<fim_suffix>right", "", Prompt{Format: "plain", Prefix: "left<fim_suffix>right"}},
		{"only a middle marker", "left<fim_middle>", "", Prompt{Format: "plain", Prefix: "left<fim_middle>"}},
		{"one marker, plus the suffix field", "<fim_prefix>left", "right", Prompt{Format: "suffix-field", Prefix: "<fim_prefix>left", Suffix: "right"}},
		{"prefix of one format, suffix of another", "<fim_prefix>a<|fim_suffix|>b", "", Prompt{Format: "plain", Prefix: "<fim_prefix>a<|fim_suffix|>b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.prompt, tc.explicit); got != tc.want {
				t.Errorf("Parse = %+v\nwant   %+v", got, tc.want)
			}
		})
	}
}

// When the prompt has FIM markers the explicit suffix field is not needed and
// must not be mixed in.
func TestExplicitSuffixIsIgnoredWhenMarkersArePresent(t *testing.T) {
	got := Parse("<fim_prefix>L<fim_suffix>R<fim_middle>", "SHOULD-NOT-APPEAR")
	if got.Prefix != "L" || got.Suffix != "R" || strings.Contains(got.Prefix+got.Suffix+got.Context, "SHOULD") {
		t.Errorf("explicit suffix leaked into %+v", got)
	}
}

func TestParseOrderOfSections(t *testing.T) {
	for _, tc := range []struct {
		name, prompt string
		want         Prompt
	}{
		// Formats with a middle marker may also come suffix-first (SPM).
		{"star_coder SPM", "<fim_suffix>R<fim_prefix>L<fim_middle>", Prompt{Format: "star_coder", Prefix: "L", Suffix: "R"}},
		{"qwen SPM", "<|fim_suffix|>R<|fim_prefix|>L<|fim_middle|>", Prompt{Format: "qwen/codegemma", Prefix: "L", Suffix: "R"}},
		{"qwen SPM with context", "ctx<|fim_suffix|>R<|fim_prefix|>L<|fim_middle|>", Prompt{Format: "qwen/codegemma", Context: "ctx", Prefix: "L", Suffix: "R"}},
		// Codestral is suffix-first in practice but either order parses.
		{"codestral SPM", "[SUFFIX]R[PREFIX]L", Prompt{Format: "codestral", Prefix: "L", Suffix: "R"}},
		{"codestral PSM", "[PREFIX]L[SUFFIX]R", Prompt{Format: "codestral", Prefix: "L", Suffix: "R"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.prompt, ""); got != tc.want {
				t.Errorf("Parse = %+v\nwant   %+v", got, tc.want)
			}
		})
	}
}

func TestDetectFormat(t *testing.T) {
	const real = "<|fim_prefix|>%s<|fim_suffix|>%s<|fim_middle|>"
	other := []string{ // every other format's complete marker set, as code might mention them
		"<｜fim▁begin｜><｜fim▁hole｜><｜fim▁end｜>", "<|fim▁begin|><|fim▁hole|><|fim▁end|>",
		"<fim_prefix><fim_suffix><fim_middle>", "<PRE><SUF><MID>", "[PREFIX][SUFFIX]", "<|code_prefix|><|code_suffix|><|code_middle|>",
	}
	for _, mention := range other {
		t.Run("code mentioning "+mention[:6], func(t *testing.T) {
			for _, where := range []string{"prefix", "suffix"} {
				p, s := "x := ", "y"
				if where == "prefix" {
					p = "s := `" + mention + "`; "
				} else {
					s = "s := `" + mention + "`"
				}
				got := Parse(strings.Replace(strings.Replace(real, "%s", p, 1), "%s", s, 1), "")
				if got.Format != "qwen/codegemma" || got.Prefix != p || got.Suffix != s {
					t.Errorf("mention in %s: format=%q prefix=%q suffix=%q", where, got.Format, got.Prefix, got.Suffix)
				}
			}
		})
	}
}

func TestDetectFormatFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, prompt string
		want         Prompt
	}{
		{"middle marker missing at the end", "<fim_prefix>L<fim_suffix>R", Prompt{Format: "star_coder", Prefix: "L", Suffix: "R"}},
		{"stray text after the middle marker", "<fim_prefix>L<fim_suffix>R<fim_middle>\n", Prompt{Format: "star_coder", Prefix: "L", Suffix: "R"}},
		{"stray text after the middle marker, longer", "<fim_prefix>L<fim_suffix>R<fim_middle>tail", Prompt{Format: "star_coder", Prefix: "L", Suffix: "R"}},
		{"deepseek full-width", "<｜fim▁begin｜>L<｜fim▁hole｜>R<｜fim▁end｜>", Prompt{Format: "deepseek_coder", Prefix: "L", Suffix: "R"}},
		{"deepseek ASCII bars", "<|fim▁begin|>L<|fim▁hole|>R<|fim▁end|>", Prompt{Format: "deepseek_coder(ascii)", Prefix: "L", Suffix: "R"}},
		{"glm", "<|code_prefix|>L<|code_suffix|>R<|code_middle|>", Prompt{Format: "glm", Prefix: "L", Suffix: "R"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.prompt, ""); got != tc.want {
				t.Errorf("Parse = %+v\nwant   %+v", got, tc.want)
			}
		})
	}
}

func TestCodeLlamaSpacing(t *testing.T) {
	for _, tc := range []struct{ name, prompt, prefix, suffix string }{
		{"format spaces are removed", "<PRE> a <SUF>b <MID>", "a", "b"},
		{"one real trailing space survives", "<PRE> x =  <SUF>b <MID>", "x = ", "b"},
		{"one real leading space survives", "<PRE>  x <SUF>b <MID>", " x", "b"},
		{"real trailing space in suffix survives", "<PRE> a <SUF>b  <MID>", "a", "b "},
		{"empty prefix", "<PRE>  <SUF>b <MID>", "", "b"},
		{"empty suffix", "<PRE> a <SUF> <MID>", "a", ""},
		{"no format spaces at all", "<PRE>a<SUF>b<MID>", "a", "b"},
		{"newlines are not trimmed", "<PRE> a\n <SUF>\nb <MID>", "a\n", "\nb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.prompt, "")
			if got.Format != "code_llama" || got.Prefix != tc.prefix || got.Suffix != tc.suffix {
				t.Errorf("prefix=%q suffix=%q, want %q and %q", got.Prefix, got.Suffix, tc.prefix, tc.suffix)
			}
		})
	}
}

func TestContext(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"none", "", ""},
		{"whitespace only", " \n\t ", ""},
		{"plain text is kept", "// utils.go\nfunc f() {}", "// utils.go\nfunc f() {}"},
		{"repo name token", "<|repo_name|>org/repo", "Repository: org/repo"},
		{"repo name token, angle form", "<repo_name>org/repo", "Repository: org/repo"},
		{"file separator token", "<|file_sep|>a.go\nx", "--- file: a.go\nx"},
		{"file separator, angle form", "<file_sep>a.go\nx", "--- file: a.go\nx"},
		{"filename token", "<filename>a.go\nx", "--- file: a.go\nx"},
		{"end of text is removed", "x<|endoftext|>y", "xy"},
		{"surrounding blank lines are trimmed", "\n\n  <|file_sep|>a.go  \n\n", "--- file: a.go"},
		{"several tokens", "<|repo_name|>r<|file_sep|>a<|file_sep|>b", "Repository: r\n--- file: a\n--- file: b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in+"<fim_prefix>L<fim_suffix>R<fim_middle>", "")
			if got.Context != tc.want || got.Prefix != "L" || got.Suffix != "R" {
				t.Errorf("context = %q, want %q (prefix %q suffix %q)", got.Context, tc.want, got.Prefix, got.Suffix)
			}
		})
	}
}

// Code that contains this format's own markers is ambiguous by nature: the
// parser cannot know which occurrence is real. What it must guarantee is that
// the markers in the suffix survive, and that no text is ever lost or invented.
func TestMarkersInsideCode(t *testing.T) {
	const P, S, M = "<|fim_prefix|>", "<|fim_suffix|>", "<|fim_middle|>"
	t.Run("all three markers inside the suffix", func(t *testing.T) {
		suffix := `a := "` + P + `" + "` + S + `" + "` + M + `"`
		got := Parse(P+"x := "+S+suffix+M, "")
		if got.Prefix != "x := " || got.Suffix != suffix {
			t.Errorf("prefix=%q suffix=%q", got.Prefix, got.Suffix)
		}
	})
	t.Run("middle marker inside the suffix, real one at the end", func(t *testing.T) {
		got := Parse(P+"L"+S+"R"+M+"R2"+M, "")
		if got.Prefix != "L" || got.Suffix != "R"+M+"R2" {
			t.Errorf("prefix=%q suffix=%q", got.Prefix, got.Suffix)
		}
	})
	t.Run("prefix marker inside the prefix", func(t *testing.T) {
		got := Parse(P+`s := "`+P+`"; `+S+"R"+M, "")
		if got.Prefix != `s := "`+P+`"; ` || got.Suffix != "R" {
			t.Errorf("prefix=%q suffix=%q", got.Prefix, got.Suffix)
		}
	})
	t.Run("ambiguous: suffix marker inside the prefix never loses text", func(t *testing.T) {
		body := `s := "` + S + `"; f(` + S + `)`
		got := Parse(P+body+M, "")
		if got.Format != "qwen/codegemma" || got.Prefix+S+got.Suffix != body {
			t.Errorf("text lost or invented: prefix=%q suffix=%q", got.Prefix, got.Suffix)
		}
	})
}

// A prompt can be large (the body limit is 1 MiB). Parsing must not be quadratic.
func TestParseIsFastOnLargePrompts(t *testing.T) {
	big := strings.Repeat("x := 1 // padding <fim_prefix> [SUFFIX] <PRE>\n", 25000) // ~1 MiB of marker-like noise
	for _, f := range fimFormats {
		Parse(build(f, "", big, big), "")
	}
}

func BenchmarkParse(b *testing.B) {
	code := strings.Repeat("func f() { return }\n", 20000)
	prompt := build(fimFormats[2], "", code, code)
	b.SetBytes(int64(len(prompt)))
	for b.Loop() {
		Parse(prompt, "")
	}
}

// split is only called for a prompt that detectFormat accepted, but it must
// still fail cleanly rather than guess.
func TestSplitReportsFailure(t *testing.T) {
	for _, f := range fimFormats {
		for _, prompt := range []string{"", "no markers here", f.prefix, f.suffix, f.prefix + "only prefix", "only suffix" + f.suffix} {
			if f.middle == "" && strings.Contains(prompt, f.prefix) && strings.Contains(prompt, f.suffix) {
				continue
			}
			if p, ok := split(prompt, f); ok {
				t.Errorf("%s: split(%q) = %+v, want failure", f.name, prompt, p)
			}
		}
	}
	if _, _, _, ok := splitOrdered("a<x>b", "<y>", "<x>"); ok {
		t.Error("splitOrdered found a marker that is not there")
	}
	before, between, after, ok := splitOrdered("c<a>d<b>e<b>f", "<a>", "<b>")
	if !ok || before != "c" || between != "d" || after != "e<b>f" {
		t.Errorf("splitOrdered = %q %q %q %v", before, between, after, ok)
	}
}
