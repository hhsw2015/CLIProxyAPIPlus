package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// foxtokenAdaptor implements TaskAdaptor for foxtoken.linkomobile.com
// (SKYROUTER huawi_zhilian channel) — an OpenAI-ish async video aggregator
// serving Seedance. Submit: POST /v1/video/generations {model,prompt,...} ->
// {task_id,status:"queued"}. Poll: GET /v1/videos/{task_id} ->
// {status,progress,metadata:{url}}. The body's model is already the upstream id
// (media/task model-remap rewrote the client alias before this runs).
type foxtokenAdaptor struct{}

func (a *foxtokenAdaptor) Platform() string { return "foxtoken" }

func (a *foxtokenAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	if strings.TrimSpace(gjson.GetBytes(body, "prompt").String()) == "" {
		return "", fmt.Errorf("foxtoken: prompt is required")
	}
	return "generate", nil
}

func (a *foxtokenAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	return base + "/v1/video/generations"
}

func (a *foxtokenAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *foxtokenAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	// OpenAI-ish: pass the body through verbatim (model already remapped to the
	// upstream id). Just guarantee Content-Type.
	return bytes.NewReader(body), "application/json", nil
}

func (a *foxtokenAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	tid := gjson.GetBytes(data, "task_id").String()
	if tid == "" {
		tid = gjson.GetBytes(data, "id").String()
	}
	if tid == "" {
		msg := gjson.GetBytes(data, "message").String()
		return "", data, fmt.Errorf("foxtoken submit failed: %s", msg)
	}
	return tid, data, nil
}

func (a *foxtokenAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	req, err := http.NewRequest(http.MethodGet, base+"/v1/videos/"+upstreamTaskID, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *foxtokenAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	info := &TaskInfo{}
	// /v1/videos/{id} shape: {status,progress,metadata:{url}}.
	status := strings.ToUpper(gjson.GetBytes(respBody, "status").String())
	url := gjson.GetBytes(respBody, "metadata.url").String()
	if url == "" {
		// Fall back to the richer /v1/video/generations/{id} data envelope.
		url = gjson.GetBytes(respBody, "data.metadata.url").String()
		if s := gjson.GetBytes(respBody, "data.status").String(); s != "" {
			status = strings.ToUpper(s)
		}
	}
	switch {
	case url != "" || status == "SUCCESS" || status == "SUCCEEDED" || status == "COMPLETED":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = url
	case status == "FAILED" || status == "FAIL" || status == "ERROR":
		info.Status = TaskStatusFailure
		info.Reason = gjson.GetBytes(respBody, "data.fail_reason").String()
		if info.Reason == "" {
			info.Reason = gjson.GetBytes(respBody, "fail_reason").String()
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

func (a *foxtokenAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
