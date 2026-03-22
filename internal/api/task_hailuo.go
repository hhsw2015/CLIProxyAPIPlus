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

type hailuoAdaptor struct{}

func (a *hailuoAdaptor) Platform() string { return "hailuo" }

func (a *hailuoAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	return "generate", nil
}

func (a *hailuoAdaptor) BuildRequestURL(baseURL, action string) string {
	return strings.TrimSuffix(baseURL, "/") + "/v1/video_generation"
}

func (a *hailuoAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *hailuoAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	// MiniMax/Hailuo: {"model":"xxx","prompt":"...","duration":5}
	var req struct {
		Prompt   string `json:"prompt"`
		Model    string `json:"model"`
		Duration int    `json:"duration"`
		Size     string `json:"size"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", err
	}

	hailuoBody := map[string]any{
		"model":  model,
		"prompt": req.Prompt,
	}
	if req.Duration > 0 {
		hailuoBody["duration"] = req.Duration
	}
	if req.Size != "" {
		hailuoBody["resolution"] = req.Size
	}

	data, err := json.Marshal(hailuoBody)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *hailuoAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	// Hailuo: {"task_id":"xxx","base_resp":{"status_code":0}}
	var result struct {
		TaskID   string `json:"task_id"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}
	if result.BaseResp.StatusCode != 0 {
		return "", data, fmt.Errorf("hailuo error: %s", result.BaseResp.StatusMsg)
	}
	if result.TaskID == "" {
		return "", data, fmt.Errorf("empty task ID: %s", string(data))
	}
	return result.TaskID, data, nil
}

func (a *hailuoAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/v1/query/video_generation?task_id=" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *hailuoAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result struct {
		TaskID string `json:"task_id"`
		Status int    `json:"status"`
		FileID string `json:"file_id"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	info := &TaskInfo{TaskID: result.TaskID}

	if result.BaseResp.StatusCode != 0 {
		info.Status = TaskStatusFailure
		info.Reason = result.BaseResp.StatusMsg
		return info, nil
	}

	// Hailuo status: 1=preparing, 2=queueing, 3=processing, 4=success, 5=failed
	switch result.Status {
	case 4:
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
	case 5:
		info.Status = TaskStatusFailure
		info.Reason = "task failed"
	case 3:
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	case 1, 2:
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusInProgress
		info.Progress = "30%"
	}
	return info, nil
}

func (a *hailuoAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
