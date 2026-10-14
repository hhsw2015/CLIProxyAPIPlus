<!-- CPA skill family — SOURCE OF TRUTH for the `tts` domain (M2-b). The JSON block is machine-read by
scripts/gen-cpa-skills.py (emits skills/cpa-tts/SKILL.md + snippets/cpa-tts.luau + the cli default). Prose is copied into
SKILL.md. Default MODEL resolved from frontier-aliases.json (media["TTS / voice"].id) at generation time. -->

```json
{
  "domain": "tts",
  "cli": "cpa-tts",
  "skill_name": "cpa-tts",
  "description": "Text-to-speech through CPA (CLIProxyAPI relay). Use when the user asks to 朗读 / 合成语音 / read this aloud / text-to-speech via the swarm's CPA endpoint. Runs cli/cpa-tts (sync /v1/audio/speech → audio bytes).",
  "argument_hint": "<text to speak> [--voice <id>] [--model <id>] [--out out.mp3]",
  "endpoint": "/v1/audio/speech",
  "method": "POST",
  "mode": "sync",
  "output": "audio",
  "default_model_key": "TTS / voice",
  "defaults": { "voice": "zh_female_xiaohe_uranus_bigtts" },
  "params": [
    {"name": "input", "required": true, "desc": "the text to speak (positional)"},
    {"name": "--voice", "required": false, "default": "zh_female_xiaohe_uranus_bigtts", "desc": "speaker/voice id (doubao default shown; ElevenLabs needs a voice_id; OpenAI tts uses alloy/echo/…)"},
    {"name": "--model", "required": false, "desc": "override the default model id"},
    {"name": "--out", "required": false, "default": "cpa-tts-<ts>.mp3", "desc": "output audio file path"}
  ],
  "gotchas": [
    "the response is RAW AUDIO BYTES (wav/mp3), NOT JSON — the CLI writes them straight to --out",
    "voice is model-family specific: doubao wants a doubao speaker (default zh_female_xiaohe_uranus_bigtts), ElevenLabs wants a voice_id, OpenAI tts wants alloy/echo/fable/onyx/nova/shimmer",
    "doubao-tts is single-shot (~2.8s whole clip), not streaming — use it for quality"
  ]
}
```

## What it does

`cpa-tts` synthesizes speech from text via CPA's `POST /v1/audio/speech` (synchronous; the body is raw audio). The CLI
writes the audio bytes to a file and prints the path.

## Request shape

```
POST {CPA}/v1/audio/speech
Authorization: Bearer <CPA_KEY_CLI>
{ "model": "<default from frontier-aliases>", "input": "<text>", "voice": "<voice id>" }
→ raw audio bytes (mp3/wav)
```

## Notes

- The default model is the current strongest `TTS / voice` id from `frontier-aliases.json`; override with `--model`.
- Match the `--voice` to the model family (see gotchas). Synchronous — no polling.
