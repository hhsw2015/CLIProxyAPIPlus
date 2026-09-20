package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// typeSafeSystemOneHost identifies the TypeSafe AI (System One / Jev) provider by
// its API host. Jev is a structured-decision model (POST /v1/systemone with a
// state + typed questions, not chat/completions), so it cannot ride the chat or
// media executors; it is served here as a verbatim REST passthrough.
const typeSafeSystemOneHost = "api.typesafe.ai"

// setupSystemOneRoutes registers the TypeSafe System One (Jev) decision passthrough
// on the authenticated v1 group (client uses a CPA key; CPA forwards to TypeSafe).
func (s *Server) setupSystemOneRoutes(v1 *gin.RouterGroup) {
	v1.POST("/systemone", s.systemOneProxyHandler())
}

// systemOneKeys collects, in config order, every configured TypeSafe api-key from
// openai-compatibility entries whose base-url host is api.typesafe.ai, so multiple
// $5 accounts can be pooled with fill-first failover. Returns the base URL and keys.
func (s *Server) systemOneKeys() (baseURL string, keys []string) {
	if s.cfg == nil {
		return "", nil
	}
	for _, compat := range s.cfg.OpenAICompatibility {
		b := strings.TrimSpace(compat.BaseURL)
		if !strings.Contains(b, typeSafeSystemOneHost) {
			continue
		}
		if baseURL == "" {
			baseURL = b
		}
		for _, e := range compat.APIKeyEntries {
			if k := strings.TrimSpace(e.APIKey); k != "" {
				keys = append(keys, k)
			}
		}
	}
	return baseURL, keys
}

// systemOneIsExhausted reports whether an upstream response indicates the current
// key ran out of credit/quota and the request should fail over to the next key.
func systemOneIsExhausted(status int, body []byte) bool {
	if status == http.StatusPaymentRequired || status == http.StatusTooManyRequests {
		return true
	}
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		lb := strings.ToLower(string(body))
		for _, kw := range []string{"credit", "quota", "insufficient", "balance", "exhaust", "limit"} {
			if strings.Contains(lb, kw) {
				return true
			}
		}
	}
	return false
}

// systemOneProxyHandler forwards POST /v1/systemone verbatim to TypeSafe, trying
// pooled keys fill-first and failing over only on credit/quota exhaustion.
func (s *Server) systemOneProxyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": fmt.Sprintf("failed to read request body: %v", err),
				"type":    "invalid_request_error",
			}})
			return
		}
		baseURL, keys := s.systemOneKeys()
		if baseURL == "" || len(keys) == 0 {
			c.JSON(http.StatusNotImplemented, gin.H{"error": gin.H{
				"message": "no TypeSafe (System One / Jev) api-key configured",
				"type":    "server_error",
			}})
			return
		}
		upstreamURL := strings.TrimRight(baseURL, "/") + "/systemone"
		model := gjson.GetBytes(body, "model").String()
		ct := c.GetHeader("Content-Type")
		if ct == "" {
			ct = "application/json"
		}

		client := &http.Client{}
		var lastStatus int
		var lastBody []byte
		for i, key := range keys {
			req, errReq := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
			if errReq != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
					"message": fmt.Sprintf("failed to build upstream request: %v", errReq),
					"type":    "server_error",
				}})
				return
			}
			req.Header.Set("Content-Type", ct)
			req.Header.Set("Authorization", "Bearer "+key)

			resp, errDo := client.Do(req)
			if errDo != nil {
				log.Errorf("systemone proxy: upstream request failed (key %d/%d) model=%s: %v", i+1, len(keys), model, errDo)
				lastStatus = http.StatusBadGateway
				lastBody = []byte(fmt.Sprintf(`{"error":{"message":"upstream request failed: %v","type":"server_error"}}`, errDo))
				continue
			}
			respBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if systemOneIsExhausted(resp.StatusCode, respBody) && i < len(keys)-1 {
				log.Warnf("systemone proxy: key %d/%d exhausted (status %d), failing over to next key", i+1, len(keys), resp.StatusCode)
				lastStatus, lastBody = resp.StatusCode, respBody
				continue
			}

			outCT := resp.Header.Get("Content-Type")
			if outCT == "" {
				outCT = "application/json"
			}
			c.Data(resp.StatusCode, outCT, respBody)
			return
		}

		if lastBody == nil {
			lastStatus = http.StatusBadGateway
			lastBody = []byte(`{"error":{"message":"systemone: all pooled keys failed","type":"server_error"}}`)
		}
		c.Data(lastStatus, "application/json", lastBody)
	}
}
