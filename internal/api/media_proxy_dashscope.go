package api

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DashScope (Alibaba Qwen/Wan) first-party image generation via the native async
// task API — CPA otherwise routes Qwen/Wan image through fal (aggregator margin).
// Submit → poll /tasks/{id} → return the OpenAI images shape. Synchronous to the
// client (image gen takes ~10-30s, acceptable).

const dashscopeBase = "https://dashscope.aliyuncs.com"

// dashscopeKey returns a first-party DashScope api-key from config.
func (s *Server) dashscopeKey() (string, bool) {
	if s.cfg == nil {
		return "", false
	}
	for i := range s.cfg.OpenAICompatibility {
		e := &s.cfg.OpenAICompatibility[i]
		if !strings.Contains(e.BaseURL, "dashscope.aliyuncs.com") {
			continue
		}
		for _, ak := range e.APIKeyEntries {
			if strings.HasPrefix(strings.TrimSpace(ak.APIKey), "sk-") {
				return ak.APIKey, true
			}
		}
	}
	return "", false
}

// handleDashScopeImage serves /v1/images/generations for Qwen/Wan native image
// models (model contains "qwen-image" or "wan" + "-image"/"t2i"). Falls through if
// no DashScope key.
func (s *Server) handleDashScopeImage(c *gin.Context, modelName string, body []byte) bool {
	key, ok := s.dashscopeKey()
	if !ok {
		return false
	}
	prompt := gjson.GetBytes(body, "prompt").String()
	if strings.TrimSpace(prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "prompt is required", "type": "invalid_request_error"}})
		return true
	}
	size := gjson.GetBytes(body, "size").String()
	if size == "" {
		size = "1024*1024"
	}
	size = strings.ReplaceAll(size, "x", "*") // OpenAI "1024x1024" → DashScope "1024*1024"
	n := gjson.GetBytes(body, "n").Int()
	if n <= 0 {
		n = 1
	}

	reqBody := []byte(`{"model":"","input":{"prompt":""},"parameters":{"size":"","n":1}}`)
	reqBody, _ = sjson.SetBytes(reqBody, "model", modelName)
	reqBody, _ = sjson.SetBytes(reqBody, "input.prompt", prompt)
	reqBody, _ = sjson.SetBytes(reqBody, "parameters.size", size)
	reqBody, _ = sjson.SetBytes(reqBody, "parameters.n", n)

	client := &http.Client{Timeout: 30 * time.Second}
	subReq, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		dashscopeBase+"/api/v1/services/aigc/text2image/image-synthesis", strings.NewReader(string(reqBody)))
	subReq.Header.Set("Authorization", "Bearer "+key)
	subReq.Header.Set("Content-Type", "application/json")
	subReq.Header.Set("X-DashScope-Async", "enable")
	subResp, doErr := client.Do(subReq)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("dashscope-image: %v", doErr), "type": "server_error"}})
		return true
	}
	sb, _ := io.ReadAll(subResp.Body)
	_ = subResp.Body.Close()
	taskID := gjson.GetBytes(sb, "output.task_id").String()
	if taskID == "" {
		c.Data(subResp.StatusCode, "application/json", sb) // submit error — surface it
		return true
	}

	// Poll until SUCCEEDED/FAILED (bounded).
	deadline := time.Now().Add(120 * time.Second)
	var urls []string
	for time.Now().Before(deadline) {
		select {
		case <-c.Request.Context().Done():
			return true
		case <-time.After(4 * time.Second):
		}
		pReq, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, dashscopeBase+"/api/v1/tasks/"+taskID, nil)
		pReq.Header.Set("Authorization", "Bearer "+key)
		pResp, pErr := client.Do(pReq)
		if pErr != nil {
			continue
		}
		pb, _ := io.ReadAll(pResp.Body)
		_ = pResp.Body.Close()
		status := gjson.GetBytes(pb, "output.task_status").String()
		if status == "SUCCEEDED" {
			gjson.GetBytes(pb, "output.results").ForEach(func(_, r gjson.Result) bool {
				if u := r.Get("url").String(); u != "" {
					urls = append(urls, u)
				}
				return true
			})
			break
		}
		if status == "FAILED" || status == "UNKNOWN" {
			c.Data(http.StatusBadGateway, "application/json", pb)
			return true
		}
	}
	if len(urls) == 0 {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": gin.H{"message": "dashscope-image: task timed out", "type": "server_error"}})
		return true
	}
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", time.Now().Unix())
	for i, u := range urls {
		out, _ = sjson.SetBytes(out, fmt.Sprintf("data.%d.url", i), u)
	}
	log.Debugf("[dashscope-image] model=%s images=%d", modelName, len(urls))
	c.Data(http.StatusOK, "application/json", out)
	return true
}

