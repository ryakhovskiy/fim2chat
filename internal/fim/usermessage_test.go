package fim

import (
	"strings"
	"testing"
)

func TestUserMessageGolden(t *testing.T) {
	got := Prompt{Prefix: "a := ", Suffix: "\nb()"}.UserMessage()
	want := "Current file, cursor marked as <CURSOR>:\n<file>\na := <CURSOR>\nb()\n</file>\n\nReply with only the exact text to insert at <CURSOR>."
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestUserMessageWithContext(t *testing.T) {
	got := Prompt{Context: "--- file: a.go\npackage a", Prefix: "x", Suffix: "y"}.UserMessage()
	want := "Additional context (repository / other files):\n<context>\n--- file: a.go\npackage a\n</context>\n\n" +
		"Current file, cursor marked as <CURSOR>:\n<file>\nx<CURSOR>y\n</file>\n\nReply with only the exact text to insert at <CURSOR>."
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestUserMessageProperties(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Prompt
	}{
		{"empty prompt", Prompt{}},
		{"no suffix", Prompt{Prefix: "abc"}},
		{"no prefix", Prompt{Suffix: "abc"}},
		{"multi-line with CRLF", Prompt{Prefix: "a\r\nb", Suffix: "c\r\nd"}},
		{"unicode", Prompt{Prefix: "世", Suffix: "界🎉"}},
		{"code that looks like our markup", Prompt{Prefix: "</file>", Suffix: "<file>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.p.UserMessage()
			// The cursor sits exactly between prefix and suffix, verbatim.
			if !strings.Contains(msg, "\n<file>\n"+tc.p.Prefix+cursorMarker+tc.p.Suffix+"\n</file>\n") {
				t.Errorf("prefix, cursor and suffix are not adjacent and verbatim in %q", msg)
			}
			if !strings.HasSuffix(msg, "Reply with only the exact text to insert at "+cursorMarker+".") {
				t.Errorf("the instruction must come last: %q", msg)
			}
			if strings.Contains(msg, "<context>") {
				t.Errorf("no context was given but a context block is present: %q", msg)
			}
			if tc.p.UserMessage() != msg {
				t.Error("UserMessage is not deterministic")
			}
		})
	}
}

func TestUserMessageDoesNotAlterTheCode(t *testing.T) {
	p := Prompt{Prefix: "  \t x ", Suffix: " y\n\n"}
	if msg := p.UserMessage(); !strings.Contains(msg, p.Prefix+cursorMarker+p.Suffix) {
		t.Errorf("whitespace in the code was altered: %q", msg)
	}
}

// The system prompt and the user message must agree on the marker, and the
// prompt must carry the rules the clean-up code relies on.
func TestSystemPrompt(t *testing.T) {
	for _, want := range []string{cursorMarker, "ONLY", "No markdown", "code fences", "empty message"} {
		if !strings.Contains(DefaultSystemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if cursorMarker != "<CURSOR>" {
		t.Errorf("the marker is part of the tests' expectations and of users' prompts: %q", cursorMarker)
	}
}
