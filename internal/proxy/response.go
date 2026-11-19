package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ryakhovskiy/fim2chat/internal/upstream"
)

func writeCompletion(w http.ResponseWriter, stream bool, cr *upstream.Response, model, text, finish string) {
	id := cr.ID
	if id == "" {
		id = fmt.Sprintf("cmpl-%d", time.Now().UnixNano())
	}
	created := cr.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	respModel := cr.Model
	if respModel == "" {
		respModel = model
	}
	resp := map[string]any{
		"id":      id,
		"object":  "text_completion",
		"created": created,
		"model":   respModel,
		"choices": []map[string]any{{
			"text":          text,
			"index":         0,
			"logprobs":      nil,
			"finish_reason": finish,
		}},
	}
	if len(cr.Usage) > 0 && string(cr.Usage) != "null" {
		resp["usage"] = cr.Usage
	}

	if stream {
		// The upstream call is not streamed; emit the whole answer as one SSE chunk.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		b, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "fim2chat_error"},
	})
}

// writeUpstreamError relays an upstream failure. A JSON body is passed through
// unchanged; anything else (an HTML gateway page, plain text) is wrapped so the
// editor always gets JSON. Non-error statuses cannot be relayed as errors.
func writeUpstreamError(w http.ResponseWriter, ue *upstream.StatusError) {
	status := ue.Status
	if status < 400 {
		status = http.StatusBadGateway
	}
	if ue.Location == "" && json.Valid(ue.Body) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(ue.Body)
		return
	}
	writeError(w, status, ue.Error())
}
