package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type klingAdaptor struct{}

func (a *klingAdaptor) Platform() string { return "kling" }

func (a *klingAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	path := c.Request.URL.Path
	if strings.Contains(path, "image2video") {
		return "image2video", nil
	}
	return "text2video", nil
}

func (a *klingAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(baseURL, "/")
	return base + "/v1/videos/" + action
}

func (a *klingAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	// Kling uses accessKey|secretKey format for JWT-like HMAC auth.
	// If apiKey contains "|", split into access/secret and sign.
	if strings.Contains(apiKey, "|") {
		parts := strings.SplitN(apiKey, "|", 2)
		accessKey := parts[0]
		secretKey := parts[1]
		timestamp := fmt.Sprintf("%d", time.Now().Unix())
		// Simple HMAC-SHA256 signature.
		mac := hmac.New(sha256.New, []byte(secretKey))
		mac.Write([]byte(accessKey + timestamp))
		signature := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s.%s.%s", accessKey, timestamp, signature))
	} else if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *klingAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	// Inject highest quality defaults if not present.
	var bodyMap map[string]any
	if json.Unmarshal(body, &bodyMap) == nil {
		if _, ok := bodyMap["watermark_info"]; !ok {
			bodyMap["watermark_info"] = map[string]any{"enabled": false}
		}
		if _, ok := bodyMap["mode"]; !ok {
			bodyMap["mode"] = "std"
		}
		if _, ok := bodyMap["sound"]; !ok {
			bodyMap["sound"] = "on"
		}
		if _, ok := bodyMap["duration"]; !ok {
			bodyMap["duration"] = "15"
		}
		if updated, err := json.Marshal(bodyMap); err == nil {
			body = updated
		}
	}
	return bytes.NewReader(body), "application/json", nil
}

func (a *klingAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("read response: %w", err)
	}
	// Kling response: {"code":0,"data":{"task_id":"xxx"}}
	var result struct {
		Code int `json:"code"`
		Data struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, fmt.Errorf("parse response: %w", err)
	}
	if result.Data.TaskID == "" {
		return "", data, fmt.Errorf("empty task ID: %s", string(data))
	}
	return result.Data.TaskID, data, nil
}

func (a *klingAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/v1/videos/" + action + "/" + upstreamTaskID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *klingAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var result struct {
		Code int `json:"code"`
		Data struct {
			TaskID    string `json:"task_id"`
			TaskStatus string `json:"task_status"`
			VideoURL  string `json:"video_url"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	info := &TaskInfo{TaskID: result.Data.TaskID}
	switch strings.ToLower(result.Data.TaskStatus) {
	case "succeed", "completed":
		info.Status = TaskStatusSuccess
		info.Progress = "100%"
		info.URL = result.Data.VideoURL
	case "failed":
		info.Status = TaskStatusFailure
		info.Reason = result.Message
	case "processing", "in_progress":
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
	case "submitted", "queued":
		info.Status = TaskStatusQueued
		info.Progress = "20%"
	default:
		info.Status = TaskStatusSubmitted
	}
	return info, nil
}

func (a *klingAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
