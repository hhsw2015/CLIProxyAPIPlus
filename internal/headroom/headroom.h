// headroom.h - C FFI bindings for headroom-core compression.
// Generated manually based on lib.rs signatures.
#ifndef __HEADROOM_H__
#define __HEADROOM_H__

#include <stdint.h>
#include <stddef.h>

// Auth modes (match headroom-core AuthMode enum order)
#define HEADROOM_AUTH_PAYG 0
#define HEADROOM_AUTH_OAUTH 1
#define HEADROOM_AUTH_SUBSCRIPTION 2
#define HEADROOM_AUTH_UNKNOWN 3

// Compress OpenAI /v1/chat/completions body.
// body: JSON bytes (not null-terminated)
// body_len: byte length of body
// model: null-terminated model name
// auth_mode: one of HEADROOM_AUTH_* constants
// Returns: heap-allocated JSON result string. Caller MUST free with
//          headroom_result_free() — never libc free().
char* headroom_compress_openai(
    const uint8_t* body,
    size_t body_len,
    const char* model,
    uint8_t auth_mode
);

// Compress Anthropic /v1/messages body.
// frozen_count: number of pinned messages from conversation start.
char* headroom_compress_anthropic(
    const uint8_t* body,
    size_t body_len,
    size_t frozen_count,
    const char* model,
    uint8_t auth_mode
);

// Compress OpenAI /v1/chat/completions body — full-history mode.
// Walks every message and crushes role=tool string content + nested
// tool_result blocks. Mirrors headroom-py SmartCrusher.apply.
char* headroom_compress_openai_full(
    const uint8_t* body,
    size_t body_len,
    const char* model,
    uint8_t auth_mode
);

// Compress OpenAI /v1/responses body — full-history mode.
// Walks every input item and crushes function_call_output / local_shell_call_output /
// apply_patch_call_output. Skips items whose call_id matches a headroom_retrieve
// function_call (those must reach the model byte-for-byte).
char* headroom_compress_openai_responses_full(
    const uint8_t* body,
    size_t body_len,
    const char* model,
    uint8_t auth_mode
);

// Compress Anthropic /v1/messages body in full-history mode.
// Walks every message past frozen_count and crushes every tool_result block
// individually (mirrors headroom-py SmartCrusher.apply, not Phase B
// live-zone semantics). Prefer this for Claude Code / agent traffic.
char* headroom_compress_anthropic_full(
    const uint8_t* body,
    size_t body_len,
    size_t frozen_count,
    const char* model,
    uint8_t auth_mode
);

// Compress OpenAI /v1/responses body (Responses API — Codex format).
char* headroom_compress_openai_responses(
    const uint8_t* body,
    size_t body_len,
    const char* model,
    uint8_t auth_mode
);

// Normalize Anthropic /v1/messages tool definitions in place (PR-E1 + PR-E2).
//   PR-E1: sort tools[] alphabetically by name (skipped when any tool already
//          carries cache_control — preserves customer-intentional order).
//   PR-E2: recursively sort tool.input_schema object keys (always runs when
//          tools exist; safe under cache_control because it never moves the
//          marker).
// auth_mode: must be HEADROOM_AUTH_PAYG; other modes pass through unchanged
//            (mutating bytes under OAuth/Subscription can look like
//            cache-evasion to upstreams).
// Returns: heap-allocated JSON {modified, body, e1_applied, e2_applied, error}.
// Caller MUST free with headroom_result_free().
char* headroom_normalize_anthropic_tools(
    const uint8_t* body,
    size_t body_len,
    uint8_t auth_mode);

// Retrieve original content from CCR store by hash.
// Returns: heap-allocated JSON of shape {found, content, error}.
// Caller MUST free with headroom_result_free().
char* headroom_ccr_get(const char* hash);

// Initialize a persistent SQLite-backed CCR store at the given path.
// ttl_seconds: entry lifetime; 0 means use the headroom default (300s).
// Returns: NULL on success, or a heap-allocated error message on failure.
// On success the previous backend is replaced atomically. In-flight calls
// still holding the old store continue with it until they finish.
char* headroom_ccr_init_sqlite(const char* path, unsigned long long ttl_seconds);

// Initialize a Redis-backed CCR store at the given URL (for fleet-wide
// shared CCR across multiple CPA instances).
// key_prefix: namespace prefix (NULL = headroom default).
// ttl_seconds: entry lifetime; 0 = headroom default (300s).
// Same atomic-swap semantics as headroom_ccr_init_sqlite.
char* headroom_ccr_init_redis(
    const char* url,
    const char* key_prefix,
    unsigned long long ttl_seconds);

// Free a string returned by any headroom_* FFI call. Passing NULL is safe;
// calling libc free() on these pointers is undefined behavior.
void headroom_result_free(char* ptr);

#endif // __HEADROOM_H__
