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
	log "github.com/sirupsen/logrus"

)

// taskRegistry maps platform names to their adaptors.
var taskRegistry = map[string]TaskAdaptor{}

// registerTaskAdaptor registers a task adaptor for a platform.
func registerTaskAdaptor(adaptor TaskAdaptor) {
	taskRegistry[adaptor.Platform()] = adaptor
}

func init() {
	registerTaskAdaptor(&soraAdaptor{})
	registerTaskAdaptor(&klingAdaptor{})
	registerTaskAdaptor(&sunoAdaptor{})
	registerTaskAdaptor(&geminiTaskAdaptor{})
	registerTaskAdaptor(&gptProxyAdaptor{})
	registerTaskAdaptor(&doubaoAdaptor{})
	registerTaskAdaptor(&hailuoAdaptor{})
	registerTaskAdaptor(&viduAdaptor{})
	registerTaskAdaptor(&falAdaptor{})
	registerTaskAdaptor(&whisperBatchAdaptor{})
	registerTaskAdaptor(&taijiaSoraAdaptor{})
}

// setupTaskRoutes registers async task API routes.
func (s *Server) setupTaskRoutes(v1 *gin.RouterGroup) {
	// Video generation (auto-detect platform from model name)
	v1.POST("/video/generations", s.taskSubmitHandler("auto"))
	v1.POST("/videos", s.taskSubmitHandler("auto"))
	v1.GET("/video/generations/:task_id", s.taskFetchHandler())
	v1.GET("/videos/:task_id", s.taskFetchHandler())

	// Kling
	kling := s.engine.Group("/kling/v1")
	kling.Use(s.proxyAuthMiddleware())
	kling.POST("/videos/text2video", s.taskSubmitHandler("kling"))
	kling.POST("/videos/image2video", s.taskSubmitHandler("kling"))
	kling.GET("/videos/text2video/:task_id", s.taskFetchHandler())
	kling.GET("/videos/image2video/:task_id", s.taskFetchHandler())

	// Suno
	suno := s.engine.Group("/suno")
	suno.Use(s.proxyAuthMiddleware())
	suno.POST("/submit/:action", s.taskSubmitHandler("suno"))
	suno.GET("/fetch/:task_id", s.taskFetchHandler())

	// Gemini/Vertex (Veo video)
	v1.POST("/video/generations/gemini", s.taskSubmitHandler("gemini"))

	// Doubao/Seedance (火山引擎 video)
	v1.POST("/video/generations/doubao", s.taskSubmitHandler("doubao"))

	// Hailuo/MiniMax video
	v1.POST("/video/generations/hailuo", s.taskSubmitHandler("hailuo"))

	// Vidu video
	v1.POST("/video/generations/vidu", s.taskSubmitHandler("vidu"))

	// Generic task fetch (works for all platforms)
	v1.GET("/tasks/:task_id", s.taskFetchHandler())
	v1.GET("/tasks/:task_id/content", s.taskContentHandler())

	// Start background polling
	go s.taskPollingLoop()
}

