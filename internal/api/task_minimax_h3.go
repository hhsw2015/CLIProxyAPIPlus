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

// minimaxH3Adaptor implements TaskAdaptor for MiniMax's v2 video API
// (SKYROUTER minimax-video-v2-direct channel, api.minimaxi.com) — MiniMax-H3 /
// -H3-Max. This is the newer /v2 protocol (content array + ratio), distinct from
// the v1 hailuoAdaptor. The channel is orphan+live (valid key, not in routes).
//
//	submit: POST /v2/video_generation {model, content:[{type:"text",text}],
//	        duration, resolution, ratio} -> {task_id}
//	poll:   GET /v1/query/video_generation?task_id=... -> {status, file_id}
//	url:    on status=Success, GET /v1/files/retrieve?file_id=... -> file.download_url
//
// The body's model is already the upstream id (MiniMax-H3) via the task
// model-remap.
type minimaxH3Adaptor struct{}

func (a *minimaxH3Adaptor) Platform() string { return "minimax-h3" }

func (a *minimaxH3Adaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	if strings.TrimSpace(gjson.GetBytes(body, "prompt").String()) == "" &&
		!gjson.GetBytes(body, "content").Exists() {
		return "", fmt.Errorf("minimax-h3: prompt (or content) is required")
	}
	return "video", nil
}

func (a *minimaxH3Adaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://api.minimaxi.com"
	}
	return base + "/v2/video_generation"
}

func (a *minimaxH3Adaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *minimaxH3Adaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	out := map[string]any{"model": gjson.GetBytes(body, "model").String()}
	// content: pass a client-provided array through, else build from prompt.
	if cv := gjson.GetBytes(body, "content"); cv.Exists() && cv.IsArray() {
		out["content"] = cv.Value()
	} else {
		out["content"] = []map[string]any{{"type": "text", "text": gjson.GetBytes(body, "prompt").String()}}
	}
	// v2 t2v requires duration, resolution and an explicit ratio (not adaptive).
	dur := gjson.GetBytes(body, "duration").Int()
	if dur == 0 {
		dur = 6
	}
	out["duration"] = dur
	res := gjson.GetBytes(body, "resolution").String()
	if res == "" {
		res = "768P" // MiniMax-H3 supports 480P/768P/2K (not 1080P)
	}
	out["resolution"] = res
	ratio := gjson.GetBytes(body, "ratio").String()
	if ratio == "" {
		ratio = gjson.GetBytes(body, "aspect_ratio").String()
	}
	if ratio == "" {
		ratio = "16:9"
	}
	out["ratio"] = ratio
	data, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *minimaxH3Adaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	if code := gjson.GetBytes(data, "base_resp.status_code").Int(); code != 0 {
		return "", data, fmt.Errorf("minimax-h3 error: %s", gjson.GetBytes(data, "base_resp.status_msg").String())
	}
	tid := gjson.GetBytes(data, "task_id").String()
	if tid == "" {
		return "", data, fmt.Errorf("minimax-h3: empty task_id: %s", string(data[:min(len(data), 200)]))
	}
	return tid, data, nil
}

// FetchTask queries status; on Success it follows through to the file-retrieve
// endpoint so ParseTaskResult sees the download_url in one body.
func (a *minimaxH3Adaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://api.minimaxi.com"
	}
	client := &http.Client{Timeout: 30 * time.Second}
	get := func(url string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		a.BuildRequestHeader(req, apiKey)
		return client.Do(req)
	}
	resp, err := get(base + "/v1/query/video_generation?task_id=" + upstreamTaskID)
	if err != nil {
		return nil, err
	}
	qb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	status := strings.ToLower(gjson.GetBytes(qb, "status").String())
	fileID := gjson.GetBytes(qb, "file_id").String()
	if status == "success" && fileID != "" {
		if fr, e := get(base + "/v1/files/retrieve?file_id=" + fileID); e == nil {
			return fr, nil
		}
	}
	return synthResponse(resp.StatusCode, qb), nil
}

func (a *minimaxH3Adaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	info := &TaskInfo{}
	// file-retrieve response carries file.download_url.
	url := gjson.GetBytes(respBody, "file.download_url").String()
	if url != "" {
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = url
		return info, nil
	}
	status := strings.ToLower(gjson.GetBytes(respBody, "status").String())
	switch status {
	case "success":
		// Completed but file not yet attached; keep polling to retrieve the url.
		info.Status = TaskStatusInProgress
		info.Progress = "90%"
	case "fail", "failed":
		info.Status = TaskStatusFailure
		info.Reason = gjson.GetBytes(respBody, "base_resp.status_msg").String()
	case "preparing", "queueing", "queued":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	}
	return info, nil
}

func (a *minimaxH3Adaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
