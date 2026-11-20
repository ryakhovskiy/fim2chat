package fim

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEditCleanCommonPipeline(t *testing.T) {
	for _, format := range []EditFormat{EditZeta, EditZeta2, EditSweep} {
		for _, tc := range []struct {
			name, region, model, want string
			stops                     []string
			usable                    bool
		}{
			{"plain full rewrite", "old", "  new code  ", "  new code  ", nil, true},
			{"unchanged full region is usable", "same", "same", "same", nil, true},
			{"never trim prefix echo", "package p\nreturn old", "package p\nreturn new", "package p\nreturn new", nil, true},
			{"never trim closing code overlap", "f(a)", "f(b)", "f(b)", nil, true},
			{"thinking before fence", "old", " \n<think>private</think>\n```go\nnew\n```\nExplanation", "new", nil, true},
			{"multiple thinking blocks", "old", "<think>one</think><think>two</think>new", "new", nil, true},
			{"thinking tags inside code survive", "old", `s := "<think>code</think>"`, `s := "<think>code</think>"`, nil, true},
			{"CRLF fenced code", "old\r\n", "```go\r\nx\r\ny\r\n```", "x\r\ny\r\n", nil, true},
			{"long fence with inner fence", "old", "````text\nx\n```\ny\n````", "x\n```\ny", nil, true},
			{"cursor markers removed", "old", "a<CURSOR>b<|user_cursor|>c<|user_cursor_is_here|>d", "abcd", nil, true},
			{"nested generic cursor", "old", "a<CUR<CURSOR>SOR>b", "ab", nil, true},
			{"nested zeta2 cursor", "old", "a<|user_<|user_cursor|>cursor|>b", "ab", nil, true},
			{"nested zeta cursor", "old", "a<|user_cursor_<|user_cursor_is_here|>is_here|>b", "ab", nil, true},
			{"cross marker nesting", "old", "a<|user_<|user_cursor_is_here|>cursor|>b", "ab", nil, true},
			{"stop after marker removal", "old", "aST<CURSOR>OPdiscard", "a", []string{"STOP"}, true},
			{"earliest requested stop", "old", "keep SECOND trash FIRST", "keep ", []string{"FIRST", "", "SECOND"}, true},
			{"multi-byte stop", "old", "世界🎉éDROP", "世界🎉", []string{"é"}, true},
			{"strip added blank lines", "old", "new\n\n", "new", nil, true},
			{"append region newline", "old\n", "new", "new\n", nil, true},
			{"preserve two region newlines", "old\n\n", "new\r\n\n", "new\n\n", nil, true},
			{"preserve CRLF region", "old\r\n\r\n", "new\n", "new\r\n\r\n", nil, true},
			{"preserve mixed trailing run", "old\r\n\n\r", "new\r\r\n", "new\r\n\n\r", nil, true},
			{"interior CRLF unchanged", "old", "a\r\nb\r\n", "a\r\nb", nil, true},
			{"newline stop recreated by normalization", "old\n", "new\ndiscard", "new\n", []string{"\n"}, true},
			{"empty answer", "old", "", "", nil, false},
			{"whitespace answer", "old", " \t\r\n", "", nil, false},
			{"unicode whitespace answer", "old", "\u2003\u3000", "", nil, false},
			{"unclosed thinking", "old", "<think>unfinished", "", nil, false},
			{"thinking only", "old", "<think>done</think>", "", nil, false},
			{"empty fence", "old", "```go\n```", "", nil, false},
			{"cursor only", "old", "<CURSOR><|user_cursor|><|user_cursor_is_here|>", "", nil, false},
			{"stop at start", "old", "STOPnew", "", []string{"STOP"}, false},
			{"empty original with usable rewrite", "", "new\n", "new", nil, true},
		} {
			t.Run(string(format)+"/"+tc.name, func(t *testing.T) {
				e := Edit{Format: format, Region: tc.region}
				want := tc.want
				if !tc.usable && format == EditZeta {
					want = tc.region
				}
				got, usable := e.Clean(tc.model, tc.stops)
				if got != want || usable != tc.usable {
					t.Fatalf("Clean(%q, %q) = %q, %v; want %q, %v", tc.model, tc.stops, got, usable, want, tc.usable)
				}
				if utf8.ValidString(tc.model) && !utf8.ValidString(got) {
					t.Error("split a UTF-8 character")
				}
			})
		}
	}
}

