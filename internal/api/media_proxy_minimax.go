package api

import (
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MiniMax first-party TTS (T2A v2). CPA otherwise routes MiniMax speech through
// fal (aggregator margin); this hits api.minimax.io directly with the same key
// already in config. The response carries the audio as a hex string in data.audio.

var minimaxSpeechRe = regexp.MustCompile(`speech-[0-9]`)

// minimaxTTSCreds returns (apiKey, baseURL) for a first-party MiniMax entry.
func (s *Server) minimaxTTSCreds() (key, base string, ok bool) {
	if s.cfg == nil {
		return "", "", false
	}
	for i := range s.cfg.OpenAICompatibility {
		e := &s.cfg.OpenAICompatibility[i]
		if !strings.Contains(e.BaseURL, "api.minimax.io") && !strings.Contains(e.BaseURL, "api.minimaxi.com") {
			continue
		}
		for _, ak := range e.APIKeyEntries {
			if strings.TrimSpace(ak.APIKey) != "" {
				b := "https://api.minimax.io/v1"
				if strings.Contains(e.BaseURL, "minimaxi.com") {
					b = "https://api.minimaxi.com/v1"
				}
				return ak.APIKey, b, true
			}
		}
	}
	return "", "", false
}

// minimaxVoiceID maps OpenAI voice ids to MiniMax voice ids; a MiniMax voice id
// passes through.
func minimaxVoiceID(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "alloy":
		return "male-qn-qingse"
	case "echo":
		return "male-qn-jingying"
	case "fable":
		return "male-qn-badao"
	case "onyx":
		return "male-qn-daxuesheng"
	case "nova":
		return "female-shaonv"
	case "shimmer":
		return "female-yujie"
	default:
		return v
	}
}

// handleMiniMaxTTS serves /v1/audio/speech for MiniMax first-party TTS (model
// contains "minimax"). Returns mp3.
func (s *Server) handleMiniMaxTTS(c *gin.Context, modelName string, body []byte) bool {
	key, base, ok := s.minimaxTTSCreds()
	if !ok {
		return false
	}
	input := gjson.GetBytes(body, "input").String()
	if input == "" {
		input = gjson.GetBytes(body, "text").String()
	}
	if strings.TrimSpace(input) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "input is required", "type": "invalid_request_error"}})
		return true
	}
	// Derive the MiniMax speech model id from the client model name; default hd.
	ttsModel := "speech-2.6-hd"
	if m := minimaxSpeechRe.FindString(strings.ToLower(modelName)); m != "" {
		if idx := strings.Index(strings.ToLower(modelName), "speech-"); idx >= 0 {
			ttsModel = modelName[idx:]
		}
	}
	voice := minimaxVoiceID(gjson.GetBytes(body, "voice").String())

	reqBody := []byte(`{"stream":false,"model":"","text":"","voice_setting":{"voice_id":"","speed":1.0,"vol":1.0,"pitch":0},"audio_setting":{"sample_rate":24000,"format":"mp3"}}`)
	reqBody, _ = sjson.SetBytes(reqBody, "model", ttsModel)
	reqBody, _ = sjson.SetBytes(reqBody, "text", input)
	reqBody, _ = sjson.SetBytes(reqBody, "voice_setting.voice_id", voice)

	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, base+"/t2a_v2", strings.NewReader(string(reqBody)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("minimax-tts: %v", doErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	if code := gjson.GetBytes(rb, "base_resp.status_code").Int(); code != 0 {
		c.Data(http.StatusBadGateway, "application/json", rb)
		return true
	}
	audioHex := gjson.GetBytes(rb, "data.audio").String()
	audio, decErr := hex.DecodeString(audioHex)
	if decErr != nil || len(audio) == 0 {
		c.Data(http.StatusOK, "application/json", rb) // no audio — return raw for debugging
		return true
	}
	log.Debugf("[minimax-tts] model=%s voice=%s bytes=%d", ttsModel, voice, len(audio))
	c.Data(http.StatusOK, "audio/mpeg", audio)
	return true
}
