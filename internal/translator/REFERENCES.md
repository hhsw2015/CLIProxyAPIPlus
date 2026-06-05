# Protocol References

CPA translates between five LLM API protocols. When the upstream spec changes,
read the official documentation first — do not infer behavior from packet
captures or other implementations.

## Protocols and their canonical specs

| Protocol | Endpoint shape | CPA package |
|---|---|---|
| OpenAI Chat Completions | `POST /v1/chat/completions` | `openai/` |
| OpenAI Responses | `POST /v1/responses` | `openai/` (alt path) |
| Anthropic Messages | `POST /v1/messages` | `claude/` |
| Google Gemini | `POST /v1{,beta}/models/{model}:generateContent` | `gemini/`, `gemini-cli/` |
| OpenAI-compatible variants (Mistral, xAI, DeepSeek, Groq, OpenRouter, etc.) | `POST /v1/chat/completions` (with provider quirks) | translator selected per executor |

Internal-only packages:

| Package | What it is |
|---|---|
| `codex/` | OpenAI Codex CLI's Responses-API dialect (UA spoofing, encrypted reasoning, prompt cache headers) |
| `kiro/` | Amazon Kiro's Claude-Messages dialect (RemoteWebSearch via MCP, profile ARN routing) |
| `antigravity/` | Google Antigravity's Gemini dialect (thought signatures) |
| `common/` | Shared helpers (tokenizers, schema cleanup) |
| `translator/` | Format registration + dispatch; entrypoint for `sdktranslator.TranslateRequest`/`TranslateResponse` |

## Official specifications

### OpenAI

- Chat Completions API — https://platform.openai.com/docs/api-reference/chat
- Responses API — https://platform.openai.com/docs/api-reference/responses
- Migration guide (Chat → Responses) — https://platform.openai.com/docs/guides/migrate-to-responses
- Embeddings API — https://platform.openai.com/docs/api-reference/embeddings
- Models API — https://platform.openai.com/docs/api-reference/models

### Anthropic Claude

- Messages API — https://platform.claude.com/docs/en/api/messages
- Working with Messages — https://platform.claude.com/docs/en/build-with-claude/working-with-messages
- Streaming Messages — https://platform.claude.com/docs/en/api/messages-streaming

### Google Gemini

- API versions (`v1` vs `v1beta`) — https://ai.google.dev/gemini-api/docs/api-versions
- generateContent reference — https://ai.google.dev/api/generate-content
- Documentation index — https://ai.google.dev/gemini-api/docs

### OpenAI-compatible providers

- Mistral — https://docs.mistral.ai/api
- xAI — https://docs.x.ai/developers/rest-api-reference/inference/chat
- DeepSeek — https://api-docs.deepseek.com/api/create-chat-completion/
- Groq OpenAI compatibility — https://console.groq.com/docs/openai
- OpenRouter Chat Completions — https://openrouter.ai/docs/api/api-reference/chat/send-chat-completion-request

## Working notes

- **Compatibility is layered.** Most "OpenAI-compatible" backends only cover
  Level 1 (basic chat). Tools, structured outputs, vision, streaming tool-call
  deltas, reasoning tokens, cached tokens — verify per provider before
  promoting a translator path.
- **`/v1` is a protocol version, not a model version.** Don't conflate API
  versioning (request/response shape, error format, streaming events) with
  model upgrades. We pin the protocol version per-provider in the translator,
  not in the model registry.
- **Anthropic top-level `system`.** Claude sends `system` as a top-level field;
  OpenAI inlines it as a message. This is the most common cross-protocol bug
  — do not silently merge them.
- **Gemini `parts` ≠ OpenAI `content`.** Gemini's `contents[].parts[]` is a
  flat array of typed parts (text/image/inline data). Cannot be 1:1 mapped to
  OpenAI's `content` string.
- **Responses is stateful.** OpenAI Responses can return multiple `output`
  items (text, tool_use, reasoning, file_search results). Don't assume a
  single assistant message.
- **Stream events are NOT a stable cross-protocol shape.** Each protocol has
  its own SSE event taxonomy:
  - OpenAI: `data: {choices: [{delta: ...}]}` + `[DONE]`
  - Anthropic: `event: message_start | content_block_{start,delta,stop} | message_{delta,stop}`
  - Responses: `response.output_item.added`/`response.completed`/etc.

## When to update this file

- A translator package gains a new dialect (e.g. a new vendor variant of
  Chat Completions). Add a row in the table.
- An official URL moves. Update the link.
- A protocol introduces a new event type or field shape that we depend on.
  Add a one-line note under "Working notes".

This file is reference material for translator maintainers. It is **not** a
how-to guide and should not grow into one.
