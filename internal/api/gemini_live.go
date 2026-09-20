package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	vertexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/vertex"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2/google"
)

// Gemini Live (Vertex BidiGenerateContent) websocket bridge.
//
// Native-protocol pass-through: the client speaks the standard BidiGenerateContent
// protocol (setup / realtimeInput audio+video frames / clientContent, and receives
// serverContent). CPA only (1) mints a Vertex SA OAuth bearer from the loaded
// file-auths, and (2) rewrites setup.model to the full Vertex resource path for a
// round-robin project, then relays every frame verbatim. Model gemini-live-2.5-flash
// (the only Live model our SA can reach) supports continuous audio + video-frame
// input for "watch-and-reason" assistants.
const geminiLiveWSURL = "wss://aiplatform.googleapis.com/ws/google.cloud.aiplatform.v1beta1.LlmBidiService/BidiGenerateContent"

// geminiLiveRR round-robins the Vertex project across the loaded vertex file-auths.
var geminiLiveRR uint64

// vertexSACred is a (project_id, normalized service-account JSON) pair.
type vertexSACred struct {
	project string
	saJSON  []byte
}

// collectVertexSACreds gathers every "vertex" file-auth's project + service account
// so the Live bridge can round-robin projects and mint OAuth tokens.
func (s *Server) collectVertexSACreds() []vertexSACred {
	out := make([]vertexSACred, 0)
	if s.handlers == nil || s.handlers.AuthManager == nil {
		return out
	}
	for _, a := range s.handlers.AuthManager.List() {
		if a == nil || strings.ToLower(strings.TrimSpace(a.Provider)) != "vertex" {
			continue
		}
		proj, _ := a.Metadata["project_id"].(string)
		proj = strings.TrimSpace(proj)
		if proj == "" {
			if v, ok := a.Metadata["project"].(string); ok {
				proj = strings.TrimSpace(v)
			}
		}
		saRaw, ok := a.Metadata["service_account"].(map[string]any)
		if proj == "" || !ok {
			continue
		}
		norm, errNorm := vertexauth.NormalizeServiceAccountMap(saRaw)
		if errNorm != nil {
			continue
		}
		saJSON, errMarshal := json.Marshal(norm)
		if errMarshal != nil {
			continue
		}
		out = append(out, vertexSACred{project: proj, saJSON: saJSON})
	}
	return out
}

func (s *Server) geminiLiveHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		model := strings.TrimSpace(c.Query("model"))
		if model == "" {
			model = "gemini-live-2.5-flash"
		}

		// Gemini 3.8 Live (launched 2026-09-15) is an AI-Studio Live API model:
		// our Vertex SAs (project sky-maas) only have the Anthropic publisher
		// enabled — every publishers/google call 403s — so these must go over the
		// AI Studio BidiGenerateContent socket keyed by an AIza key, not Vertex.
		if geminiLiveIsAIStudioModel(model) {
			s.geminiLiveAIStudioBridge(c, model)
			return
		}

		creds := s.collectVertexSACreds()
		if len(creds) == 0 {
			c.JSON(http.StatusBadGateway, gin.H{"error": "no vertex service-account credentials configured"})
			return
		}
		cred := creds[int(atomic.AddUint64(&geminiLiveRR, 1)-1)%len(creds)]

		token, errTok := mintVertexToken(c.Request.Context(), cred.saJSON)
		if errTok != nil {
			log.Errorf("gemini-live: mint token failed: %v", errTok)
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to mint vertex token"})
			return
		}

		clientConn, errUp := realtimeUpgrader.Upgrade(c.Writer, c.Request, nil)
		if errUp != nil {
			log.Errorf("gemini-live: client upgrade failed: %v", errUp)
			return
		}

		header := http.Header{}
		header.Set("Authorization", "Bearer "+token)
		dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
		upstream, resp, errDial := dialer.Dial(geminiLiveWSURL, header)
		if errDial != nil {
			msg := "upstream connection failed"
			if resp != nil {
				msg += " (status " + resp.Status + ")"
			}
			log.Errorf("gemini-live: upstream dial failed: %v", errDial)
			clientConn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, msg))
			clientConn.Close()
			return
		}

		// Inject the full Vertex model path into the first (setup) frame, then relay
		// everything else verbatim.
		modelPath := "projects/" + cred.project + "/locations/global/publishers/google/models/" + model
		if !forwardLiveSetup(clientConn, upstream, modelPath) {
			clientConn.Close()
			upstream.Close()
			return
		}
		log.Infof("gemini-live: session started project=%s model=%s", cred.project, model)
		relay := &wsRelay{
			client:   clientConn,
			upstream: upstream,
			done:     make(chan struct{}),
		}
		relay.run()
	}
}

