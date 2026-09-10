package api

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

// Volcengine (Doubao) BigTTS v3 bidirectional binary WebSocket protocol.
// This bridge translates a simple client JSON protocol (identical to the
// ElevenLabs stream-input bridge: client sends {"text":...}, receives
// {"audio":"<b64>"} / {"isFinal":true}) into Volcengine's binary framing, so
// callers use one protocol for either provider. Realtime: ~0.3s TTFB, native
// Mandarin voices.
const (
	volcWSURL = "wss://openspeech.bytedance.com/api/v3/tts/bidirection"

	volcMsgFullClient  = 0b0001
	volcMsgAudioServer = 0b1011
	volcMsgError       = 0b1111
	volcFlagEvent      = 0b0100

	volcEventStartConnection    = 1
	volcEventFinishConnection   = 2
	volcEventStartSession       = 100
	volcEventFinishSession      = 102
	volcEventTaskRequest        = 200
	volcEventConnectionStarted  = 50
	volcEventConnectionFailed   = 51
	volcEventConnectionFinished = 52
	volcEventSessionStarted     = 150
	volcEventSessionFinished    = 152
	volcEventSessionFailed      = 153
	volcEventTTSResponse        = 352
)

// isVolcengineTTSModel reports whether a stream-input model_id targets the
// Volcengine BigTTS bridge rather than ElevenLabs.
func isVolcengineTTSModel(modelID string) bool {
	m := strings.ToLower(strings.TrimSpace(modelID))
	return m == "doubao-tts" || m == "bigtts" || strings.HasPrefix(m, "seed-tts") || strings.HasPrefix(m, "volc")
}

