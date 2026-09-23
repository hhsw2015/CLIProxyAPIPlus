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

// murekaAdaptor drives Mureka's music-generation API (api.mureka.cn / api.mureka.ai).
// Submit: POST /v1/song/generate (or /v1/instrumental/generate) {model, lyrics, prompt}
//
//	-> {"id","status":"preparing"}
//
// Poll:   GET  /v1/song/query/{id} -> {"status","choices":[{"url","flac_url",...}]}
// Auth:   Authorization: Bearer <key>. Models: mureka-8 / mureka-9 / mureka-9.5.
type murekaAdaptor struct{}

func (a *murekaAdaptor) Platform() string { return "mureka" }

func (a *murekaAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	action := c.Param("action")
	if action == "" {
		// Route by shape: an instrumental request carries no (non-empty) lyrics.
		action = "song"
		var probe struct {
			Lyrics string `json:"lyrics"`
		}
		if json.Unmarshal(body, &probe) == nil && strings.TrimSpace(probe.Lyrics) == "" {
			action = "instrumental"
		}
	}
	return action, nil
}

func (a *murekaAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(baseURL, "/")
	if action == "instrumental" {
		return base + "/v1/instrumental/generate"
	}
	return base + "/v1/song/generate"
}

func (a *murekaAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *murekaAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	return bytes.NewReader(body), "application/json", nil
}

func (a *murekaAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("read response: %w", err)
	}
	var result struct {
		ID    string `json:"id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, fmt.Errorf("parse response: %w", err)
	}
	if result.ID == "" {
		if result.Error.Message != "" {
			return "", data, fmt.Errorf("mureka submit error: %s", result.Error.Message)
		}
		return "", data, fmt.Errorf("empty task ID: %s", string(data))
	}
	return result.ID, data, nil
}

func (a *murekaAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/v1/song/query/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *murekaAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var r struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Choices []struct {
			URL     string `json:"url"`
			FlacURL string `json:"flac_url"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return nil, fmt.Errorf("parse mureka result: %w", err)
	}
	info := &TaskInfo{TaskID: r.ID}
	switch strings.ToLower(r.Status) {
	case "succeeded", "success", "completed", "complete":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		if len(r.Choices) > 0 {
			if r.Choices[0].URL != "" {
				info.URL = r.Choices[0].URL
			} else if r.Choices[0].FlacURL != "" {
				info.URL = r.Choices[0].FlacURL
			}
		}
	case "failed", "error", "timeouted", "cancelled":
		info.Status = TaskStatusFailure
		info.Reason = r.Error.Message
	case "running":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	case "preparing", "queued", "pending":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusSubmitted
		info.Progress = "10%"
	}
	return info, nil
}

func (a *murekaAdaptor) BuildClientResponse(task *Task) any {
	resp := map[string]any{
		"id":         task.ID,
		"platform":   "mureka",
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