// taskSubmitHandler returns a handler for submitting async tasks.
func (s *Server) taskSubmitHandler(platform string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": fmt.Sprintf("failed to read request body: %v", err),
				"type":    "invalid_request_error",
			}})
			return
		}

		// Extract model name early for auto-detection.
		modelName := ""
		var bodyMap map[string]any
		if json.Unmarshal(body, &bodyMap) == nil {
			if m, ok := bodyMap["model"].(string); ok {
				modelName = m
			}
		}

		// Auto-detect platform from model name if platform is "auto".
		if platform == "auto" && modelName != "" {
			platform = s.detectPlatformForModel(modelName)
		}

		adaptor, ok := taskRegistry[platform]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": fmt.Sprintf("unsupported platform: %s for model %s", platform, modelName),
				"type":    "invalid_request_error",
			}})
			return
		}

		// Validate and determine action.
		action, err := adaptor.ValidateAndSetAction(c, body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": err.Error(),
				"type":    "invalid_request_error",
			}})
			return
		}

		// Find provider from openai-compatibility config.
		provider := s.resolveTaskProvider(modelName, platform)
		if provider == nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
				"message": fmt.Sprintf("no provider configured for model %s on platform %s", modelName, platform),
				"type":    "server_error",
			}})
			return
		}

		// Build upstream request.
		// If the provider base-url is a gpt-proxy URL (contains /gpt-proxy/),
		// use gpt-proxy-specific URL construction.
		upstreamURL := provider.baseURL
		isPassthrough := strings.Contains(provider.baseURL, "/gpt-proxy/")
		if isPassthrough {
			upstreamURL = (&gptProxyAdaptor{}).buildSubmitURL(provider.baseURL)
		} else {
			upstreamURL = adaptor.BuildRequestURL(provider.baseURL, action)
		}

		var reqBody io.Reader
		var contentType string
		if isPassthrough {
			// gpt-proxy: transform body and use correct submit URL.
			c.Set("gpt_proxy_base_url", provider.baseURL)
			var err error
			gptAdaptor := &gptProxyAdaptor{}
			reqBody, contentType, err = gptAdaptor.BuildRequestBody(c, body, modelName)
			if err != nil {
				reqBody = bytes.NewReader(body)
				contentType = "application/json"
			}
		} else {
			var err error
			reqBody, contentType, err = adaptor.BuildRequestBody(c, body, modelName)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
					"message": fmt.Sprintf("failed to build request: %v", err),
					"type":    "invalid_request_error",
				}})
				return
			}
		}

		upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, reqBody)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
				"message": fmt.Sprintf("failed to create upstream request: %v", err),
				"type":    "server_error",
			}})
			return
		}
		if contentType != "" {
			upstreamReq.Header.Set("Content-Type", contentType)
		}
		adaptor.BuildRequestHeader(upstreamReq, provider.apiKey)

		// Send request.
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(upstreamReq)
		if err != nil {
			log.Errorf("task submit: upstream request failed for %s/%s: %v", platform, modelName, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
				"message": fmt.Sprintf("upstream request failed: %v", err),
				"type":    "server_error",
			}})
			return
		}
		defer resp.Body.Close()

		// Handle non-success responses.
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(resp.Body)
			log.Errorf("task submit: upstream returned %d for %s/%s: %s", resp.StatusCode, platform, modelName, string(errBody))
			c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), errBody)
			return
		}

		// For gpt-proxy passthrough, parse response to extract task ID
		// and store it in CPA's task store for unified polling.
		if isPassthrough {
			respBody, _ := io.ReadAll(resp.Body)
			// Try to extract task ID from gpt-proxy response.
			var gptProxyResp map[string]any
			upstreamTaskID := ""
			if json.Unmarshal(respBody, &gptProxyResp) == nil {
				// Try common task ID field names.
				for _, field := range []string{"id", "task_id", "taskId", "data.task_id"} {
					if v, ok := gptProxyResp[field]; ok {
						if s, ok := v.(string); ok && s != "" {
							upstreamTaskID = s
							break
						}
					}
				}
				// Check nested data.task_id
				if upstreamTaskID == "" {
					if data, ok := gptProxyResp["data"].(map[string]any); ok {
						if tid, ok := data["task_id"].(string); ok {
							upstreamTaskID = tid
						}
					}
				}
			}

			task := &Task{
				ID:              generateTaskID(),
				Model:           modelName,
				Platform:        "gpt-proxy",
				Action:          action,
				Status:          TaskStatusSubmitted,
				Progress:        "10%",
				Data:            respBody,
				CreatedAt:       time.Now(),
				UpstreamTaskID:  upstreamTaskID,
				ProviderBaseURL: provider.baseURL,
				ProviderAPIKey:  provider.apiKey,
			}
			globalTaskStore.Insert(task)
			go s.pollTaskUntilDone(task.ID)
			log.Infof("task created (gpt-proxy): %s model=%s upstream=%s", task.ID, modelName, upstreamTaskID)

			c.JSON(http.StatusOK, (&soraAdaptor{}).BuildClientResponse(task))
			return
		}

		// Parse upstream response.
		upstreamTaskID, data, err := adaptor.ParseSubmitResponse(resp)
		if err != nil {
			log.Errorf("task submit: failed to parse response for %s/%s: %v", platform, modelName, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
				"message": fmt.Sprintf("failed to parse upstream response: %v", err),
				"type":    "server_error",
			}})
			return
		}

		// Create and store task.
		task := &Task{
			ID:              generateTaskID(),
			Model:           modelName,
			Platform:        platform,
			Action:          action,
			Status:          TaskStatusSubmitted,
			Progress:        "10%",
			Data:            data,
			CreatedAt:       time.Now(),
			UpstreamTaskID:  upstreamTaskID,
			ProviderBaseURL: provider.baseURL,
			ProviderAPIKey:  provider.apiKey,
		}
		globalTaskStore.Insert(task)
		go s.pollTaskUntilDone(task.ID)

		log.Infof("task created: %s platform=%s model=%s upstream=%s", task.ID, platform, modelName, upstreamTaskID)

		// Return response.
		c.JSON(http.StatusOK, adaptor.BuildClientResponse(task))
	}
}

