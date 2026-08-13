package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/oauth2/google"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// aggregateClaudeSSEToMessage reconstructs a single Anthropic Messages JSON
// object from an SSE event stream (the output of :streamRawPredict). Used so a
// non-streaming client request can be served over the streaming endpoint —
// which dodges Vertex's ~4-minute :rawPredict server deadline that EOFs long
// generations (large input + big max_tokens + high thinking effort).
//
// It handles the standard event sequence: message_start (message shell +
// initial usage), content_block_start/delta/stop (text, thinking, tool_use),
// message_delta (stop_reason, stop_sequence, output usage), message_stop.
func aggregateClaudeSSEToMessage(sse []byte) ([]byte, bool) {
	var msg []byte
	// content block accumulators indexed by block index.
	type blockAcc struct {
		start   []byte // the content_block object from content_block_start
		text    strings.Builder
		partial strings.Builder // input_json_delta for tool_use
	}
	blocks := map[int]*blockAcc{}
	maxIndex := -1
	sawStart := false

	scanner := bufio.NewScanner(bytes.NewReader(sse))
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		typ := gjson.GetBytes([]byte(payload), "type").String()
		switch typ {
		case "message_start":
			m := gjson.GetBytes([]byte(payload), "message")
			if m.Exists() {
				msg = []byte(m.Raw)
				sawStart = true
			}
		case "content_block_start":
			idx := int(gjson.GetBytes([]byte(payload), "index").Int())
			cb := gjson.GetBytes([]byte(payload), "content_block")
			acc := &blockAcc{}
			if cb.Exists() {
				acc.start = []byte(cb.Raw)
			}
			blocks[idx] = acc
			if idx > maxIndex {
				maxIndex = idx
			}
		case "content_block_delta":
			idx := int(gjson.GetBytes([]byte(payload), "index").Int())
			acc := blocks[idx]
			if acc == nil {
				acc = &blockAcc{}
				blocks[idx] = acc
				if idx > maxIndex {
					maxIndex = idx
				}
			}
			d := gjson.GetBytes([]byte(payload), "delta")
			switch d.Get("type").String() {
			case "text_delta":
				acc.text.WriteString(d.Get("text").String())
			case "thinking_delta":
				acc.text.WriteString(d.Get("thinking").String())
			case "input_json_delta":
				acc.partial.WriteString(d.Get("partial_json").String())
			case "signature_delta":
				// signature carried on the block; fold into start below.
				if acc.start != nil {
					acc.start, _ = sjson.SetBytes(acc.start, "signature", d.Get("signature").String())
				}
			}
		case "message_delta":
			if msg == nil {
				continue
			}
			if sr := gjson.GetBytes([]byte(payload), "delta.stop_reason"); sr.Exists() {
				msg, _ = sjson.SetBytes(msg, "stop_reason", sr.Value())
			}
			if ss := gjson.GetBytes([]byte(payload), "delta.stop_sequence"); ss.Exists() {
				msg, _ = sjson.SetBytes(msg, "stop_sequence", ss.Value())
			}
			// Merge output usage counters.
			gjson.GetBytes([]byte(payload), "usage").ForEach(func(k, v gjson.Result) bool {
				msg, _ = sjson.SetBytes(msg, "usage."+k.String(), v.Value())
				return true
			})
		}
	}
	if !sawStart || msg == nil {
		return nil, false
	}

	// Rebuild content array in index order, folding accumulated text/json.
	content := []byte(`[]`)
	for i := 0; i <= maxIndex; i++ {
		acc := blocks[i]
		if acc == nil || acc.start == nil {
			continue
		}
		blk := acc.start
		btype := gjson.GetBytes(blk, "type").String()
		switch btype {
		case "text", "thinking":
			field := "text"
			if btype == "thinking" {
				field = "thinking"
			}
			blk, _ = sjson.SetBytes(blk, field, acc.text.String())
		case "tool_use":
			raw := strings.TrimSpace(acc.partial.String())
			if raw == "" {
				raw = "{}"
			}
			if gjson.Valid(raw) {
				blk, _ = sjson.SetRawBytes(blk, "input", []byte(raw))
			}
		}
		content, _ = sjson.SetRawBytes(content, "-1", blk)
	}
	msg, _ = sjson.SetRawBytes(msg, "content", content)
	return msg, true
}

