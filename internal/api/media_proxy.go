package api

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// mediaEndpoint defines a supported media API type.
type mediaEndpoint struct {
	// pathSuffix is the Azure deployment path suffix (e.g., "images/generations").
	pathSuffix string
	// contentType expected in the request.
	contentType string
	// isMultipart indicates if the request uses multipart/form-data (e.g., whisper).
	isMultipart bool
}

var (
	mediaImageGen   = mediaEndpoint{pathSuffix: "images/generations", contentType: "application/json"}
	mediaImageEdit  = mediaEndpoint{pathSuffix: "images/edits", contentType: "", isMultipart: true}
	mediaAudioTTS   = mediaEndpoint{pathSuffix: "audio/speech", contentType: "application/json"}
	mediaAudioSTT   = mediaEndpoint{pathSuffix: "audio/transcriptions", contentType: "", isMultipart: true}
	mediaAudioTrans = mediaEndpoint{pathSuffix: "audio/translations", contentType: "", isMultipart: true}
	mediaEmbeddings = mediaEndpoint{pathSuffix: "embeddings", contentType: "application/json"}
)

// mediaProviderConfig holds the resolved upstream provider details.
type mediaProviderConfig struct {
	baseURL string
	apiKey  string
}

// setupMediaRoutes registers media API proxy routes on the given router group.
func (s *Server) setupMediaRoutes(v1 *gin.RouterGroup) {
	v1.POST("/images/generations", s.mediaProxyHandler(mediaImageGen))
	v1.POST("/images/edits", s.mediaProxyHandler(mediaImageEdit))
	v1.POST("/audio/speech", s.mediaProxyHandler(mediaAudioTTS))
	v1.POST("/audio/transcriptions", s.mediaProxyHandler(mediaAudioSTT))
	v1.POST("/audio/translations", s.mediaProxyHandler(mediaAudioTrans))
	v1.POST("/embeddings", s.mediaProxyHandler(mediaEmbeddings))
}

// mediaProxyHandler returns a gin handler that transparently proxies media requests
// to the upstream provider (Azure, OpenAI, etc.).
func (s *Server) mediaProxyHandler(ep mediaEndpoint) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": fmt.Sprintf("failed to read request body: %v", err),
				"type":    "invalid_request_error",
			}})
			return
		}

		// Extract model name from JSON body or form field.
		modelName := ""
		if ep.isMultipart {
			// Model lives in a multipart form field. GetRawData already drained the
			// body, so gin's PostForm can't re-parse it; parse the captured raw body
			// with the FULL Content-Type header. c.ContentType() drops the
			// "; boundary=..." parameter, which makes multipart parsing fail and the
			// model silently fall back to "whisper" (wrong provider -> Azure 404).
			modelName = extractModelFromMultipart(body, c.GetHeader("Content-Type"))
			if modelName == "" {
				modelName = c.PostForm("model")
			}
			// Some callers send a JSON body to /images/edits; honor its model field
			// instead of defaulting to an unrelated media provider.
			if modelName == "" {
				modelName = gjson.GetBytes(body, "model").String()
			}
			if modelName == "" {
				modelName = "whisper"
			}
		} else {
			modelName = gjson.GetBytes(body, "model").String()
		}
		if modelName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": "model field is required",
				"type":    "invalid_request_error",
			}})
			return
		}

		// Vertex Imagen (model="imagen-*") requires SA OAuth + body translation
		// (OpenAI images/generations → Vertex :predict). Detected before the
		// generic OpenAI-compat resolver since imagen entries live in the
		// gemini-api-key section with credentials-b64, not openai-compatibility.
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelName)), "imagen-") && ep.pathSuffix == "images/generations" {
			if s.handleVertexImagen(c, modelName, body) {
				return
			}
		}

		// Find provider config for this model + endpoint type.
		provider := s.resolveMediaProvider(modelName, ep)
		if provider == nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
				"message": fmt.Sprintf("no media provider configured for model %s", modelName),
				"type":    "server_error",
			}})
			return
		}

		// Build upstream URL.
		upstreamURL := provider.baseURL

		// Create upstream request.
		upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
				"message": fmt.Sprintf("failed to create upstream request: %v", err),
				"type":    "server_error",
			}})
			return
		}

		// Copy Content-Type from the original request (preserves multipart boundary).
		if ct := c.GetHeader("Content-Type"); ct != "" {
			upstreamReq.Header.Set("Content-Type", ct)
		} else if ep.contentType != "" {
			upstreamReq.Header.Set("Content-Type", ep.contentType)
		}

		// Set provider auth header.
		if provider.apiKey != "" {
			upstreamReq.Header.Set("api-key", provider.apiKey)
		}

		// Send request.
		client := &http.Client{}
		resp, err := client.Do(upstreamReq)
		if err != nil {
			log.Errorf("media proxy: upstream request failed for %s: %v", modelName, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
				"message": fmt.Sprintf("upstream request failed: %v", err),
				"type":    "server_error",
			}})
			return
		}
		defer resp.Body.Close()

		// Forward response headers.
		for k, vals := range resp.Header {
			for _, v := range vals {
				c.Writer.Header().Add(k, v)
			}
		}
		c.Writer.WriteHeader(resp.StatusCode)

		// Stream response body to client.
		if _, err := io.Copy(c.Writer, resp.Body); err != nil {
			log.Errorf("media proxy: failed to copy response body: %v", err)
		}
	}
}

