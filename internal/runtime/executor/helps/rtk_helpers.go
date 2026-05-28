package helps

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/rtk"
)

const rtkBytesFloor = 500

// applyRTKToolResultCompression compresses tool_result content blocks in-place
// using the vendored RTK port. Returns the (possibly modified) payload.
//
// Walks both Anthropic-style requests (messages[].content[] tool_result) and
// OpenAI Responses-style requests (input[] function_call_output). For each
// tool_result we look up the matching tool_use call_id to recover the original
// shell command, classify it via rtk.Filter, and replace the content with the
// compressed form when the savings clear cfg.RTK.MinSavingsPct.
//
// Errors and edge cases (missing call_id, unclassified command, content under
// rtkBytesFloor, compression growing the payload) preserve the original.
//
// Implementation: single json.Unmarshal → mutate map in place → json.Marshal.
// We do this rather than per-block sjson.SetBytes (which would re-allocate the
// full payload N times for N tool_results) so the cost stays linear on payload
// size regardless of how many tool_result blocks need rewriting.
func applyRTKToolResultCompression(cfg *config.Config, payload []byte) []byte {
	if cfg == nil || !cfg.RTK.Enabled || len(payload) == 0 {
		return payload
	}
	minPct := cfg.RTK.MinSavingsPct
	if minPct <= 0 {
		minPct = 5
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}

	// Cheap pre-flight: if there's no plausible tool_result anywhere, skip the
	// expensive walk. This keeps non-tool requests on the fast path.
	if !hasToolResultLikeKeys(root) {
		return payload
	}

	callIDToCmd := indexToolCommandsMap(root)
	if len(callIDToCmd) == 0 {
		return payload
	}

	mutated := false
	if compressAnthropicMap(root, callIDToCmd, minPct) {
		mutated = true
	}
	if compressOpenAIMap(root, callIDToCmd, minPct) {
		mutated = true
	}
	if !mutated {
		return payload
	}

	out, err := json.Marshal(root)
	if err != nil {
		return payload
	}
	return out
}

// hasToolResultLikeKeys peeks at the top-level structure to decide if a full
// walk is worthwhile. Cheap structural check — no recursion, no per-block work.
func hasToolResultLikeKeys(root map[string]any) bool {
	if _, ok := root["messages"]; ok {
		return true
	}
	if _, ok := root["input"]; ok {
		return true
	}
	return false
}

// indexToolCommandsMap returns a map from tool_use id / function_call call_id
// to the originating shell command. Tools without a recoverable command string
// are skipped — we cannot classify them via rtk.IsClassified.
func indexToolCommandsMap(root map[string]any) map[string]string {
	out := make(map[string]string)

	// Anthropic: messages[].content[].{type:"tool_use", id, input.command}
	if msgs, ok := root["messages"].([]any); ok {
		for _, msg := range msgs {
			obj, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			content, ok := obj["content"].([]any)
			if !ok {
				continue
			}
			for _, block := range content {
				bobj, ok := block.(map[string]any)
				if !ok {
					continue
				}
				if asString(bobj["type"]) != "tool_use" {
					continue
				}
				id := asString(bobj["id"])
				if id == "" {
					continue
				}
				input, ok := bobj["input"].(map[string]any)
				if !ok {
					continue
				}
				if cmd := asString(input["command"]); cmd != "" {
					out[id] = cmd
				}
			}
		}
	}

	// OpenAI Responses: input[].{type:"function_call", call_id, arguments}.
	// arguments is a JSON-encoded string with .command.
	if items, ok := root["input"].([]any); ok {
		for _, item := range items {
			iobj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if asString(iobj["type"]) != "function_call" {
				continue
			}
			callID := asString(iobj["call_id"])
			if callID == "" {
				continue
			}
			args := asString(iobj["arguments"])
			if args == "" {
				continue
			}
			var argMap map[string]any
			if err := json.Unmarshal([]byte(args), &argMap); err != nil {
				continue
			}
			if cmd := asString(argMap["command"]); cmd != "" {
				out[callID] = cmd
			}
		}
	}

	return out
}

// compressAnthropicMap walks messages[].content[] tool_result blocks and rewrites
// their content. Skips is_error=true to preserve error traces. Returns true if
// any block was rewritten.
func compressAnthropicMap(root map[string]any, callIDToCmd map[string]string, minPct int) bool {
	msgs, ok := root["messages"].([]any)
	if !ok {
		return false
	}
	mutated := false
	for _, msg := range msgs {
		obj, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content, ok := obj["content"].([]any)
		if !ok {
			continue
		}
		for _, block := range content {
			bobj, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if asString(bobj["type"]) != "tool_result" {
				continue
			}
			if isErr, _ := bobj["is_error"].(bool); isErr {
				continue
			}
			useID := asString(bobj["tool_use_id"])
			cmd := callIDToCmd[useID]
			if cmd == "" {
				continue
			}
			switch v := bobj["content"].(type) {
			case string:
				if compressed, ok := tryCompress(cmd, v, minPct); ok {
					bobj["content"] = compressed
					mutated = true
				}
			case []any:
				for _, part := range v {
					pobj, ok := part.(map[string]any)
					if !ok {
						continue
					}
					if asString(pobj["type"]) != "text" {
						continue
					}
					txt := asString(pobj["text"])
					if txt == "" {
						continue
					}
					if compressed, ok := tryCompress(cmd, txt, minPct); ok {
						pobj["text"] = compressed
						mutated = true
					}
				}
			}
		}
	}
	return mutated
}

// compressOpenAIMap walks OpenAI Responses input[] function_call_output entries
// and rewrites their output. Returns true if any block was rewritten.
func compressOpenAIMap(root map[string]any, callIDToCmd map[string]string, minPct int) bool {
	items, ok := root["input"].([]any)
	if !ok {
		return false
	}
	mutated := false
	for _, item := range items {
		iobj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if asString(iobj["type"]) != "function_call_output" {
			continue
		}
		callID := asString(iobj["call_id"])
		cmd := callIDToCmd[callID]
		if cmd == "" {
			continue
		}
		switch v := iobj["output"].(type) {
		case string:
			if compressed, ok := tryCompress(cmd, v, minPct); ok {
				iobj["output"] = compressed
				mutated = true
			}
		case []any:
			for _, part := range v {
				pobj, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if asString(pobj["type"]) != "input_text" {
					continue
				}
				txt := asString(pobj["text"])
				if txt == "" {
					continue
				}
				if compressed, ok := tryCompress(cmd, txt, minPct); ok {
					pobj["text"] = compressed
					mutated = true
				}
			}
		}
	}
	return mutated
}

// tryCompress runs RTK on (cmd, text) and returns the compressed text only when
// the savings clear minPct AND length actually shrunk. Otherwise returns "", false.
func tryCompress(cmd, text string, minPct int) (string, bool) {
	if len(text) < rtkBytesFloor {
		return "", false
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", false
	}
	if !rtk.IsClassified(cmd) {
		return "", false
	}
	res := rtk.Filter(cmd, text)
	if res.SavedBytes <= 0 {
		return "", false
	}
	if res.SavingsPct < float64(minPct) {
		return "", false
	}
	return res.Filtered, true
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