// handleDashScopeRerank serves /v1/rerank for DashScope rerank models (model
// contains "rerank"). Translates the OpenAI/Cohere-style body to the DashScope
// native rerank API and its output.results back to the standard rerank shape.
func (s *Server) handleDashScopeRerank(c *gin.Context, modelName string, body []byte) bool {
	key, ok := s.dashscopeKey()
	if !ok {
		return false
	}
	query := gjson.GetBytes(body, "query").String()
	docs := gjson.GetBytes(body, "documents")
	if strings.TrimSpace(query) == "" || !docs.IsArray() {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "query and documents are required", "type": "invalid_request_error"}})
		return true
	}
	reqBody := []byte(`{"model":"","input":{"query":"","documents":[]},"parameters":{"return_documents":true}}`)
	reqBody, _ = sjson.SetBytes(reqBody, "model", modelName)
	reqBody, _ = sjson.SetBytes(reqBody, "input.query", query)
	reqBody, _ = sjson.SetRawBytes(reqBody, "input.documents", []byte(docs.Raw))
	if tn := gjson.GetBytes(body, "top_n").Int(); tn > 0 {
		reqBody, _ = sjson.SetBytes(reqBody, "parameters.top_n", tn)
	}

	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		dashscopeBase+"/api/v1/services/rerank/text-rerank/text-rerank", strings.NewReader(string(reqBody)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("dashscope-rerank: %v", doErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		c.Data(resp.StatusCode, "application/json", rb)
		return true
	}
	out := []byte(`{"model":"","results":[]}`)
	out, _ = sjson.SetBytes(out, "model", modelName)
	i := 0
	gjson.GetBytes(rb, "output.results").ForEach(func(_, r gjson.Result) bool {
		out, _ = sjson.SetBytes(out, fmt.Sprintf("results.%d.index", i), r.Get("index").Int())
		out, _ = sjson.SetBytes(out, fmt.Sprintf("results.%d.relevance_score", i), r.Get("relevance_score").Float())
		if d := r.Get("document"); d.Exists() {
			out, _ = sjson.SetRawBytes(out, fmt.Sprintf("results.%d.document", i), []byte(d.Raw))
		}
		i++
		return true
	})
	if u := gjson.GetBytes(rb, "usage"); u.Exists() {
		out, _ = sjson.SetRawBytes(out, "usage", []byte(u.Raw))
	}
	log.Debugf("[dashscope-rerank] model=%s results=%d", modelName, i)
	c.Data(http.StatusOK, "application/json", out)
	return true
}

// isDashScopeEmbeddingModel reports whether an /v1/embeddings model should route to
// the DashScope first-party handler: text-embedding-v* (native v3/v4) or a Qwen
// *text-embedding* id. OpenAI text-embedding-3-* and siliconflow qwen3-embedding-8b
// must NOT match (they keep the generic passthrough / their own channel).
func isDashScopeEmbeddingModel(model string) bool {
	lm := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(lm, "text-embedding-v") ||
		(strings.Contains(lm, "qwen") && strings.Contains(lm, "text-embedding"))
}

// isDashScopeTTSModel reports whether an /v1/audio/speech model should route to the
// DashScope first-party TTS handler (qwen*-tts / cosyvoice). Other TTS providers
// (MiniMax/Azure/Vertex/Polly) must not match.
func isDashScopeTTSModel(model string) bool {
	lm := strings.ToLower(strings.TrimSpace(model))
	return (strings.Contains(lm, "qwen") && strings.Contains(lm, "tts")) ||
		strings.Contains(lm, "cosyvoice")
}

