package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// gptProxyAdaptor handles tasks routed through gpt-proxy (chisel tunnel).
// Transforms standard requests to gpt-proxy format, parses wrapped responses.
type gptProxyAdaptor struct{}

func (a *gptProxyAdaptor) Platform() string { return "gpt-proxy" }

func (a *gptProxyAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	return "generate", nil
}

func (a *gptProxyAdaptor) BuildRequestURL(baseURL, action string) string {
	return baseURL
}

func (a *gptProxyAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("app_key", apiKey)
	}
}

// buildSubmitURL constructs the gpt-proxy submit URL.
func (a *gptProxyAdaptor) buildSubmitURL(baseURL string) string {
	base := strings.TrimSuffix(baseURL, "/")
	if strings.Contains(base, "/google/veo") {
		return base + "/predict"
	}
	if strings.Contains(base, "/klingai") && !strings.HasSuffix(base, "/submit") {
		return base + "/submit"
	}
	if strings.Contains(base, "/volengine") && !strings.HasSuffix(base, "/submit") {
		return base + "/submit"
	}
	return base
}

func (a *gptProxyAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	// Transform standard request to gpt-proxy format based on provider.
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return bytes.NewReader(body), "application/json", nil
	}

	prompt, _ := req["prompt"].(string)

	// Detect provider from context (will be set by task handler).
	baseURL, _ := c.Get("gpt_proxy_base_url")
	base, _ := baseURL.(string)

	var transformed any

	switch {
	case strings.Contains(base, "/google/veo"):
		// Veo format: instances + parameters
		params := map[string]any{"sampleCount": 1, "personGeneration": "allow_all"}
		if d, ok := req["duration"]; ok {
			params["durationSeconds"] = d
		}
		transformed = map[string]any{
			"model":      model,
			"instances":  []map[string]any{{"prompt": prompt}},
			"parameters": params,
		}

	case strings.Contains(base, "/google/imagen"):
		// Imagen format: instances + parameters
		params := map[string]any{"sampleCount": 1, "personGeneration": "allow_all"}
		if ar, ok := req["aspect_ratio"]; ok {
			params["aspectRatio"] = ar
		}
		transformed = map[string]any{
			"model":      model,
			"instances":  []map[string]any{{"prompt": prompt}},
			"parameters": params,
		}

	case strings.Contains(base, "/klingai"):
		// Kling format
		transformed = map[string]any{
			"model_name":     model,
			"prompt":         prompt,
			"duration":       "10",
			"aspect_ratio":   "16:9",
			"watermark_info": map[string]any{"enabled": false},
		}

	case strings.Contains(base, "/volengine/video"):
		// Doubao/Seedance format
		content := []map[string]any{{"type": "text", "text": prompt}}
		transformed = map[string]any{
			"model":          model,
			"content":        content,
			"generate_audio": false,
			"duration":       5,
			"resolution":     "720p",
			"watermark":      false,
		}

	default:
		// Passthrough as-is
		return bytes.NewReader(body), "application/json", nil
	}

	data, err := json.Marshal(transformed)
	if err != nil {
		return bytes.NewReader(body), "application/json", nil
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *gptProxyAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}

	// gpt-proxy wraps responses: {"code":200, "resp_data":{...}}
	var wrapped struct {
		Code     int             `json:"code"`
		CodeMsg  string          `json:"code_msg"`
		RespData json.RawMessage `json:"resp_data"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		// Try unwrapped format
		return a.parseRawSubmitResponse(data)
	}

	if wrapped.Code != 200 && wrapped.Code != 0 {
		return "", data, fmt.Errorf("gpt-proxy error %d: %s", wrapped.Code, wrapped.CodeMsg)
	}

	if wrapped.RespData == nil {
		return "", data, fmt.Errorf("empty resp_data: %s", string(data))
	}

	return a.parseRawSubmitResponse(wrapped.RespData)
}

func (a *gptProxyAdaptor) parseRawSubmitResponse(data []byte) (string, []byte, error) {
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}

	// Veo: {"name": "projects/xxx/operations/xxx"}
	if name, ok := result["name"].(string); ok && name != "" {
		return name, data, nil
	}
	// Kling: {"data": {"task_id": "xxx"}}
	if dataObj, ok := result["data"].(map[string]any); ok {
		if tid, ok := dataObj["task_id"].(string); ok && tid != "" {
			return tid, data, nil
		}
	}
	// Generic: {"task_id": "xxx"} or {"id": "xxx"}
	taskID := extractStringField(result, "task_id", "id", "taskId")
	if taskID != "" {
		return taskID, data, nil
	}

	return "", data, fmt.Errorf("no task ID found in: %s", string(data))
}

// FetchTask polls gpt-proxy for task status.
func (a *gptProxyAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	base := strings.TrimSuffix(baseURL, "/")

	var fetchURL string
	var body []byte

	if strings.Contains(base, "/google/veo") {
		// Veo: POST /fetch with {"operationName": "..."}
		fetchURL = base + "/fetch"
		body, _ = json.Marshal(map[string]string{"operationName": upstreamTaskID})
	} else if strings.Contains(base, "/klingai") {
		// Kling: GET or POST fetch
		fetchURL = strings.Replace(base, "/submit", "/fetch", 1)
		if !strings.HasSuffix(fetchURL, "/fetch") {
			fetchURL = base + "/fetch"
		}
		body, _ = json.Marshal(map[string]string{"task_id": upstreamTaskID})
	} else {
		// Default: POST with task_id
		fetchURL = base + "/fetch"
		body, _ = json.Marshal(map[string]string{"task_id": upstreamTaskID})
	}

	req, err := http.NewRequest(http.MethodPost, fetchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *gptProxyAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	// Unwrap gpt-proxy response.
	var wrapped struct {
		Code     int             `json:"code"`
		RespData json.RawMessage `json:"resp_data"`
	}
	inner := respBody
	if json.Unmarshal(respBody, &wrapped) == nil && wrapped.RespData != nil {
		inner = wrapped.RespData
	}

	var result map[string]any
	if err := json.Unmarshal(inner, &result); err != nil {
		return nil, err
	}

	info := &TaskInfo{}

	// Veo format: {"done": true/false, "name": "...", "response": {...}, "error": {...}}
	if done, ok := result["done"]; ok {
		if d, ok := done.(bool); ok && d {
			info.Status = TaskStatusSuccess
			info.Progress = "100%"
			// Video data is in response
			return info, nil
		}
		if errObj, ok := result["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok && msg != "" {
				info.Status = TaskStatusFailure
				info.Reason = msg
				return info, nil
			}
		}
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
		return info, nil
	}

	// Kling/Doubao format: {"status": "...", "data": {...}}
	status := extractStringField(result, "status", "state", "task_status")
	switch strings.ToLower(status) {
	case "completed", "succeeded", "success", "done":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
	case "failed", "error":
		info.Status = TaskStatusFailure
		info.Reason = extractStringField(result, "fail_reason", "error_message", "message")
	case "processing", "running", "in_progress":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	case "queued", "pending", "submitted":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusInProgress
		info.Progress = "30%"
	}

	info.URL = extractStringField(result, "result_url", "video_url", "audio_url", "url")
	if info.URL == "" {
		if content, ok := result["content"].(map[string]any); ok {
			info.URL = extractStringField(content, "video_url", "url")
		}
	}
	return info, nil
}

// extractStringField returns the first non-empty string value from the map for the given keys.
func extractStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func (a *gptProxyAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