// forwardLiveSetup reads the first client frame (the BidiGenerateContent setup),
// overwrites setup.model with the Vertex resource path, and forwards it upstream.
func forwardLiveSetup(client, upstream *websocket.Conn, modelPath string) bool {
	msgType, data, err := client.ReadMessage()
	if err != nil {
		return false
	}
	var msg map[string]any
	if json.Unmarshal(data, &msg) == nil {
		if setup, ok := msg["setup"].(map[string]any); ok {
			setup["model"] = modelPath
			if out, errOut := json.Marshal(msg); errOut == nil {
				data = out
				msgType = websocket.TextMessage
			}
		}
	}
	return upstream.WriteMessage(msgType, data) == nil
}

func mintVertexToken(ctx context.Context, saJSON []byte) (string, error) {
	creds, err := google.CredentialsFromJSON(ctx, saJSON, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", err
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// geminiLiveAIStudioWSURL is the AI Studio (generativelanguage) Live API socket.
// Auth is the ?key= query param (an AIza key), NOT an OAuth bearer, and the
// setup.model is a bare "models/<id>" resource (no projects/.../publishers path).
const geminiLiveAIStudioWSURL = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

// geminiLiveAIStudioRR round-robins the AIza key pool; geminiLiveAIStudioGood
// remembers the last key that worked (+1; 0 = none) so warm sessions hit a live
// key on the first try instead of re-walking the mostly-dead pool.
var geminiLiveAIStudioRR uint64
var geminiLiveAIStudioGood int64

// geminiLiveIsAIStudioModel routes the Gemini 3.8 Live family (AI-Studio-only) to
// the AI Studio bridge; older gemini-live-2.5-flash stays on the Vertex path.
func geminiLiveIsAIStudioModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "gemini-3.8-live") || strings.Contains(m, "-live-extended-thinking")
}

// geminiAIStudioKeys returns the plain AI Studio (AIza) keys from the config —
// the gemini-api-key entries WITHOUT a Vertex service-account (those carry
// credentials-b64 / vertex-location and belong to the Vertex path).
func (s *Server) geminiAIStudioKeys() []string {
	out := make([]string, 0)
	if s.cfg == nil {
		return out
	}
	for i := range s.cfg.GeminiKey {
		e := &s.cfg.GeminiKey[i]
		if e.Disabled || strings.TrimSpace(e.CredentialsB64) != "" {
			continue
		}
		k := strings.TrimSpace(e.APIKey)
		if strings.HasPrefix(k, "AIza") {
			out = append(out, k)
		}
	}
	return out
}

