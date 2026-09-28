package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// VertexMaaSExecutor serves Vertex AI "Model-as-a-Service" open + partner models
// (DeepSeek / Qwen / GLM / gpt-oss / Kimi / MiniMax / xAI Grok / ...) exposed
// through the Vertex OpenAI-compatible endpoint:
//
//	POST {host}/v1/projects/{project}/locations/{loc}/endpoints/openapi/chat/completions
//
// It reuses OpenAICompatExecutor for all request/response translation and
// streaming, and only injects Vertex auth per request: mint the SA OAuth token,
// pick a project from the model-project-pool (the SA's own project may not have
// aiplatform enabled), and point the base URL at the Vertex openapi path. The SA
// itself comes from a keyless gemini-api-key config entry whose model names are
// publisher-prefixed (e.g. "xai/grok-4.20-reasoning") — the synthesizer routes
// those to Provider "vertex-maas".
type VertexMaaSExecutor struct {
	cfg   *config.Config
	inner *OpenAICompatExecutor
}

// NewVertexMaaSExecutor creates a Vertex MaaS executor bound to the shared config.
func NewVertexMaaSExecutor(cfg *config.Config) *VertexMaaSExecutor {
	return &VertexMaaSExecutor{cfg: cfg, inner: NewOpenAICompatExecutor("vertex-maas", cfg)}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *VertexMaaSExecutor) Identifier() string { return "vertex-maas" }

// injectVertexAuth returns a shallow copy of auth with base_url + api_key set to
// a freshly minted Vertex openapi endpoint + SA token for the pooled project, so
// the inner OpenAICompat executor can run unmodified.
func (e *VertexMaaSExecutor) injectVertexAuth(ctx context.Context, auth *cliproxyauth.Auth, model string) (*cliproxyauth.Auth, error) {
	projectID, location, saJSON, errCreds := vertexCreds(auth)
	if errCreds != nil {
		return nil, errCreds
	}
	pooled := pickVertexClaudeProject(ctx, auth, thinking.ParseSuffix(model).ModelName)
	if pooled == "" {
		// No per-model pool (e.g. HttpRequest passthrough has no model) — fall back
		// to any configured pool project so we never hit the (often disabled) SA
		// home project.
		pooled = anyVertexMaaSPoolProject(auth)
	}
	if pooled != "" {
		projectID = pooled
	}
	token, errTok := vertexAccessToken(ctx, e.cfg, auth, saJSON)
	if errTok != nil {
		return nil, errTok
	}
	if strings.TrimSpace(token) == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "vertex-maas: empty access token"}
	}
	baseURL := vertexBaseURL(location) + "/v1/projects/" + projectID + "/locations/" + location + "/endpoints/openapi"
	a2 := *auth
	attrs := make(map[string]string, len(auth.Attributes)+2)
	for k, v := range auth.Attributes {
		attrs[k] = v
	}
	// Drop config_index so the inner OpenAICompat executor's resolveCompatConfig
	// does not mis-index cfg.OpenAICompatibility with our gemini-key index.
	delete(attrs, "config_index")
	attrs["base_url"] = baseURL
	attrs["api_key"] = token
	a2.Attributes = attrs
	return &a2, nil
}

// Execute performs a non-streaming MaaS request via the inner OpenAI-compat path.
func (e *VertexMaaSExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	a2, err := e.injectVertexAuth(ctx, auth, req.Model)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return e.inner.Execute(ctx, a2, req, opts)
}

// ExecuteStream performs a streaming MaaS request via the inner OpenAI-compat path.
func (e *VertexMaaSExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	a2, err := e.injectVertexAuth(ctx, auth, req.Model)
	if err != nil {
		return nil, err
	}
	return e.inner.ExecuteStream(ctx, a2, req, opts)
}

// CountTokens counts tokens locally (tokenizer); no upstream call, no auth needed.
func (e *VertexMaaSExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.inner.CountTokens(ctx, auth, req, opts)
}

// HttpRequest injects Vertex auth and delegates to the inner OpenAI-compat path.
// Used for generic passthrough (no model context), so it relies on the model-less
// pool fallback in injectVertexAuth.
func (e *VertexMaaSExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	a2, err := e.injectVertexAuth(ctx, auth, "")
	if err != nil {
		return nil, err
	}
	return e.inner.HttpRequest(ctx, a2, req)
}

// Refresh is a no-op: Vertex SA tokens are minted per request in injectVertexAuth.
func (e *VertexMaaSExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

// anyVertexMaaSPoolProject returns any project from the auth's flattened
// model-project-pool attributes (used when no model context is available).
func anyVertexMaaSPoolProject(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	for k, v := range auth.Attributes {
		if strings.HasPrefix(k, "model-project-pool/") {
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					return p
				}
			}
		}
	}
	return ""
}
