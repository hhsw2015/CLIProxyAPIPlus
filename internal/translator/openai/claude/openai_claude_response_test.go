package claude

import (
	"context"
	"strings"
	"testing"
)

func TestConvertOpenAIResponseToClaude_StreamSkipsToolUseStartWhenNameEmpty(t *testing.T) {
	var param any

	got := ConvertOpenAIResponseToClaude(
		context.Background(),
		"claude-opus-4.5",
		[]byte(`{"stream":true}`),
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"claude-opus-4.5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"","arguments":"{\"pattern\":\"**/*\"}"}}]}}]}`),
		&param,
	)

	joined := strings.Join(got, "")
	if !strings.Contains(joined, `"type":"message_start"`) {
		t.Fatalf("expected message_start in output, got %q", joined)
	}
	if strings.Contains(joined, `"type":"tool_use"`) {
		t.Fatalf("did not expect tool_use block for empty tool name, got %q", joined)
	}
}

func TestConvertOpenAIResponseToClaude_StreamStartsToolUseOnlyOncePerIndex(t *testing.T) {
	var param any
	originalRequest := []byte(`{"stream":true}`)

	first := ConvertOpenAIResponseToClaude(
		context.Background(),
		"claude-opus-4.5",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"claude-opus-4.5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Glob","arguments":"{\"pattern\":\"**/*\"}"}}]}}]}`),
		&param,
	)
	second := ConvertOpenAIResponseToClaude(
		context.Background(),
		"claude-opus-4.5",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"claude-opus-4.5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Glob","arguments":"{\"pattern\":\"CLAUDE.md\"}"}}]}}]}`),
		&param,
	)

	firstJoined := strings.Join(first, "")
	secondJoined := strings.Join(second, "")
	if count := strings.Count(firstJoined, `"type":"tool_use"`); count != 1 {
		t.Fatalf("expected first chunk to open one tool_use block, got %d in %q", count, firstJoined)
	}
	if strings.Contains(secondJoined, `"type":"tool_use"`) {
		t.Fatalf("did not expect duplicate tool_use start on later chunk, got %q", secondJoined)
	}
}

func TestConvertOpenAIResponseToClaude_StreamUsesChoiceIndexAndRootFinishReason(t *testing.T) {
	var param any
	originalRequest := []byte(`{"stream":true}`)

	chunks := [][]byte{
		[]byte(`data: {"id":"chatcmpl_1","model":"claude-opus-4.5","choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_glob","function":{"name":"Glob","arguments":""}}]}}]}`),
		[]byte(`data: {"id":"chatcmpl_1","model":"claude-opus-4.5","choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"","function":{"name":"","arguments":"{\"pattern\":\"**/*\"}"}}]}}]}`),
		[]byte(`data: {"id":"chatcmpl_1","model":"claude-opus-4.5","choices":[{"index":2,"delta":{"tool_calls":[{"index":0,"id":"call_read","function":{"name":"Read","arguments":""}}]}}]}`),
		[]byte(`data: {"id":"chatcmpl_1","model":"claude-opus-4.5","choices":[{"index":2,"delta":{"tool_calls":[{"index":0,"id":"","function":{"name":"","arguments":"{\"file_path\":\"README.md\"}"}}]}}]}`),
		[]byte(`data: {"id":"chatcmpl_1","model":"claude-opus-4.5","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5},"finish_reason":"tool_use"}`),
		[]byte(`data: [DONE]`),
	}

	var all string
	for _, chunk := range chunks {
		all += strings.Join(ConvertOpenAIResponseToClaude(context.Background(), "claude-opus-4.5", originalRequest, nil, chunk, &param), "")
	}
	state := param.(*ConvertOpenAIResponseToAnthropicParams)
	if got := state.ToolCallsAccumulator[1000].Arguments.String(); got == "" {
		t.Fatalf("expected accumulated args for first tool call, got empty; state=%+v", state.ToolCallsAccumulator)
	}
	if got := state.ToolCallsAccumulator[2000].Arguments.String(); got == "" {
		t.Fatalf("expected accumulated args for second tool call, got empty; state=%+v", state.ToolCallsAccumulator)
	}
	if len(state.ToolCallBlockIndexes) != 0 {
		t.Fatalf("expected tool block indexes to be cleared after finish, got %+v", state.ToolCallBlockIndexes)
	}

	if strings.Count(all, `"type":"tool_use"`) != 2 {
		t.Fatalf("expected two tool_use blocks, got output %q", all)
	}
	if !strings.Contains(all, `"name":"Glob"`) || !strings.Contains(all, `"name":"Read"`) {
		t.Fatalf("expected both tool names in output, got %q", all)
	}
	if !strings.Contains(all, `"partial_json":"{\"pattern\":\"**/*\"}"`) {
		t.Fatalf("expected Glob args delta in output, got %q", all)
	}
	if !strings.Contains(all, `"partial_json":"{\"file_path\":\"README.md\"}"`) {
		t.Fatalf("expected Read args delta in output, got %q", all)
	}
	if strings.Count(all, `"type":"content_block_stop"`) != 2 {
		t.Fatalf("expected two content_block_stop events, got %q", all)
	}
	if !strings.Contains(all, `"type":"message_delta"`) {
		t.Fatalf("expected message_delta from root finish_reason/usage, got %q", all)
	}
	if !strings.Contains(all, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected tool_use stop_reason, got %q", all)
	}
	if !strings.Contains(all, `"type":"message_stop"`) {
		t.Fatalf("expected message_stop, got %q", all)
	}
}
