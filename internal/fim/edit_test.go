package fim

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// These builders mirror zeta1::format_zeta1_prompt/format_zeta1_excerpt,
// seed_coder::assemble_fim_prompt, and sweep's write_file_block in Zed.
// Keep the wire delimiters here independent of the production parser.
const zedZetaHeader = "### Instruction:\nYou are a code completion assistant and your task is to analyze user edits and then rewrite an excerpt that the user provides, suggesting the appropriate edits within the excerpt, taking into account the cursor location.\n\n### User Edits:\n\n"

func zedZeta(path, events, before, left, right, after string, startOfFile bool) string {
	start := ""
	if startOfFile {
		start = "<|start_of_file|>\n"
	}
	return zedZetaHeader + events + "\n\n### User Excerpt:\n\n```" + path + "\n" + start + before + "<|editable_region_start|>\n" + left + "<|user_cursor_is_here|>" + right + "\n<|editable_region_end|>" + after + "\n```\n\n### Response:\n"
}

func zedLine(s string) string {
	if !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}

func zedZeta2(path, context, before, left, right, after string) string {
	if context != "" && !strings.HasSuffix(context, "\n\n") {
		context = zedLine(context) + "\n"
	}
	return "<[fim-suffix]>" + zedLine(after) + "<[fim-prefix]>" + context + "<filename>" + path + "\n" + before + "<<<<<<< CURRENT\n" + zedLine(left+"<|user_cursor|>"+right) + "=======\n<[fim-middle]>"
}

func zedSweep(path, context, original, current string) string {
	return context + "<|file_sep|>original/" + path + "\n" + zedLine(original) + "<|file_sep|>current/" + path + "\n" + zedLine(current) + "<|file_sep|>updated/" + path + "\n"
}

func requireEdit(t *testing.T, prompt string) Edit {
	t.Helper()
	e, ok, err := ParseEdit(prompt)
	if err != nil || !ok {
		t.Fatalf("ParseEdit(%q): ok=%v err=%v", prompt, ok, err)
	}
	return e
}

func TestParseEditZedLayouts(t *testing.T) {
	for _, tc := range []struct{ name, left, right, before, after string }{
		{"middle", "alpha", "omega", "package p\n", "\n// after"},
		{"empty left", "", "omega", "", ""},
		{"empty right", "alpha", "", "", ""},
		{"empty region", "", "", "", ""},
		{"LF region", "a\n", "b\n\n", "before\n", "\nafter\n"},
		{"CRLF region", "a\r\n", "b\r\n", "before\r\n", "\r\nafter\r\n"},
		{"multi-byte", "世界🎉", "éλ", "// начало\n", "\n// 끝"},
		{"FIM tokens in code", `s := "<|fim_prefix|>`, `<|fim_suffix|>"`, "", ""},
		{"marker-like text in code", `s := "<|editable_region_start|>"`, ` + "<<<<<<<"`, "", ""},
	} {
		for _, rich := range []bool{false, true} {
			for _, format := range []EditFormat{EditZeta, EditZeta2, EditSweep} {
				t.Run(string(format)+"/"+tc.name+"/context="+map[bool]string{false: "no", true: "yes"}[rich], func(t *testing.T) {
					var prompt, want string
					switch format {
					case EditZeta:
						events := ""
						if rich {
							events = "User edited old.go:\n```diff\n-old\n+new\n```\n\nUser edited next.go:\n```diff\n-x\n+y\n```"
						}
						prompt = zedZeta("src/目标.go", events, tc.before, tc.left, tc.right, tc.after, rich)
						want = tc.left + tc.right
					case EditZeta2:
						context := ""
						if rich {
							context = "<filename>related.go\npackage related\n\n<filename>edit_history\n-old\n+new\n\n"
						}
						prompt = zedZeta2("src/目标.go", context, tc.before, tc.left, tc.right, tc.after)
						// Zed forces a newline after the region including its cursor.
						want = strings.ReplaceAll(zedLine(tc.left+"<|user_cursor|>"+tc.right), "<|user_cursor|>", "")
					case EditSweep:
						context := ""
						if rich {
							context = "<|file_sep|>related.go\npackage related\n<|file_sep|>src/目标.go.diff\noriginal:\nold\nupdated:\nnew\n"
						}
						prompt = zedSweep("src/目标.go", context, "original", tc.left+tc.right)
						want = strings.TrimSuffix(zedLine(tc.left+tc.right), "\n")
					}
					e := requireEdit(t, prompt)
					if e.Format != format || e.Region != want {
						t.Fatalf("got format=%q region=%q, want %q %q", e.Format, e.Region, format, want)
					}
					if format != EditSweep && !strings.Contains(e.UserMessage(), tc.left+"<CURSOR>"+tc.right) {
						t.Errorf("cursor lost its byte position: %q", e.UserMessage())
					}
				})
			}
		}
	}
}

