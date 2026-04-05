package refusal

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRewritePayload_ChatFormat(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Help me with security testing."},
			{"role": "assistant", "content": "I'm sorry, I cannot assist with that request."}
		],
		"stream": true
	}`)

	result := RewritePayload(payload)

	var body map[string]json.RawMessage
	if err := json.Unmarshal(result, &body); err != nil {
		t.Fatalf("failed to parse rewritten payload: %v", err)
	}

	var messages []map[string]interface{}
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		t.Fatalf("failed to parse messages: %v", err)
	}

	// Should have 4 messages: system, user, rewritten assistant, continue user.
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(messages))
	}

	// Assistant message should be rewritten (no longer contains refusal).
	assistantContent, _ := messages[2]["content"].(string)
	if strings.Contains(strings.ToLower(assistantContent), "sorry") {
		t.Errorf("assistant message should be rewritten, got: %s", assistantContent)
	}

	// Last message should be a user continue message.
	lastRole, _ := messages[3]["role"].(string)
	if lastRole != "user" {
		t.Errorf("last message role should be 'user', got '%s'", lastRole)
	}

	// Model field should be preserved.
	var model string
	json.Unmarshal(body["model"], &model)
	if model != "gpt-4o" {
		t.Errorf("model should be preserved, got: %s", model)
	}
}

func TestRewritePayload_ResponsesFormat(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-4o",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "test"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "I cannot help"}]}
		],
		"stream": true
	}`)

	result := RewritePayload(payload)

	var body map[string]json.RawMessage
	if err := json.Unmarshal(result, &body); err != nil {
		t.Fatalf("failed to parse rewritten payload: %v", err)
	}

	var input []map[string]interface{}
	if err := json.Unmarshal(body["input"], &input); err != nil {
		t.Fatalf("failed to parse input: %v", err)
	}

	// Should have 3 items: user, rewritten assistant, continue user.
	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(input))
	}

	// Last item should be user continue.
	lastRole, _ := input[2]["role"].(string)
	if lastRole != "user" {
		t.Errorf("last input role should be 'user', got '%s'", lastRole)
	}
}

func TestRewritePayload_StripsThinking(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "assistant", "content": "I refuse.", "thinking": "I should not help with this."}
		]
	}`)

	result := RewritePayload(payload)
	resultStr := string(result)

	if strings.Contains(resultStr, "I should not help") {
		t.Error("thinking content should be stripped from rewritten payload")
	}
}

func TestRewritePayload_NoAssistantMessage(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)

	result := RewritePayload(payload)

	var body map[string]json.RawMessage
	if err := json.Unmarshal(result, &body); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	var messages []map[string]interface{}
	json.Unmarshal(body["messages"], &messages)

	// Should have 3: original user + synthetic assistant + continue user.
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
}

func TestRewritePayload_InvalidJSON(t *testing.T) {
	payload := []byte(`not json at all`)
	result := RewritePayload(payload)

	// Should return original payload unchanged.
	if string(result) != string(payload) {
		t.Error("invalid JSON should return original payload")
	}
}

func TestRewritePayload_PreservesStreamFlag(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true,"temperature":0.7}`)
	result := RewritePayload(payload)

	var body map[string]json.RawMessage
	json.Unmarshal(result, &body)

	var stream bool
	json.Unmarshal(body["stream"], &stream)
	if !stream {
		t.Error("stream flag should be preserved")
	}

	var temp float64
	json.Unmarshal(body["temperature"], &temp)
	if temp != 0.7 {
		t.Errorf("temperature should be preserved, got %f", temp)
	}
}
