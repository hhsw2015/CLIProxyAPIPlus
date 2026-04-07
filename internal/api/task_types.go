package api

import (
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Task represents an async media generation task.
type Task struct {
	ID         string    `json:"id"`          // Public ID: "task_xxxx"
	Model      string    `json:"model"`       // Requested model name
	Platform   string    `json:"platform"`    // Provider platform (sora, kling, suno, etc.)
	Action     string    `json:"action"`      // e.g. "text2video", "image2video", "generate"
	Status     string    `json:"status"`      // NOT_START, SUBMITTED, QUEUED, IN_PROGRESS, SUCCESS, FAILURE
	Progress   string    `json:"progress"`    // "0%".."100%"
	FailReason string    `json:"fail_reason,omitempty"`
	ResultURL  string    `json:"result_url,omitempty"`
	Data       []byte    `json:"-"` // Raw upstream response data
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	// Private fields for polling (not exposed to client).
	UpstreamTaskID  string `json:"-"`
	ProviderBaseURL string `json:"-"`
	ProviderAPIKey  string `json:"-"`
	ProviderProxy   string `json:"-"`
	PollingActive   bool   `json:"-"` // true when a dedicated goroutine is polling this task
}

// Task status constants.
const (
	TaskStatusNotStart   = "NOT_START"
	TaskStatusSubmitted  = "SUBMITTED"
	TaskStatusQueued     = "QUEUED"
	TaskStatusInProgress = "IN_PROGRESS"
	TaskStatusSuccess    = "SUCCESS"
	TaskStatusFailure    = "FAILURE"
)

// TaskInfo is the parsed result from polling an upstream task.
type TaskInfo struct {
	TaskID   string `json:"task_id"`
	Status   string `json:"status"`
	Progress string `json:"progress,omitempty"`
	Reason   string `json:"reason,omitempty"`
	URL      string `json:"url,omitempty"`
}

// OpenAIVideoResponse is the client-facing response format for video tasks.
type OpenAIVideoResponse struct {
	ID          string         `json:"id"`
	Object      string         `json:"object"`
	Model       string         `json:"model"`
	Status      string         `json:"status"`
	Progress    string         `json:"progress,omitempty"`
	CreatedAt   int64          `json:"created_at"`
	CompletedAt int64          `json:"completed_at,omitempty"`
	ResultURL   string         `json:"result_url,omitempty"`
	Error       *TaskErrorResp `json:"error,omitempty"`
}

// TaskErrorResp is an error detail in a task response.
type TaskErrorResp struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// TaskAdaptor defines the interface each async media provider must implement.
type TaskAdaptor interface {
	// Platform returns the provider platform name (e.g., "sora", "kling").
	Platform() string

	// ValidateAndSetAction validates the request and returns the action string.
	ValidateAndSetAction(c *gin.Context, body []byte) (action string, err error)

	// BuildRequestURL constructs the upstream submit URL.
	BuildRequestURL(baseURL, action string) string

	// BuildRequestHeader sets provider-specific headers on the upstream request.
	BuildRequestHeader(req *http.Request, apiKey string)

	// BuildRequestBody converts the client body to the upstream format.
	BuildRequestBody(c *gin.Context, body []byte, model string) (io.Reader, string, error)

	// ParseSubmitResponse parses the upstream submit response and returns the upstream task ID.
	ParseSubmitResponse(resp *http.Response) (upstreamTaskID string, data []byte, err error)

	// FetchTask polls the upstream for task status.
	FetchTask(baseURL, apiKey, upstreamTaskID, action string) (*http.Response, error)

	// ParseTaskResult parses the upstream poll response into a TaskInfo.
	ParseTaskResult(respBody []byte) (*TaskInfo, error)

	// BuildClientResponse converts a Task to the client-facing response.
	BuildClientResponse(task *Task) any
}
