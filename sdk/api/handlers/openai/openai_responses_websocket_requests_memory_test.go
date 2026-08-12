package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const (
	responsesWebsocketLargeTranscriptSize     = 1 << 20
	responsesWebsocketBenchmarkTranscriptSize = 8 << 20
)

var (
	responsesWebsocketMergedInputSink       []byte
	responsesWebsocketNormalizedRequestSink []byte
)

func TestMergeResponsesWebsocketInputMatchesReferenceSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		lastRequest        string
		lastResponseOutput string
		appendInput        string
	}{
		{
			name:               "messages and paired tool call",
			lastRequest:        `{"model":"gpt-5.4","input":[{"type":"message","id":"msg-1","role":"user","content":"hello"}]}`,
			lastResponseOutput: `[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"}]`,
			appendInput:        `[{"type":"function_call_output","id":"fco-1","call_id":"call-1","output":"done"}]`,
		},
		{
			name:               "duplicate function call keeps first",
			lastRequest:        `{"input":[{"type":"function_call","id":"fc-first","call_id":"call-1","name":"first","arguments":"{}"}]}`,
			lastResponseOutput: `[{"type":"function_call","id":"fc-second","call_id":"call-1","name":"second","arguments":"{}"}]`,
			appendInput:        `[{"type":"function_call_output","id":"fco-1","call_id":"call-1","output":"done"}]`,
		},
		{
			name:               "duplicate id keeps item referenced by output",
			lastRequest:        `{"input":[{"type":"function_call","id":"fc-1","call_id":"call-kept","name":"first","arguments":"{}"}]}`,
			lastResponseOutput: `[{"type":"function_call","id":"fc-1","call_id":"call-other","name":"second","arguments":"{}"}]`,
			appendInput:        `[{"type":"function_call_output","id":"fco-1","call_id":"call-kept","output":"done"}]`,
		},
		{
			name:               "raw JSON values and escaping",
			lastRequest:        `{"input":[ {"type":"message","id":"msg-1","content":"<tag> & \\u263a"}, true ]}`,
			lastResponseOutput: `[null, 42, "line\\nvalue"]`,
			appendInput:        `[{"id":"last","nested":{"value":[1,2,3]}}]`,
		},
		{
			name:               "invalid response output remains ignored",
			lastRequest:        `{"input":[{"id":"first"}]}`,
			lastResponseOutput: `[{"id":`,
			appendInput:        `[{"id":"last"}]`,
		},
		{
			name:               "missing previous input and null append",
			lastRequest:        `{"model":"gpt-5.4"}`,
			lastResponseOutput: `[{"id":"response"}]`,
			appendInput:        `null`,
		},
		{
			name:               "null previous request",
			lastRequest:        `null`,
			lastResponseOutput: `[]`,
			appendInput:        `[{"id":"last"}]`,
		},
		{
			name:               "duplicate metadata keys follow encoding json",
			lastRequest:        `{"input":[{"type":"message","type":"function_call","id":"first","id":"fc-1","call_id":"call-other","call_id":"call-kept"}]}`,
			lastResponseOutput: `[{"type":"function_call","id":"fc-2","call_id":"call-kept"}]`,
			appendInput:        `[{"type":"function_call_output","id":"fco-1","call_id":"call-kept","output":"done"}]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			want, errWant := mergeResponsesWebsocketInputReference([]byte(test.lastRequest), []byte(test.lastResponseOutput), test.appendInput)
			if errWant != nil {
				t.Fatalf("reference merge failed: %v", errWant)
			}
			got, errGot := mergeResponsesWebsocketInput([]byte(test.lastRequest), []byte(test.lastResponseOutput), test.appendInput)
			if errGot != nil {
				t.Fatalf("mergeResponsesWebsocketInput() error = %v", errGot)
			}
			assertJSONSemanticallyEqual(t, got, want)
		})
	}
}

func TestMergeResponsesWebsocketInputMatchesReferenceErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		lastRequest        string
		lastResponseOutput string
		appendInput        string
	}{
		{name: "invalid previous request", lastRequest: `{"input":`, appendInput: `[]`},
		{name: "non-array previous input", lastRequest: `{"input":{"id":"item"}}`, appendInput: `[]`},
		{name: "invalid appended input", lastRequest: `{"input":[]}`, appendInput: `[{"id":`},
		{name: "non-array appended input", lastRequest: `{"input":[]}`, appendInput: `{"id":"item"}`},
		{name: "array previous request", lastRequest: `[]`, appendInput: `[]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, errWant := mergeResponsesWebsocketInputReference([]byte(test.lastRequest), []byte(test.lastResponseOutput), test.appendInput)
			_, errGot := mergeResponsesWebsocketInput([]byte(test.lastRequest), []byte(test.lastResponseOutput), test.appendInput)
			if (errGot == nil) != (errWant == nil) {
				t.Fatalf("error presence differs: got %v, reference %v", errGot, errWant)
			}
			if errGot == nil {
				t.Fatal("expected merge error")
			}
			if errGot.Error() != errWant.Error() {
				t.Fatalf("error differs:\n got: %v\nwant: %v", errGot, errWant)
			}
		})
	}
}

func TestMergeResponsesWebsocketInputMatchesReferenceAcrossGeneratedTranscripts(t *testing.T) {
	t.Parallel()

	random := rand.New(rand.NewSource(0xC0DE))
	for iteration := 0; iteration < 250; iteration++ {
		previous := generatedResponsesWebsocketInput(t, random, random.Intn(12))
		response := generatedResponsesWebsocketInput(t, random, random.Intn(12))
		appendInput := generatedResponsesWebsocketInput(t, random, random.Intn(12))
		lastRequest := append(append([]byte(`{"model":"gpt-5.4","input":`), previous...), '}')

		want, errWant := mergeResponsesWebsocketInputReference(lastRequest, response, string(appendInput))
		if errWant != nil {
			t.Fatalf("iteration %d reference merge failed: %v", iteration, errWant)
		}
		got, errGot := mergeResponsesWebsocketInput(lastRequest, response, string(appendInput))
		if errGot != nil {
			t.Fatalf("iteration %d merge failed: %v", iteration, errGot)
		}
		assertJSONSemanticallyEqual(t, got, want)
	}
}

func generatedResponsesWebsocketInput(t *testing.T, random *rand.Rand, count int) []byte {
	t.Helper()

	items := make([]any, 0, count)
	itemTypes := []string{"message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "reasoning"}
	for index := 0; index < count; index++ {
		if random.Intn(10) == 0 {
			items = append(items, []any{true, float64(index), nil}[random.Intn(3)])
			continue
		}
		item := map[string]any{
			"type":    itemTypes[random.Intn(len(itemTypes))],
			"id":      fmt.Sprintf("item-%d", random.Intn(8)),
			"call_id": fmt.Sprintf("call-%d", random.Intn(6)),
			"content": fmt.Sprintf("iteration-%d <tag> & \\u263a", index),
		}
		items = append(items, item)
	}
	out, errMarshal := json.Marshal(items)
	if errMarshal != nil {
		t.Fatalf("marshal generated input: %v", errMarshal)
	}
	return out
}

func TestNormalizeResponseSubsequentRequestDetachesSourceBuffers(t *testing.T) {
	t.Parallel()

	lastRequest := []byte(`{"model":"gpt-5.4","instructions":"keep me","input":[{"type":"message","id":"msg-1","role":"user","content":"history sentinel"}]}`)
	lastResponseOutput := []byte(`[{"type":"message","id":"msg-2","role":"assistant","content":[{"type":"output_text","text":"response sentinel"}]}]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"message","id":"msg-3","role":"user","content":"append sentinel"}]}`)

	normalized, next, errMessage := normalizeResponseSubsequentRequest(raw, lastRequest, lastResponseOutput, "", nil, false, false)
	if errMessage != nil {
		t.Fatalf("normalizeResponseSubsequentRequest() error = %v", errMessage.Error)
	}
	wantNormalized := bytes.Clone(normalized)
	wantNext := bytes.Clone(next)

	for _, source := range [][]byte{lastRequest, lastResponseOutput, raw} {
		for index := range source {
			source[index] = 'x'
		}
	}
	runtime.KeepAlive(lastRequest)
	runtime.KeepAlive(lastResponseOutput)
	runtime.KeepAlive(raw)

	if !bytes.Equal(normalized, wantNormalized) {
		t.Fatal("normalized request aliases a source buffer")
	}
	if !bytes.Equal(next, wantNext) {
		t.Fatal("stored next request aliases a source buffer")
	}
}

func TestMergeResponsesWebsocketInputBoundsLargeTranscriptAllocations(t *testing.T) {
	lastRequest, lastResponseOutput, appendInput := responsesWebsocketLargeTranscriptFixture()
	inputBytes := len(lastRequest) + len(lastResponseOutput) + len(appendInput)

	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			merged, errMerge := mergeResponsesWebsocketInput(lastRequest, lastResponseOutput, appendInput)
			if errMerge != nil {
				b.Fatalf("mergeResponsesWebsocketInput() error = %v", errMerge)
			}
			responsesWebsocketMergedInputSink = merged
		}
	})

	const maxAllocationMultiple = 2
	maxAllocatedBytes := int64(inputBytes * maxAllocationMultiple)
	t.Logf("merge allocated %d bytes per operation for %d input bytes", result.AllocedBytesPerOp(), inputBytes)
	if allocatedBytes := result.AllocedBytesPerOp(); allocatedBytes > maxAllocatedBytes {
		t.Fatalf("merging %d input bytes allocated %d bytes per operation, want at most %d", inputBytes, allocatedBytes, maxAllocatedBytes)
	}
}

