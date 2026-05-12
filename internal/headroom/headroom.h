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

// Compress OpenAI /v1/responses body (Responses API — Codex format).
char* headroom_compress_openai_responses(
    const uint8_t* body,
    size_t body_len,
    const char* model,
    uint8_t auth_mode
);

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
