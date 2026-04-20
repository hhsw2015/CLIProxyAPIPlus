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

// whisperBatchAdaptor implements TaskAdaptor for Azure Cognitive Services
// batch speech-to-text (whisper-batch). Uses Ocp-Apim-Subscription-Key auth
// and non-standard endpoints (not OpenAI /v1/audio/transcriptions).
type whisperBatchAdaptor struct{}

func (a *whisperBatchAdaptor) Platform() string { return "whisper-batch" }

func (a *whisperBatchAdaptor) ValidateAndSetAction(_ *gin.Context, _ []byte) (string, error) {
	return "submit", nil
}

func (a *whisperBatchAdaptor) BuildRequestURL(baseURL, _ string) string {
	return strings.TrimSuffix(baseURL, "/")
}

func (a *whisperBatchAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)
	}
}

func (a *whisperBatchAdaptor) BuildRequestBody(_ *gin.Context, body []byte, _ string) (io.Reader, string, error) {
	return bytes.NewReader(body), "application/json", nil
}

func (a *whisperBatchAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	var result struct {
		Self string `json:"self"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}
	// Extract task ID from self URL (last path segment)
	id := result.Self
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		id = id[idx+1:]
	}
	if id == "" {
		return "", data, fmt.Errorf("whisper-batch: no task ID in response: %s", string(data))
	}
	return id, data, nil
}

func (a *whisperBatchAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, _ string) (*http.Response, error) {
	// Status: GET base + /{taskID}
	url := strings.TrimSuffix(baseURL, "/") + "/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *whisperBatchAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Links  struct {
			Files string `json:"files"`
		} `json:"links"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	info := &TaskInfo{}
	switch strings.ToLower(result.Status) {
	case "succeeded":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = result.Links.Files
	case "failed":
		info.Status = TaskStatusFailure
		info.Reason = result.Error
		if info.Reason == "" {
			info.Reason = "whisper-batch transcription failed"
		}
	case "running":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	default:
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	}
	return info, nil
}

func (a *whisperBatchAdaptor) BuildClientResponse(task *Task) any {
	return map[string]any{
		"id":     task.ID,
		"status": task.Status,
		"result": task.ResultURL,
		"error":  task.FailReason,
	}
}
