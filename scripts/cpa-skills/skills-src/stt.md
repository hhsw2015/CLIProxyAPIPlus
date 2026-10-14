<!-- CPA skill family — SOURCE OF TRUTH for the `stt` domain (M2-b). The JSON block is machine-read by
scripts/gen-cpa-skills.py (emits skills/cpa-stt/SKILL.md + snippets/cpa-stt.luau + the cli default). Prose is copied into
SKILL.md. Default MODEL resolved from frontier-aliases.json (media["STT (speech-to-text)"].id) at generation time. -->

```json
{
  "domain": "stt",
  "cli": "cpa-stt",
  "skill_name": "cpa-stt",
  "description": "Speech-to-text transcription through CPA (CLIProxyAPI relay). Use when the user asks to 转写 / 语音转文字 / transcribe audio via the swarm's CPA endpoint. Runs cli/cpa-stt (sync /v1/audio/transcriptions, multipart).",
  "argument_hint": "<audio file path> [--model <id>] [--out transcript.txt]",
  "endpoint": "/v1/audio/transcriptions",
  "method": "POST",
  "mode": "sync",
  "output": "json",
  "default_model_key": "STT (speech-to-text)",
  "params": [
    {"name": "file", "required": true, "desc": "path to the audio file to transcribe (positional)"},
    {"name": "--model", "required": false, "desc": "override the default model id (e.g. whisper for the Azure path)"},
    {"name": "--out", "required": false, "desc": "write the transcript text to this file (default: stdout)"}
  ],
  "gotchas": [
    "multipart/form-data: the file goes as -F file=@<path>, the model as -F model=<id> (NOT a JSON body)",
    "response is JSON {text: ...}; the CLI prints .text (or writes it to --out)",
    "scribe_v2 (elevenlabs) gives Simplified Chinese; whisper returns Traditional — pick via --model if that matters"
  ]
}
```

## What it does

`cpa-stt` transcribes an audio file via CPA's `POST /v1/audio/transcriptions` (synchronous, multipart form). The CLI
sends the file + model as form fields and prints the returned transcript text.

## Request shape

```
POST {CPA}/v1/audio/transcriptions   (multipart/form-data)
Authorization: Bearer <CPA_KEY_CLI>
-F file=@<audio>  -F model=<default from frontier-aliases>
→ { "text": "…transcript…" }
```

## Notes

- The default model is the current strongest `STT (speech-to-text)` id from `frontier-aliases.json`; override with `--model`.
- Multipart (not JSON). Synchronous — no polling.