// handleDashScopeEmbeddings serves /v1/embeddings for DashScope embedding models
// (text-embedding-v* / qwen*-text-embedding). DashScope's /compatible-mode/v1
// speaks the OpenAI embeddings shape, so the request body is forwarded verbatim on
// the first-party key. Falls through if no DashScope key.
func (s *Server) handleDashScopeEmbeddings(c *gin.Context, modelName string, body []byte) bool {
	key, ok := s.dashscopeKey()
	if !ok {
		return false
	}
	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		dashscopeBase+"/compatible-mode/v1/embeddings", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("dashscope-embeddings: %v", doErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	log.Debugf("[dashscope-embeddings] model=%s status=%d", modelName, resp.StatusCode)
	c.Data(resp.StatusCode, "application/json", rb) // already OpenAI embeddings shape
	return true
}

// handleDashScopeTTS serves /v1/audio/speech for DashScope TTS models (qwen*-tts /
// cosyvoice). DashScope has no OpenAI-compatible /audio/speech; it uses the native
// multimodal-generation API which returns a (synchronous) audio URL. So: generate →
// fetch the URL → return the raw audio bytes (OpenAI /v1/audio/speech returns audio,
// not JSON). Falls through if no DashScope key.
func (s *Server) handleDashScopeTTS(c *gin.Context, modelName string, body []byte) bool {
	key, ok := s.dashscopeKey()
	if !ok {
		return false
	}
	text := gjson.GetBytes(body, "input").String()
	if strings.TrimSpace(text) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "input is required", "type": "invalid_request_error"}})
		return true
	}
	// OpenAI voices (alloy/echo/...) are not valid DashScope voices; default to a
	// real one and pass through any explicit DashScope voice (Cherry/Ethan/Serena...).
	voice := strings.TrimSpace(gjson.GetBytes(body, "voice").String())
	switch voice {
	case "", "alloy", "echo", "fable", "onyx", "nova", "shimmer":
		voice = "Cherry"
	}
	reqBody := []byte(`{"model":"","input":{"text":"","voice":""}}`)
	reqBody, _ = sjson.SetBytes(reqBody, "model", modelName)
	reqBody, _ = sjson.SetBytes(reqBody, "input.text", text)
	reqBody, _ = sjson.SetBytes(reqBody, "input.voice", voice)

	client := &http.Client{Timeout: 60 * time.Second}
	genReq, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		dashscopeBase+"/api/v1/services/aigc/multimodal-generation/generation", strings.NewReader(string(reqBody)))
	genReq.Header.Set("Authorization", "Bearer "+key)
	genReq.Header.Set("Content-Type", "application/json")
	genResp, doErr := client.Do(genReq)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("dashscope-tts: %v", doErr), "type": "server_error"}})
		return true
	}
	gb, _ := io.ReadAll(genResp.Body)
	_ = genResp.Body.Close()
	if genResp.StatusCode != 200 {
		c.Data(genResp.StatusCode, "application/json", gb) // surface the upstream error
		return true
	}
	// Inline base64 audio (some models) → return directly.
	if dataB64 := gjson.GetBytes(gb, "output.audio.data").String(); dataB64 != "" {
		if raw, decErr := base64.StdEncoding.DecodeString(dataB64); decErr == nil && len(raw) > 0 {
			c.Data(http.StatusOK, "audio/wav", raw)
			return true
		}
	}
	audioURL := gjson.GetBytes(gb, "output.audio.url").String()
	if audioURL == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "dashscope-tts: no audio in response", "type": "server_error"}})
		return true
	}
	aReq, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, audioURL, nil)
	aResp, aErr := client.Do(aReq)
	if aErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("dashscope-tts fetch: %v", aErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = aResp.Body.Close() }()
	audio, _ := io.ReadAll(aResp.Body)
	ct := aResp.Header.Get("Content-Type")
	if ct == "" {
		ct = "audio/wav"
	}
	log.Debugf("[dashscope-tts] model=%s voice=%s bytes=%d", modelName, voice, len(audio))
	c.Data(http.StatusOK, ct, audio)
	return true
}
