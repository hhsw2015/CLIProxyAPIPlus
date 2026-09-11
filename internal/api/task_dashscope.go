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

// dashscopeAdaptor implements TaskAdaptor for Alibaba DashScope async generation
// (SKYROUTER aliyun-image / aliyun-video channels) — Qwen-Image / Z-Image and
// Wan video. These channels carry valid keys but are not wired in SKYROUTER's
// routes, so CPA exposes them straight from the channel config.
//
//	image (qwen-image*/z-image*): POST /api/v1/services/aigc/text2image/image-synthesis
//	video (wan*):                 POST /api/v1/services/aigc/video-generation/video-synthesis
//
// Both are async: submit with header X-DashScope-Async: enable -> {output.task_id};
// poll GET /api/v1/tasks/{id} -> {output.task_status, output.results[].url |
// output.video_url}.
type dashscopeAdaptor struct{}

func (a *dashscopeAdaptor) Platform() string { return "dashscope" }

func (a *dashscopeAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	if strings.TrimSpace(gjson.GetBytes(body, "prompt").String()) == "" &&
		strings.TrimSpace(gjson.GetBytes(body, "input.prompt").String()) == "" {
		return "", fmt.Errorf("dashscope: prompt is required")
	}
	m := strings.ToLower(gjson.GetBytes(body, "model").String())
	if strings.Contains(m, "wan") || strings.Contains(m, "video") || strings.Contains(m, "t2v") {
		return "video", nil
	}
	return "image", nil
}

func (a *dashscopeAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://dashscope.aliyuncs.com"
	}
	if action == "video" {
		return base + "/api/v1/services/aigc/video-generation/video-synthesis"
	}
	return base + "/api/v1/services/aigc/text2image/image-synthesis"
}

func (a *dashscopeAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// Async submit; the GET poll re-uses this but the header is harmless there.
	req.Header.Set("X-DashScope-Async", "enable")
}

func (a *dashscopeAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	up := gjson.GetBytes(body, "model").String()
	prompt := gjson.GetBytes(body, "prompt").String()
	if prompt == "" {
		prompt = gjson.GetBytes(body, "input.prompt").String()
	}
	input := map[string]any{"prompt": prompt}
	if v := gjson.GetBytes(body, "negative_prompt"); v.Exists() {
		input["negative_prompt"] = v.String()
	}
	// i2v / image-edit: pass an input image url through if present.
	for _, k := range []string{"img_url", "image_url", "image"} {
		if v := gjson.GetBytes(body, k); v.Exists() && v.String() != "" {
			input["img_url"] = v.String()
			break
		}
	}
	params := map[string]any{}
	for _, k := range []string{"size", "n", "resolution", "duration", "seed"} {
		if v := gjson.GetBytes(body, k); v.Exists() {
			params[k] = v.Value()
		}
	}
	out := map[string]any{"model": up, "input": input}
	if len(params) > 0 {
		out["parameters"] = params
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *dashscopeAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	tid := gjson.GetBytes(data, "output.task_id").String()
	if tid == "" {
		code := gjson.GetBytes(data, "code").String()
		msg := gjson.GetBytes(data, "message").String()
		return "", data, fmt.Errorf("dashscope submit failed (%s): %s", code, msg)
	}
	return tid, data, nil
}

func (a *dashscopeAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://dashscope.aliyuncs.com"
	}
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/tasks/"+upstreamTaskID, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *dashscopeAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	info := &TaskInfo{}
	status := strings.ToUpper(gjson.GetBytes(respBody, "output.task_status").String())
	// image: output.results[0].url ; video: output.video_url
	url := gjson.GetBytes(respBody, "output.video_url").String()
	if url == "" {
		res := gjson.GetBytes(respBody, "output.results")
		if res.IsArray() && len(res.Array()) > 0 {
			url = res.Array()[0].Get("url").String()
		}
	}
	switch {
	case url != "" || status == "SUCCEEDED":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = url
	case status == "FAILED" || status == "CANCELED" || status == "UNKNOWN":
		info.Status = TaskStatusFailure
		info.Reason = gjson.GetBytes(respBody, "output.message").String()
		if info.Reason == "" {
			info.Reason = gjson.GetBytes(respBody, "message").String()
		}
	case status == "PENDING":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	}
	return info, nil
}

func (a *dashscopeAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
