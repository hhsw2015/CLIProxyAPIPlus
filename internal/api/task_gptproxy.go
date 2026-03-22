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
// It doesn't transform requests/responses — just transparently proxies them.
// The task lifecycle is managed by CPA, but the actual API calls go through gpt-proxy.
type gptProxyAdaptor struct{}

func (a *gptProxyAdaptor) Platform() string { return "gpt-proxy" }

func (a *gptProxyAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	return "generate", nil
}

func (a *gptProxyAdaptor) BuildRequestURL(baseURL, action string) string {
	return baseURL // Already complete
}

func (a *gptProxyAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("app_key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *gptProxyAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	return bytes.NewReader(body), "application/json", nil
}

func (a *gptProxyAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	// gpt-proxy may return task ID in various formats. Try common patterns.
	var result map[string]any
	if json.Unmarshal(data, &result) != nil {
		return "", data, fmt.Errorf("invalid JSON response: %s", string(data))
	}
	taskID := extractStringField(result, "id", "task_id", "taskId")
	if taskID == "" {
		if dataObj, ok := result["data"].(map[string]any); ok {
			taskID = extractStringField(dataObj, "id", "task_id", "taskId")
		}
	}
	return taskID, data, nil
}

// BuildSubmitURL constructs the gpt-proxy submit URL.
// For Veo: base + /predict; for others: base + /submit or base as-is.
func (a *gptProxyAdaptor) buildSubmitURL(baseURL string) string {
	base := strings.TrimSuffix(baseURL, "/")
	// Veo uses /predict
	if strings.Contains(base, "/google/veo") {
		return base + "/predict"
	}
	// Kling uses /submit
	if strings.Contains(base, "/klingai") {
		return base + "/submit"
	}
	// Volengine uses /submit
	if strings.Contains(base, "/volengine") {
		return base + "/submit"
	}
	return base
}

// FetchTask polls gpt-proxy for task status.
// gpt-proxy uses POST for polling (not GET), with task_id in body.
func (a *gptProxyAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	base := strings.TrimSuffix(baseURL, "/")
	var fetchURL string
	// Veo uses /fetch
	if strings.Contains(base, "/google/veo") {
		fetchURL = base + "/fetch"
	} else {
		// Others: append /fetch or /{taskID}
		fetchURL = base + "/fetch"
	}

	// gpt-proxy polling uses POST with task_id in body.
	body, _ := json.Marshal(map[string]string{"task_id": upstreamTaskID})
	req, err := http.NewRequest(http.MethodPost, fetchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	a.BuildRequestHeader(req, apiKey)
	req.Header.Set("Accept", "application/json")
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *gptProxyAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	info := &TaskInfo{}

	// Extract status from common fields.
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
		if status != "" {
			info.Status = TaskStatusInProgress
			info.Progress = "30%"
		}
	}

	// Extract result URL.
	info.URL = extractStringField(result, "result_url", "video_url", "audio_url", "url")
	if info.URL == "" {
		// Check nested content.video_url
		if content, ok := result["content"].(map[string]any); ok {
			info.URL = extractStringField(content, "video_url", "url")
		}
		// Check nested data[0].url
		if dataArr, ok := result["data"].([]any); ok && len(dataArr) > 0 {
			if item, ok := dataArr[0].(map[string]any); ok {
				info.URL = extractStringField(item, "url", "video_url", "audio_url")
			}
		}
	}

	return info, nil
}

func (a *gptProxyAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
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
