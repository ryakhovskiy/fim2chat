package proxy_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"ryakhovskiy/fim2chat/internal/fim"
	"ryakhovskiy/fim2chat/internal/logging"
	"ryakhovskiy/fim2chat/internal/proxy"
	"ryakhovskiy/fim2chat/internal/upstream"
)

type editFixture struct{ name, prompt, region, rewritten string }

// Independent wire fixtures preserve the exact Zed layouts, including the
// newline forced on zeta2 regions and the one appended to sweep block bodies.
func editFixtures() []editFixture {
	return []editFixture{
		{"zeta", "### Instruction:\nYou are a code completion assistant and your task is to analyze user edits and then rewrite an excerpt that the user provides, suggesting the appropriate edits within the excerpt, taking into account the cursor location.\n\n### User Edits:\n\nUser edited old.go:\n```diff\n-old\n+recent\n```\n\n### User Excerpt:\n\n```target.go\n<|start_of_file|>\nBEFORE\n<|editable_region_start|>\nold<|user_cursor_is_here|> region\n<|editable_region_end|>\nAFTER\n```\n\n### Response:\n", "old region", "new region"},
		{"zeta2", "<[fim-suffix]>AFTER\n<[fim-prefix]><filename>related.go\nRELATED\n\n<filename>edit_history\n-old\n+recent\n\n<filename>target.go\nBEFORE\n<<<<<<< CURRENT\nold<|user_cursor|> region\n=======\n<[fim-middle]>", "old region\n", "new region\n"},
		{"sweep", "<|file_sep|>related.go\nRELATED\n<|file_sep|>target.go.diff\noriginal:\nold\nupdated:\nrecent\n<|file_sep|>original/target.go\noriginal window\n<|file_sep|>current/target.go\nold region\n<|file_sep|>updated/target.go\n", "old region", "new region"},
	}
}

func editJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func editUpstreamReply(t *testing.T, w http.ResponseWriter, text, finish string) {
	t.Helper()
	io.WriteString(w, editJSON(t, map[string]any{
		"id": "edit-id", "created": 123, "model": "upstream-model",
		"choices": []any{map[string]any{"message": map[string]string{"content": text}, "finish_reason": finish}},
		"usage":   map[string]int{"total_tokens": 19},
	}))
}

func editResponse(t *testing.T, w *httptest.ResponseRecorder, stream bool, want, finish string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body)
	}
	data := w.Body.String()
	if stream {
		if w.Header().Get("Content-Type") != "text/event-stream" || w.Header().Get("Cache-Control") != "no-cache" || strings.Count(data, "data: ") != 2 || !strings.HasSuffix(data, "\n\ndata: [DONE]\n\n") {
			t.Fatalf("bad SSE: headers=%v body=%q", w.Header(), data)
		}
		data = strings.TrimSuffix(strings.TrimPrefix(data, "data: "), "\n\ndata: [DONE]\n\n")
	} else if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("bad content type: %v", w.Header())
	}
	var resp struct {
		ID, Object, Model string
		Created           int
		Choices           []struct {
			Text     string
			Index    int
			Logprobs any
			Finish   string `json:"finish_reason"`
		}
		Usage map[string]int
	}
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "edit-id" || resp.Object != "text_completion" || resp.Model != "upstream-model" || resp.Created != 123 || len(resp.Choices) != 1 || resp.Usage["total_tokens"] != 19 {
		t.Fatalf("bad response envelope: %s", data)
	}
	choice := resp.Choices[0]
	if choice.Text != want || choice.Finish != finish || choice.Index != 0 || choice.Logprobs != nil {
		t.Fatalf("choice = %+v, want text=%q finish=%q", choice, want, finish)
	}
}