func TestEditCleanFormatWrappers(t *testing.T) {
	for _, tc := range []struct {
		name        string
		format      EditFormat
		model, want string
		usable      bool
	}{
		{"zeta full wrapper", EditZeta, "preamble<|editable_region_start|>\nnew\n<|editable_region_end|>discard", "new", true},
		{"zeta start without newline", EditZeta, "before<|editable_region_start|>new", "new", true},
		{"zeta end only", EditZeta, "new\n<|editable_region_end|>discard", "new", true},
		{"zeta end without newline", EditZeta, "new<|editable_region_end|>discard", "new", true},
		{"zeta wrapper after thinking fence", EditZeta, "<think>x</think>```\n<|editable_region_start|>\nnew\n<|editable_region_end|>\n```", "new", true},
		{"zeta empty wrapper", EditZeta, "<|editable_region_start|>\n\n<|editable_region_end|>", "old", false},
		{"zeta2 current and updated", EditZeta2, "<<<<<<< CURRENT\nnew\n>>>>>>> UPDATED\ndiscard", "new", true},
		{"zeta2 updated anywhere", EditZeta2, "new>>>>>>> UPDATEDdiscard>>>>>>> UPDATED", "new", true},
		{"zeta2 current only when leading", EditZeta2, "x<<<<<<< CURRENT\nnew", "x<<<<<<< CURRENT\nnew", true},
		{"zeta2 empty wrapper", EditZeta2, "<<<<<<< CURRENT\n>>>>>>> UPDATED\n", "", false},
		{"sweep file separator", EditSweep, "new<|file_sep|>other\ncode", "new", true},
		{"sweep EOS", EditSweep, "new</s>discard", "new", true},
		{"sweep earliest builtin", EditSweep, "new</s>x<|file_sep|>y", "new", true},
		{"sweep separator only", EditSweep, "<|file_sep|>discard", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Edit{Format: tc.format, Region: "old"}
			if got, usable := e.Clean(tc.model, nil); got != tc.want || usable != tc.usable {
				t.Errorf("got %q %v, want %q %v", got, usable, tc.want, tc.usable)
			}
		})
	}
}

func TestEditCleanCursorOwnership(t *testing.T) {
	for _, tc := range []struct{ name, prompt string }{
		{"zeta region", zedZeta("a", "", "", "literal <CURSOR>", "", "", false)},
		{"zeta before", zedZeta("a", "", "literal <CURSOR>\n", "old", "", "", false)},
		{"zeta after", zedZeta("a", "", "", "old", "", "\nliteral <CURSOR>", false)},
		{"zeta events", zedZeta("a", "User edited a:\n```diff\n+<CURSOR>\n```", "", "old", "", "", false)},
		{"zeta2 region", zedZeta2("a", "", "", "literal <CURSOR>", "", "")},
		{"zeta2 related", zedZeta2("a", "<filename>b\nliteral <CURSOR>\n\n", "", "old", "", "")},
		{"zeta2 before", zedZeta2("a", "", "literal <CURSOR>\n", "old", "", "")},
		{"zeta2 after", zedZeta2("a", "", "", "old", "", "literal <CURSOR>")},
		{"sweep original", zedSweep("a", "", "literal <CURSOR>", "old")},
		{"sweep current", zedSweep("a", "", "old", "literal <CURSOR>")},
		{"sweep related", zedSweep("a", "<|file_sep|>b\nliteral <CURSOR>\n", "old", "current")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := requireEdit(t, tc.prompt)
			out, usable := e.Clean("new <CURSOR><|user_cursor|><|user_cursor_is_here|>", nil)
			if !usable || strings.TrimRight(out, "\r\n") != "new <CURSOR>" {
				t.Errorf("user-owned marker removed or Zed marker leaked: %q %v", out, usable)
			}
		})
	}
}

func TestEditNoChange(t *testing.T) {
	for _, format := range []EditFormat{EditZeta, EditZeta2, EditSweep} {
		for _, region := range []string{"", "code", "世界🎉\r\n\r\n", "<CURSOR> STOP <|user_cursor|>"} {
			e := Edit{Format: format, Region: region}
			want := ""
			if format == EditZeta {
				want = region
			}
			if got := e.NoChange(); got != want {
				t.Errorf("%s NoChange(%q) = %q, want %q", format, region, got, want)
			}
			if got, usable := e.Clean(" \r\n", []string{"STOP"}); got != want || usable {
				t.Errorf("%s unusable fallback = %q %v, want %q false", format, got, usable, want)
			}
		}
	}
}