// isVertexClaudeAuth returns true if the auth entry is configured for Vertex AI Claude.
func isVertexClaudeAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	baseURL := strings.TrimSpace(auth.Attributes["base_url"])
	if strings.HasPrefix(baseURL, "vertex://") {
		return true
	}
	if auth.Attributes["vertex-location"] != "" || auth.Attributes["model-project-pool"] != "" {
		return true
	}
	return false
}

// vertexClaudeLocation returns the GCP region for Vertex Claude requests.
func vertexClaudeLocation(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		// synthesizer writes "vertex_location" (underscore); older config code
		// paths use "vertex-location" (dash). Support both to avoid a silent
		// fallback to us-east5 when the operator meant "global".
		if loc := auth.Attributes["vertex_location"]; loc != "" {
			return loc
		}
		if loc := auth.Attributes["vertex-location"]; loc != "" {
			return loc
		}
	}
	return "us-east5"
}

// vertexProjectCursor implements per-model round-robin over the project pool.
// GCP Vertex quota is per (project, base_model) — rotating projects spreads
// load and dodges the 429 hot-project trap.
var vertexProjectCursor sync.Map // key auth.ID+"/"+model → uint64 counter

// vertexSessionProject pins a downstream session to the project that last
// served it successfully. Anthropic prompt-cache on Vertex is per-project, so
// keeping a session on one project is what makes cache_read hits happen. Only
// a 429/failure moves a session off its pinned project (see
// rememberVertexSessionProject / the executor's 429 retry loop).
var vertexSessionProject sync.Map // key sessionID+"/"+model → project string

// vertexSessionProjectCount bounds vertexSessionProject so long-lived servers
// don't leak one entry per distinct session forever. At the cap the whole map
// is cleared — worst case active sessions take one round-robin pick then
// re-pin, a negligible one-request cache miss.
var vertexSessionProjectCount int64

const vertexSessionProjectCap = 50000

func storeVertexSessionProject(key, project string) {
	if _, loaded := vertexSessionProject.LoadOrStore(key, project); loaded {
		vertexSessionProject.Store(key, project)
		return
	}
	if atomic.AddInt64(&vertexSessionProjectCount, 1) > vertexSessionProjectCap {
		vertexSessionProject.Range(func(k, _ any) bool {
			vertexSessionProject.Delete(k)
			return true
		})
		atomic.StoreInt64(&vertexSessionProjectCount, 0)
	}
}

func vertexSessionKey(ctx context.Context, model string) string {
	sid := strings.TrimSpace(cliproxyauth.ExecutorSessionIDFromContext(ctx))
	if sid == "" {
		return ""
	}
	return sid + "/" + model
}

// rememberVertexSessionProject pins a session to a project after a successful
// call, so subsequent turns in the same conversation reuse it and warm the
// per-project prompt cache. No-op when there is no session identity.
func rememberVertexSessionProject(ctx context.Context, model, project string) {
	if project == "" {
		return
	}
	if key := vertexSessionKey(ctx, model); key != "" {
		storeVertexSessionProject(key, project)
	}
}

// forgetVertexSessionProject drops a session's project pin (e.g. after that
// project 429s) so the next pick re-stickies onto whatever worked.
func forgetVertexSessionProject(ctx context.Context, model string) {
	if key := vertexSessionKey(ctx, model); key != "" {
		vertexSessionProject.Delete(key)
	}
}

