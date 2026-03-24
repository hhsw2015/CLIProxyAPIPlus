package responses

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// feedChunks sends a sequence of SSE data lines through the streaming converter
// and returns all emitted response events concatenated.
func feedChunks(t *testing.T, chunks []string) []string {
	t.Helper()
	var param any
	var all []string
	for _, c := range chunks {
		events := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(
			context.Background(), "gpt-5.4", nil, nil, []byte(c), &param,
		)
		for _, ev := range events {
			all = append(all, string(ev))
		}
	}
	return all
}

// extractEventPayloads filters events by type prefix and returns their JSON data payloads.
func extractEventPayloads(events []string, eventType string) []string {
	var out []string
	prefix := "event: " + eventType + "\ndata: "
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			data := strings.TrimPrefix(e, "event: "+eventType+"\ndata: ")
			out = append(out, data)
		}
	}
	return out
}

func TestStreamingParallelToolCalls(t *testing.T) {
	// Simulate an SSE stream where GPT-5.4 returns 3 parallel tool calls.
	// Each tool call is announced in its own SSE chunk with a unique index and call_id,
	// followed by an arguments chunk, then a finish_reason chunk.

	chunks := []string{
		// Initial chunk — announces the response
		`data: {"id":"gen-abc","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,

		// Tool call 0: header with id and name
		`data: {"id":"gen-abc","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_AAA","type":"function","function":{"name":"exec_command","arguments":""}}]},"finish_reason":null}]}`,
		// Tool call 0: arguments
		`data: {"id":"gen-abc","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]}`,

		// Tool call 1: header with id and name
		`data: {"id":"gen-abc","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_BBB","type":"function","function":{"name":"exec_command","arguments":""}}]},"finish_reason":null}]}`,
		// Tool call 1: arguments
		`data: {"id":"gen-abc","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"cmd\":\"pwd\"}"}}]},"finish_reason":null}]}`,

		// Tool call 2: header with id and name
		`data: {"id":"gen-abc","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_CCC","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`,
		// Tool call 2: arguments
		`data: {"id":"gen-abc","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"function":{"arguments":"{\"path\":\"/tmp/x\"}"}}]},"finish_reason":null}]}`,

		// Finish
		`data: {"id":"gen-abc","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}

	events := feedChunks(t, chunks)

	// Verify: should have 3 separate output_item.added events for function_call
	added := extractEventPayloads(events, "response.output_item.added")
	funcAdded := filterByType(added, "function_call")
	if len(funcAdded) != 3 {
		t.Fatalf("expected 3 function_call output_item.added events, got %d\nevents: %v", len(funcAdded), funcAdded)
	}

	// Verify call_ids are distinct
	callIDs := make(map[string]bool)
	for _, fa := range funcAdded {
		cid := gjson.Get(fa, "item.call_id").String()
		if callIDs[cid] {
			t.Errorf("duplicate call_id: %s", cid)
		}
		callIDs[cid] = true
	}
	if !callIDs["call_AAA"] || !callIDs["call_BBB"] || !callIDs["call_CCC"] {
		t.Errorf("expected call_ids call_AAA, call_BBB, call_CCC, got %v", callIDs)
	}

	// Verify: should have 3 separate function_call_arguments.done events
	argsDone := extractEventPayloads(events, "response.function_call_arguments.done")
	if len(argsDone) != 3 {
		t.Fatalf("expected 3 function_call_arguments.done events, got %d", len(argsDone))
	}

	// Verify each arguments.done has valid JSON arguments (not concatenated)
	expectedArgs := map[string]string{
		"call_AAA": `{"cmd":"ls"}`,
		"call_BBB": `{"cmd":"pwd"}`,
		"call_CCC": `{"path":"/tmp/x"}`,
	}
	for _, ad := range argsDone {
		itemID := gjson.Get(ad, "item_id").String()
		args := gjson.Get(ad, "arguments").String()
		// item_id is "fc_call_XXX", extract call_id
		cid := strings.TrimPrefix(itemID, "fc_")
		expected, ok := expectedArgs[cid]
		if !ok {
			t.Errorf("unexpected call_id in arguments.done: %s", cid)
			continue
		}
		if !jsonEqual(t, args, expected) {
			t.Errorf("call %s: expected args %s, got %s", cid, expected, args)
		}
	}

	// Verify: output_index values are distinct for each function_call
	outputIndices := make(map[int64]string)
	for _, fa := range funcAdded {
		oi := gjson.Get(fa, "output_index").Int()
		cid := gjson.Get(fa, "item.call_id").String()
		if prev, exists := outputIndices[oi]; exists {
			t.Errorf("output_index %d shared by %s and %s", oi, prev, cid)
		}
		outputIndices[oi] = cid
	}
}

func TestStreamingSingleToolCall(t *testing.T) {
	// Regression test: a single tool call should still work correctly.
	chunks := []string{
		`data: {"id":"gen-single","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`data: {"id":"gen-single","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ONLY","type":"function","function":{"name":"exec_command","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"gen-single","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]},"finish_reason":null}]}`,
		`data: {"id":"gen-single","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hello\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"gen-single","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}

	events := feedChunks(t, chunks)

	funcAdded := filterByType(extractEventPayloads(events, "response.output_item.added"), "function_call")
	if len(funcAdded) != 1 {
		t.Fatalf("expected 1 function_call, got %d", len(funcAdded))
	}

	cid := gjson.Get(funcAdded[0], "item.call_id").String()
	if cid != "call_ONLY" {
		t.Errorf("expected call_id call_ONLY, got %s", cid)
	}

	argsDone := extractEventPayloads(events, "response.function_call_arguments.done")
	if len(argsDone) != 1 {
		t.Fatalf("expected 1 arguments.done, got %d", len(argsDone))
	}

	args := gjson.Get(argsDone[0], "arguments").String()
	if !jsonEqual(t, args, `{"cmd":"hello"}`) {
		t.Errorf("expected args {\"cmd\":\"hello\"}, got %s", args)
	}
}

func TestStreamingTextThenParallelToolCalls(t *testing.T) {
	// Model emits some text content, then parallel tool calls.
	// Verifies message and function_call items get distinct output_index values.
	chunks := []string{
		`data: {"id":"gen-mixed","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me check"},"finish_reason":null}]}`,
		`data: {"id":"gen-mixed","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"content":" that."},"finish_reason":null}]}`,
		// Tool call 0
		`data: {"id":"gen-mixed","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_X","type":"function","function":{"name":"cmd","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
		// Tool call 1
		`data: {"id":"gen-mixed","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_Y","type":"function","function":{"name":"cmd","arguments":"{\"b\":2}"}}]},"finish_reason":null}]}`,
		// Finish
		`data: {"id":"gen-mixed","object":"chat.completion.chunk","created":1700000000,"model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}

	events := feedChunks(t, chunks)

	// Should have: 1 message + 2 function_calls = 3 output_item.added events
	allAdded := extractEventPayloads(events, "response.output_item.added")
	msgAdded := filterByType(allAdded, "message")
	funcAdded := filterByType(allAdded, "function_call")

	if len(msgAdded) != 1 {
		t.Errorf("expected 1 message output_item.added, got %d", len(msgAdded))
	}
	if len(funcAdded) != 2 {
		t.Errorf("expected 2 function_call output_item.added, got %d", len(funcAdded))
	}

	// All output_index values should be distinct
	allIndices := make(map[int64]bool)
	for _, a := range allAdded {
		oi := gjson.Get(a, "output_index").Int()
		if allIndices[oi] {
			t.Errorf("duplicate output_index: %d", oi)
		}
		allIndices[oi] = true
	}
}

// --- helpers ---

func filterByType(payloads []string, itemType string) []string {
	var out []string
	for _, p := range payloads {
		if gjson.Get(p, "item.type").String() == itemType {
			out = append(out, p)
		}
	}
	return out
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var ja, jb any
	if err := json.Unmarshal([]byte(a), &ja); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &jb); err != nil {
		return false
	}
	ab, _ := json.Marshal(ja)
	bb, _ := json.Marshal(jb)
	return string(ab) == string(bb)
}
