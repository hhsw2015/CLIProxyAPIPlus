package api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// handleVertexGeminiSTT serves /v1/audio/transcriptions for Vertex Gemini
// transcribe models (gemini-3.5-transcribe-preview) via SA OAuth. Gemini
// transcribes by passing the audio as an inlineData part to :generateContent and
// asking for a transcript. Reuses extractAudioFromMultipart + findImagenEntry +
// mintGCPToken. Returns true if handled; false if no gemini-SA entry lists the model.
func (s *Server) handleVertexGeminiSTT(c *gin.Context, modelName string, body []byte) bool {
	if s.cfg == nil {
		return false
	}
	entry, project, region := s.findImagenEntry(modelName)
	if entry == nil {
		return false
	}
	saJSON, decErr := base64.StdEncoding.DecodeString(entry.CredentialsB64)
	if decErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": fmt.Sprintf("gemini-stt: decode SA creds: %v", decErr), "type": "server_error"}})
		return true
	}
	token, tokErr := mintGCPToken(saJSON)
	if tokErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("gemini-stt: token mint: %v", tokErr), "type": "server_error"}})
		return true
	}

	audio, filename := extractAudioFromMultipart(body, c.GetHeader("Content-Type"))
	if len(audio) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "audio file is required (multipart 'file')", "type": "invalid_request_error"}})
		return true
	}
	mimeType := audioMimeFromFilename(filename)

	vBody := []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"","data":""}},{"text":"Generate a complete, accurate transcript of the speech in this audio. Output only the transcript text."}]}]}`)
	vBody, _ = sjson.SetBytes(vBody, "contents.0.parts.0.inlineData.mimeType", mimeType)
	vBody, _ = sjson.SetBytes(vBody, "contents.0.parts.0.inlineData.data", base64.StdEncoding.EncodeToString(audio))

	host := region + "-aiplatform.googleapis.com"
	if region == "global" {
		host = "aiplatform.googleapis.com"
	}
	upstreamURL := fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent", host, project, region, modelName)
	req, reqErr := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(vBody))
	if reqErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": fmt.Sprintf("gemini-stt: build request: %v", reqErr), "type": "server_error"}})
		return true
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, doErr := client.Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("gemini-stt: upstream POST: %v", doErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rspBody, _ := io.ReadAll(resp.Body)
	log.Debugf("[gemini-stt] project=%s region=%s model=%s status=%d", project, region, modelName, resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.Data(resp.StatusCode, "application/json", rspBody)
		return true
	}

	var text strings.Builder
	gjson.GetBytes(rspBody, "candidates").ForEach(func(_, cand gjson.Result) bool {
		cand.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
			if t := p.Get("text").String(); t != "" {
				text.WriteString(t)
			}
			return true
		})
		return true
	})
	out, _ := sjson.SetBytes([]byte(`{"text":""}`), "text", text.String())
	c.Data(http.StatusOK, "application/json", out)
	return true
}

// audioMimeFromFilename maps a common audio file extension to a MIME type Gemini
// accepts for inlineData; defaults to audio/wav.
func audioMimeFromFilename(name string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
	case "mp3":
		return "audio/mp3"
	case "m4a", "mp4":
		return "audio/mp4"
	case "flac":
		return "audio/flac"
	case "ogg", "oga":
		return "audio/ogg"
	case "webm":
		return "audio/webm"
	case "aac":
		return "audio/aac"
	case "aiff", "aif":
		return "audio/aiff"
	default:
		return "audio/wav"
	}
}
