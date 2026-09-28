package api

import (
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
