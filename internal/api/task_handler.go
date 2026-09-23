package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	registerTaskAdaptor(&runninghubAdaptor{})
	registerTaskAdaptor(&foxtokenAdaptor{})
	registerTaskAdaptor(&topazAdaptor{})
	registerTaskAdaptor(&dashscopeAdaptor{})
	registerTaskAdaptor(&minimaxH3Adaptor{})
	registerTaskAdaptor(&skyreelsAdaptor{})
	registerTaskAdaptor(&whisperBatchAdaptor{})
	registerTaskAdaptor(&taijiaSoraAdaptor{})
	registerTaskAdaptor(&atlasAdaptor{})
	registerTaskAdaptor(&murekaAdaptor{})
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

	// Music generation (Mureka/Suno; auto-detect platform from model name)
	v1.POST("/music/generations", s.taskSubmitHandler("auto"))
	v1.POST("/audio/music", s.taskSubmitHandler("auto"))

	// Generic task fetch (works for all platforms)
	v1.GET("/tasks/:task_id", s.taskFetchHandler())
	v1.GET("/tasks/:task_id/content", s.taskContentHandler())

	// Start background polling
	go s.taskPollingLoop()
}

// taskSubmitHandler returns a handler for submitting async tasks.
//
// Every openai-compatibility entry serving the model is a candidate channel,
// tried highest priority first (config order breaks ties). A channel-side
// failure (transport error, 401/402/403/404/408/429, 5xx) falls over to the next
// candidate; a 400 is the client's own body and is returned as-is. Before this,
// the first config-order match was the only channel ever used, so a dead
// backend hid every other entry for that model.
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

		modelName := ""
		var bodyMap map[string]any
		if json.Unmarshal(body, &bodyMap) == nil {
			if m, ok := bodyMap["model"].(string); ok {
				modelName = m
			}
		}

		candidates := s.resolveTaskCandidates(modelName, platform)
		if len(candidates) == 0 {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
				"message": fmt.Sprintf("no provider configured for model %s on platform %s", modelName, platform),
				"type":    "server_error",
			}})
			return
		}

		// ponytail: per-request fill-first failover, no cooldown. Add a cooldown map
		// if a dead top-priority channel keeps eating the first attempt of every request.
		var lastStatus int
		var lastBody []byte
		var lastErr error
		for i, cand := range candidates {
			done, status, errBody, errSubmit := s.submitTaskTo(c, cand, body, modelName)
			if done {
				return
			}
			lastStatus, lastBody, lastErr = status, errBody, errSubmit
			log.Warnf("task submit: channel %s (%s) failed for %s [%d/%d] status=%d err=%v; failing over",
				cand.name, cand.platform, modelName, i+1, len(candidates), status, errSubmit)
		}
		if lastBody != nil && lastStatus != 0 {
			c.Data(lastStatus, "application/json", lastBody)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": fmt.Sprintf("upstream request failed on all %d channels for %s: %v", len(candidates), modelName, lastErr),
			"type":    "server_error",
		}})
	}
}

