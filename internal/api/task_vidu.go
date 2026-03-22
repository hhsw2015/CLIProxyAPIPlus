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

type viduAdaptor struct{}

func (a *viduAdaptor) Platform() string { return "vidu" }

func (a *viduAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	var req struct {
		Images []string `json:"images"`
	}
	if err := json.Unmarshal(body, &req); err == nil && len(req.Images) > 0 {
		if len(req.Images) >= 2 {
			return "start-end2video", nil
		}
		return "img2video", nil
	}
	return "text2video", nil
}

func (a *viduAdaptor) BuildRequestURL(baseURL, action string) string {
	return strings.TrimSuffix(baseURL, "/") + "/ent/v2/" + action
}

func (a *viduAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Token "+apiKey)
	}
}

func (a *viduAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	var req struct {
		Prompt   string   `json:"prompt"`
		Model    string   `json:"model"`
		Images   []string `json:"images"`
		Duration int      `json:"duration"`
		Size     string   `json:"size"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", err
	}

	viduBody := map[string]any{
		"model":              model,
		"prompt":             req.Prompt,
		"duration":           max(req.Duration, 8),
		"resolution":         "1080p",
		"movement_amplitude": "auto",
		"bgm":                false,
	}
	if req.Size != "" {
		viduBody["resolution"] = req.Size
	}
	if len(req.Images) > 0 {
		viduBody["images"] = req.Images
	}

	data, err := json.Marshal(viduBody)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *viduAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	var result struct {
		TaskID string `json:"task_id"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}
	if result.TaskID == "" {
		return "", data, fmt.Errorf("empty task_id: %s", string(data))
	}
	return result.TaskID, data, nil
}

func (a *viduAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/ent/v2/tasks/" + upstreamTaskID + "/creations"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *viduAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result struct {
		State     string `json:"state"`
		ErrCode   string `json:"err_code"`
		Creations []struct {
			URL string `json:"url"`
		} `json:"creations"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	info := &TaskInfo{}
	switch strings.ToLower(result.State) {
	case "success":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		if len(result.Creations) > 0 {
			info.URL = result.Creations[0].URL
		}
	case "failed":
		info.Status = TaskStatusFailure
		info.Reason = result.ErrCode
	case "processing":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	case "created", "queueing":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusSubmitted
	}
	return info, nil
}

func (a *viduAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