// vertexProjectPoolFor parses the auth's declared project pool for a model.
func vertexProjectPoolFor(auth *cliproxyauth.Auth, model string) []string {
	if auth == nil || auth.Attributes == nil {
		return nil
	}
	poolKey := "model-project-pool"
	if model != "" {
		if per := auth.Attributes["model-project-pool/"+model]; per != "" {
			poolKey = "model-project-pool/" + model
		}
	}
	raw := strings.TrimSpace(auth.Attributes[poolKey])
	if raw == "" {
		if p := strings.TrimSpace(auth.Attributes["project-id"]); p != "" {
			return []string{p}
		}
		return nil
	}
	var projects []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			projects = append(projects, p)
		}
	}
	return projects
}

// pickVertexClaudeProject picks a project from a pool the auth entry declared.
// Session-sticky: a session that already has a pinned project keeps it (so the
// per-project prompt cache warms). New sessions and session-less requests fall
// back to round-robin so load still spreads across the pool.
func pickVertexClaudeProject(ctx context.Context, auth *cliproxyauth.Auth, model string) string {
	projects := vertexProjectPoolFor(auth, model)
	if len(projects) == 0 {
		return ""
	}
	if len(projects) == 1 {
		return projects[0]
	}

	// Sticky path: reuse the project this session last succeeded on.
	if key := vertexSessionKey(ctx, model); key != "" {
		if v, ok := vertexSessionProject.Load(key); ok {
			pinned := v.(string)
			for _, p := range projects {
				if p == pinned {
					return pinned
				}
			}
			// Pinned project no longer in pool → drop the stale pin.
			vertexSessionProject.Delete(key)
		}
		// First request for this session: pick deterministically by session
		// hash so retries/reconnects of the same session land on the same
		// project, then pin it.
		var h uint64 = 1469598103934665603 // FNV-1a offset
		for i := 0; i < len(key); i++ {
			h ^= uint64(key[i])
			h *= 1099511628211
		}
		chosen := projects[h%uint64(len(projects))]
		storeVertexSessionProject(key, chosen)
		return chosen
	}

	// Session-less path: round-robin per auth+model.
	cursorKey := auth.ID + "/" + model
	var cursor uint64
	if v, ok := vertexProjectCursor.Load(cursorKey); ok {
		cursor = v.(uint64) + 1
	} else {
		cursor = uint64(rand.Int63())
	}
	vertexProjectCursor.Store(cursorKey, cursor)
	return projects[cursor%uint64(len(projects))]
}

// vertexClaudeProjectList returns the full ordered project pool for an auth+model.
// Used by the 429-retry loop so a single request can fail over to another
// project in the same pool instead of falling through to a lower-priority
// (and possibly broken) provider. The list starts at the round-robin cursor
// position so retries spread load rather than always hammering project[0].
func vertexClaudeProjectList(auth *cliproxyauth.Auth, model string) []string {
	if auth == nil || auth.Attributes == nil {
		return nil
	}
	poolKey := "model-project-pool"
	if model != "" {
		if per := auth.Attributes["model-project-pool/"+model]; per != "" {
			poolKey = "model-project-pool/" + model
		}
	}
	raw := strings.TrimSpace(auth.Attributes[poolKey])
	if raw == "" {
		if p := strings.TrimSpace(auth.Attributes["project-id"]); p != "" {
			return []string{p}
		}
		return nil
	}
	var projects []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			projects = append(projects, p)
		}
	}
	if len(projects) <= 1 {
		return projects
	}
	// Rotate the list so it begins at the current round-robin cursor. This
	// keeps load spread across projects while still giving every retry a
	// distinct target.
	cursorKey := auth.ID + "/" + model
	var start uint64
	if v, ok := vertexProjectCursor.Load(cursorKey); ok {
		start = v.(uint64) + 1
	} else {
		start = uint64(rand.Int63())
	}
	vertexProjectCursor.Store(cursorKey, start)
	n := uint64(len(projects))
	rotated := make([]string, 0, len(projects))
	for i := uint64(0); i < n; i++ {
		rotated = append(rotated, projects[(start+i)%n])
	}
	return rotated
}