// taskSubmitFailsOver reports whether a failed submit should fall through to the
// next candidate channel: per-channel auth/quota/not-found, throttling and
// upstream 5xx. A 400 is the client's own body and must not fail over.
func taskSubmitFailsOver(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

// submitTaskTo tries one candidate channel. done=true means a response has been
// written (success, or a client error that must not fail over). Otherwise the
// caller moves on with the returned upstream status/body or transport error.
func (s *Server) submitTaskTo(c *gin.Context, cand taskCandidate, body []byte, modelName string) (bool, int, []byte, error) {
	adaptor, ok := taskRegistry[cand.platform]
	if !ok {
		return false, 0, nil, fmt.Errorf("unsupported platform %s", cand.platform)
	}
	// Client called an alias; rewrite the task body's model to THIS candidate's
	// upstream id BEFORE ValidateAndSetAction so adaptors that derive the endpoint
	// from the model (e.g. runninghub) see the upstream id. Per candidate, because
	// providers sharing a client name each have their own id.
	if cand.upstream != "" && !strings.EqualFold(cand.upstream, modelName) {
		body = rewriteBodyModel(body, cand.upstream)
	}
	action, err := adaptor.ValidateAndSetAction(c, body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
		}})
		return true, 0, nil, nil
	}

	provider := cand.provider
	// Expose the resolved provider key to BuildRequestBody for adaptors that
	// must carry it in the request body (e.g. skyreels needs api_key inline).
	c.Set("task_provider_api_key", provider.apiKey)

	// A gpt-proxy base-url (contains /gpt-proxy/) uses gpt-proxy URL + body construction.
	isPassthrough := strings.Contains(provider.baseURL, "/gpt-proxy/")
	var upstreamURL, contentType string
	var reqBody io.Reader
	if isPassthrough {
		c.Set("gpt_proxy_base_url", provider.baseURL)
		gptAdaptor := &gptProxyAdaptor{}
		upstreamURL = gptAdaptor.buildSubmitURL(provider.baseURL)
		reqBody, contentType, err = gptAdaptor.BuildRequestBody(c, body, modelName)
		if err != nil {
			reqBody, contentType = bytes.NewReader(body), "application/json"
		}
	} else {
		upstreamURL = adaptor.BuildRequestURL(provider.baseURL, action)
		reqBody, contentType, err = adaptor.BuildRequestBody(c, body, modelName)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": fmt.Sprintf("failed to build request: %v", err),
				"type":    "invalid_request_error",
			}})
			return true, 0, nil, nil
		}
	}

	upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, reqBody)
	if err != nil {
		return false, 0, nil, fmt.Errorf("create upstream request: %w", err)
	}
	if contentType != "" {
		upstreamReq.Header.Set("Content-Type", contentType)
	}
	adaptor.BuildRequestHeader(upstreamReq, provider.apiKey)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(upstreamReq)
	if err != nil {
		return false, 0, nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("task submit: close upstream body: %v", errClose)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		log.Errorf("task submit: upstream %s returned %d for %s/%s: %s", cand.name, resp.StatusCode, cand.platform, modelName, string(errBody))
		if taskSubmitFailsOver(resp.StatusCode) {
			return false, resp.StatusCode, errBody, nil
		}
		c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), errBody)
		return true, 0, nil, nil
	}

	if isPassthrough {
		respBody, _ := io.ReadAll(resp.Body)
		task := s.storeTask(&Task{
			Model: modelName, Platform: "gpt-proxy", Action: action, Data: respBody,
			UpstreamTaskID: gptProxyTaskID(respBody), ProviderBaseURL: provider.baseURL, ProviderAPIKey: provider.apiKey,
		})
		log.Infof("task created (gpt-proxy): %s model=%s channel=%s upstream=%s", task.ID, modelName, cand.name, task.UpstreamTaskID)
		c.JSON(http.StatusOK, (&soraAdaptor{}).BuildClientResponse(task))
		return true, 0, nil, nil
	}

	upstreamTaskID, data, err := adaptor.ParseSubmitResponse(resp)
	if err != nil {
		// A 2xx we cannot parse is a broken channel, not a client error: fall over.
		return false, 0, nil, fmt.Errorf("parse upstream response: %w", err)
	}
	task := s.storeTask(&Task{
		Model: modelName, Platform: cand.platform, Action: action, Data: data,
		UpstreamTaskID: upstreamTaskID, ProviderBaseURL: provider.baseURL, ProviderAPIKey: provider.apiKey,
	})
	log.Infof("task created: %s platform=%s model=%s channel=%s upstream=%s", task.ID, cand.platform, modelName, cand.name, upstreamTaskID)
	c.JSON(http.StatusOK, adaptor.BuildClientResponse(task))
	return true, 0, nil, nil
}

// storeTask assigns id/status/timestamps, stores the task and starts polling it.
func (s *Server) storeTask(task *Task) *Task {
	task.ID = generateTaskID()
	task.Status = TaskStatusSubmitted
	task.Progress = "10%"
	task.CreatedAt = time.Now()
	globalTaskStore.Insert(task)
	go s.pollTaskUntilDone(task.ID)
	return task
}

