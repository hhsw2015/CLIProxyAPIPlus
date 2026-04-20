package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/sjson"
	"golang.org/x/oauth2/google"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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
		if loc := auth.Attributes["vertex-location"]; loc != "" {
			return loc
		}
	}
	return "us-east5"
}

// pickVertexClaudeProject selects a project ID from the model-project-pool attribute.
func pickVertexClaudeProject(auth *cliproxyauth.Auth, model string) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	pool := auth.Attributes["model-project-pool"]
	if pool == "" {
		return auth.Attributes["project-id"]
	}
	// Pool format: {"project1":limit1,"project2":limit2,...}
	// Simple: pick first project from the JSON map
	pool = strings.TrimSpace(pool)
	if !strings.HasPrefix(pool, "{") {
		return pool
	}
	// Extract first key
	pool = strings.TrimPrefix(pool, "{")
	idx := strings.Index(pool, "\"")
	if idx < 0 {
		return ""
	}
	pool = pool[idx+1:]
	end := strings.Index(pool, "\"")
	if end < 0 {
		return ""
	}
	return pool[:end]
}

// buildVertexClaudeURL constructs the Vertex AI Claude streaming endpoint URL.
func buildVertexClaudeURL(location, project, model string) string {
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:streamRawPredict",
		location, project, location, model,
	)
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

// vertexClaudeToken obtains a GCP OAuth2 token for Vertex AI authentication.
func vertexClaudeToken(ctx context.Context, cfg *internalconfig.Config, auth *cliproxyauth.Auth) (string, error) {
	// Use Application Default Credentials (ADC)
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", fmt.Errorf("vertex-claude: failed to find credentials: %w", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("vertex-claude: failed to get token: %w", err)
	}
	return tok.AccessToken, nil
}
