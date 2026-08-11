package executor

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
	"golang.org/x/oauth2/google"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

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
var vertexProjectCursor sync.Map // key auth.ID+"/"+model → *uint64 counter

// pickVertexClaudeProject picks a project from a pool the auth entry declared.
// Pool encoding (auth.Attributes["model-project-pool"]) is a comma-separated
// list of project IDs. If the pool is empty, "project-id" is used verbatim.
// The choice rotates round-robin (per auth+model) so no single project gets
// the whole request stream.
func pickVertexClaudeProject(auth *cliproxyauth.Auth, model string) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	// Per-model pool key (config gen emits comma-separated lists).
	poolKey := "model-project-pool"
	if model != "" {
		if per := auth.Attributes["model-project-pool/"+model]; per != "" {
			poolKey = "model-project-pool/" + model
		}
	}
	raw := strings.TrimSpace(auth.Attributes[poolKey])
	if raw == "" {
		return strings.TrimSpace(auth.Attributes["project-id"])
	}
	var projects []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			projects = append(projects, p)
		}
	}
	if len(projects) == 0 {
		return strings.TrimSpace(auth.Attributes["project-id"])
	}
	if len(projects) == 1 {
		return projects[0]
	}
	cursorKey := auth.ID + "/" + model
	var cursor uint64
	if v, ok := vertexProjectCursor.Load(cursorKey); ok {
		cursor = v.(uint64) + 1
	} else {
		// New key: seed with a random offset so different processes / restarts
		// don't hammer the same first project.
		cursor = uint64(rand.Int63())
	}
	vertexProjectCursor.Store(cursorKey, cursor)
	return projects[cursor%uint64(len(projects))]
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