// taskFetchHandler returns a handler for polling task status.
func (s *Server) taskFetchHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID := c.Param("task_id")
		if taskID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": "task_id is required",
				"type":    "invalid_request_error",
			}})
			return
		}

		task := globalTaskStore.Get(taskID)
		if task == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
				"message": fmt.Sprintf("task %s not found", taskID),
				"type":    "not_found_error",
			}})
			return
		}

		adaptor, ok := taskRegistry[task.Platform]
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
				"message": fmt.Sprintf("unknown platform: %s", task.Platform),
				"type":    "server_error",
			}})
			return
		}

		c.JSON(http.StatusOK, adaptor.BuildClientResponse(task))
	}
}

// detectPlatformForModel determines the task platform based on model name and config.
func (s *Server) detectPlatformForModel(modelName string) string {
	if s.cfg != nil {
		for _, compat := range s.cfg.OpenAICompatibility {
			for _, m := range compat.Models {
				name := strings.TrimSpace(m.Name)
				alias := strings.TrimSpace(m.Alias)
				if !strings.EqualFold(name, modelName) && !strings.EqualFold(alias, modelName) {
					continue
				}
				// gpt-proxy passthrough
				if strings.Contains(compat.BaseURL, "/gpt-proxy/") {
					return "gpt-proxy"
				}
				// Detect by config entry name prefix
				entryName := strings.ToLower(compat.Name)
				switch {
				case strings.HasPrefix(entryName, "kling"):
					return "kling"
				case strings.HasPrefix(entryName, "hailuo") || strings.HasPrefix(entryName, "minimax"):
					return "hailuo"
				case strings.HasPrefix(entryName, "doubao") || strings.HasPrefix(entryName, "seedance"):
					return "doubao"
				case strings.HasPrefix(entryName, "vidu"):
					return "vidu"
				case strings.HasPrefix(entryName, "suno"):
					return "suno"
				case strings.HasPrefix(entryName, "gemini") || strings.HasPrefix(entryName, "vertex"):
					return "gemini"
				case strings.HasPrefix(entryName, "azure-sora") || strings.Contains(entryName, "sora"):
					return "sora"
				case strings.HasPrefix(entryName, "fal"):
					return "fal"
				}
			}
		}
	}
	// Fallback: detect by model name pattern
	lower := strings.ToLower(modelName)
	switch {
	case strings.Contains(lower, "veo"):
		return "gemini"
	case strings.Contains(lower, "sora"):
		return "sora"
	case strings.Contains(lower, "kling"):
		return "kling"
	case strings.Contains(lower, "suno"):
		return "suno"
	case strings.Contains(lower, "seedance") || strings.Contains(lower, "seedream"):
		return "doubao"
	case strings.Contains(lower, "hailuo") || strings.Contains(lower, "minimax"):
		return "hailuo"
	case strings.Contains(lower, "vidu"):
		return "vidu"
	case strings.Contains(lower, "fal"):
		return "fal"
	default:
		return "sora" // default fallback
	}
}

// taskContentHandler serves the raw video/audio data for a completed task.
func (s *Server) taskContentHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID := c.Param("task_id")
		task := globalTaskStore.Get(taskID)
		if task == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		if task.Status != TaskStatusSuccess || len(task.Data) == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "task content not available"})
			return
		}
		c.Header("Content-Type", "application/json")
		c.Writer.Write(task.Data)
	}
}

// resolveTaskProvider finds provider config for async tasks.
func (s *Server) resolveTaskProvider(modelName, platform string) *mediaProviderConfig {
	if s.cfg == nil {
		return nil
	}

	// Search openai-compatibility entries for matching model.
	for _, compat := range s.cfg.OpenAICompatibility {
		for _, m := range compat.Models {
			name := strings.TrimSpace(m.Name)
			alias := strings.TrimSpace(m.Alias)
			if !strings.EqualFold(name, modelName) && !strings.EqualFold(alias, modelName) {
				continue
			}
			baseURL := strings.TrimSpace(compat.BaseURL)
			apiKey := ""
			if len(compat.APIKeyEntries) > 0 {
				apiKey = strings.TrimSpace(compat.APIKeyEntries[0].APIKey)
			}
			if apiKey == "" {
				if v, ok := compat.Headers["api-key"]; ok {
					apiKey = v
				}
			}
			return &mediaProviderConfig{baseURL: baseURL, apiKey: apiKey}
		}
	}

	return nil
}

// taskPollingLoop periodically polls unfinished tasks.
func (s *Server) taskPollingLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// Also clean up old tasks every hour.
	cleanupTicker := time.NewTicker(1 * time.Hour)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ticker.C:
			s.pollUnfinishedTasks()
		case <-cleanupTicker.C:
			globalTaskStore.Cleanup(24 * time.Hour)
		}
	}
}