// resolveMediaProvider finds the appropriate upstream provider for a media model.
// It searches openai-compatibility entries that have this model configured,
// and builds the correct upstream URL.
func (s *Server) resolveMediaProvider(modelName string, ep mediaEndpoint) *mediaProviderConfig {
	if s.cfg == nil {
		return nil
	}

	// Search dedicated media entries in openai-compatibility config.
	// Convention: entries with name prefix "media-" are media providers.
	// Also search entries whose models list includes the requested model.
	for _, compat := range s.cfg.OpenAICompatibility {
		for _, m := range compat.Models {
			name := strings.TrimSpace(m.Name)
			alias := strings.TrimSpace(m.Alias)
			if !strings.EqualFold(name, modelName) && !strings.EqualFold(alias, modelName) {
				continue
			}

			// Found a matching entry. Build URL.
			baseURL := strings.TrimSpace(compat.BaseURL)
			apiKey := ""
			if len(compat.APIKeyEntries) > 0 {
				apiKey = strings.TrimSpace(compat.APIKeyEntries[0].APIKey)
			}
			// Also check headers for api-key (Azure style).
			if v, ok := compat.Headers["api-key"]; ok && apiKey == "" {
				apiKey = v
			}
			if apiKey == "" {
				if v, ok := compat.Headers["api-key"]; ok {
					apiKey = v
				}
			}

			// If the base URL already contains the media path, use as-is.
			if strings.Contains(baseURL, "/"+ep.pathSuffix) {
				return &mediaProviderConfig{baseURL: baseURL, apiKey: apiKey}
			}

			// Otherwise, construct Azure-style URL:
			// base-url is typically a full Azure endpoint like
			//   https://host/openai/deployments/{dep}/images/generations?api-version=...
			// Rebuild it to target ep.pathSuffix while preserving the query string
			// (Azure requires ?api-version=...). It must operate on the URL path, not
			// the raw string: naive concatenation places the new path after the query
			// (".../images/generations?api-version=X/images/edits"), which corrupts both
			// the path and the api-version and makes Azure return 404.
			if strings.Contains(baseURL, "/openai/deployments/") {
				if u, errParse := url.Parse(baseURL); errParse == nil && u.Path != "" {
					p := strings.TrimSuffix(u.Path, "/")
					for _, suffix := range []string{"/chat/completions", "/images/generations", "/images/edits", "/audio/transcriptions", "/audio/translations", "/audio/speech"} {
						if strings.HasSuffix(p, suffix) {
							p = strings.TrimSuffix(p, suffix)
							break
						}
					}
					u.Path = p + "/" + ep.pathSuffix
					// Azure gpt-image edits only exist on api-version 2025-04-01-preview;
					// older configured versions (e.g. 2024-02-01, used for generations)
					// return 404 on /images/edits. Bump just the edits endpoint so
					// generations keeps its configured version.
					// ponytail: pinned version; revisit if Azure GAs image edits.
					if ep.pathSuffix == "images/edits" && strings.HasPrefix(strings.ToLower(modelName), "gpt-image") {
						q := u.Query()
						if q.Get("api-version") != "" {
							q.Set("api-version", "2025-04-01-preview")
							u.RawQuery = q.Encode()
						}
					}
					return &mediaProviderConfig{baseURL: u.String(), apiKey: apiKey}
				}
				// Fallback: legacy string concatenation (no query present).
				trimmed := strings.TrimSuffix(baseURL, "/")
				trimmed = strings.TrimSuffix(trimmed, "/chat/completions")
				return &mediaProviderConfig{
					baseURL: trimmed + "/" + ep.pathSuffix,
					apiKey:  apiKey,
				}
			}

			// Generic fallback: append /v1/{pathSuffix}.
			trimmed := strings.TrimSuffix(baseURL, "/")
			return &mediaProviderConfig{
				baseURL: trimmed + "/" + ep.pathSuffix,
				apiKey:  apiKey,
			}
		}
	}

	return nil
}

// setupGptProxyRoutes registers a catch-all proxy for gpt-proxy media endpoints.
// This handles both submit (POST) and poll (GET) requests transparently.
func (s *Server) setupGptProxyRoutes(engine *gin.Engine) {
	// Catch-all for gpt-proxy media routes.
	// Clients use the same paths as gpt-proxy, CPA just forwards.
	proxy := engine.Group("/gpt-proxy")
	proxy.Use(s.proxyAuthMiddleware())
	proxy.Any("/*path", s.gptProxyPassthrough())
}

// gptProxyPassthrough transparently forwards requests to the local gpt-proxy
// via chisel tunnel (127.0.0.1:19900).
func (s *Server) gptProxyPassthrough() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Param("path")
		upstreamURL := "http://127.0.0.1:19900/gpt-proxy" + path
		if c.Request.URL.RawQuery != "" {
			upstreamURL += "?" + c.Request.URL.RawQuery
		}

		var bodyReader io.Reader
		if c.Request.Body != nil {
			body, err := io.ReadAll(c.Request.Body)
			if err == nil && len(body) > 0 {
				bodyReader = bytes.NewReader(body)
			}
		}

		upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, bodyReader)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream request"})
			return
		}

		// Forward Content-Type and auth headers.
		if ct := c.GetHeader("Content-Type"); ct != "" {
			upstreamReq.Header.Set("Content-Type", ct)
		}
		// Set gpt-proxy auth.
		upstreamReq.Header.Set("app_key", "gpt-5739025d9e453d483a6595f95591")

		resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(upstreamReq)
		if err != nil {
			log.Errorf("gpt-proxy passthrough: request failed: %v", err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
			return
		}
		defer resp.Body.Close()

		for k, vals := range resp.Header {
			for _, v := range vals {
				c.Writer.Header().Add(k, v)
			}
		}
		c.Writer.WriteHeader(resp.StatusCode)
		io.Copy(c.Writer, resp.Body)
	}
}

// extractModelFromMultipart extracts the "model" form field from a multipart body.
func extractModelFromMultipart(body []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return ""
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == "model" {
			val, err := io.ReadAll(io.LimitReader(part, 256))
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(val))
		}
	}
}
