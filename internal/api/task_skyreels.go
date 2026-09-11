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
	"github.com/tidwall/gjson"
)

// skyreelsAdaptor implements TaskAdaptor for SkyReels (api-gateway.skyreels.ai,
// SKYROUTER skyreels / skyreels-nsfw channels) — skywork-h3 (Skywork's own H3).
// Orphan+live: valid key + model_ids present, not wired in SKYROUTER routes.
//
//	submit: POST /api/v1/h3/t2av/submit {api_key, content:[{type:"text",text}]}
//	        (i2av when an input image is provided) -> {task_id, status}
//	poll:   GET /api/v1/h3/t2av/task/{task_id} -> {status, code, data}
//
// Auth is carried in the body's api_key field (Bearer alone is rejected); the
// resolved provider key is read from the gin context set by taskSubmitHandler.
type skyreelsAdaptor struct{}

func (a *skyreelsAdaptor) Platform() string { return "skyreels" }

func (a *skyreelsAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	if strings.TrimSpace(gjson.GetBytes(body, "prompt").String()) == "" &&
		!gjson.GetBytes(body, "content").Exists() {
		return "", fmt.Errorf("skyreels: prompt (or content) is required")
	}
	// image-to-audio-video when an input image is present, else text-to-av.
	for _, k := range []string{"image", "image_url", "img_url"} {
		if gjson.GetBytes(body, k).String() != "" {
			return "i2av", nil
		}
	}
	return "t2av", nil
}

func (a *skyreelsAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://api-gateway.skyreels.ai"
	}
	if action == "" {
		action = "t2av"
	}
	return base + "/api/v1/h3/" + action + "/submit"
}

func (a *skyreelsAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *skyreelsAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	out := map[string]any{"api_key": c.GetString("task_provider_api_key")}
	if cv := gjson.GetBytes(body, "content"); cv.Exists() && cv.IsArray() {
		out["content"] = cv.Value()
	} else {
		out["content"] = []map[string]any{{"type": "text", "text": gjson.GetBytes(body, "prompt").String()}}
	}
	for _, k := range []string{"image", "image_url", "img_url"} {
		if v := gjson.GetBytes(body, k).String(); v != "" {
			out["image_url"] = v
			break
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *skyreelsAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	tid := gjson.GetBytes(data, "task_id").String()
	if tid == "" {
		tid = gjson.GetBytes(data, "data.task_id").String()
	}
	if tid == "" {
		return "", data, fmt.Errorf("skyreels submit failed: %s", gjson.GetBytes(data, "msg").String())
	}
	return tid, data, nil
}

func (a *skyreelsAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://api-gateway.skyreels.ai"
	}
	if action == "" {
		action = "t2av"
	}
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/h3/"+action+"/task/"+upstreamTaskID, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *skyreelsAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	info := &TaskInfo{}
	status := strings.ToLower(gjson.GetBytes(respBody, "status").String())
	// url appears under data once complete (video_url / url / results[].url).
	url := gjson.GetBytes(respBody, "data.video_url").String()
	if url == "" {
		url = gjson.GetBytes(respBody, "data.url").String()
	}
	if url == "" {
		if r := gjson.GetBytes(respBody, "data.results"); r.IsArray() && len(r.Array()) > 0 {
			url = r.Array()[0].Get("url").String()
		}
	}
	switch {
	case url != "" || status == "succeeded" || status == "success" || status == "completed":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = url
	case status == "failed" || status == "fail" || status == "error":
		info.Status = TaskStatusFailure
		info.Reason = gjson.GetBytes(respBody, "msg").String()
	case status == "submitted" || status == "queued" || status == "pending":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	}
	return info, nil
}

func (a *skyreelsAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