// gptProxyTaskID extracts the upstream task id from a gpt-proxy submit response
// (id / task_id / taskId at top level, or nested data.task_id).
func gptProxyTaskID(respBody []byte) string {
	var m map[string]any
	if json.Unmarshal(respBody, &m) != nil {
		return ""
	}
	for _, field := range []string{"id", "task_id", "taskId"} {
		if v, ok := m[field].(string); ok && v != "" {
			return v
		}
	}
	if data, ok := m["data"].(map[string]any); ok {
		if tid, ok := data["task_id"].(string); ok {
			return tid
		}
	}
	return ""
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

// taskCandidate is one openai-compatibility entry able to serve an async-task model.
type taskCandidate struct {
	name     string
	platform string
	priority int
	provider mediaProviderConfig
	// upstream is this entry's own id for the client model (models[].name when the
	// client called an alias). Rewritten per candidate so a client name shared by
	// several providers (e.g. wan3.0-video on dashscope AND fal) sends each one
	// its own id instead of whatever the first config match happened to use.
	upstream string
}

// resolveTaskCandidates returns every openai-compatibility entry serving
// modelName, highest priority first (config order breaks ties). With platform
// "auto" the platform is detected per entry, so mixed backends behind one client
// model name (azure sora / gpt-proxy / a reseller's /v1/videos) fail over to
// each other.
func (s *Server) resolveTaskCandidates(modelName, platform string) []taskCandidate {
	if s.cfg == nil {
		return nil
	}
	var out []taskCandidate
	for i := range s.cfg.OpenAICompatibility {
		compat := &s.cfg.OpenAICompatibility[i]
		upstream := ""
		for _, m := range compat.Models {
			if strings.EqualFold(strings.TrimSpace(m.Name), modelName) || strings.EqualFold(strings.TrimSpace(m.Alias), modelName) {
				upstream = strings.TrimSpace(m.Name)
				break
			}
		}
		if upstream == "" {
			continue
		}
		p := platform
		if p == "auto" {
			p = platformForEntry(compat.Name, compat.BaseURL)
		}
		if p == "" {
			p = platformForModelName(modelName)
		}
		apiKey := ""
		if len(compat.APIKeyEntries) > 0 {
			apiKey = strings.TrimSpace(compat.APIKeyEntries[0].APIKey)
		}
		if apiKey == "" {
			apiKey = compat.Headers["api-key"]
		}
		out = append(out, taskCandidate{
			name:     compat.Name,
			platform: p,
			priority: compat.Priority,
			provider: mediaProviderConfig{baseURL: strings.TrimSpace(compat.BaseURL), apiKey: apiKey},
			upstream: upstream,
		})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].priority > out[b].priority })
	return out
}

// platformForEntry maps a config entry to its task adaptor by base-url /
// entry-name convention. "" means no convention matched.
func platformForEntry(entryName, baseURL string) string {
	if strings.Contains(baseURL, "/gpt-proxy/") {
		return "gpt-proxy"
	}
	entryName = strings.ToLower(entryName)
	switch {
	case strings.HasPrefix(entryName, "foxtoken") || strings.HasPrefix(entryName, "huawi"):
		return "foxtoken"
	case strings.HasPrefix(entryName, "topaz"):
		return "topaz"
	case strings.HasPrefix(entryName, "dashscope") || strings.HasPrefix(entryName, "aliyun"):
		return "dashscope"
	case strings.HasPrefix(entryName, "minimaxh3"):
		return "minimax-h3"
	case strings.HasPrefix(entryName, "skyreels") || strings.HasPrefix(entryName, "skywork"):
		return "skyreels"
	case strings.HasPrefix(entryName, "kling"):
		return "kling"
	case strings.HasPrefix(entryName, "mureka"):
		return "mureka"
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
	case strings.HasPrefix(entryName, "atlas-media"):
		return "atlas"
	case strings.HasPrefix(entryName, "fal"):
		return "fal"
	case strings.HasPrefix(entryName, "runninghub") || strings.HasPrefix(entryName, "skymedia-rh"):
		return "runninghub"
	}
	return ""
}

// platformForModelName is the last-resort platform guess from the model name.
func platformForModelName(modelName string) string {
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
	case strings.Contains(lower, "mureka"):
		return "mureka"
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