func BenchmarkNormalizeResponseSubsequentRequestLargeTranscript(b *testing.B) {
	lastRequest, lastResponseOutput, appendInput := responsesWebsocketTranscriptFixture(responsesWebsocketBenchmarkTranscriptSize)
	raw := []byte(`{"type":"response.create","input":` + appendInput + `}`)

	b.SetBytes(int64(len(lastRequest) + len(lastResponseOutput) + len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		normalized, _, errMessage := normalizeResponseSubsequentRequest(raw, lastRequest, lastResponseOutput, "", nil, false, false)
		if errMessage != nil {
			b.Fatalf("normalizeResponseSubsequentRequest() error = %v", errMessage.Error)
		}
		responsesWebsocketNormalizedRequestSink = normalized
	}
}

func responsesWebsocketLargeTranscriptFixture() ([]byte, []byte, string) {
	return responsesWebsocketTranscriptFixture(responsesWebsocketLargeTranscriptSize)
}

func responsesWebsocketTranscriptFixture(transcriptSize int) ([]byte, []byte, string) {
	lastRequest := []byte(`{"model":"gpt-5.4","instructions":"coding","stream":true,"input":[{"type":"message","id":"msg-large","role":"user","content":"` + strings.Repeat("x", transcriptSize) + `"}]}`)
	lastResponseOutput := []byte(`[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"}]`)
	appendInput := `[{"type":"function_call_output","id":"fco-1","call_id":"call-1","output":"done"}]`
	return lastRequest, lastResponseOutput, appendInput
}

type referenceResponsesWebsocketInputItem struct {
	raw      json.RawMessage
	itemType string
	id       string
	callID   string
}

func mergeResponsesWebsocketInputReference(lastRequest []byte, lastResponseOutput []byte, appendRaw string) (string, error) {
	var previousRequest struct {
		Input []json.RawMessage `json:"input"`
	}
	if errUnmarshal := json.Unmarshal(lastRequest, &previousRequest); errUnmarshal != nil {
		return "", fmt.Errorf("invalid previous request input: %w", errUnmarshal)
	}
	items, errExisting := appendReferenceResponsesWebsocketRawInputItems(nil, previousRequest.Input)
	if errExisting != nil {
		return "", fmt.Errorf("invalid previous request input: %w", errExisting)
	}

	var responseItems []json.RawMessage
	trimmedResponse := bytes.TrimSpace(lastResponseOutput)
	if len(trimmedResponse) > 0 && trimmedResponse[0] == '[' && json.Valid(trimmedResponse) {
		if errUnmarshal := json.Unmarshal(trimmedResponse, &responseItems); errUnmarshal != nil {
			return "", fmt.Errorf("invalid previous response output: %w", errUnmarshal)
		}
	}
	items, errResponse := appendReferenceResponsesWebsocketRawInputItems(items, responseItems)
	if errResponse != nil {
		return "", fmt.Errorf("invalid previous response output: %w", errResponse)
	}

	appendRaw = strings.TrimSpace(appendRaw)
	if appendRaw == "" {
		appendRaw = "[]"
	}
	var appendItems []json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(appendRaw), &appendItems); errUnmarshal != nil {
		return "", fmt.Errorf("invalid request input: %w", errUnmarshal)
	}
	items, errAppend := appendReferenceResponsesWebsocketRawInputItems(items, appendItems)
	if errAppend != nil {
		return "", fmt.Errorf("invalid request input: %w", errAppend)
	}

	items = dedupeReferenceResponsesWebsocketFunctionCalls(items)
	items = dedupeReferenceResponsesWebsocketInputItems(items)
	rawItems := make([]json.RawMessage, len(items))
	for index := range items {
		rawItems[index] = items[index].raw
	}
	out, errMarshal := json.Marshal(rawItems)
	if errMarshal != nil {
		return "", errMarshal
	}
	return string(out), nil
}

