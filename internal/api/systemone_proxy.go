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
	"github.com/tidwall/sjson"
)

// Jev (TypeSafe System One) is a structured-decision model (POST with a state +
// typed questions, not chat/completions), so it cannot ride the chat or media
// executors; it is served here as a verbatim REST passthrough with per-channel
// priority failover.
//
// Two upstreams serve the same Jev model:
//   - OpenRouter (/api/alpha/decisions): output tokens are free and we already
//     hold OpenRouter keys, so it is the PRIMARY channel.
//   - Native TypeSafe (/v1/systemone): metered $5-credit accounts, used only as
//     failover once OpenRouter is exhausted. It is also the only channel that
//     serves non-1.13 variants (e.g. jev-preview).
const (
	typeSafeSystemOneHost = "api.typesafe.ai"
	openRouterHost        = "openrouter.ai"
	// openRouterDecisionsURL is OpenRouter's decisions endpoint proxying Jev.
	openRouterDecisionsURL = "https://openrouter.ai/api/alpha/decisions"
	// openRouterJevModel is the only Jev decisions model OpenRouter exposes.
	openRouterJevModel = "typesafe/jev-1.13"
)

// systemOneChannel is one upstream that can serve a Jev decision request.
// Channels are tried in priority order; keys within a channel are tried fill-first.
type systemOneChannel struct {
	name     string
	endpoint string
	keys     []string
	// rewriteModel maps the client-supplied model to the id this channel expects.
	// ok=false means the channel cannot serve the requested model and is skipped
	// (e.g. OpenRouter only has jev-1.13, not jev-preview).
	rewriteModel func(incoming string) (string, bool)
}

// setupSystemOneRoutes registers the Jev (System One) decision passthrough on the
// authenticated v1 group (client uses a CPA key; CPA forwards to the upstream).
func (s *Server) setupSystemOneRoutes(v1 *gin.RouterGroup) {
	v1.POST("/systemone", s.systemOneProxyHandler())
}

// systemOneBasename strips any provider prefix (e.g. "typesafe/") from a model id.
func systemOneBasename(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m
}

// systemOneChannels builds the ordered channel list: OpenRouter first (free output
// on our existing keys), native TypeSafe second (metered $5 credit). Keys are
// collected in config order from openai-compatibility entries by base-url host, so
// multiple accounts pool with fill-first failover.
func (s *Server) systemOneChannels() []systemOneChannel {
	if s.cfg == nil {
		return nil
	}
	var orKeys, tsKeys []string
	var tsBase string
	for _, compat := range s.cfg.OpenAICompatibility {
		b := strings.TrimSpace(compat.BaseURL)
		switch {
		case strings.Contains(b, openRouterHost):
			for _, e := range compat.APIKeyEntries {
				if k := strings.TrimSpace(e.APIKey); k != "" {
					orKeys = append(orKeys, k)
				}
			}
		case strings.Contains(b, typeSafeSystemOneHost):
			if tsBase == "" {
				tsBase = b
			}
			for _, e := range compat.APIKeyEntries {
				if k := strings.TrimSpace(e.APIKey); k != "" {
					tsKeys = append(tsKeys, k)
				}
			}
		}
	}

	var channels []systemOneChannel
	if len(orKeys) > 0 {
		channels = append(channels, systemOneChannel{
			name:     "openrouter",
			endpoint: openRouterDecisionsURL,
			keys:     orKeys,
			rewriteModel: func(incoming string) (string, bool) {
				// OpenRouter only serves jev-1.13; jev-latest currently resolves to
				// it. Anything else (e.g. jev-preview) is not on OpenRouter.
				switch systemOneBasename(incoming) {
				case "jev-1.13", "jev-latest", "jev":
					return openRouterJevModel, true
				default:
					return "", false
				}
			},
		})
	}
	if len(tsKeys) > 0 && tsBase != "" {
		channels = append(channels, systemOneChannel{
			name:     "typesafe",
			endpoint: strings.TrimRight(tsBase, "/") + "/systemone",
			keys:     tsKeys,
			// Native TypeSafe wants the un-namespaced id.
			rewriteModel: func(incoming string) (string, bool) {
				return systemOneBasename(incoming), true
			},
		})
	}
	return channels
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

// systemOneProxyHandler forwards POST /v1/systemone to the highest-priority Jev
// channel, rewriting the model per channel, trying pooled keys fill-first, and
// failing over on credit/quota exhaustion (to the next key, then the next channel).
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
		channels := s.systemOneChannels()
		if len(channels) == 0 {
			c.JSON(http.StatusNotImplemented, gin.H{"error": gin.H{
				"message": "no Jev (System One) channel configured (need OpenRouter or TypeSafe api-key)",
				"type":    "server_error",
			}})
			return
		}
		incomingModel := gjson.GetBytes(body, "model").String()
		ct := c.GetHeader("Content-Type")
		if ct == "" {
			ct = "application/json"
		}

		client := &http.Client{}
		var lastStatus int
		var lastBody []byte
		anyTried := false

		for _, ch := range channels {
			outModel, ok := ch.rewriteModel(incomingModel)
			if !ok {
				continue
			}
			chBody := body
			if outModel != incomingModel {
				if patched, errSet := sjson.SetBytes(body, "model", outModel); errSet == nil {
					chBody = patched
				}
			}
			for i, key := range ch.keys {
				anyTried = true
				req, errReq := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, ch.endpoint, bytes.NewReader(chBody))
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
					log.Errorf("systemone proxy: %s upstream request failed (key %d/%d) model=%s: %v", ch.name, i+1, len(ch.keys), outModel, errDo)
					lastStatus = http.StatusBadGateway
					lastBody = []byte(fmt.Sprintf(`{"error":{"message":"upstream request failed: %v","type":"server_error"}}`, errDo))
					continue
				}
				respBody, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				if systemOneIsExhausted(resp.StatusCode, respBody) {
					log.Warnf("systemone proxy: %s key %d/%d exhausted (status %d), failing over", ch.name, i+1, len(ch.keys), resp.StatusCode)
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
		}

		if !anyTried {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": fmt.Sprintf("no Jev channel can serve model %q", incomingModel),
				"type":    "invalid_request_error",
			}})
			return
		}
		if lastBody == nil {
			lastStatus = http.StatusBadGateway
			lastBody = []byte(`{"error":{"message":"systemone: all channels/keys failed","type":"server_error"}}`)
		}
		c.Data(lastStatus, "application/json", lastBody)
	}
}
