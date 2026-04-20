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

// taijiaSoraAdaptor implements TaskAdaptor for TaijiAI Sora video generation.
// Uses Bearer auth with the XP Claude key. Async submit/poll pattern.
type taijiaSoraAdaptor struct{}

func (a *taijiaSoraAdaptor) Platform() string { return "taijia-sora" }

func (a *taijiaSoraAdaptor) ValidateAndSetAction(_ *gin.Context, _ []byte) (string, error) {
	return "generate", nil
}

func (a *taijiaSoraAdaptor) BuildRequestURL(baseURL, _ string) string {
	return strings.TrimSuffix(baseURL, "/") + "/video/generations"
}

func (a *taijiaSoraAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *taijiaSoraAdaptor) BuildRequestBody(_ *gin.Context, body []byte, _ string) (io.Reader, string, error) {
	return bytes.NewReader(body), "application/json", nil
}

func (a *taijiaSoraAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	var result struct {
		ID     string `json:"id"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}
	id := result.ID
	if id == "" {
		id = result.TaskID
	}
	if id == "" {
		return "", data, fmt.Errorf("taijia-sora: no task ID in response: %s", string(data))
	}
	return id, data, nil
}

func (a *taijiaSoraAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, _ string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/video/generations/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *taijiaSoraAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		URL    string `json:"url"`
		Output struct {
			URL string `json:"url"`
		} `json:"output"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	info := &TaskInfo{}
	switch strings.ToLower(result.Status) {
	case "completed", "succeeded":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = result.URL
		if info.URL == "" {
			info.URL = result.Output.URL
		}
	case "failed":
		info.Status = TaskStatusFailure
		info.Reason = result.Error
		if info.Reason == "" {
			info.Reason = "taijia-sora generation failed"
		}
	case "running", "processing":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	default:
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	}
	return info, nil
}

func (a *taijiaSoraAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
