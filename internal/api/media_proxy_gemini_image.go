package api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// handleVertexGeminiImage serves /v1/images/generations for Vertex Gemini image
// models (gemini-3-pro-image, gemini-2.5-flash-image, gemini-3.1-flash-image —
// the "nano-banana" family). Imagen is retired (2026-08-17), so these are the
// image path. Unlike Imagen (:predict), Gemini image models generate via
// :generateContent with generationConfig.responseModalities:["IMAGE"], returning
// inline PNG bytes in candidates[].content.parts[].inlineData.data.
//
// Mirrors handleVertexImagen: reuses findImagenEntry + mintGCPToken. Returns true
// if handled; false if no gemini-SA entry lists the model (caller falls through).
func (s *Server) handleVertexGeminiImage(c *gin.Context, modelName string, body []byte) bool {
	if s.cfg == nil {
		return false
	}
	entry, project, region := s.findImagenEntry(modelName)
	if entry == nil {
		return false
	}

	saJSON, decErr := base64.StdEncoding.DecodeString(entry.CredentialsB64)
	if decErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
			"message": fmt.Sprintf("gemini-image: decode SA creds: %v", decErr),
			"type":    "server_error",
		}})
		return true
	}
	token, tokErr := mintGCPToken(saJSON)
	if tokErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": fmt.Sprintf("gemini-image: token mint: %v", tokErr),
			"type":    "server_error",
		}})
		return true
	}

	prompt := gjson.GetBytes(body, "prompt").String()
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": "prompt is required",
			"type":    "invalid_request_error",
		}})
		return true
	}
	vBody := []byte(`{"contents":[{"role":"user","parts":[{"text":""}]}],"generationConfig":{"responseModalities":["IMAGE"]}}`)
	vBody, _ = sjson.SetBytes(vBody, "contents.0.parts.0.text", prompt)

	host := region + "-aiplatform.googleapis.com"
	if region == "global" {
		host = "aiplatform.googleapis.com"
	}
	upstreamURL := fmt.Sprintf(
		"https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		host, project, region, modelName,
	)
	req, reqErr := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(vBody))
	if reqErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
			"message": fmt.Sprintf("gemini-image: build upstream request: %v", reqErr),
			"type":    "server_error",
		}})
		return true
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, doErr := client.Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": fmt.Sprintf("gemini-image: upstream POST: %v", doErr),
			"type":    "server_error",
		}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rspBody, _ := io.ReadAll(resp.Body)
	log.Debugf("[gemini-image] project=%s region=%s model=%s status=%d", project, region, modelName, resp.StatusCode)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.Data(resp.StatusCode, "application/json", rspBody)
		return true
	}
	c.Data(http.StatusOK, "application/json", convertGeminiImageToOpenAI(rspBody))
	return true
}

// convertGeminiImageToOpenAI extracts inline image bytes from a Gemini
// :generateContent response into the OpenAI /v1/images/generations shape
// ({"created": ts, "data": [{"b64_json": "..."}, ...]}).
func convertGeminiImageToOpenAI(vertexBody []byte) []byte {
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", time.Now().Unix())
	i := 0
	gjson.GetBytes(vertexBody, "candidates").ForEach(func(_, cand gjson.Result) bool {
		cand.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
			b64 := p.Get("inlineData.data").String()
			if b64 == "" {
				b64 = p.Get("inline_data.data").String()
			}
			if b64 != "" {
				out, _ = sjson.SetBytes(out, fmt.Sprintf("data.%d.b64_json", i), b64)
				i++
			}
			return true
		})
		return true
	})
	if i == 0 {
		return vertexBody // no image extracted — return raw for debugging
	}
	return out
}
