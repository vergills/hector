package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	APIKey           string
	Model            string
	SystemPrompt     string
	MaxResponseChars int
	MaxOutputTokens  int
	HTTP             *http.Client
}

type requestPayload struct {
	Model             string `json:"model"`
	Input             string `json:"input"`
	SystemInstruction string `json:"system_instruction,omitempty"`
	GenerationConfig  struct {
		MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	} `json:"generation_config,omitempty"`
	Store bool `json:"store"`
}

type responsePayload struct {
	Steps []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"steps"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func NewClient(apiKey, model, systemPrompt string, maxResponseChars, maxOutputTokens int) *Client {
	if model == "" {
		model = "gemini-3.5-flash-lite"
	}
	if systemPrompt == "" {
		systemPrompt = "You are a helpful assistant in Discord. Keep responses brief, clear, and useful. Do not exceed the configured character budget."
	}
	if maxResponseChars <= 0 {
		maxResponseChars = 700
	}
	if maxOutputTokens <= 0 {
		maxOutputTokens = 512
	}
	return &Client{
		APIKey:           apiKey,
		Model:            model,
		SystemPrompt:     systemPrompt,
		MaxResponseChars: maxResponseChars,
		MaxOutputTokens:  maxOutputTokens,
		HTTP:             &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("gemini client is nil")
	}
	if c.APIKey == "" {
		return "", fmt.Errorf("gemini api key is empty")
	}

	payload := requestPayload{
		Model:             c.Model,
		Input:             prompt,
		SystemInstruction: c.SystemPrompt,
		Store:             false,
	}
	payload.GenerationConfig.MaxOutputTokens = c.MaxOutputTokens

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := "https://generativelanguage.googleapis.com/v1beta/interactions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("call Gemini API: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read Gemini response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("gemini API error: %s", strings.TrimSpace(string(data)))
	}

	var out responsePayload
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode Gemini response: %w", err)
	}

	if out.Error.Message != "" {
		return "", fmt.Errorf("gemini API: %s", out.Error.Message)
	}
	var text string
	for _, step := range out.Steps {
		if step.Type != "model_output" {
			continue
		}
		for _, item := range step.Content {
			if item.Type == "text" {
				text += item.Text
			}
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("gemini response text was empty")
	}
	return text, nil
}
