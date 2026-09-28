package api

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// handleVertexGeminiTTS serves /v1/audio/speech for Vertex Gemini TTS models
// (gemini-2.5-flash-tts, gemini-2.5-pro-tts) via SA OAuth. Gemini TTS generates
// via :generateContent with responseModalities:["AUDIO"] + speechConfig, returning
// raw PCM (audio/L16;rate=24000). We wrap it in a WAV header so OpenAI clients get
// a playable file. Reuses findImagenEntry + mintGCPToken.
//
// Returns true if handled; false if no gemini-SA entry lists the model (fall through).
func (s *Server) handleVertexGeminiTTS(c *gin.Context, modelName string, body []byte) bool {
	if s.cfg == nil {
		return false
	}
	entry, project, region := s.findImagenEntry(modelName)
	if entry == nil {
		return false
	}
	saJSON, decErr := base64.StdEncoding.DecodeString(entry.CredentialsB64)
	if decErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": fmt.Sprintf("gemini-tts: decode SA creds: %v", decErr), "type": "server_error"}})
		return true
	}
	token, tokErr := mintGCPToken(saJSON)
	if tokErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("gemini-tts: token mint: %v", tokErr), "type": "server_error"}})
		return true
	}

	input := gjson.GetBytes(body, "input").String()
	if input == "" {
		input = gjson.GetBytes(body, "text").String()
	}
	if strings.TrimSpace(input) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "input is required", "type": "invalid_request_error"}})
		return true
	}
	voiceName := mapOpenAIVoiceToGemini(gjson.GetBytes(body, "voice").String())

	vBody := []byte(`{"contents":[{"role":"user","parts":[{"text":""}]}],"generationConfig":{"responseModalities":["AUDIO"],"speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"Kore"}}}}}`)
	vBody, _ = sjson.SetBytes(vBody, "contents.0.parts.0.text", input)
	vBody, _ = sjson.SetBytes(vBody, "generationConfig.speechConfig.voiceConfig.prebuiltVoiceConfig.voiceName", voiceName)

	host := region + "-aiplatform.googleapis.com"
	if region == "global" {
		host = "aiplatform.googleapis.com"
	}
	upstreamURL := fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent", host, project, region, modelName)
	req, reqErr := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(vBody))
	if reqErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": fmt.Sprintf("gemini-tts: build request: %v", reqErr), "type": "server_error"}})
		return true
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, doErr := client.Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("gemini-tts: upstream POST: %v", doErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rspBody, _ := io.ReadAll(resp.Body)
	log.Debugf("[gemini-tts] project=%s region=%s model=%s status=%d", project, region, modelName, resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.Data(resp.StatusCode, "application/json", rspBody)
		return true
	}

	var b64, mime string
	gjson.GetBytes(rspBody, "candidates").ForEach(func(_, cand gjson.Result) bool {
		cand.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
			if d := p.Get("inlineData.data").String(); d != "" {
				b64 = d
				mime = p.Get("inlineData.mimeType").String()
				return false
			}
			return true
		})
		return b64 == ""
	})
	if b64 == "" {
		c.Data(http.StatusOK, "application/json", rspBody) // no audio — return raw for debugging
		return true
	}
	pcm, _ := base64.StdEncoding.DecodeString(b64)
	rate := 24000
	if i := strings.Index(mime, "rate="); i >= 0 {
		if r, err := strconv.Atoi(strings.TrimSpace(mime[i+5:])); err == nil && r > 0 {
			rate = r
		}
	}
	c.Data(http.StatusOK, "audio/wav", pcmToWav(pcm, rate, 1, 16))
	return true
}

// mapOpenAIVoiceToGemini maps OpenAI /v1/audio/speech voice names to Gemini
// prebuilt voices, defaulting to Kore. Passing a Gemini voice name through works too.
func mapOpenAIVoiceToGemini(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "alloy":
		return "Kore"
	case "echo":
		return "Puck"
	case "fable":
		return "Charon"
	case "onyx":
		return "Fenrir"
	case "nova":
		return "Aoede"
	case "shimmer":
		return "Leda"
	default:
		return v // assume a Gemini voice name (Kore/Puck/Charon/...)
	}
}

// pcmToWav wraps raw little-endian PCM samples in a 44-byte WAV/RIFF header so the
// output is a playable audio file.
func pcmToWav(pcm []byte, sampleRate, channels, bitsPerSample int) []byte {
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataLen := len(pcm)
	buf := new(bytes.Buffer)
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(buf, binary.LittleEndian, uint16(bitsPerSample))
	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, uint32(dataLen))
	buf.Write(pcm)
	return buf.Bytes()
}
