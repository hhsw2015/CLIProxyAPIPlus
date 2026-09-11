package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// topazAdaptor implements TaskAdaptor for Topaz Labs (api.topazlabs.com) image
// and video enhancement (SKYROUTER topaz channel). Topaz is an UPSCALER: every
// request needs an input image/video. Two backends:
//
//	image (topaz-image-*): POST /image/v1/enhance/async (multipart: model,image,
//	  output_format) -> {process_id}; poll GET /image/v1/status/{id}; on complete
//	  GET /image/v1/download/{id} -> {url}.
//	video (topaz-video-*): POST /video/express (JSON: source,filters[],output) ->
//	  {requestId}; poll GET /video/{id}/status -> {download:{url}} on complete.
//
// The body's model is already the upstream id ("Standard V2" / "apo-8") via the
// media/task model-remap. Auth is the X-API-Key header.
type topazAdaptor struct{}

func (a *topazAdaptor) Platform() string { return "topaz" }

// ValidateAndSetAction returns "video" for topaz-video-* models, else "image".
func (a *topazAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	// The remapped upstream id ("apo-8"/"prob-4") loses the topaz-video prefix,
	// so also honor an explicit client hint carried on the body.
	hint := strings.ToLower(gjson.GetBytes(body, "topaz_type").String())
	if hint == "video" || strings.Contains(model, "video") || model == "apo-8" || model == "prob-4" {
		return "video", nil
	}
	return "image", nil
}

func (a *topazAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if action == "video" {
		return base + "/video/express"
	}
	return base + "/image/v1/enhance/async"
}

func (a *topazAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
}

func (a *topazAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	upModel := gjson.GetBytes(body, "model").String()
	action, _ := a.ValidateAndSetAction(c, body)
	if action == "video" {
		// Video express: pass a source through, wrap the model as a filter.
		// If the client already supplied source/filters/output, forward verbatim;
		// otherwise build a minimal request from video_url + resolution.
		if gjson.GetBytes(body, "source").Exists() && gjson.GetBytes(body, "filters").Exists() {
			return bytes.NewReader(body), "application/json", nil
		}
		src := gjson.GetBytes(body, "video_url").String()
		if src == "" {
			src = gjson.GetBytes(body, "image").String()
		}
		res := gjson.GetBytes(body, "resolution").String()
		if res == "" {
			res = "1080p"
		}
		out := map[string]any{
			"source":  map[string]any{"url": src, "container": "mp4"},
			"filters": []map[string]any{{"model": upModel}},
			"output":  map[string]any{"resolution": res, "container": "mp4"},
		}
		data, err := json.Marshal(out)
		if err != nil {
			return nil, "", err
		}
		return bytes.NewReader(data), "application/json", nil
	}

	// Image enhance: multipart form (model, output_format, image bytes).
	imgBytes, err := loadInputImage(body)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", upModel)
	outFmt := gjson.GetBytes(body, "output_format").String()
	if outFmt == "" {
		outFmt = "jpg"
	}
	_ = w.WriteField("output_format", outFmt)
	fw, err := w.CreateFormFile("image", "input."+outFmt)
	if err != nil {
		return nil, "", err
	}
	if _, err := fw.Write(imgBytes); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

// loadInputImage resolves the body's image field (http(s) URL or data-uri/base64)
// into raw bytes for the Topaz multipart upload.
func loadInputImage(body []byte) ([]byte, error) {
	src := gjson.GetBytes(body, "image").String()
	if src == "" {
		src = gjson.GetBytes(body, "image_url").String()
	}
	if src == "" {
		return nil, fmt.Errorf("topaz image: missing image (url or base64)")
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(src)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	if i := strings.Index(src, "base64,"); i >= 0 {
		src = src[i+len("base64,"):]
	}
	return base64.StdEncoding.DecodeString(src)
}

func (a *topazAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	tid := gjson.GetBytes(data, "process_id").String()
	if tid == "" {
		tid = gjson.GetBytes(data, "requestId").String()
	}
	if tid == "" {
		return "", data, fmt.Errorf("topaz submit failed: %s", string(data[:min(len(data), 300)]))
	}
	return tid, data, nil
}

// FetchTask polls status; for a completed image job it follows through to the
// download endpoint so ParseTaskResult sees the final url in one body.
func (a *topazAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	client := &http.Client{Timeout: 30 * time.Second}
	do := func(method, url string) (*http.Response, error) {
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			return nil, err
		}
		a.BuildRequestHeader(req, apiKey)
		return client.Do(req)
	}
	if action == "video" {
		return do(http.MethodGet, base+"/video/"+upstreamTaskID+"/status")
	}
	// image: status, then download when complete.
	resp, err := do(http.MethodGet, base+"/image/v1/status/"+upstreamTaskID)
	if err != nil {
		return nil, err
	}
	sb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	st := strings.ToUpper(gjson.GetBytes(sb, "status").String())
	if st == "COMPLETE" || st == "COMPLETED" || st == "SUCCESS" || st == "DONE" {
		if dl, e := do(http.MethodGet, base+"/image/v1/download/"+upstreamTaskID); e == nil {
			return dl, nil
		}
	}
	return synthResponse(resp.StatusCode, sb), nil
}

// synthResponse wraps a pre-read body back into an *http.Response.
func synthResponse(code int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func (a *topazAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	info := &TaskInfo{}
	status := strings.ToUpper(gjson.GetBytes(respBody, "status").String())
	url := gjson.GetBytes(respBody, "url").String()
	if url == "" {
		url = gjson.GetBytes(respBody, "download.url").String()
	}
	switch {
	case url != "":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = url
	case status == "FAILED" || status == "FAIL" || status == "ERROR":
		info.Status = TaskStatusFailure
		info.Reason = gjson.GetBytes(respBody, "error").String()
		if info.Reason == "" {
			info.Reason = gjson.GetBytes(respBody, "message").String()
		}
	case status == "COMPLETE" || status == "COMPLETED" || status == "SUCCESS" || status == "DONE":
		// Completed but the follow-up download was not attached; keep polling.
		info.Status = TaskStatusInProgress
		info.Progress = "90%"
	case status == "QUEUED" || status == "PENDING":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	}
	return info, nil
}

func (a *topazAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
