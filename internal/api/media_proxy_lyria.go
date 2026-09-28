package api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// handleVertexLyria serves /v1/music for Vertex Lyria models (lyria-002) via SA
// OAuth + the :predict endpoint. Lyria only serves in us-central1; the generator
// pins that via a dedicated vertex-lyria-uc1 entry, so findImagenEntry returns
// the right region. Lyria returns a complete WAV (RIFF, stereo 48kHz 16-bit) in
// predictions[0].bytesBase64Encoded, so we decode and return it as audio/wav.
//
// Returns true if handled; false if no gemini-SA entry lists the model (caller
// falls through to the ElevenLabs/fal music resolver).
func (s *Server) handleVertexLyria(c *gin.Context, modelName string, body []byte) bool {
	if s.cfg == nil {
		return false
	}
	entry, project, region := s.findImagenEntry(modelName)
	if entry == nil {
		return false
	}
	saJSON, decErr := base64.StdEncoding.DecodeString(entry.CredentialsB64)
	if decErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": fmt.Sprintf("lyria: decode SA creds: %v", decErr), "type": "server_error"}})
		return true
	}
	token, tokErr := mintGCPToken(saJSON)
	if tokErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("lyria: token mint: %v", tokErr), "type": "server_error"}})
		return true
	}

	prompt := gjson.GetBytes(body, "prompt").String()
	if prompt == "" {
		prompt = gjson.GetBytes(body, "input").String()
	}
	if strings.TrimSpace(prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "prompt is required", "type": "invalid_request_error"}})
		return true
	}

	vBody := []byte(`{"instances":[{"prompt":""}],"parameters":{"sample_count":1}}`)
	vBody, _ = sjson.SetBytes(vBody, "instances.0.prompt", prompt)
	if neg := gjson.GetBytes(body, "negative_prompt").String(); neg != "" {
		vBody, _ = sjson.SetBytes(vBody, "instances.0.negative_prompt", neg)
	}
	if seed := gjson.GetBytes(body, "seed"); seed.Exists() {
		vBody, _ = sjson.SetBytes(vBody, "parameters.seed", seed.Int())
	}

	host := region + "-aiplatform.googleapis.com"
	if region == "global" {
		host = "aiplatform.googleapis.com"
	}
	upstreamURL := fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:predict", host, project, region, modelName)
	req, reqErr := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(vBody))
	if reqErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": fmt.Sprintf("lyria: build request: %v", reqErr), "type": "server_error"}})
		return true
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, doErr := client.Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("lyria: upstream POST: %v", doErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rspBody, _ := io.ReadAll(resp.Body)
	log.Debugf("[lyria] project=%s region=%s model=%s status=%d", project, region, modelName, resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.Data(resp.StatusCode, "application/json", rspBody)
		return true
	}

	b64 := gjson.GetBytes(rspBody, "predictions.0.bytesBase64Encoded").String()
	if b64 == "" {
		c.Data(http.StatusOK, "application/json", rspBody) // no audio — return raw for debugging
		return true
	}
	wav, wErr := base64.StdEncoding.DecodeString(b64)
	if wErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("lyria: decode audio: %v", wErr), "type": "server_error"}})
		return true
	}
	c.Data(http.StatusOK, "audio/wav", wav) // Lyria bytesBase64Encoded is already a complete WAV
	return true
}