// geminiLiveAIStudioBridge bridges a client Live socket to AI Studio. Because the
// AIza pool is mostly dead keys, it reads the client's setup frame ONCE, then
// tries several keys: for each it dials, sends the setup (model rewritten to
// "models/<id>"), and reads the first upstream frame with a short deadline —
// a key that returns a real frame is committed; a dial error / immediate close /
// auth error frame moves to the next key. The validated first frame is forwarded
// to the client, then frames relay verbatim both ways.
func (s *Server) geminiLiveAIStudioBridge(c *gin.Context, model string) {
	keys := s.geminiAIStudioKeys()
	if len(keys) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "no AI Studio (gemini-api-key) credentials configured"})
		return
	}

	clientConn, errUp := realtimeUpgrader.Upgrade(c.Writer, c.Request, nil)
	if errUp != nil {
		log.Errorf("gemini-live[aistudio]: client upgrade failed: %v", errUp)
		return
	}
	defer func() { _ = clientConn.Close() }()

	// Read the client's setup frame once, rewrite the model to the AI Studio form.
	msgType, setupData, errRead := clientConn.ReadMessage()
	if errRead != nil {
		return
	}
	if setupData, msgType = rewriteAIStudioSetup(setupData, msgType, model); setupData == nil {
		clientConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "first frame must be a BidiGenerateContent setup"))
		return
	}

	// Start from the last-known-good key (warm path = 1 try); else round-robin.
	// The AIza pool is mostly dead, so cap generously: dead keys fail fast (dial
	// reject or a quick error frame), a live one commits and is remembered.
	start := int(atomic.AddUint64(&geminiLiveAIStudioRR, 1) - 1)
	if g := int(atomic.LoadInt64(&geminiLiveAIStudioGood)); g > 0 {
		start = g - 1
	}
	tries := len(keys)
	if tries > 24 {
		tries = 24
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	for i := 0; i < tries; i++ {
		idx := (start + i) % len(keys)
		key := keys[idx]
		upstream, _, errDial := dialer.Dial(geminiLiveAIStudioWSURL+"?key="+url.QueryEscape(key), nil)
		if errDial != nil {
			continue
		}
		if upstream.WriteMessage(msgType, setupData) != nil {
			_ = upstream.Close()
			continue
		}
		// Validate by the first upstream frame. Three outcomes:
		//   - a real frame (setupComplete) -> commit this key.
		//   - an AUTH/quota failure (bad key) -> try the next key.
		//   - any other close/error (e.g. 1007 "Thinking level must be specified"
		//     for the extended-thinking model) -> a client SETUP problem that is
		//     identical on every key, so surface it and stop, don't burn the pool.
		_ = upstream.SetReadDeadline(time.Now().Add(6 * time.Second))
		firstType, firstData, errFirst := upstream.ReadMessage()
		if errFirst != nil {
			_ = upstream.Close()
			if ce, ok := errFirst.(*websocket.CloseError); ok && !geminiLiveCloseIsAuth(ce) {
				clientConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(ce.Code, ce.Text))
				return
			}
			continue // dial/auth/broken key -> next
		}
		if geminiLiveFrameIsError(firstData) {
			_ = upstream.Close()
			if !geminiLiveErrFrameIsAuth(firstData) {
				clientConn.WriteMessage(firstType, firstData) // client-side error, surface it
				return
			}
			continue // auth error frame -> next key
		}
		_ = upstream.SetReadDeadline(time.Time{})
		atomic.StoreInt64(&geminiLiveAIStudioGood, int64(idx+1)) // remember the live key
		if clientConn.WriteMessage(firstType, firstData) != nil {
			_ = upstream.Close()
			return
		}
		log.Infof("gemini-live[aistudio]: session started model=%s key=…%s (attempt %d)", model, tail4(key), i+1)
		relay := &wsRelay{client: clientConn, upstream: upstream, done: make(chan struct{})}
		relay.run()
		return
	}
	clientConn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "no live AI Studio key for gemini live"))
}

// rewriteAIStudioSetup forces setup.model to the AI Studio "models/<id>" form.
// Returns (nil, 0) if the frame is not a valid setup object.
func rewriteAIStudioSetup(data []byte, msgType int, model string) ([]byte, int) {
	var msg map[string]any
	if json.Unmarshal(data, &msg) != nil {
		return nil, 0
	}
	setup, ok := msg["setup"].(map[string]any)
	if !ok {
		return nil, 0
	}
	setup["model"] = "models/" + model
	out, err := json.Marshal(msg)
	if err != nil {
		return nil, 0
	}
	return out, websocket.TextMessage
}

// geminiLiveFrameIsError reports whether an upstream frame is a JSON error frame
// (of any kind) rather than a normal setupComplete/serverContent frame.
func geminiLiveFrameIsError(data []byte) bool {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return false // non-JSON (e.g. binary audio) is not an error signal
	}
	_, hasErr := m["error"]
	return hasErr
}

// geminiLiveAuthRx matches the auth/quota/permission wording that means "this KEY
// is the problem" (so the bridge should try the next key), as opposed to a
// client setup/content error that would recur identically on every key.
var geminiLiveAuthRx = regexp.MustCompile(`(?i)api[ _-]?key|unauthor|permission|forbidden|quota|billing|resource_?exhausted|invalid authentication|expired`)

// geminiLiveCloseIsAuth classifies a WebSocket close as a key/auth failure.
func geminiLiveCloseIsAuth(ce *websocket.CloseError) bool {
	if ce == nil {
		return false
	}
	if ce.Code == websocket.ClosePolicyViolation { // 1008: AI Studio uses it for auth/quota
		return true
	}
	return geminiLiveAuthRx.MatchString(ce.Text)
}

// geminiLiveErrFrameIsAuth classifies a JSON error frame as a key/auth failure.
func geminiLiveErrFrameIsAuth(data []byte) bool {
	return geminiLiveAuthRx.Match(data)
}

func tail4(k string) string {
	if len(k) <= 4 {
		return k
	}
	return k[len(k)-4:]
}
