package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2/google"
)

type geminiTaskAdaptor struct{}

func (a *geminiTaskAdaptor) Platform() string { return "gemini" }

func (a *geminiTaskAdaptor) ValidateAndSetAction(c *gin.Context, body []byte) (string, error) {
	return "generate", nil
}

func (a *geminiTaskAdaptor) BuildRequestURL(baseURL, action string) string {
	base := strings.TrimSuffix(baseURL, "/")
	if strings.Contains(base, ":predictLongRunning") || strings.Contains(base, ":predict") {
		return base
	}
	// Vertex AI endpoint: contains aiplatform.googleapis.com
	// Needs project ID from credentials — handled in BuildRequestHeader
	if strings.Contains(base, "aiplatform.googleapis.com") {
		return base // Full URL already constructed
	}
	// Gemini API key mode: generativelanguage.googleapis.com
	return base + "/v1beta/models/veo-3.0-generate-001:predictLongRunning"
}

func (a *geminiTaskAdaptor) BuildRequestHeader(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	if apiKey == "" {
		return
	}
	// If apiKey looks like a service account JSON (starts with '{'),
	// use Vertex OAuth to get a Bearer token.
	if strings.HasPrefix(strings.TrimSpace(apiKey), "{") {
		token, projectID, err := acquireVertexOAuthToken(apiKey)
		if err == nil && token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
			if projectID != "" {
				req.Header.Set("x-goog-user-project", projectID)
			}
			return
		}
		// Fall through to API key mode on failure.
	}
	// Default: Gemini API key mode.
	req.Header.Set("x-goog-api-key", apiKey)
}

// acquireVertexOAuthToken exchanges a service account JSON for an OAuth2 access token.
// The apiKey field contains the full service account JSON.
func acquireVertexOAuthToken(saJSON string) (token, projectID string, err error) {
	var sa struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(saJSON), &sa); err != nil {
		return "", "", fmt.Errorf("parse service account: %w", err)
	}
	creds, err := google.CredentialsFromJSON(
		context.Background(),
		[]byte(saJSON),
		"https://www.googleapis.com/auth/cloud-platform",
	)
	if err != nil {
		return "", "", fmt.Errorf("credentials from json: %w", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", "", fmt.Errorf("get token: %w", err)
	}
	return tok.AccessToken, sa.ProjectID, nil
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
	nameBytes, err := base64.RawURLEncoding.DecodeString(upstreamTaskID)
	if err != nil {
		return nil, fmt.Errorf("decode task ID: %w", err)
	}
	operationName := string(nameBytes)

	// For Vertex AI, the operation name contains the full path including project/location.
	// Use fetchPredictOperation endpoint if it looks like a Vertex operation.
	var fetchURL string
	if strings.Contains(operationName, "projects/") {
		// Vertex AI: POST fetchPredictOperation
		region := extractRegionFromName(operationName)
		project := extractProjectFromName(operationName)
		modelName := extractModelFromName(operationName)
		if region == "" {
			region = "us-central1"
		}
		if region == "global" {
			fetchURL = fmt.Sprintf("https://aiplatform.googleapis.com/v1/projects/%s/locations/global/publishers/google/models/%s:fetchPredictOperation", project, modelName)
		} else {
			fetchURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:fetchPredictOperation", region, project, region, modelName)
		}
		payload, _ := json.Marshal(map[string]string{"operationName": operationName})
		req, err := http.NewRequest(http.MethodPost, fetchURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		a.BuildRequestHeader(req, apiKey)
		return (&http.Client{Timeout: 30 * time.Second}).Do(req)
	}

	// Gemini API key mode: GET operation status.
	fetchURL = strings.TrimSuffix(baseURL, "/") + "/v1beta/" + operationName
	req, err := http.NewRequest(http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, err
	}
	a.BuildRequestHeader(req, apiKey)
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

// Helper functions for extracting components from Vertex operation names.
func extractRegionFromName(name string) string {
	// Pattern: projects/xxx/locations/REGION/...
	parts := strings.Split(name, "/")
	for i, p := range parts {
		if p == "locations" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func extractProjectFromName(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func extractModelFromName(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		if p == "models" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
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