func appendReferenceResponsesWebsocketRawInputItems(items []referenceResponsesWebsocketInputItem, rawItems []json.RawMessage) ([]referenceResponsesWebsocketInputItem, error) {
	for _, rawItem := range rawItems {
		item := referenceResponsesWebsocketInputItem{raw: rawItem}
		trimmed := bytes.TrimSpace(rawItem)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			var metadata struct {
				Type   json.RawMessage `json:"type"`
				ID     json.RawMessage `json:"id"`
				CallID json.RawMessage `json:"call_id"`
			}
			if errUnmarshal := json.Unmarshal(trimmed, &metadata); errUnmarshal != nil {
				return nil, errUnmarshal
			}
			item.itemType = responsesWebsocketMetadataString(metadata.Type)
			item.id = responsesWebsocketMetadataString(metadata.ID)
			item.callID = responsesWebsocketMetadataString(metadata.CallID)
		}
		items = append(items, item)
	}
	return items, nil
}

func dedupeReferenceResponsesWebsocketFunctionCalls(items []referenceResponsesWebsocketInputItem) []referenceResponsesWebsocketInputItem {
	seenCallIDs := make(map[string]struct{}, len(items))
	filtered := items[:0]
	for _, item := range items {
		if isResponsesToolCallType(item.itemType) && item.callID != "" {
			if _, ok := seenCallIDs[item.callID]; ok {
				continue
			}
			seenCallIDs[item.callID] = struct{}{}
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func dedupeReferenceResponsesWebsocketInputItems(items []referenceResponsesWebsocketInputItem) []referenceResponsesWebsocketInputItem {
	referencedCallIDs := make(map[string]struct{}, len(items))
	for _, item := range items {
		if isResponsesToolCallOutputType(item.itemType) && item.callID != "" {
			referencedCallIDs[item.callID] = struct{}{}
		}
	}

	keepIndexByID := make(map[string]int, len(items))
	keepReferencedByID := make(map[string]bool, len(items))
	for index, item := range items {
		if item.id == "" {
			continue
		}
		_, referenced := referencedCallIDs[item.callID]
		referenced = referenced && item.callID != ""
		if _, seen := keepIndexByID[item.id]; !seen {
			keepIndexByID[item.id] = index
			keepReferencedByID[item.id] = referenced
			continue
		}
		if referenced || !keepReferencedByID[item.id] {
			keepIndexByID[item.id] = index
			keepReferencedByID[item.id] = referenced
		}
	}

	filtered := items[:0]
	for index, item := range items {
		if item.id != "" && keepIndexByID[item.id] != index {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func assertJSONSemanticallyEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue any
	if errUnmarshal := json.Unmarshal(got, &gotValue); errUnmarshal != nil {
		t.Fatalf("invalid actual JSON: %v\n%s", errUnmarshal, got)
	}
	var wantValue any
	if errUnmarshal := json.Unmarshal([]byte(want), &wantValue); errUnmarshal != nil {
		t.Fatalf("invalid reference JSON: %v\n%s", errUnmarshal, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON values differ:\n got: %s\nwant: %s", got, want)
	}
}
