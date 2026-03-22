package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type geminiTaskAdaptor struct{}

func (a *geminiTaskAdaptor) Platform() string { return "gemini" }

func (a *geminiTaskAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	return "generate", nil
}

func (a *geminiTaskAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(baseURL, "/")
	// Gemini Veo: POST {base}/v1beta/models/{model}:predictLongRunning
	// For Imagen: POST {base}/v1beta/models/{model}:predict
	// We'll use the base URL as-is since it should contain the full path.
	if strings.Contains(base, ":predictLongRunning") || strings.Contains(base, ":predict") {
		return base
	}
	return base + "/v1beta/models/veo-3.0-generate-001:predictLongRunning"
}

func (a *geminiTaskAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
	}
}

func (a *geminiTaskAdaptor) BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error) {
	// Convert generic task request to Veo format.
	var req struct {
		Prompt   string         `json:"prompt"`
		Model    string         `json:"model"`
		Duration int            `json:"duration"`
		Size     string         `json:"size"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", err
	}

	veoBody := map[string]any{
		"instances": []map[string]any{
			{"prompt": req.Prompt},
		},
		"parameters": map[string]any{
			"sampleCount": 1,
		},
	}
	if req.Duration > 0 {
		veoBody["parameters"].(map[string]any)["durationSeconds"] = req.Duration
	}
	data, err := json.Marshal(veoBody)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(data), "application/json", nil
}

func (a *geminiTaskAdaptor) ParseSubmitResponse(resp *http.Response) (string, []byte, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	// Gemini returns: {"name": "models/veo.../operations/xxx", "done": false}
	var result struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", data, err
	}
	if result.Name == "" {
		return "", data, fmt.Errorf("empty operation name: %s", string(data))
	}
	// Encode the operation name as task ID.
	taskID := base64.RawURLEncoding.EncodeToString([]byte(result.Name))
	return taskID, data, nil
}

func (a *geminiTaskAdaptor) FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error) {
	// Decode the operation name.
	nameBytes, err := base64.RawURLEncoding.DecodeString(upstreamTaskID)
	if err != nil {
		return nil, fmt.Errorf("decode task ID: %w", err)
	}
	operationName := string(nameBytes)

	url := strings.TrimSuffix(baseURL, "/") + "/v1beta/" + operationName
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (a *geminiTaskAdaptor) ParseTaskResult(respBody []byte) (*TaskInfo, error) {
	var op struct {
		Name  string `json:"name"`
		Done  bool   `json:"done"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Response struct {
			GenerateVideoResponse struct {
				GeneratedVideos []struct {
					Video struct {
						URI string `json:"uri"`
					} `json:"video"`
				} `json:"generatedVideos"`
			} `json:"generateVideoResponse"`
		} `json:"response"`
	}
	if err := json.Unmarshal(respBody, &op); err != nil {
		return nil, err
	}

	info := &TaskInfo{}
	if op.Error.Message != "" {
		info.Status = TaskStatusFailure
		info.Reason = op.Error.Message
		return info, nil
	}
	if !op.Done {
		info.Status = TaskStatusInProgress
		info.Progress = "50%"
		return info, nil
	}
	info.Status = TaskStatusSuccess
	info.Progress = "100%"
	if len(op.Response.GenerateVideoResponse.GeneratedVideos) > 0 {
		info.URL = op.Response.GenerateVideoResponse.GeneratedVideos[0].Video.URI
	}
	return info, nil
}

func (a *geminiTaskAdaptor) BuildClientResponse(task *Task) any {
	return (&soraAdaptor{}).BuildClientResponse(task)
}
