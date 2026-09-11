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

// runninghubAdaptor implements TaskAdaptor for RunningHub (www.runninghub.ai).
// RunningHub RHArt / model APIs are queue-based: POST /openapi/v2/{endpoint}
// with {prompt,resolution,...} returns a taskId; poll POST /openapi/v2/query
// {taskId} until status=SUCCESS with results[].url. The {endpoint} is the
// upstream model id (e.g. rhart-image-n-pro-official-token/text-to-image or
// bytedance/seedance-2.0-global-fast-token/text-to-video); it arrives as the
// request body's "model" (the media/task model-remap already rewrote the client
// alias to the upstream id) and is carried via the action string into the URL.
type runninghubAdaptor struct{}

func (a *runninghubAdaptor) Platform() string { return "runninghub" }

func (a *runninghubAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	// The upstream endpoint is the (already-remapped) model id.
	ep := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if ep == "" {
		return "", fmt.Errorf("runninghub: missing model")
	}
	return ep, nil
}

func (a *runninghubAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if !strings.Contains(base, "/openapi/") {
		base += "/openapi/v2"
	}
	return base + "/" + strings.TrimPrefix(action, "/")
}

func (a *runninghubAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *runninghubAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	// Translate OpenAI-ish {prompt,size,image,...} into RunningHub {prompt,
	// resolution,...}. resolution defaults to 1K (RunningHub rejects WxH).
	prompt := gjson.GetBytes(body, "prompt").String()
	if prompt == "" {
		prompt = gjson.GetBytes(body, "input").String()
	}
	// RunningHub video endpoints reject the image "1K" resolution — they want
	// 480p/720p/1080p/2k/4k. Default per endpoint kind (model carries the
	// remapped upstream path, e.g. .../text-to-video).
	res := gjson.GetBytes(body, "resolution").String()
	if res == "" {
		lm := strings.ToLower(model)
		if strings.Contains(lm, "video") || strings.Contains(lm, "t2v") ||
			strings.Contains(lm, "i2v") || strings.Contains(lm, "seedance") {
			res = "1080p"
		} else {
			res = "1K"
		}
	}
	out := map[string]any{"prompt": prompt, "resolution": res}
	// Pass through image/duration/ratio when present (i2v / video tuning).
	for _, k := range []string{"image", "images", "image_url", "duration", "ratio", "aspect_ratio", "seed"} {
		if v := gjson.GetBytes(body, k); v.Exists() {
			out[k] = v.Value()
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *runninghubAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	tid := gjson.GetBytes(data, "taskId").String()
	if tid == "" {
		ec := gjson.GetBytes(data, "errorCode").String()
		msg := gjson.GetBytes(data, "errorMessage").String()
		return "", data, fmt.Errorf("runninghub submit failed (code=%s): %s", ec, msg)
	}
	return tid, data, nil
}

func (a *runninghubAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if !strings.Contains(base, "/openapi/") {
		base += "/openapi/v2"
	}
	q := map[string]string{"taskId": upstreamTaskID}
	b, _ := json.Marshal(q)
	req, err := http.NewRequest(http.MethodPost, base+"/query", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *runninghubAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	info := &TaskInfo{}
	status := strings.ToUpper(gjson.GetBytes(respBody, "status").String())
	results := gjson.GetBytes(respBody, "results")
	url := ""
	if results.IsArray() && len(results.Array()) > 0 {
		url = results.Array()[0].Get("url").String()
	}
	switch {
	case url != "" || status == "SUCCESS" || status == "COMPLETED":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = url
	case status == "FAILED" || status == "FAIL":
		info.Status = TaskStatusFailure
		info.Reason = gjson.GetBytes(respBody, "failedReason").String()
		if info.Reason == "" {
			info.Reason = gjson.GetBytes(respBody, "errorMessage").String()
		}
	case status == "QUEUED":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	}
	return info, nil
}

func (a *runninghubAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
