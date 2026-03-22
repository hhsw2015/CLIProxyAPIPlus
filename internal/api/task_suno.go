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

type sunoAdaptor struct{}

func (a *sunoAdaptor) Platform() string { return "suno" }

func (a *sunoAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	action := c.Param("action")
	if action == "" {
		action = "generate"
	}
	return action, nil
}

func (a *sunoAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(baseURL, "/")
	return base + "/api/generate/v2"
}

func (a *sunoAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *sunoAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	return bytes.NewReader(body), "application/json", nil
}

func (a *sunoAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("read response: %w", err)
	}
	// Suno response varies; try common formats.
	var result struct {
		ID   string `json:"id"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, fmt.Errorf("parse response: %w", err)
	}
	taskID := result.ID
	if taskID == "" && len(result.Data) > 0 {
		taskID = result.Data[0].ID
	}
	if taskID == "" {
		return "", data, fmt.Errorf("empty task ID: %s", string(data))
	}
	return taskID, data, nil
}

func (a *sunoAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/api/feed/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *sunoAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	// Suno returns an array of clips.
	var clips []struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		AudioURL  string `json:"audio_url"`
		VideoURL  string `json:"video_url"`
		ErrorMsg  string `json:"error_message"`
	}
	if err := json.Unmarshal(respBody, &clips); err != nil {
		// Try wrapping format.
		var wrapped struct {
			Data []struct {
				ID       string `json:"id"`
				Status   string `json:"status"`
				AudioURL string `json:"audio_url"`
			} `json:"data"`
		}
		if err2 := json.Unmarshal(respBody, &wrapped); err2 != nil {
			return nil, fmt.Errorf("parse suno result: %w", err)
		}
		if len(wrapped.Data) > 0 {
			clips = append(clips, struct {
				ID        string `json:"id"`
				Status    string `json:"status"`
				AudioURL  string `json:"audio_url"`
				VideoURL  string `json:"video_url"`
				ErrorMsg  string `json:"error_message"`
			}{
				ID:       wrapped.Data[0].ID,
				Status:   wrapped.Data[0].Status,
				AudioURL: wrapped.Data[0].AudioURL,
			})
		}
	}

	if len(clips) == 0 {
		return &TaskInfo{Status: TaskStatusSubmitted, Progress: "10%"}, nil
	}

	clip := clips[0]
	info := &TaskInfo{TaskID: clip.ID}
	switch strings.ToLower(clip.Status) {
	case "complete", "completed":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		if clip.AudioURL != "" {
			info.URL = clip.AudioURL
		} else if clip.VideoURL != "" {
			info.URL = clip.VideoURL
		}
	case "error", "failed":
		info.Status = TaskStatusFailure
		info.Reason = clip.ErrorMsg
	case "streaming", "processing":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	default:
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	}
	return info, nil
}

func (a *sunoAdaptor) BuildClientResponse(task *Task) any {
	resp := map[string]any{
		"id":         task.ID,
		"platform":   "suno",
		"model":      task.Model,
		"status":     task.Status,
		"progress":   task.Progress,
		"created_at": task.CreatedAt.Unix(),
	}
	if task.ResultURL != "" {
		resp["audio_url"] = task.ResultURL
	}
	if task.FailReason != "" {
		resp["fail_reason"] = task.FailReason
	}
	if !task.FinishedAt.IsZero() {
		resp["finished_at"] = task.FinishedAt.Unix()
	}
	return resp
}