func TestEditCompletionRoundTrip(t *testing.T) {
	for _, fixture := range editFixtures() {
		for _, stream := range []bool{false, true} {
			for _, override := range []bool{false, true} {
				t.Run(fixture.name+"/stream="+map[bool]string{false: "no", true: "yes"}[stream]+"/override="+map[bool]string{false: "no", true: "yes"}[override], func(t *testing.T) {
					cfg := proxy.Config{Timeout: time.Second, DefaultMaxTokens: 999, Temperature: 0.25, SystemPrompt: "CUSTOM FIM SYSTEM MUST NOT APPLY", SendStop: override, ExtraBody: map[string]any{"reasoning_effort": "none"}}
					wantModel := "zed-model"
					field := "max_tokens"
					if override {
						cfg.Model = "configured-model"
						wantModel = cfg.Model
						field = "max_completion_tokens"
					}
					cfg.MaxTokensField = field
					ed, ok, err := fim.ParseEdit(fixture.prompt)
					if err != nil || !ok {
						t.Fatal("fixture did not parse", err)
					}
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							return
						}
						if r.Method != "POST" || body["model"] != wantModel || body[field] != float64(321) || body["stream"] != false || body["temperature"] != 0.25 || body["reasoning_effort"] != "none" {
							t.Errorf("wrong upstream fields: %+v", body)
						}
						other := "max_tokens"
						if field == other {
							other = "max_completion_tokens"
						}
						if _, present := body[other]; present {
							t.Errorf("both token fields sent: %+v", body)
						}
						wantMessages := []any{map[string]any{"role": "system", "content": ed.SystemPrompt()}, map[string]any{"role": "user", "content": ed.UserMessage()}}
						if !reflect.DeepEqual(body["messages"], wantMessages) {
							t.Errorf("wrong rewrite messages: %+v", body["messages"])
						}
						if override {
							if !reflect.DeepEqual(body["stop"], []any{"CLIENT_STOP"}) {
								t.Errorf("stop = %v", body["stop"])
							}
						} else if _, present := body["stop"]; present {
							t.Errorf("stop sent without SendStop: %v", body)
						}
						editUpstreamReply(t, w, "<think>plan</think>```go\nnew<CURSOR> region\n```", "stop")
					}))
					t.Cleanup(srv.Close)
					client, err := upstream.New(upstream.Config{URL: srv.URL})
					if err != nil {
						t.Fatal(err)
					}
					h := proxy.NewHandler(cfg, client)
					w := post(h, editJSON(t, map[string]any{"model": "zed-model", "prompt": []string{fixture.prompt, "ignored"}, "suffix": "EXPLICIT SUFFIX MUST BE IGNORED", "max_tokens": 321, "temperature": nil, "stop": []string{"CLIENT_STOP"}, "stream": stream}))
					editResponse(t, w, stream, fixture.rewritten, "stop")
				})
			}
		}
	}
}

func TestEditReplySafetyAndRaw(t *testing.T) {
	for _, fixture := range editFixtures() {
		for _, raw := range []bool{false, true} {
			for _, tc := range []struct {
				name, text, finish string
				fallback           bool
			}{
				{"truncated code", "partial code", "length", true},
				{"truncated blank", "", "length", true},
				{"empty", "", "stop", true},
				{"whitespace", " \t\r\n\u2003", "stop", true},
				{"thinking only", "<think>no answer</think>", "stop", !raw},
				{"local stop only", "STOPdiscard", "stop", !raw},
				{"raw wrapper", "```go\nnew<CURSOR> region\n```\nSTOPdiscard", "", false},
			} {
				t.Run(fixture.name+"/raw="+map[bool]string{false: "no", true: "yes"}[raw]+"/"+tc.name, func(t *testing.T) {
					logs := captureLogs(t, false)
					h := newProxy(t, func(w http.ResponseWriter, _ *http.Request) { editUpstreamReply(t, w, tc.text, tc.finish) }, proxy.Config{Raw: raw}, upstream.Config{})
					w := post(h, editJSON(t, map[string]any{"prompt": fixture.prompt, "stop": "STOP"}))
					want := fixture.rewritten
					if raw {
						want = tc.text
					}
					if tc.fallback {
						want = ""
						if fixture.name == "zeta" {
							want = fixture.region
						}
					}
					finish := tc.finish
					if finish == "" {
						finish = "stop"
					}
					editResponse(t, w, false, want, finish)
					if tc.fallback {
						reason := "unusable"
						if tc.finish == "length" {
							reason = "truncated"
						}
						found := false
						for _, line := range strings.Split(logs.String(), "\n") {
							if strings.Contains(line, `level=WARN`) && strings.Contains(line, `msg="edit reply replaced by no-change reply"`) && strings.Contains(line, "reason="+reason) {
								found = true
								break
							}
						}
						if !found {
							t.Errorf("fallback reason not logged: %s", logs.String())
						}
					} else if strings.Contains(logs.String(), "edit reply replaced") {
						t.Errorf("usable answer reported as fallback: %s", logs.String())
					}
				})
			}
		}
	}
}

