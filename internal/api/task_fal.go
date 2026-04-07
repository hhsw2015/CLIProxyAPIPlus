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

// falAdaptor implements TaskAdaptor for Fal.ai (https://fal.ai).
// Fal.ai is a universal AI platform supporting image/video/audio generation.
// API: Queue-based async — submit to queue, poll for status, fetch result.
type falAdaptor struct{}

func (a *falAdaptor) Platform() string { return "fal" }

func (a *falAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	return "generate", nil
}

func (a *falAdaptor) BuildRequestURL(baseURL, action string) string {
	// baseURL is the full queue endpoint, e.g. https://queue.fal.run/fal-ai/flux-pro/v1.1
	return strings.TrimSuffix(baseURL, "/")
}

func (a *falAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Key "+apiKey)
	}
}

func (a *falAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	// Fal.ai accepts arbitrary JSON bodies depending on the model.
	// Pass through the client body directly.
	return bytes.NewReader(body), "application/json", nil
}

func (a *falAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	// Fal queue response: {"request_id":"xxx","status":"IN_QUEUE",...}
	var result struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}
	if result.RequestID == "" {
		return "", data, fmt.Errorf("fal: empty request_id: %s", string(data))
	}
	return result.RequestID, data, nil
}

func (a *falAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	// Poll status: GET {baseURL}/requests/{request_id}/status
	url := strings.TrimSuffix(baseURL, "/") + "/requests/" + upstreamTaskID + "/status"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *falAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	// Fal status response: {"status":"IN_QUEUE|IN_PROGRESS|COMPLETED|FAILED",...}
	var result struct {
		Status       string `json:"status"`
		ResponseURL  string `json:"response_url"`
		Error        string `json:"error"`
		QueuePos     int    `json:"queue_position"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	info := &TaskInfo{}
	switch strings.ToUpper(result.Status) {
	case "COMPLETED":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = result.ResponseURL
	case "FAILED":
		info.Status = TaskStatusFailure
		info.Reason = result.Error
		if info.Reason == "" {
			info.Reason = "fal task failed"
		}
	case "IN_PROGRESS":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	case "IN_QUEUE":
		info.Status = TaskStatusQueued
		if result.QueuePos > 0 {
			info.Progress = fmt.Sprintf("queue #%d", result.QueuePos)
		} else {
			info.Progress = "20%"
		}
	default:
		info.Status = TaskStatusInProgress
		info.Progress = "30%"
	}
	return info, nil
}

func (a *falAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
