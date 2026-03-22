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

type soraAdaptor struct{}

func (a *soraAdaptor) Platform() string { return "sora" }

func (a *soraAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	path := c.Request.URL.Path
	if strings.Contains(path, "remix") {
		return "remix", nil
	}
	return "generate", nil
}

func (a *soraAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(baseURL, "/")
	if action == "remix" {
		return base + "/v1/videos/remix"
	}
	return base + "/v1/videos"
}

func (a *soraAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("api-key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *soraAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	return bytes.NewReader(body), "application/json", nil
}

func (a *soraAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("read response: %w", err)
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, fmt.Errorf("parse response: %w", err)
	}
	if result.ID == "" {
		return "", data, fmt.Errorf("empty task ID in response: %s", string(data))
	}
	return result.ID, data, nil
}

func (a *soraAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/v1/videos/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *soraAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse task result: %w", err)
	}

	info := &TaskInfo{TaskID: result.ID}
	switch strings.ToLower(result.Status) {
	case "completed":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
	case "failed":
		info.Status = TaskStatusFailure
		if result.Error != nil {
			info.Reason = result.Error.Message
		}
	case "in_progress", "processing":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	case "queued":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusSubmitted
		info.Progress = "10%"
	}
	return info, nil
}

func (a *soraAdaptor) BuildClientResponse(task *Task) any {
	resp := OpenAIVideoResponse{
		ID:        task.ID,
		Object:    "video",
		Model:     task.Model,
		Status:    mapStatusToOpenAI(task.Status),
		Progress:  task.Progress,
		CreatedAt: task.CreatedAt.Unix(),
	}
	if !task.FinishedAt.IsZero() {
		resp.CompletedAt = task.FinishedAt.Unix()
	}
	if task.ResultURL != "" {
		resp.ResultURL = task.ResultURL
	}
	if task.Status == TaskStatusFailure && task.FailReason != "" {
		resp.Error = &TaskErrorResp{Message: task.FailReason, Code: "generation_failed"}
	}
	return resp
}

// mapStatusToOpenAI converts internal status to OpenAI video status.
func mapStatusToOpenAI(status string) string {
	switch status {
	case TaskStatusSuccess:
		return "completed"
	case TaskStatusFailure:
		return "failed"
	case TaskStatusInProgress:
		return "in_progress"
	case TaskStatusQueued, TaskStatusSubmitted, TaskStatusNotStart:
		return "queued"
	default:
		return "queued"
	}
}
