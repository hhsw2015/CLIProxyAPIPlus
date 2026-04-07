package refusal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// VerifySystemPrompt is the system prompt used for AI-assisted refusal verification.
const VerifySystemPrompt = `You are a binary classifier. Given a text snippet from an AI assistant's response, determine whether the assistant is REFUSING to help the user.

Rules:
- Reply ONLY "YES" if the assistant is refusing, declining, or saying it cannot help.
- Reply ONLY "NO" if the assistant is being helpful, explaining something, or apologizing while still providing assistance.
- Do not explain your reasoning. Output exactly one word: YES or NO.`

// AIVerifyConfig holds settings for the AI verification call.
type AIVerifyConfig struct {
	Endpoint string
	APIKey   string
	Model    string
	Timeout  time.Duration
}

// AIVerify calls an OpenAI-compatible model to determine whether a text snippet
// is a genuine refusal. Returns true only if the model explicitly replies "YES".
// Returns false on any error, timeout, or ambiguous response (fail-open design).
func AIVerify(ctx context.Context, cfg AIVerifyConfig, text string) bool {
	if cfg.Endpoint == "" || text == "" {
		return false
	}

	if len(text) > 300 {
		text = text[:300]
	}

	reqBody := chatCompletionRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: VerifySystemPrompt},
			{Role: "user", Content: text},
		},
		MaxTokens:   3,
		Temperature: 0,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return false
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.Endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return false
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}

	var chatResp chatCompletionResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return false
	}

	if len(chatResp.Choices) == 0 {
		return false
	}

	answer := strings.TrimSpace(strings.ToUpper(chatResp.Choices[0].Message.Content))
	return answer == "YES"
}
