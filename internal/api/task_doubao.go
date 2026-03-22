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

type doubaoAdaptor struct{}

func (a *doubaoAdaptor) Platform() string { return "doubao" }

func (a *doubaoAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	return "generate", nil
}

func (a *doubaoAdaptor) BuildRequestURL(baseURL, action string) string {
	return strings.TrimSuffix(baseURL, "/") + "/api/v3/contents/generations/tasks"
}

func (a *doubaoAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *doubaoAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	// Doubao expects: {"model":"xxx","content":[{"type":"text","text":"..."}]}
	var req struct {
		Prompt string         `json:"prompt"`
		Model  string         `json:"model"`
		Images []string       `json:"images"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", err
	}

	content := []map[string]any{}
	if req.Prompt != "" {
		content = append(content, map[string]any{"type": "text", "text": req.Prompt})
	}
	for _, img := range req.Images {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": img},
		})
	}

	doubaoBody := map[string]any{
		"model":   model,
		"content": content,
	}
	// Pass through metadata fields.
	if req.Metadata != nil {
		for k, v := range req.Metadata {
			doubaoBody[k] = v
		}
	}

	data, err := json.Marshal(doubaoBody)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *doubaoAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}
	if result.ID == "" {
		return "", data, fmt.Errorf("empty task ID: %s", string(data))
	}
	return result.ID, data, nil
}

func (a *doubaoAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/api/v3/contents/generations/tasks/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *doubaoAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Content struct {
			VideoURL string `json:"video_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	info := &TaskInfo{TaskID: result.ID}
	switch strings.ToLower(result.Status) {
	case "succeeded":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = result.Content.VideoURL
	case "failed":
		info.Status = TaskStatusFailure
		info.Reason = "task failed"
	case "processing", "running":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	case "pending", "queued":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusInProgress
		info.Progress = "30%"
	}
	return info, nil
}

func (a *doubaoAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