func TestParseEditRejectsUnsupportedAndUnanchored(t *testing.T) {
	zeta := zedZeta("a.go", "", "", "left", "right", "", false)
	zeta2 := zedZeta2("a.go", "", "", "left", "right", "")
	sweep := zedSweep("a.go", "", "old", "current")
	for _, tc := range []struct {
		name, prompt string
		unsupported  bool
	}{
		{"plain", "plain <|editable_region_start|>code<|editable_region_end|>", false},
		{"empty", "", false},
		{"zeta missing excerpt", zedZetaHeader + "\n\n### Response:\n", true},
		{"zeta missing start", strings.Replace(zeta, "<|editable_region_start|>\n", "", 1), true},
		{"zeta missing end", strings.Replace(zeta, "\n<|editable_region_end|>", "", 1), true},
		{"zeta missing leading anchor", " " + zeta, false},
		{"zeta missing trailing anchor", zeta + " ", false},
		{"zeta2_1", "<[fim-suffix]>after\n<[fim-prefix]><filename>a.go\n<|marker_0|>code<|marker_1|>\n<[fim-middle]>", true},
		{"zeta2 missing current", strings.Replace(zeta2, "<<<<<<< CURRENT\n", "", 1), true},
		{"zeta2 missing filename", strings.Replace(zeta2, "<filename>", "", 1), true},
		{"zeta2 missing separator", strings.Replace(zeta2, "=======\n", "", 1), true},
		{"zeta2 separator not terminal", strings.Replace(zeta2, "=======\n", "=======\nextra\n", 1), true},
		{"zeta2 missing leading anchor", " " + zeta2, false},
		{"zeta2 missing trailing anchor", zeta2 + "\n", false},
		{"sweep mismatched paths", strings.Replace(sweep, "current/a.go", "current/b.go", 1), false},
		{"sweep wrong order", strings.Replace(sweep, "original/", "current/", 1), false},
		{"sweep updated has body", sweep + "code\n", false},
		{"sweep missing terminal newline", strings.TrimSuffix(sweep, "\n"), false},
		{"sweep only two blocks", "<|file_sep|>current/a\nx\n<|file_sep|>updated/a\n", false},
		{"sweep missing leading anchor", " " + sweep, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ok, err := ParseEdit(tc.prompt)
			if ok || !reflect.DeepEqual(e, Edit{}) || errors.Is(err, ErrUnsupportedEdit) != tc.unsupported || (!tc.unsupported && err != nil) {
				t.Errorf("got %+v, %v, %v; want zero, false, unsupported=%v", e, ok, err, tc.unsupported)
			}
		})
	}
}

func TestParseEditDoesNotHijackFIM(t *testing.T) {
	for _, format := range fimFormats {
		for _, context := range []string{"", "<|repo_name|>repo<|file_sep|>a.go\n"} {
			for _, code := range []string{"code", zedZeta("a", "", "", "x", "y", "", false), zedZeta2("a", "", "", "x", "y", ""), zedSweep("a", "", "old", "new")} {
				for _, sides := range [][2]string{{code, "right"}, {"left", code}} {
					if e, ok, err := ParseEdit(build(format, context, sides[0], sides[1])); ok || err != nil || !reflect.DeepEqual(e, Edit{}) {
						t.Errorf("%s context=%q: FIM was hijacked: %+v %v %v", format.name, context, e, ok, err)
					}
				}
			}
		}
	}
}

func TestParseEditUsesTargetAfterHistory(t *testing.T) {
	t.Run("zeta last excerpt", func(t *testing.T) {
		// A real start marker in history is excluded by Zed and is a
		// documented parser limit. A copied excerpt heading alone is legal.
		history := "old\n\n### User Excerpt:\n\n```old.go\nwrong\n```"
		e := requireEdit(t, zedZeta("target.go", history, "", "correct", "", "", false))
		if e.Region != "correct" {
			t.Fatalf("history selected as region: %q", e.Region)
		}
		if !strings.Contains(e.UserMessage(), history) {
			t.Errorf("recent edits were split at a quoted heading: %q", e.UserMessage())
		}
	})
	t.Run("zeta2 last current", func(t *testing.T) {
		history := "<filename>old.go\n<<<<<<< CURRENT\nwrong\n=======\n\n"
		e := requireEdit(t, zedZeta2("target.go", history, "", "correct", "", ""))
		if e.Region != "correct\n" || !strings.Contains(e.UserMessage(), "target.go") {
			t.Fatalf("history selected as target: %+v", e)
		}
	})
}

