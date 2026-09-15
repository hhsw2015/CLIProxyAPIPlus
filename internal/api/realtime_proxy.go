package api

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

var realtimeUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func (s *Server) setupRealtimeRoutes(v1 *gin.RouterGroup) {
	v1.GET("/realtime", s.realtimeProxyHandler())
	// ElevenLabs realtime TTS (WebSocket stream-input): stream text in, audio
	// out with ~300ms TTFB. voice_id is in the path; model_id (default
	// eleven_flash_v2_5) and other options ride the query.
	v1.GET("/text-to-speech/:voice_id/stream-input", s.elevenTTSStreamHandler())
	// Gemini Live (Vertex BidiGenerateContent) — stateful audio + video-frame
	// streaming. Native-protocol pass-through; CPA injects the Vertex SA bearer
	// and rewrites setup.model. (/v1/live is taken by codex live, so use a
	// distinct path.)
	v1.GET("/gemini-live", s.geminiLiveHandler())
	// Volcengine (Doubao) RealtimeDialog — native-Chinese full-duplex speech-to-
	// speech. Binary-frame pass-through; CPA injects the X-Api-* auth headers.
	v1.GET("/realtime/dialogue", s.volcDialogueHandler())
}

// elevenTTSStreamHandler bridges a client WebSocket to ElevenLabs' realtime TTS
// stream-input socket. Frames are relayed verbatim; the client speaks the
// ElevenLabs stream-input protocol (send {"text":...} messages, receive
// {"audio":"<b64>"} messages).
func (s *Server) elevenTTSStreamHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		voiceID := strings.TrimSpace(c.Param("voice_id"))
		if voiceID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "voice_id path parameter required"})
			return
		}
		// Volcengine BigTTS (doubao-tts) uses a separate binary-protocol bridge;
		// the path voice_id is its speaker. Everything else is ElevenLabs.
		if isVolcengineTTSModel(c.Query("model_id")) {
			s.volcengineTTSStream(c, voiceID)
			return
		}
		apiKey := s.resolveElevenLabsAPIKey()
		if apiKey == "" {
			c.JSON(http.StatusBadGateway, gin.H{"error": "no ElevenLabs provider configured"})
			return
		}

		u, errParse := url.Parse("wss://api.elevenlabs.io/v1/text-to-speech/" + url.PathEscape(voiceID) + "/stream-input")
		if errParse != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build upstream url"})
			return
		}
		q := u.Query()
		for k, vs := range c.Request.URL.Query() {
			for _, v := range vs {
				q.Set(k, v)
			}
		}
		if q.Get("model_id") == "" {
			q.Set("model_id", "eleven_flash_v2_5")
		}
		u.RawQuery = q.Encode()
		upstreamURL := u.String()

		clientConn, err := realtimeUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			log.Errorf("eleven-tts: client upgrade failed: %v", err)
			return
		}

		header := http.Header{}
		header.Set("xi-api-key", apiKey)
		dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
		upstreamConn, resp, err := dialer.Dial(upstreamURL, header)
		if err != nil {
			log.Errorf("eleven-tts: upstream dial failed: %v", err)
			msg := "upstream connection failed"
			if resp != nil {
				msg += " (status " + resp.Status + ")"
			}
			clientConn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, msg))
			clientConn.Close()
			return
		}

		log.Infof("eleven-tts: session started voice=%s", voiceID)
		relay := &wsRelay{
			client:   clientConn,
			upstream: upstreamConn,
			done:     make(chan struct{}),
		}
		relay.run()
	}
}

// resolveElevenLabsAPIKey returns the xi-api-key from any configured ElevenLabs
// openai-compatibility channel (scribe or TTS), so the realtime TTS bridge does
// not need its own config entry.
func (s *Server) resolveElevenLabsAPIKey() string {
	if s.cfg == nil {
		return ""
	}
	for _, compat := range s.cfg.OpenAICompatibility {
		if !isElevenLabsProvider(strings.TrimSpace(compat.BaseURL)) {
			continue
		}
		if len(compat.APIKeyEntries) > 0 {
			if k := strings.TrimSpace(compat.APIKeyEntries[0].APIKey); k != "" {
				return k
			}
		}
		if v, ok := compat.Headers["api-key"]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (s *Server) realtimeProxyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		model := c.Query("model")
		if model == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "model query parameter required"})
			return
		}

		provider := s.resolveRealtimeProvider(model)
		if provider == nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "no realtime provider configured for model " + model})
			return
		}

		clientConn, err := realtimeUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			log.Errorf("realtime: client upgrade failed: %v", err)
			return
		}

		upstreamURL := provider.baseURL
		// Forward the client's query params (except "model") onto the upstream
		// socket so callers can tune realtime options — e.g. ElevenLabs Scribe
		// wants audio_format / commit_strategy / language_code as query params.
		// Params baked into the configured base-url (Azure api-version/deployment)
		// are preserved unless the client overrides them.
		if u, e := url.Parse(upstreamURL); e == nil {
			q := u.Query()
			for k, vs := range c.Request.URL.Query() {
				if k == "model" {
					continue
				}
				for _, v := range vs {
					q.Set(k, v)
				}
			}
			u.RawQuery = q.Encode()
			upstreamURL = u.String()
		}
		// Direct OpenAI realtime (api.openai.com) needs the model in the query
		// string and the realtime beta header; Azure bakes the deployment+model
		// into the base-url instead. Add ?model= for the OpenAI case (the client's
		// model param was skipped above to avoid clobbering Azure deployments).
		if isOpenAIRealtimeProvider(upstreamURL) {
			if u, e := url.Parse(upstreamURL); e == nil {
				q := u.Query()
				if q.Get("model") == "" {
					q.Set("model", model)
					u.RawQuery = q.Encode()
					upstreamURL = u.String()
				}
			}
		}
		header := http.Header{}
		if provider.apiKey != "" {
			// ElevenLabs realtime STT authenticates with xi-api-key; direct OpenAI
			// uses Authorization: Bearer + OpenAI-Beta; Azure uses the api-key header.
			if isElevenLabsProvider(upstreamURL) {
				header.Set("xi-api-key", provider.apiKey)
			} else if isOpenAIRealtimeProvider(upstreamURL) {
				header.Set("Authorization", "Bearer "+provider.apiKey)
				header.Set("OpenAI-Beta", "realtime=v1")
			} else {
				header.Set("api-key", provider.apiKey)
			}
		}

		dialer := websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
		}
		upstreamConn, resp, err := dialer.Dial(upstreamURL, header)
		if err != nil {
			log.Errorf("realtime: upstream dial failed for %s: %v", model, err)
			msg := "upstream connection failed"
			if resp != nil {
				msg += " (status " + resp.Status + ")"
			}
			clientConn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, msg))
			clientConn.Close()
			return
		}

		log.Infof("realtime: session started model=%s upstream=%s", model, upstreamURL)

		relay := &wsRelay{
			client:   clientConn,
			upstream: upstreamConn,
			done:     make(chan struct{}),
		}
		relay.run()
	}
}