func TestEditStopsAreAppliedLocallyBeyondUpstreamLimit(t *testing.T) {
	for _, fixture := range editFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			for _, send := range []bool{false, true} {
				h := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if send {
						if !reflect.DeepEqual(body["stop"], []any{"S1", "S2", "S3", "S4"}) {
							t.Errorf("stop = %v", body["stop"])
						}
					} else if _, ok := body["stop"]; ok {
						t.Error("stop sent without SendStop")
					}
					if body["max_tokens"] != float64(777) {
						t.Errorf("default max_tokens = %v", body["max_tokens"])
					}
					if _, ok := body["temperature"]; ok {
						t.Error("negative temperature sent")
					}
					editUpstreamReply(t, w, "new regionS6discard", "stop")
				}, proxy.Config{SendStop: send, DefaultMaxTokens: 777, Temperature: -1}, upstream.Config{})
				editResponse(t, post(h, editJSON(t, map[string]any{"prompt": fixture.prompt, "stop": []string{"S1", "S2", "S3", "S4", "S5", "S6"}})), false, fixture.rewritten, "stop")
			}
		})
	}
}

func TestUnsupportedEditReturns400WithoutUpstreamCall(t *testing.T) {
	for _, prompt := range []string{
		"<[fim-suffix]>PRIVATE_AFTER\n<[fim-prefix]><filename>private.go\n<|marker_0|>PRIVATE_CODE<|marker_1|>\n<[fim-middle]>",
		"### Instruction:\nYou are a code completion assistant\nPRIVATE_CODE\n\n### Response:\n",
	} {
		logs := captureLogs(t, false)
		h := newProxy(t, func(http.ResponseWriter, *http.Request) { t.Error("unsupported edit reached upstream") }, proxy.Config{}, upstream.Config{})
		w := post(h, editJSON(t, map[string]any{"prompt": prompt}))
		var resp struct {
			Error struct{ Message, Type string }
		}
		if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.Error.Type != "fim2chat_error" || resp.Error.Message != "this prompt_format is not supported by fim2chat (zeta2_1 and other marker formats); set prompt_format to zeta, zeta2, sweep or a FIM format" {
			t.Errorf("HTTP %d: %s", w.Code, w.Body)
		}
		if !strings.Contains(logs.String(), `reason="unsupported edit prompt"`) {
			t.Errorf("missing rejection reason: %s", logs.String())
		}
		for _, secret := range []string{"PRIVATE_CODE", "PRIVATE_AFTER", "private.go"} {
			if strings.Contains(w.Body.String()+logs.String(), secret) {
				t.Errorf("prompt text %q leaked", secret)
			}
		}
	}
}

func TestEditLogsMetadataWithoutCodeAboveDebug(t *testing.T) {
	for _, fixture := range editFixtures() {
		for _, level := range []slog.Level{slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
			t.Run(fixture.name+"/"+level.String(), func(t *testing.T) {
				old := slog.Default()
				t.Cleanup(func() { slog.SetDefault(old) })
				logs := &logBuffer{}
				if _, err := logging.Setup(logs, logging.FormatText, level); err != nil {
					t.Fatal(err)
				}
				h := newProxy(t, func(w http.ResponseWriter, _ *http.Request) { editUpstreamReply(t, w, "PRIVATE_MODEL_OUTPUT", "stop") }, proxy.Config{}, upstream.Config{})
				w := post(h, editJSON(t, map[string]any{"prompt": fixture.prompt}))
				if w.Code != 200 {
					t.Fatalf("HTTP %d", w.Code)
				}
				out := logs.String()
				for _, text := range []string{"old region", "BEFORE", "AFTER", "RELATED", "original window", "target.go", "PRIVATE_MODEL_OUTPUT", "User edited", "<filename>", "<|file_sep|>"} {
					if strings.Contains(out, text) {
						t.Errorf("source %q logged at %s: %s", text, level, out)
					}
				}
				if level == slog.LevelInfo {
					for _, event := range []string{"completion requested", "completion produced"} {
						found := false
						for _, line := range strings.Split(out, "\n") {
							if strings.Contains(line, `msg="`+event+`"`) {
								found = true
								for _, attr := range []string{"mode=rewrite", "format=" + fixture.name, "context_bytes=", "region_bytes="} {
									if !strings.Contains(line, attr) {
										t.Errorf("missing %s in %s", attr, line)
									}
								}
								if strings.Contains(line, "prefix_bytes=") || strings.Contains(line, "suffix_bytes=") {
									t.Errorf("insert metadata in rewrite: %s", line)
								}
							}
						}
						if !found {
							t.Errorf("event %s missing: %s", event, out)
						}
					}
				}
			})
		}
	}
}

