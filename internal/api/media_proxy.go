package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

		// Client called an alias; send the upstream provider's model name in the
		// body (JSON media endpoints only — multipart model fields are handled
		// per-provider elsewhere).
		if !ep.isMultipart {
			if up := s.resolveUpstreamModel(modelName); up != "" {
				body = rewriteBodyModel(body, up)
			}
		}

		// Build upstream URL.
		upstreamURL := provider.baseURL

		// ElevenLabs Scribe STT expects the model in a "model_id" multipart field,
		// while OpenAI clients send "model". Rename it in the captured body so the
		// same client request works unchanged. Only the field-name in the
		// Content-Disposition header changes; the boundary is untouched.
		if ep.isMultipart && isElevenLabsProvider(upstreamURL) {
			body = bytes.Replace(body, []byte(`name="model"`), []byte(`name="model_id"`), 1)
		}

		// ElevenLabs TTS: translate OpenAI's {model,input,voice} into ElevenLabs
		// {text,model_id} with the voice_id in the URL path. The caller passes the
		// ElevenLabs voice_id in "voice" and a real ElevenLabs model id (e.g.
		// eleven_multilingual_v2) in "model".
		if ep.pathSuffix == "audio/speech" && isElevenLabsProvider(upstreamURL) {
			voiceID := strings.TrimSpace(gjson.GetBytes(body, "voice").String())
			if voiceID == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
					"message": "voice (ElevenLabs voice_id) is required for ElevenLabs TTS",
					"type":    "invalid_request_error",
				}})
				return
			}
			elBody, errMarshal := json.Marshal(map[string]string{
				"text":     gjson.GetBytes(body, "input").String(),
				"model_id": gjson.GetBytes(body, "model").String(),
			})
			if errMarshal != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
					"message": fmt.Sprintf("failed to build ElevenLabs TTS body: %v", errMarshal),
					"type":    "server_error",
				}})
				return
			}
			body = elBody
			upstreamURL = strings.TrimRight(upstreamURL, "/") + "/" + url.PathEscape(voiceID)
		}

		// Volcengine (Doubao) BigTTS: translate OpenAI {input,voice} into the
		// req_params body; "voice" is the speaker and drives the resource id.
		volcResource := ""
		if ep.pathSuffix == "audio/speech" && isVolcengineProvider(upstreamURL) {
			speaker := strings.TrimSpace(gjson.GetBytes(body, "voice").String())
			if speaker == "" {
				speaker = "zh_female_xiaohe_uranus_bigtts"
			}
			volcResource = volcResourceID(speaker)
			volcBody, errMarshal := json.Marshal(map[string]any{
				"user": map[string]any{"uid": "cpa"},
				"req_params": map[string]any{
					"text":         gjson.GetBytes(body, "input").String(),
					"speaker":      speaker,
					"audio_params": map[string]any{"format": "mp3", "sample_rate": 24000},
				},
			})
			if errMarshal != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
					"message": fmt.Sprintf("failed to build Volcengine TTS body: %v", errMarshal),
					"type":    "server_error",
				}})
				return
			}
			body = volcBody
		}

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

		// Set provider auth header. ElevenLabs uses "xi-api-key"; Volcengine uses
		// "X-Api-Key" + "X-Api-Resource-Id"; everything else the Azure "api-key".
		if provider.apiKey != "" {
			switch {
			case isElevenLabsProvider(upstreamURL):
				upstreamReq.Header.Set("xi-api-key", provider.apiKey)
			case isVolcengineProvider(upstreamURL):
				upstreamReq.Header.Set("X-Api-Key", provider.apiKey)
				upstreamReq.Header.Set("X-Api-Resource-Id", volcResource)
			case s.mediaAuthStyle(modelName) == "bearer":
				upstreamReq.Header.Set("Authorization", "Bearer "+provider.apiKey)
			default:
				upstreamReq.Header.Set("api-key", provider.apiKey)
			}
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

		// Volcengine streams a chunked concatenated-JSON response with base64 audio.
		// Decode it incrementally and flush each audio chunk so the client gets
		// realtime mp3 (TTFB = time to the first synth chunk, not the whole clip).
		if ep.pathSuffix == "audio/speech" && isVolcengineProvider(upstreamURL) {
			if resp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				c.Data(resp.StatusCode, "application/json", raw)
				return
			}
			streamVolcAudio(c, resp.Body, modelName)
			return
		}

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

			// ElevenLabs Scribe STT / Volcengine TTS: base-url is the full API URL
			// with non-Azure path + auth. Use it verbatim (the handler applies the
			// provider-specific header/body transforms).
			if isElevenLabsProvider(baseURL) || isVolcengineProvider(baseURL) {
				return &mediaProviderConfig{baseURL: baseURL, apiKey: apiKey}
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

// isElevenLabsProvider reports whether a resolved upstream URL points at the
// ElevenLabs API, which uses xi-api-key auth and a "model_id" form field.
func isElevenLabsProvider(baseURL string) bool {
	return strings.Contains(baseURL, "elevenlabs.io")
}

// resolveUpstreamModel returns the upstream provider model name for a client-
// facing model id. openai-compatibility entries follow the convention Name =
// upstream provider id, Alias = client-facing id. When a request arrives under
// the Alias (client name), media/task proxies must send the upstream Name to
// the provider. Returns "" when no remap is needed (matched by Name, or not
// found).
func (s *Server) resolveUpstreamModel(clientModel string) string {
	if s.cfg == nil {
		return ""
	}
	// Prefer an alias→name remap over a plain name match: a stale entry may list
	// the public id as its Name, but the routes-driven entry carries
	// Name=upstream + Alias=public. Scan all before giving up.
	for _, compat := range s.cfg.OpenAICompatibility {
		for _, m := range compat.Models {
			name := strings.TrimSpace(m.Name)
			alias := strings.TrimSpace(m.Alias)
			if alias != "" && strings.EqualFold(alias, clientModel) && name != "" && !strings.EqualFold(name, clientModel) {
				return name
			}
		}
	}
	return ""
}

// mediaAuthStyle returns the lower-cased auth-style of the openai-compatibility
// entry that serves modelName (matched by name or alias). Used so generic
// OpenAI-format image hosts (auth-style: bearer) get an Authorization header
// instead of the Azure api-key default.
func (s *Server) mediaAuthStyle(modelName string) string {
	if s.cfg == nil {
		return ""
	}
	for _, compat := range s.cfg.OpenAICompatibility {
		for _, m := range compat.Models {
			if strings.EqualFold(strings.TrimSpace(m.Name), modelName) ||
				strings.EqualFold(strings.TrimSpace(m.Alias), modelName) {
				return strings.ToLower(strings.TrimSpace(compat.AuthStyle))
			}
		}
	}
	return ""
}

// rewriteBodyModel replaces the top-level "model" field of a JSON request body.
// Returns the original body unchanged on parse failure or empty model.
func rewriteBodyModel(body []byte, model string) []byte {
	if model == "" || len(body) == 0 {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, ok := m["model"]; !ok {
		return body
	}
	m["model"] = model
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// isVolcengineProvider reports whether a resolved upstream URL points at the
// Volcengine (ByteDance/Doubao) OpenSpeech TTS API, which uses X-Api-Key +
// X-Api-Resource-Id auth, a bespoke request body, and a chunked concatenated-JSON
// response carrying base64 audio.
func isVolcengineProvider(baseURL string) bool {
	return strings.Contains(baseURL, "openspeech.bytedance.com")
}

// volcResourceID picks the X-Api-Resource-Id required by a Volcengine speaker.
// Wrong id -> "55000000: resource ID is mismatched".
func volcResourceID(speaker string) string {
	switch {
	case strings.HasPrefix(speaker, "S_"):
		return "seed-icl-2.0" // cloned voices
	case strings.Contains(speaker, "_uranus_") || strings.HasPrefix(speaker, "saturn_"):
		return "seed-tts-2.0" // 2.0 voices
	default:
		return "seed-tts-1.0"
	}
}

// streamVolcAudio decodes Volcengine's chunked response — concatenated JSON
// objects (no delimiters), each optionally carrying a base64 "data" audio chunk —
// and flushes each decoded chunk to the client as it arrives, so playback can
// start on the first synth chunk. Volcengine codes: 0 = ok chunk,
// 20000000 = session finished; anything else is an error.
func streamVolcAudio(c *gin.Context, body io.Reader, modelName string) {
	dec := json.NewDecoder(body)
	flusher, _ := c.Writer.(http.Flusher)
	wrote := false
	for {
		var obj struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		}
		if err := dec.Decode(&obj); err != nil {
			break // EOF or trailing garbage — stop with whatever we streamed
		}
		if obj.Code != 0 && obj.Code != 20000000 {
			if !wrote {
				c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
					"message": fmt.Sprintf("volcengine tts error %d: %s", obj.Code, obj.Message),
					"type":    "server_error",
				}})
				return
			}
			log.Errorf("media proxy: volcengine tts for %s: mid-stream error %d: %s", modelName, obj.Code, obj.Message)
			break
		}
		if obj.Data == "" {
			continue
		}
		chunk, errDec := base64.StdEncoding.DecodeString(obj.Data)
		if errDec != nil {
			continue
		}
		if !wrote {
			c.Header("Content-Type", "audio/mpeg")
			c.Status(http.StatusOK)
			wrote = true
		}
		if _, errWrite := c.Writer.Write(chunk); errWrite != nil {
			return // client gone
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if !wrote {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": "volcengine tts returned no audio",
			"type":    "server_error",
		}})
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
