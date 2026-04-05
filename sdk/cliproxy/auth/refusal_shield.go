package auth

import (
	"context"
	"encoding/json"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/refusal"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// refusalShieldError is returned when the refusal shield detects a model refusal
// in the initial stream bytes. It triggers the outer credential retry loop
// with a rewritten payload.
type refusalShieldError struct {
	rewrittenPayload []byte
}

func (e *refusalShieldError) Error() string {
	return "refusal_shield: model refusal detected, retrying with rewritten payload"
}

// refusalShieldConfig returns the current refusal shield configuration, or nil
// if the feature is disabled.
func (m *Manager) refusalShieldConfig() *internalconfig.RefusalShieldConfig {
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return nil
	}
	if !cfg.RefusalShield.Enabled {
		return nil
	}
	c := internalconfig.RefusalShieldDefaults(cfg.RefusalShield)
	return &c
}

// refusalShieldCheck inspects the buffered bootstrap chunks for a model refusal.
// If a refusal is detected, it rewrites the request payload and returns a
// refusalShieldError so that the caller can retry with the modified payload.
//
// If no refusal is detected, it returns nil and the stream continues normally.
//
// The function is designed to be a no-op (returns nil immediately) when:
//   - RefusalShield is disabled in config.
//   - No buffered chunks contain payload data.
//   - The buffered content passes the detection filter.
func (m *Manager) refusalShieldCheck(ctx context.Context, shieldCfg *internalconfig.RefusalShieldConfig, buffered []cliproxyexecutor.StreamChunk, originalPayload []byte) error {
	if shieldCfg == nil || len(buffered) == 0 {
		return nil
	}

	detector := refusal.NewDetector(shieldCfg.ExtraStrongPatterns, shieldCfg.ExtraWeakPatterns)

	// Extract text from the buffered chunks using the peeker's extraction logic.
	text := refusal.ExtractTextFromChunks(buffered)
	if len(text) == 0 {
		return nil
	}

	if !detector.IsRefusal(text) {
		return nil
	}

	// Refusal detected — rewrite the payload for retry.
	entry := log.WithContext(ctx).WithField("refusal_text_preview", truncateStr(text, 80))
	entry.Info("[refusal-shield] refusal detected, rewriting payload for retry")

	rewritten := m.rewriteWithStrategy(ctx, shieldCfg, originalPayload, text)
	return &refusalShieldError{rewrittenPayload: rewritten}
}

// rewriteWithStrategy applies the configured rewrite strategy:
//
//  1. ai-rewrite: true + endpoint configured → call external OpenAI-compatible API.
//  2. ai-rewrite: true + no endpoint         → call CPA's own provider pool.
//  3. ai-rewrite: false                       → use static templates.
//
// On any AI rewrite failure, falls back silently to static templates.
func (m *Manager) rewriteWithStrategy(ctx context.Context, shieldCfg *internalconfig.RefusalShieldConfig, originalPayload []byte, refusalText string) []byte {
	if !shieldCfg.AIRewrite {
		return refusal.RewritePayload(originalPayload)
	}

	userMsg := refusal.ExtractLastUserMessage(originalPayload)
	timeout := time.Duration(shieldCfg.AIRewriteTimeoutSeconds) * time.Second

	var aiAcceptance string

	if shieldCfg.AIRewriteEndpoint != "" {
		// Path 1: call user-specified external endpoint.
		aiAcceptance = refusal.AIRewrite(ctx, refusal.AIRewriterConfig{
			Endpoint:    shieldCfg.AIRewriteEndpoint,
			APIKey:      shieldCfg.AIRewriteKey,
			Model:       shieldCfg.AIRewriteModel,
			Timeout:     timeout,
			UserMessage: userMsg,
			RefusalText: refusalText,
		})
	} else {
		// Path 2: call CPA's own provider pool via Manager.Execute.
		aiAcceptance = m.rewriteViaCPAPool(ctx, shieldCfg, userMsg, refusalText, timeout)
	}

	if aiAcceptance != "" {
		log.WithContext(ctx).WithField("ai_acceptance_preview", truncateStr(aiAcceptance, 60)).
			Debug("[refusal-shield] using AI-generated acceptance")
		return refusal.RewritePayloadWithAcceptance(originalPayload, aiAcceptance)
	}

	log.WithContext(ctx).Debug("[refusal-shield] AI rewrite unavailable, using static template")
	return refusal.RewritePayload(originalPayload)
}

// rewriteViaCPAPool builds a minimal chat completions request and routes it
// through CPA's own Manager.Execute, leveraging whatever providers are
// currently available in the pool.
func (m *Manager) rewriteViaCPAPool(ctx context.Context, shieldCfg *internalconfig.RefusalShieldConfig, userMsg, refusalText string, timeout time.Duration) string {
	model := shieldCfg.AIRewriteModel
	if model == "" {
		model = "gpt-4o-mini"
	}

	rewritePrompt := refusal.BuildRewritePrompt(userMsg, refusalText)

	payload, err := json.Marshal(map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": refusal.RewriteSystemPrompt},
			{"role": "user", "content": rewritePrompt},
		},
		"max_tokens":  80,
		"temperature": 0.7,
		"stream":      false,
	})
	if err != nil {
		return ""
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Determine providers to use. We try all registered providers.
	providers := m.registeredProviderKeys()

	req := cliproxyexecutor.Request{
		Model:   model,
		Payload: payload,
	}
	opts := cliproxyexecutor.Options{
		Stream: false,
	}

	resp, err := m.Execute(reqCtx, providers, req, opts)
	if err != nil {
		log.WithContext(ctx).WithError(err).Debug("[refusal-shield] CPA pool rewrite call failed")
		return ""
	}

	return extractContentFromChatResponse(resp.Payload)
}

// registeredProviderKeys returns all provider keys registered in the manager.
func (m *Manager) registeredProviderKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.executors))
	for k := range m.executors {
		keys = append(keys, k)
	}
	return keys
}

// extractContentFromChatResponse extracts the assistant content from an OpenAI
// chat completions response payload.
func extractContentFromChatResponse(payload []byte) string {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return ""
	}
	if len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].Message.Content
}

// truncateStr returns the first n characters of s, appending "..." if truncated.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