func TestFIMUpstreamBodiesRemainByteIdentical(t *testing.T) {
	for _, tc := range []struct{ name, prompt string }{
		{"deepseek", "<｜fim▁begin｜>left<｜fim▁hole｜>right<｜fim▁end｜>"},
		{"deepseek ASCII", "<|fim▁begin|>left<|fim▁hole|>right<|fim▁end|>"},
		{"qwen", "<|fim_prefix|>left<|fim_suffix|>right<|fim_middle|>"},
		{"starcoder", "<fim_prefix>left<fim_suffix>right<fim_middle>"},
		{"codellama", "<PRE> left <SUF>right <MID>"},
		{"codestral", "[SUFFIX]right[PREFIX]left"},
		{"glm", "<|code_prefix|>left<|code_suffix|>right<|code_middle|>"},
	} {
		for _, rich := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "", true: " with context"}[rich], func(t *testing.T) {
				prompt, user := tc.prompt, "Current file, cursor marked as <CURSOR>:\n<file>\nleft<CURSOR>right\n</file>\n\nReply with only the exact text to insert at <CURSOR>."
				if rich {
					prompt = "<|repo_name|>repo<|file_sep|>related.go\npackage related\n" + prompt
					user = "Additional context (repository / other files):\n<context>\nRepository: repo\n--- file: related.go\npackage related\n</context>\n\n" + user
				}
				// Captures the pre-refactor chatBody's exact serialization contract.
				want := `{"max_tokens":23,"messages":[{"content":"custom FIM system","role":"system"},{"content":` + editJSON(t, user) + `,"role":"user"}],"model":"zed-model","reasoning_effort":"none","stop":["STOP"],"stream":false,"temperature":0.25}`
				h := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					if string(body) != want {
						t.Errorf("FIM body changed\ngot  %s\nwant %s", body, want)
					}
					editUpstreamReply(t, w, " + ", "stop")
				}, proxy.Config{SystemPrompt: "custom FIM system", SendStop: true, Temperature: 0.25, ExtraBody: map[string]any{"reasoning_effort": "none"}}, upstream.Config{})
				editResponse(t, post(h, editJSON(t, map[string]any{"prompt": prompt, "model": "zed-model", "max_tokens": 23, "stop": "STOP"})), false, " + ", "stop")
			})
		}
	}
}

func TestFIMContainingZetaTextStillUsesInsertion(t *testing.T) {
	for _, fixture := range editFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			logs := captureLogs(t, false)
			prefix := "literal := `" + fixture.prompt + "`\nx := "
			h := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body.Messages) != 2 || body.Messages[0].Content != "FIM-only system" || !strings.Contains(body.Messages[1].Content, prefix+"<CURSOR>\n") {
					t.Errorf("FIM hijacked: %+v", body)
				}
				editUpstreamReply(t, w, "x := 42", "stop")
			}, proxy.Config{SystemPrompt: "FIM-only system"}, upstream.Config{})
			editResponse(t, post(h, editJSON(t, map[string]any{"prompt": "<fim_prefix>" + prefix + "<fim_suffix>\n<fim_middle>"})), false, "42", "stop")
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, `msg="completion requested"`) || strings.Contains(line, `msg="completion produced"`) {
					if !strings.Contains(line, "mode=insert") {
						t.Errorf("missing insert mode: %s", line)
					}
				}
			}
		})
	}
}

func TestZetaExcerptHeadingInsideCodeStillRewrites(t *testing.T) {
	fixture := editFixtures()[0]
	const code = "literal := `\n\n### User Excerpt:\n\n`"
	fixture.prompt = strings.Replace(fixture.prompt, "old<|user_cursor_is_here|> region", code+"<|user_cursor_is_here|>", 1)
	h := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.Messages) != 2 || !strings.Contains(req.Messages[1].Content, code+"<CURSOR>") {
			t.Errorf("code heading was lost: %+v", req)
		}
		editUpstreamReply(t, w, "updated", "stop")
	}, proxy.Config{}, upstream.Config{})
	editResponse(t, post(h, editJSON(t, map[string]any{"prompt": fixture.prompt})), false, "updated", "stop")
}
