package api

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
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
		header := http.Header{}
		if provider.apiKey != "" {
			header.Set("api-key", provider.apiKey)
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

