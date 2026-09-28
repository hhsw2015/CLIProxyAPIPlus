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

// zhipuAdaptor serves Zhipu CogVideoX video generation first-party (open.bigmodel.cn)
// via the async task API — submit to /videos/generations, poll /async-result/{id}.
// CPA otherwise has no Zhipu video route (Zhipu was chat-only). Same key as chat.
type zhipuAdaptor struct{}

func (a *zhipuAdaptor) Platform() string { return "zhipu" }

func (a *zhipuAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	return "generate", nil
}

func (a *zhipuAdaptor) BuildRequestURL(baseURL, action string) string {
	return strings.TrimSuffix(baseURL, "/") + "/videos/generations"
}

func (a *zhipuAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *zhipuAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	var req struct {
		Prompt   string `json:"prompt"`
		Model    string `json:"model"`
		Size     string `json:"size"`
		Quality  string `json:"quality"`
		Duration int    `json:"duration"`
		FPS      int    `json:"fps"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", err
	}
	out := map[string]any{"model": model, "prompt": req.Prompt}
	if req.Size != "" {
		out["size"] = req.Size
	}
	if req.Quality != "" {
		out["quality"] = req.Quality
	}
	if req.Duration > 0 {
		out["duration"] = req.Duration
	}
	if req.FPS > 0 {
		out["fps"] = req.FPS
	}
	if req.ImageURL != "" { // image-to-video
		out["image_url"] = req.ImageURL
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *zhipuAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	// Zhipu: {"id":"xxx","request_id":"...","task_status":"PROCESSING"}
	var result struct {
		ID    string `json:"id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}
	if result.ID == "" {
		if result.Error.Message != "" {
			return "", data, fmt.Errorf("zhipu error: %s", result.Error.Message)
		}
		return "", data, fmt.Errorf("empty task ID: %s", string(data))
	}
	return result.ID, data, nil
}

func (a *zhipuAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/async-result/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *zhipuAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result struct {
		TaskStatus  string `json:"task_status"`
		VideoResult []struct {
			URL string `json:"url"`
		} `json:"video_result"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	info := &TaskInfo{}
	switch strings.ToUpper(result.TaskStatus) {
	case "SUCCESS":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		if len(result.VideoResult) > 0 {
			info.URL = result.VideoResult[0].URL
		}
	case "FAIL":
		info.Status = TaskStatusFailure
		info.Reason = "task failed"
	default: // PROCESSING
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	}
	return info, nil
}

func (a *zhipuAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