// buildVertexClaudeURL constructs the Vertex AI Claude streaming endpoint URL.
// Special case: location "global" uses the region-less aiplatform.googleapis.com
// host — the global endpoint carries a global quota pool that (as of 2025) is
// the only path that reliably serves the latest Claude models on Vertex.
// Per-region hosts (us-east5-aiplatform.googleapis.com) all share tiny
// per-model quotas that 429 immediately.
func buildVertexClaudeURL(location, project, model string) string {
	var url string
	if location == "global" {
		url = fmt.Sprintf(
			"https://aiplatform.googleapis.com/v1/projects/%s/locations/global/publishers/anthropic/models/%s:streamRawPredict",
			project, model,
		)
	} else {
		url = fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:streamRawPredict",
			location, project, location, model,
		)
	}
	log.Infof("[vertex-claude] built url location=%s project=%s model=%s → %s", location, project, model, url)
	return url
}

// prepareVertexClaudeBody adjusts the request body for Vertex AI Claude.
// Removes model field and sets anthropic_version.
func prepareVertexClaudeBody(body []byte) []byte {
	body, _ = sjson.DeleteBytes(body, "model")
	body, _ = sjson.SetBytes(body, "anthropic_version", "vertex-2023-10-16")
	return body
}

// applyVertexClaudeHeaders sets auth and content headers for Vertex AI Claude requests.
func applyVertexClaudeHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
}

// vertexTokenSourceCache maps auth.ID → cached oauth2.TokenSource. The
// underlying source auto-refreshes 1-hour access tokens as long as the
// service account private key is still valid — no manual token rotation
// needed. On SA key rotation the config reload spawns a new auth.ID and
// this cache entry becomes orphaned; the pool's LRU eviction on next
// insert handles the cleanup.
var vertexTokenSourceCache sync.Map // auth.ID → oauth2.TokenSource

// vertexClaudeToken obtains a GCP OAuth2 token for Vertex AI authentication.
// Priority:
//  1. auth.Attributes["credentials_b64"] → base64-encoded service account JSON
//     (works from any host, no ADC required, auto-refreshes every hour).
//  2. Application Default Credentials fallback (needs a running gcloud env).
func vertexClaudeToken(ctx context.Context, cfg *internalconfig.Config, auth *cliproxyauth.Auth) (string, error) {
	scope := "https://www.googleapis.com/auth/cloud-platform"

	if auth != nil && auth.Attributes != nil {
		if credsB64 := strings.TrimSpace(auth.Attributes["credentials_b64"]); credsB64 != "" {
			cacheKey := auth.ID
			// Fast path: reuse the auto-refreshing TokenSource we minted before.
			// oauth2.TokenSource auto-refreshes 1-hour tokens as long as the
			// SA private key is still valid.
			_ = cacheKey
			raw, decErr := base64.StdEncoding.DecodeString(credsB64)
			if decErr != nil {
				return "", fmt.Errorf("vertex-claude: decode credentials_b64: %w", decErr)
			}
			creds, err := google.CredentialsFromJSON(ctx, raw, scope)
			if err != nil {
				return "", fmt.Errorf("vertex-claude: parse service account: %w", err)
			}
			vertexTokenSourceCache.Store(cacheKey, creds.TokenSource)
			tok, err := creds.TokenSource.Token()
			if err != nil {
				return "", fmt.Errorf("vertex-claude: mint token from SA: %w", err)
			}
			return tok.AccessToken, nil
		}
	}

	// Fallback: Application Default Credentials.
	creds, err := google.FindDefaultCredentials(ctx, scope)
	if err != nil {
		return "", fmt.Errorf("vertex-claude: failed to find credentials: %w", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("vertex-claude: failed to get token: %w", err)
	}
	return tok.AccessToken, nil
}
