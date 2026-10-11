package helps

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// interactionsAgentPresets maps a model id to the Google Interactions `agent_config.type`
// it must run as. These models are AGENTS, not sync chat models: the Interactions API
// rejects them in the `model` field ("refers to an agent, use the 'agent' field") and
// requires `background:true` + an `agent_config.type`. Execution is async (submit ->
// poll until status leaves in_progress).
//
// Only deep-research is reachable on the AI-Studio (AIza) tier today; the other
// agent_config.type values the API advertises (dynamic, code-mender, antigravity) have
// no AIza-reachable agent id (404), so they are intentionally absent until one exists.
// A client may still drive any of them by sending an explicit `agent_config` in its
// request (handled below without needing a preset entry).
var interactionsAgentPresets = map[string]string{
	"deep-research-preview-04-2026": "deep-research",
}

// IsGeminiInteractionsAgentModel reports whether a model id is a known background agent.
func IsGeminiInteractionsAgentModel(model string) bool {
	_, ok := interactionsAgentPresets[model]
	return ok
}

// ApplyGeminiInteractionsAgentMode rewrites an already-translated Interactions request
// body into background-agent form when `model` names a known agent, or when the client
// already supplied an `agent`/`agent_config`. It moves `model` -> `agent`, forces
// `background:true`, and fills `agent_config.type` from the preset map when absent.
// Returns the (possibly unchanged) body and whether it is now a background agent request.
//
// This is a built-in payload transform: callers must run it BEFORE the final payload-
// config barrier, like every other built-in normalization.
func ApplyGeminiInteractionsAgentMode(body []byte, model string) ([]byte, bool) {
	presetType, isPreset := interactionsAgentPresets[model]
	hasAgentCfg := gjson.GetBytes(body, "agent_config").Exists()
	agentField := gjson.GetBytes(body, "agent").String()
	if !isPreset && !hasAgentCfg && agentField == "" {
		return body, false
	}

	agent := agentField
	if agent == "" {
		if m := gjson.GetBytes(body, "model").String(); m != "" {
			agent = m
		} else {
			agent = model
		}
	}
	body, _ = sjson.SetBytes(body, "agent", agent)
	body, _ = sjson.DeleteBytes(body, "model")
	body, _ = sjson.SetBytes(body, "background", true)
	// Background agents are submit+poll: the submit must never be a streaming request
	// (a streamed submit holds the connection for the whole run instead of returning an
	// id). The client's stream preference is honored by the executor replaying the final
	// interaction as synthetic SSE.
	body, _ = sjson.DeleteBytes(body, "stream")
	if !gjson.GetBytes(body, "agent_config.type").Exists() {
		t := presetType
		if t == "" {
			t = "dynamic"
		}
		body, _ = sjson.SetBytes(body, "agent_config.type", t)
	}
	return body, true
}

// BuildInteractionsStreamFramesFromFinal reconstructs SSE-style Interactions event frames
// from a COMPLETED (non-streamed) interaction object so the background-agent path can
// reuse the existing streaming translators. The background mode returns the whole
// interaction at once (submit -> poll -> final), with no incremental SSE, so we replay it
// as: interaction.created -> (per step) step.start + step.delta(text) -> interaction.completed.
// Each returned element is a raw JSON frame; the caller feeds it to the stream translator.
func BuildInteractionsStreamFramesFromFinal(finalData []byte) [][]byte {
	root := gjson.ParseBytes(finalData)
	interaction := root
	if nested := root.Get("interaction"); nested.Exists() {
		interaction = nested
	}
	id := firstNonEmptyStr(interaction.Get("id").String(), root.Get("id").String())
	model := firstNonEmptyStr(interaction.Get("model").String(), interaction.Get("agent").String(), root.Get("model").String(), root.Get("agent").String())

	frames := make([][]byte, 0, 8)

	created := []byte(`{"event_type":"interaction.created","interaction":{"id":"","model":""}}`)
	created, _ = sjson.SetBytes(created, "interaction.id", id)
	created, _ = sjson.SetBytes(created, "interaction.model", model)
	frames = append(frames, created)

	steps := interaction.Get("steps")
	if !steps.Exists() {
		steps = root.Get("steps")
	}
	index := 0
	steps.ForEach(func(_, step gjson.Result) bool {
		stepType := step.Get("type").String()
		var deltaType string
		switch stepType {
		case "model_output":
			deltaType = "text"
		case "thought":
			deltaType = "thought_summary"
		default:
			return true // user_input echo, tool steps etc. are not emitted as text
		}
		var text string
		step.Get("content").ForEach(func(_, part gjson.Result) bool {
			if t := part.Get("text").String(); t != "" {
				text += t
			}
			return true
		})
		if deltaType == "thought_summary" {
			step.Get("summary").ForEach(func(_, part gjson.Result) bool {
				if t := part.Get("text").String(); t != "" {
					text += t
				}
				return true
			})
		}
		if text == "" {
			return true
		}
		start := []byte(`{"event_type":"step.start","index":0,"step":{"type":""}}`)
		start, _ = sjson.SetBytes(start, "index", index)
		start, _ = sjson.SetBytes(start, "step.type", stepType)
		frames = append(frames, start)

		delta := []byte(`{"event_type":"step.delta","index":0,"delta":{"type":"","text":""}}`)
		delta, _ = sjson.SetBytes(delta, "index", index)
		delta, _ = sjson.SetBytes(delta, "delta.type", deltaType)
		delta, _ = sjson.SetBytes(delta, "delta.text", text)
		frames = append(frames, delta)
		index++
		return true
	})

	completed := []byte(`{"event_type":"interaction.completed","interaction":{}}`)
	completed, _ = sjson.SetRawBytes(completed, "interaction", []byte(interaction.Raw))
	frames = append(frames, completed)
	return frames
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
