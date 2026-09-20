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

// atlasAdaptor implements TaskAdaptor for the Atlas Cloud media Predictions API
// (https://www.atlascloud.ai/docs/predictions). It is separate from Atlas's
// OpenAI-compatible LLM host (origin-api.atlascloud.ai): media models are NOT
// listed by /v1/models and use their own async endpoints on api.atlascloud.ai:
//
//	POST {base}/api/v1/model/generateImage | generateVideo | generateAudio
//	     body {"model": "<vendor>/<model>/<task>", "prompt": ..., ...} -> data.id
//	GET  {base}/api/v1/model/prediction/{id}
//	     -> data.status processing|completed|failed, data.outputs [url...], data.error
//
// The generate* path is derived from the upstream model id (the task slug
// after the last "/": text-to-image / edit -> image, *-to-video / video-* ->
// video, tts / music / asr / speech -> audio). Entries are named
// "atlas-media-*" so platformForEntry picks this adaptor.
type atlasAdaptor struct{}

func (a *atlasAdaptor) Platform() string { return "atlas" }

// ValidateAndSetAction maps the upstream model id to the generate* endpoint.
// The body has already been rewritten to the candidate's upstream id.
func (a *atlasAdaptor) ValidateAndSetAction(_ *gin.Context, body []byte) (string, error) {
	model := strings.ToLower(gjson.GetBytes(body, "model").String())
	if model == "" {
		return "", fmt.Errorf("atlas: model is required")
	}
	return atlasActionForModel(model), nil
}

// atlasActionForModel classifies an Atlas media model id into image/video/audio.
func atlasActionForModel(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "video") || strings.Contains(m, "animate") ||
		strings.Contains(m, "avatar") || strings.Contains(m, "lipsync") ||
		strings.Contains(m, "kling-v") || strings.Contains(m, "hailuo") ||
		strings.Contains(m, "seedance-v1-pro-t2v") || strings.Contains(m, "seedance-v1-pro-i2v"):
		return "video"
	case strings.Contains(m, "tts") || strings.Contains(m, "stt") || strings.Contains(m, "speech") ||
		strings.Contains(m, "music") || strings.Contains(m, "asr") || strings.Contains(m, "audio") || strings.Contains(m, "lyrics") ||
		strings.HasPrefix(m, "suno/") || strings.Contains(m, "chirp"):
		return "audio"
	default:
		return "image"
	}
}

func (a *atlasAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(baseURL, "/")
	switch action {
	case "video":
		return base + "/api/v1/model/generateVideo"
	case "audio":
		return base + "/api/v1/model/generateAudio"
	default:
		return base + "/api/v1/model/generateImage"
	}
}

func (a *atlasAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

// BuildRequestBody passes the client body through: Atlas takes the same
// {"model","prompt",...} shape and per-model parameters as the client sends.
func (a *atlasAdaptor) BuildRequestBody(_ *gin.Context, body []byte, _ string) (io.Reader, string, error) {
	return bytes.NewReader(body), "application/json", nil
}

func (a *atlasAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	id := gjson.GetBytes(data, "data.id").String()
	if id == "" {
		return "", data, fmt.Errorf("atlas: no prediction id in response: %s", string(data))
	}
	return id, data, nil
}

func (a *atlasAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, _ string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/api/v1/model/prediction/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *atlasAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	if !gjson.ValidBytes(respBody) {
		return nil, fmt.Errorf("atlas: invalid poll response")
	}
	d := gjson.GetBytes(respBody, "data")
	info := &TaskInfo{}
	switch strings.ToLower(d.Get("status").String()) {
	case "completed":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		if outs := d.Get("outputs").Array(); len(outs) > 0 {
			info.URL = outs[0].String()
		}
	case "failed":
		info.Status = TaskStatusFailure
		info.Reason = d.Get("error").String()
		if info.Reason == "" {
			info.Reason = "atlas task failed"
		}
	case "processing":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	default:
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	}
	return info, nil
}

func (a *atlasAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
