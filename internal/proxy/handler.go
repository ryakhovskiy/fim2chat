package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"ryakhovskiy/fim2chat/internal/fim"
	"ryakhovskiy/fim2chat/internal/logging"
	"ryakhovskiy/fim2chat/internal/upstream"
)

// DefaultMaxBodyBytes caps the size of a request body from the editor.
const DefaultMaxBodyBytes = 1 << 20

// statusClientClosedRequest is the de-facto (nginx) status for a client that hung up.
const statusClientClosedRequest = 499

type Config struct {
	// MaxBodyBytes limits the editor's request body; 0 means DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// AuthToken, if set, must be presented as "Authorization: Bearer <token>".
	AuthToken string
	// AllowedHosts are host names accepted in the Host header in addition to
	// loopback names and IP addresses; AnyHost ("*") disables the check.
	AllowedHosts     []string
	Model            string
	MaxTokensField   string
	DefaultMaxTokens int
	Temperature      float64
	SendStop         bool
	Timeout          time.Duration
	Raw              bool
	SystemPrompt     string
	ExtraBody        map[string]any
}

type handler struct {
	cfg    Config
	client *upstream.Client
}

func NewHandler(cfg Config, client *upstream.Client) http.Handler {
	p := &handler{cfg: cfg, client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/completions", p.handleCompletions)
	mux.HandleFunc("/completions", p.handleCompletions)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logging.From(r.Context()).Warn("unhandled request", "method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusNotFound, "fim2chat only serves /v1/completions")
	})
	// Outermost: log every request received and response sent, with a request
	// ID, including the ones the guard turns away.
	return logging.Middleware(newGuard(cfg).wrap(mux))
}

// reject answers a bad request and logs why.
func reject(w http.ResponseWriter, logger *slog.Logger, status int, reason, message string, attrs ...any) {
	logger.Warn("request rejected", append([]any{"status", status, "reason", reason}, attrs...)...)
	writeError(w, status, message)
}

func (p *handler) handleCompletions(w http.ResponseWriter, r *http.Request) {
	logger := logging.From(r.Context())
	if r.Method != http.MethodPost {
		reject(w, logger, http.StatusMethodNotAllowed, "method not allowed", "only POST is supported", "method", r.Method)
		return
	}
	start := time.Now()

	limit := p.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			// A truncated body is not valid JSON, so the request is rejected
			// rather than parsed.
			logger.Warn("request body truncated: larger than the byte limit, rejecting request",
				"limit_bytes", tooLarge.Limit, "setting", "-max-body-bytes / FIM_MAX_BODY_BYTES")
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
			return
		}
		reject(w, logger, http.StatusBadRequest, "cannot read body", "cannot read body: "+err.Error(), "error", err.Error())
		return
	}
	logger.Debug("request body read", "bytes", len(body), "body", logging.Truncate(string(body), 20000))

	var req completionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		reject(w, logger, http.StatusBadRequest, "invalid JSON", "invalid JSON: "+err.Error(), "error", err.Error())
		return
	}
	prompt, err := firstString(req.Prompt)
	if err != nil {
		reject(w, logger, http.StatusBadRequest, "invalid prompt", err.Error(), "error", err.Error())
		return
	}
	stops := stringList(req.Stop)
	parts := fim.Parse(prompt, req.Suffix)

	model := p.cfg.Model
	modelSource := "config"
	if model == "" {
		model, modelSource = req.Model, "client"
	}
	// Upstreams accept at most 4 stop sequences. The limit applies only to what
	// is sent: every stop sequence is still applied locally by Clean.
	upstreamStops := stops
	if p.cfg.SendStop && len(upstreamStops) > 4 {
		logger.Debug("too many stop sequences for upstream, sending the first 4", "stops", len(upstreamStops))
		upstreamStops = upstreamStops[:4]
	}
	logger.Info("completion requested",
		"client_model", req.Model, "model", model, "model_source", modelSource,
		"client_max_tokens", req.MaxTokens, "stream", req.Stream, "stop_sequences", len(stops),
		"format", parts.Format, "context_bytes", len(parts.Context),
		"prefix_bytes", len(parts.Prefix), "suffix_bytes", len(parts.Suffix))

	chatBody, err := p.chatBody(model, parts, req.MaxTokens, upstreamStops)
	if err != nil {
		logger.Error("cannot build upstream request", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "cannot build upstream request: "+err.Error())
		return
	}
	logger.Debug("upstream request built", "bytes", len(chatBody), "body", logging.Truncate(string(chatBody), 20000))

	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.Timeout)
	defer cancel()

	cr, err := p.client.Complete(ctx, chatBody, r.Header.Get("Authorization"))
	if err != nil {
		var ue *upstream.StatusError
		switch {
		case errors.Is(err, context.Canceled):
			logger.Info("request cancelled by client", "after", time.Since(start).Round(time.Millisecond))
			writeError(w, statusClientClosedRequest, "client closed request")
		case errors.Is(err, context.DeadlineExceeded):
			logger.Error("upstream did not respond in time", "timeout", p.cfg.Timeout, "after", time.Since(start).Round(time.Millisecond))
			writeError(w, http.StatusGatewayTimeout, "upstream did not respond within "+p.cfg.Timeout.String())
		case errors.As(err, &ue):
			attrs := []any{"status_code", ue.Status, "body", logging.Truncate(string(ue.Body), 500)}
			if ue.Location != "" {
				attrs = append(attrs, "redirect_location", ue.Location, "redirect_followed", false)
			}
			logger.Error("upstream returned an error status", attrs...)
			writeUpstreamError(w, ue)
		default:
			logger.Error("upstream request failed", "error", err.Error())
			writeError(w, http.StatusBadGateway, err.Error())
		}
		return
	}

	rawText := cr.Text
	text := rawText
	if !p.cfg.Raw {
		text = parts.Clean(rawText, stops)
	}
	finish := cr.FinishReason
	if finish == "" {
		finish = "stop"
	}

	logger.Debug("model output cleaned", "raw", rawText, "cleaned", text, "raw_cleanup_disabled", p.cfg.Raw)
	attrs := []any{
		"format", parts.Format, "finish_reason", finish,
		"raw_bytes", len(rawText), "completion_bytes", len(text),
		"duration", time.Since(start).Round(time.Millisecond),
	}
	if len(cr.Usage) > 0 && string(cr.Usage) != "null" {
		attrs = append(attrs, "usage", string(cr.Usage))
	}
	logger.Info("completion produced", attrs...)

	writeCompletion(w, req.Stream, cr, model, text, finish)
}
