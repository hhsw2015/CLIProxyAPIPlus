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
	"github.com/google/uuid"
)

// pixverseAdaptor serves PixVerse text-to-video first-party (app-api.pixverse.ai)
// via the async OpenAPI: submit /openapi/v2/video/text/generate → poll
// /openapi/v2/video/result/{id}. Auth = API-KEY header + a per-request Ai-trace-id.
type pixverseAdaptor struct{}

func (a *pixverseAdaptor) Platform() string { return "pixverse" }

func (a *pixverseAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	return "text", nil
}

func (a *pixverseAdaptor) BuildRequestURL(baseURL, action string) string {
	return strings.TrimSuffix(baseURL, "/") + "/openapi/v2/video/text/generate"
}

func (a *pixverseAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Ai-trace-id", uuid.NewString())
	if apiKey != "" {
		req.Header.Set("API-KEY", apiKey)
	}
}

// pixverseModel maps a client model name (e.g. "PixVerse-v6") to the PixVerse
// version param ("v6"/"c1").
func pixverseModel(model string) string {
	l := strings.ToLower(model)
	switch {
	case strings.Contains(l, "c1"):
		return "c1"
	case strings.Contains(l, "v6"):
		return "v6"
	default:
		return "v6"
	}
}

func (a *pixverseAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	var req struct {
		Prompt         string `json:"prompt"`
		Duration       int    `json:"duration"`
		Quality        string `json:"quality"`
		AspectRatio    string `json:"aspect_ratio"`
		NegativePrompt string `json:"negative_prompt"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", err
	}
	out := map[string]any{
		"model":        pixverseModel(model),
		"prompt":       req.Prompt,
		"duration":     5,
		"quality":      "540p",
		"aspect_ratio": "16:9",
	}
	if req.Duration > 0 {
		out["duration"] = req.Duration
	}
	if req.Quality != "" {
		out["quality"] = req.Quality
	}
	if req.AspectRatio != "" {
		out["aspect_ratio"] = req.AspectRatio
	}
	if req.NegativePrompt != "" {
		out["negative_prompt"] = req.NegativePrompt
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *pixverseAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	var result struct {
		ErrCode int    `json:"ErrCode"`
		ErrMsg  string `json:"ErrMsg"`
		Resp    struct {
			VideoID json.Number `json:"video_id"`
		} `json:"Resp"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}
	if result.ErrCode != 0 {
		return "", data, fmt.Errorf("pixverse error %d: %s", result.ErrCode, result.ErrMsg)
	}
	vid := result.Resp.VideoID.String()
	if vid == "" || vid == "0" {
		return "", data, fmt.Errorf("empty video_id: %s", string(data))
	}
	return vid, data, nil
}

func (a *pixverseAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/openapi/v2/video/result/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *pixverseAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result struct {
		ErrCode int `json:"ErrCode"`
		Resp    struct {
			Status int    `json:"status"`
			URL    string `json:"url"`
		} `json:"Resp"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	info := &TaskInfo{}
	// PixVerse status: 1=success, 5=generating, 7=moderation-fail, 8=failed.
	switch result.Resp.Status {
	case 1:
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = result.Resp.URL
	case 5:
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	default:
		info.Status = TaskStatusFailure
		info.Reason = fmt.Sprintf("pixverse status %d", result.Resp.Status)
	}
	return info, nil
}

func (a *pixverseAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