// resolveVolcengineTTSKey returns the X-Api-Key from the configured Volcengine
// TTS channel (base-url openspeech.bytedance.com), so the bridge needs no config
// entry of its own.
func (s *Server) resolveVolcengineTTSKey() string {
	if s.cfg == nil {
		return ""
	}
	for _, compat := range s.cfg.OpenAICompatibility {
		if !isVolcengineProvider(strings.TrimSpace(compat.BaseURL)) {
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

// volcFrame builds one Volcengine binary frame (JSON serialization, no
// compression). session id is included only for session-scoped events.
func volcFrame(event int32, sessionID string, payload []byte, withSession bool) []byte {
	buf := []byte{0x11, (volcMsgFullClient << 4) | volcFlagEvent, 0x10, 0x00}
	ev := make([]byte, 4)
	binary.BigEndian.PutUint32(ev, uint32(event))
	buf = append(buf, ev...)
	if withSession {
		l := make([]byte, 4)
		binary.BigEndian.PutUint32(l, uint32(len(sessionID)))
		buf = append(buf, l...)
		buf = append(buf, []byte(sessionID)...)
	}
	pl := make([]byte, 4)
	binary.BigEndian.PutUint32(pl, uint32(len(payload)))
	buf = append(buf, pl...)
	buf = append(buf, payload...)
	return buf
}

type volcServerMsg struct {
	msgType byte
	event   int32
	payload []byte
}

// volcParse decodes a Volcengine server frame. Connection-scoped events
// (50/51/52) carry no session id; every other event does.
func volcParse(data []byte) (volcServerMsg, bool) {
	if len(data) < 4 {
		return volcServerMsg{}, false
	}
	m := volcServerMsg{msgType: data[1] >> 4}
	flags := data[1] & 0x0f
	i := 4
	if flags&volcFlagEvent != 0 {
		if len(data) < i+4 {
			return volcServerMsg{}, false
		}
		m.event = int32(binary.BigEndian.Uint32(data[i : i+4]))
		i += 4
	}
	if m.event != 0 && m.event != volcEventConnectionStarted && m.event != volcEventConnectionFailed && m.event != volcEventConnectionFinished {
		if len(data) < i+4 {
			return volcServerMsg{}, false
		}
		ln := int(binary.BigEndian.Uint32(data[i : i+4]))
		i += 4
		if len(data) < i+ln {
			return volcServerMsg{}, false
		}
		i += ln // skip session id
	}
	if len(data) < i+4 {
		return volcServerMsg{}, false
	}
	pl := int(binary.BigEndian.Uint32(data[i : i+4]))
	i += 4
	if pl > len(data)-i {
		pl = len(data) - i
	}
	m.payload = data[i : i+pl]
	return m, true
}

// volcengineTTSStream upgrades the client connection, performs the Volcengine
// bidirectional handshake, then bridges: client {"text":...} -> TaskRequest,
// Volcengine audio frames -> client {"audio":"<b64>"}.
func (s *Server) volcengineTTSStream(c *gin.Context, speaker string) {
	if speaker == "" {
		speaker = "zh_female_xiaohe_uranus_bigtts"
	}
	apiKey := s.resolveVolcengineTTSKey()
	if apiKey == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "no Volcengine TTS provider configured"})
		return
	}

	clientConn, err := realtimeUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Errorf("volc-tts: client upgrade failed: %v", err)
		return
	}
	defer clientConn.Close()

	header := http.Header{}
	header.Set("X-Api-Key", apiKey)
	header.Set("X-Api-Resource-Id", volcResourceID(speaker))
	header.Set("X-Api-Connect-Id", uuid.NewString())
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	upstream, resp, err := dialer.Dial(volcWSURL, header)
	if err != nil {
		msg := "upstream connection failed"
		if resp != nil {
			msg += " (status " + resp.Status + ")"
		}
		log.Errorf("volc-tts: upstream dial failed: %v", err)
		clientConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, msg))
		return
	}
	defer upstream.Close()

	// Output format is caller-driven: pcm (raw, no decode — best for realtime
	// playback) or mp3/ogg_opus/wav. Defaults keep mp3/24000 for generic clients.
	format := strings.TrimSpace(c.Query("format"))
	if format == "" {
		format = "mp3"
	}
	sampleRate := 24000
	if sr := strings.TrimSpace(c.Query("sample_rate")); sr != "" {
		if v, errConv := strconv.Atoi(sr); errConv == nil && v > 0 {
			sampleRate = v
		}
	}

	sessionID := uuid.NewString()
	// Pipeline the handshake: StartConnection + StartSession are written
	// back-to-back without waiting for their acks, so the client's first
	// TaskRequest can follow immediately. The server processes the ordered
	// stream in sequence; the upstream reader below just ignores the
	// ConnectionStarted / SessionStarted acks. This removes two network
	// round-trips of TTFB (matters most from a far-from-Beijing host).
	startPayload, _ := json.Marshal(map[string]any{
		"user":      map[string]any{"uid": "cpa"},
		"event":     volcEventStartSession,
		"namespace": "BidirectionalTTS",
		"req_params": map[string]any{
			"speaker":      speaker,
			"audio_params": map[string]any{"format": format, "sample_rate": sampleRate},
		},
	})
	if err := upstream.WriteMessage(websocket.BinaryMessage, volcFrame(volcEventStartConnection, "", []byte("{}"), false)); err != nil {
		return
	}
	if err := upstream.WriteMessage(websocket.BinaryMessage, volcFrame(volcEventStartSession, sessionID, startPayload, true)); err != nil {
		return
	}
	log.Infof("volc-tts: session started speaker=%s", speaker)

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done); clientConn.Close(); upstream.Close() }) }

	// client -> upstream: {"text":...} becomes a TaskRequest; empty text closes.
	go func() {
		defer stop()
		for {
			_, data, err := clientConn.ReadMessage()
			if err != nil {
				upstream.WriteMessage(websocket.BinaryMessage, volcFrame(volcEventFinishSession, sessionID, []byte("{}"), true))
				return
			}
			var msg struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			if strings.TrimSpace(msg.Text) == "" {
				upstream.WriteMessage(websocket.BinaryMessage, volcFrame(volcEventFinishSession, sessionID, []byte("{}"), true))
				continue
			}
			taskPayload, _ := json.Marshal(map[string]any{
				"user":       map[string]any{"uid": "cpa"},
				"event":      volcEventTaskRequest,
				"namespace":  "BidirectionalTTS",
				"req_params": map[string]any{"text": msg.Text, "speaker": speaker},
			})
			if err := upstream.WriteMessage(websocket.BinaryMessage, volcFrame(volcEventTaskRequest, sessionID, taskPayload, true)); err != nil {
				return
			}
		}
	}()

	// upstream -> client: audio frames become {"audio":"<b64>"}; end -> {"isFinal":true}.
	go func() {
		defer stop()
		sawAudio := false
		final := func() { clientConn.WriteMessage(websocket.TextMessage, []byte(`{"isFinal":true}`)) }
		for {
			_, data, err := upstream.ReadMessage()
			if err != nil {
				// Upstream closed. If audio already flowed, treat as a clean end
				// so the client sees isFinal instead of a bare disconnect.
				if sawAudio {
					final()
				}
				return
			}
			// "the stream is done" is Volcengine's normal end-of-stream, delivered
			// as an error-typed frame — not a failure. Match the raw bytes because
			// the error-frame layout differs from audio frames.
			if bytes.Contains(data, []byte("the stream is done")) {
				final()
				return
			}
			m, ok := volcParse(data)
			if !ok {
				continue
			}
			switch {
			case m.msgType == volcMsgAudioServer || m.event == volcEventTTSResponse:
				if len(m.payload) == 0 {
					continue
				}
				sawAudio = true
				out, _ := json.Marshal(map[string]string{"audio": base64.StdEncoding.EncodeToString(m.payload)})
				if err := clientConn.WriteMessage(websocket.TextMessage, out); err != nil {
					return
				}
			case m.event == volcEventSessionFinished:
				final()
				return
			case m.msgType == volcMsgError || m.event == volcEventSessionFailed || m.event == volcEventConnectionFailed:
				log.Errorf("volc-tts: upstream error: %s", string(m.payload))
				if sawAudio {
					final()
				}
				return
			}
		}
	}()

	<-done
}