// pollUnfinishedTasks polls unfinished tasks that don't have a dedicated polling goroutine.
// Acts as a fallback for tasks that were created before the per-task polling was added,
// or whose goroutine exited unexpectedly.
func (s *Server) pollUnfinishedTasks() {
	tasks := globalTaskStore.GetUnfinished()
	for _, task := range tasks {
		if task.PollingActive {
			continue
		}
		adaptor, ok := taskRegistry[task.Platform]
		if !ok {
			continue
		}

		resp, err := adaptor.FetchTask(task.ProviderBaseURL, task.ProviderAPIKey, task.UpstreamTaskID, task.Action)
		if err != nil {
			log.Debugf("task poll: failed to fetch %s: %v", task.ID, err)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		if task.Status != TaskStatusSuccess && task.Status != TaskStatusFailure {
			log.Debugf("task poll raw response for %s: %s", task.ID, string(respBody[:min(len(respBody), 500)]))
		}
		info, err := adaptor.ParseTaskResult(respBody)
		if err != nil {
			log.Debugf("task poll: failed to parse %s: %v", task.ID, err)
			continue
		}

		globalTaskStore.Update(task.ID, func(t *Task) {
			if info.Status != "" {
				t.Status = info.Status
			}
			if info.Progress != "" {
				t.Progress = info.Progress
			}
			if info.URL != "" {
				t.ResultURL = info.URL
			}
			// Store raw response for completed tasks (contains video data).
			if info.Status == TaskStatusSuccess {
				t.Data = respBody
			}
			if info.Reason != "" {
				t.FailReason = info.Reason
			}
			if t.Status == TaskStatusSuccess || t.Status == TaskStatusFailure {
				t.FinishedAt = time.Now()
			}
			if t.Status == TaskStatusInProgress && t.StartedAt.IsZero() {
				t.StartedAt = time.Now()
			}
		})

		if info.Status == TaskStatusSuccess {
			log.Infof("task completed: %s url=%s", task.ID, info.URL)
		} else if info.Status == TaskStatusFailure {
			log.Warnf("task failed: %s reason=%s", task.ID, info.Reason)
		}
	}
}

// pollTaskUntilDone polls a single task in a dedicated goroutine with adaptive backoff.
// Initial interval 5s, gradually increasing to 15s. Gives up after 30 minutes.
func (s *Server) pollTaskUntilDone(taskID string) {
	const maxPollDuration = 30 * time.Minute

	globalTaskStore.Update(taskID, func(t *Task) {
		t.PollingActive = true
	})
	defer globalTaskStore.Update(taskID, func(t *Task) {
		t.PollingActive = false
	})

	deadline := time.Now().Add(maxPollDuration)
	interval := 5 * time.Second

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		task := globalTaskStore.Get(taskID)
		if task == nil {
			return
		}
		if task.Status == TaskStatusSuccess || task.Status == TaskStatusFailure {
			return
		}

		adaptor, ok := taskRegistry[task.Platform]
		if !ok {
			return
		}

		resp, err := adaptor.FetchTask(task.ProviderBaseURL, task.ProviderAPIKey, task.UpstreamTaskID, task.Action)
		if err != nil {
			log.Debugf("task poll (dedicated): failed to fetch %s: %v", taskID, err)
			if interval < 15*time.Second {
				interval += 2 * time.Second
			}
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		info, err := adaptor.ParseTaskResult(respBody)
		if err != nil {
			log.Debugf("task poll (dedicated): failed to parse %s: %v", taskID, err)
			continue
		}

		globalTaskStore.Update(taskID, func(t *Task) {
			if info.Status != "" {
				t.Status = info.Status
			}
			if info.Progress != "" {
				t.Progress = info.Progress
			}
			if info.URL != "" {
				t.ResultURL = info.URL
			}
			if info.Status == TaskStatusSuccess {
				t.Data = respBody
			}
			if info.Reason != "" {
				t.FailReason = info.Reason
			}
			if t.Status == TaskStatusSuccess || t.Status == TaskStatusFailure {
				t.FinishedAt = time.Now()
			}
			if t.Status == TaskStatusInProgress && t.StartedAt.IsZero() {
				t.StartedAt = time.Now()
			}
		})

		if info.Status == TaskStatusSuccess {
			log.Infof("task completed (dedicated): %s url=%s", taskID, info.URL)
			return
		}
		if info.Status == TaskStatusFailure {
			log.Warnf("task failed (dedicated): %s reason=%s", taskID, info.Reason)
			return
		}

		// Adaptive backoff: increase interval up to 15s
		if interval < 15*time.Second {
			interval += 2 * time.Second
		}
	}

	// Timeout: mark as failure
	globalTaskStore.Update(taskID, func(t *Task) {
		if t.Status != TaskStatusSuccess && t.Status != TaskStatusFailure {
			t.Status = TaskStatusFailure
			t.FailReason = "polling timeout (30 minutes)"
			t.FinishedAt = time.Now()
		}
	})
	log.Warnf("task timeout (dedicated): %s", taskID)
}