func TestParseEditZetaIgnoresExcerptHeadingsAfterRegionStart(t *testing.T) {
	const heading = "\n\n### User Excerpt:\n\n"
	for _, tc := range []struct{ name, left, right, after string }{
		{"heading before cursor", "literal := `" + heading + "`\n", "tail", ""},
		{"heading after cursor", "head", "\nliteral := `" + heading + "`", ""},
		{"heading in following context", "head", "tail", "\nliteral := `" + heading + "`"},
		{"multiple code headings", heading + "first", heading + "second", heading + "third"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := requireEdit(t, zedZeta("target.go", "", "", tc.left, tc.right, tc.after, false))
			if e.Region != tc.left+tc.right {
				t.Errorf("lost region: got %q want %q", e.Region, tc.left+tc.right)
			}
			if !strings.Contains(e.UserMessage(), tc.left+"<CURSOR>"+tc.right) || !strings.Contains(e.UserMessage(), tc.after) {
				t.Errorf("lost quoted heading or cursor position: %q", e.UserMessage())
			}
		})
	}
}

func TestEditMessagesContainContextWithoutWireWrappers(t *testing.T) {
	for _, tc := range []struct {
		name, prompt string
		pieces       []string
	}{
		{"zeta", zedZeta("target.go", "User edited old.go:\n```diff\n-old\n+new\n```", "BEFORE\n", "LEFT字", "RIGHT🎉", "\nAFTER", true), []string{"target.go", "User edited old.go:", "-old\n+new", "BEFORE\n", "LEFT字<CURSOR>RIGHT🎉", "\nAFTER"}},
		{"zeta2", zedZeta2("target.go", "<filename>related.go\nRELATED\n\n<filename>edit_history\n-old\n+new\n\n", "BEFORE\n", "LEFT字", "RIGHT🎉", "\nAFTER"), []string{"target.go", "--- file: related.go", "RELATED", "--- file: edit_history", "-old\n+new", "BEFORE\n", "LEFT字<CURSOR>RIGHT🎉", "\nAFTER\n"}},
		{"sweep", zedSweep("target.go", "<|file_sep|>related.go\nRELATED\n<|file_sep|>target.go.diff\noriginal:\n-old\nupdated:\n+new\n", "ORIGINAL\r\n字", "CURRENT\r\n🎉"), []string{"target.go", "related.go", "RELATED", "target.go.diff", "-old", "+new", "ORIGINAL\r\n字", "CURRENT\r\n🎉"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := requireEdit(t, tc.prompt)
			msg := e.UserMessage()
			for _, piece := range tc.pieces {
				if !strings.Contains(msg, piece) {
					t.Errorf("message lacks %q: %q", piece, msg)
				}
			}
			for _, token := range []string{"### ", "<|", "<[", "<filename>", "<<<<<<<", "======="} {
				if strings.Contains(msg, token) {
					t.Errorf("wire wrapper %q leaked: %q", token, msg)
				}
			}
			if e.Format == EditSweep && strings.Contains(msg, "<CURSOR>") {
				t.Errorf("sweep invented a cursor: %q", msg)
			}
			// Wording is deliberately free; assert the instructions' essential concepts.
			sys := strings.ToLower(e.SystemPrompt())
			for _, alternatives := range [][]string{{"code-editing", "code editing"}, {"only"}, {"full", "complete", "entire"}, {"verbatim"}, {"unchanged"}, {"abbreviat", "elid"}, {"markdown"}, {"fence"}, {"explanation"}, {"marker"}, {"<cursor>"}} {
				found := false
				for _, word := range alternatives {
					found = found || strings.Contains(sys, word)
				}
				if !found {
					t.Errorf("system prompt lacks concept %v: %q", alternatives, sys)
				}
			}
			if e.Format == EditSweep {
				for _, word := range []string{"original", "current", "predict", "window"} {
					if !strings.Contains(sys, word) {
						t.Errorf("sweep system lacks %q", word)
					}
				}
			} else if !strings.Contains(sys, "region") {
				t.Errorf("system does not request a region: %q", sys)
			}
		})
	}
}

func TestEditUserCodeIsQuotedVerbatim(t *testing.T) {
	const code = `literal := "<|fim_prefix|><|fim_suffix|><CURSOR>"`
	for _, prompt := range []string{zedZeta("a", "", "", code, "", "", false), zedZeta2("a", "", "", code, "", ""), zedSweep("a", "", code, code)} {
		e := requireEdit(t, prompt)
		if !strings.Contains(e.UserMessage(), code) {
			t.Errorf("%s rewrote user code: %q", e.Format, e.UserMessage())
		}
	}
}
