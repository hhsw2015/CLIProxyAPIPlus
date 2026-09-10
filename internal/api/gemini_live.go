package api

import (
	"context"
	"encoding/json"
	"net/http"
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