type wsRelay struct {
	client     *websocket.Conn
	upstream   *websocket.Conn
	done       chan struct{}
	once       sync.Once
	upstreamMu sync.Mutex
}

func (r *wsRelay) run() {
	go r.clientToUpstream()
	go r.upstreamToClient()
	go r.keepAlive()
}

func (r *wsRelay) stop() {
	r.once.Do(func() {
		close(r.done)
		r.client.Close()
		r.upstream.Close()
		log.Infof("realtime: session closed")
	})
}

func (r *wsRelay) clientToUpstream() {
	defer r.stop()
	for {
		msgType, data, err := r.client.ReadMessage()
		if err != nil {
			return
		}
		r.upstreamMu.Lock()
		err = r.upstream.WriteMessage(msgType, data)
		r.upstreamMu.Unlock()
		if err != nil {
			return
		}
	}
}

func (r *wsRelay) upstreamToClient() {
	defer r.stop()
	for {
		msgType, data, err := r.upstream.ReadMessage()
		if err != nil {
			return
		}
		if err := r.client.WriteMessage(msgType, data); err != nil {
			return
		}
	}
}

func (r *wsRelay) keepAlive() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			r.upstreamMu.Lock()
			err := r.upstream.WriteMessage(websocket.PingMessage, nil)
			r.upstreamMu.Unlock()
			if err != nil {
				r.stop()
				return
			}
		}
	}
}

type realtimeProviderConfig struct {
	baseURL string
	apiKey  string
}

// volcDialogueHandler bridges a client WebSocket to Volcengine's RealtimeDialog
// (full-duplex native-Chinese speech-to-speech, wss .../api/v3/realtime/dialogue).
// Binary-frame pass-through: the client speaks the RealtimeDialog protocol; CPA
// only injects the X-Api-* auth headers from the configured channel and relays.
func (s *Server) volcDialogueHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var baseURL string
		var headers map[string]string
		if s.cfg != nil {
			for _, compat := range s.cfg.OpenAICompatibility {
				b := strings.TrimSpace(compat.BaseURL)
				if strings.Contains(b, "realtime/dialogue") {
					baseURL = b
					headers = compat.Headers
					break
				}
			}
		}
		if baseURL == "" {
			c.JSON(http.StatusBadGateway, gin.H{"error": "no volcengine dialogue provider configured"})
			return
		}

		clientConn, err := realtimeUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			log.Errorf("volc-dialogue: client upgrade failed: %v", err)
			return
		}

		header := http.Header{}
		for k, v := range headers {
			header.Set(k, v)
		}
		if header.Get("X-Api-Connect-Id") == "" {
			header.Set("X-Api-Connect-Id", uuid.NewString())
		}
		dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
		upstream, resp, err := dialer.Dial(httpToWS(baseURL), header)
		if err != nil {
			msg := "upstream connection failed"
			if resp != nil {
				msg += " (status " + resp.Status + ")"
			}
			log.Errorf("volc-dialogue: upstream dial failed: %v", err)
			clientConn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, msg))
			clientConn.Close()
			return
		}
		log.Infof("volc-dialogue: session started")
		(&wsRelay{client: clientConn, upstream: upstream, done: make(chan struct{})}).run()
	}
}

func (s *Server) resolveRealtimeProvider(modelName string) *realtimeProviderConfig {
	if s.cfg == nil {
		return nil
	}
	for _, compat := range s.cfg.OpenAICompatibility {
		for _, m := range compat.Models {
			name := strings.TrimSpace(m.Name)
			alias := strings.TrimSpace(m.Alias)
			if !strings.EqualFold(name, modelName) && !strings.EqualFold(alias, modelName) {
				continue
			}
			baseURL := strings.TrimSpace(compat.BaseURL)
			if !strings.Contains(baseURL, "realtime") {
				continue
			}

			apiKey := ""
			if len(compat.APIKeyEntries) > 0 {
				apiKey = strings.TrimSpace(compat.APIKeyEntries[0].APIKey)
			}
			if v, ok := compat.Headers["api-key"]; ok && apiKey == "" {
				apiKey = v
			}

			wsURL := httpToWS(baseURL)
			return &realtimeProviderConfig{baseURL: wsURL, apiKey: apiKey}
		}
	}
	return nil
}

func httpToWS(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	}
	return parsed.String()
}
