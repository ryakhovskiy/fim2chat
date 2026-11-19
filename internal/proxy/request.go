package proxy

import (
	"encoding/json"
	"errors"

	"ryakhovskiy/fim2chat/internal/fim"
)

type completionRequest struct {
	Model     string          `json:"model"`
	Prompt    json.RawMessage `json:"prompt"`
	Suffix    string          `json:"suffix,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Stop      json.RawMessage `json:"stop,omitempty"`
	Stream    bool            `json:"stream,omitempty"`
}

func firstString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("missing 'prompt'")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr[0], nil
	}
	return "", errors.New("'prompt' must be a string or an array of strings")
}

func stringList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	return nil
}

func (p *handler) chatBody(model string, parts fim.Prompt, maxTokens int, stops []string) ([]byte, error) {
	if maxTokens <= 0 {
		maxTokens = p.cfg.DefaultMaxTokens
	}

	chat := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": p.cfg.SystemPrompt},
			{"role": "user", "content": parts.UserMessage()},
		},
		"stream":             false,
		p.cfg.MaxTokensField: maxTokens,
	}
	if p.cfg.Temperature >= 0 {
		chat["temperature"] = p.cfg.Temperature
	}
	if p.cfg.SendStop && len(stops) > 0 {
		chat["stop"] = stops
	}
	for k, v := range p.cfg.ExtraBody {
		chat[k] = v
	}
	return json.Marshal(chat)
}
