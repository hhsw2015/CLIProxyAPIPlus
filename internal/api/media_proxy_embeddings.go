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

// handleVertexEmbeddings serves /v1/embeddings for Vertex embedding models
// (text-embedding-004/005, gemini-embedding-001, text-multilingual-embedding-002)
// via SA OAuth + the :predict endpoint. Mirrors handleVertexImagen and reuses its
// generic entry finder + token minter.
//
// Returns true if handled (success OR failure with response written). Returns
// false if no gemini-api-key SA entry lists modelName — caller falls through to
// the generic media resolver (e.g. OpenAI text-embedding-3-* via other channels).
func (s *Server) handleVertexEmbeddings(c *gin.Context, modelName string, body []byte) bool {
	if s.cfg == nil {
		return false
	}
	entry, project, region := s.findImagenEntry(modelName) // generic: any gemini-SA entry whose pool lists modelName
	if entry == nil {
		return false
	}

	saJSON, decErr := base64.StdEncoding.DecodeString(entry.CredentialsB64)
	if decErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
			"message": fmt.Sprintf("vertex-embeddings: decode SA creds: %v", decErr),
			"type":    "server_error",
		}})
		return true
	}
	token, tokErr := mintGCPToken(saJSON)
	if tokErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": fmt.Sprintf("vertex-embeddings: token mint: %v", tokErr),
			"type":    "server_error",
		}})
		return true
	}

	vBody, tErr := convertOpenAIEmbeddingsToVertex(body)
	if tErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": fmt.Sprintf("vertex-embeddings: %v", tErr),
			"type":    "invalid_request_error",
		}})
		return true
	}

	host := region + "-aiplatform.googleapis.com"
	if region == "global" {
		host = "aiplatform.googleapis.com"
	}
	upstreamURL := fmt.Sprintf(
		"https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
		host, project, region, modelName,
	)
	req, reqErr := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(vBody))
	if reqErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
			"message": fmt.Sprintf("vertex-embeddings: build upstream request: %v", reqErr),
			"type":    "server_error",
		}})
		return true
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, doErr := client.Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": fmt.Sprintf("vertex-embeddings: upstream POST: %v", doErr),
			"type":    "server_error",
		}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rspBody, _ := io.ReadAll(resp.Body)
	log.Debugf("[vertex-embeddings] project=%s region=%s model=%s status=%d", project, region, modelName, resp.StatusCode)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.Data(resp.StatusCode, "application/json", rspBody)
		return true
	}
	c.Data(http.StatusOK, "application/json", convertVertexToOpenAIEmbeddings(rspBody, modelName))
	return true
}

// convertOpenAIEmbeddingsToVertex translates an OpenAI /v1/embeddings body
// ({"model":..., "input": str | []str}) into Vertex's :predict shape
// ({"instances":[{"content":"..."}]}).
func convertOpenAIEmbeddingsToVertex(openaiBody []byte) ([]byte, error) {
	input := gjson.GetBytes(openaiBody, "input")
	if !input.Exists() {
		return nil, fmt.Errorf("input is required")
	}
	var texts []string
	if input.IsArray() {
		input.ForEach(func(_, v gjson.Result) bool {
			texts = append(texts, v.String())
			return true
		})
	} else {
		texts = []string{input.String()}
	}
	if len(texts) == 0 {
		return nil, fmt.Errorf("input is empty")
	}
	out := []byte(`{"instances":[]}`)
	for i, t := range texts {
		out, _ = sjson.SetBytes(out, fmt.Sprintf("instances.%d.content", i), t)
	}
	return out, nil
}

// convertVertexToOpenAIEmbeddings translates a Vertex :predict response
// ({"predictions":[{"embeddings":{"values":[...],"statistics":{"token_count":N}}}]})
// into the OpenAI /v1/embeddings list shape.
func convertVertexToOpenAIEmbeddings(vertexBody []byte, modelName string) []byte {
	preds := gjson.GetBytes(vertexBody, "predictions")
	if !preds.Exists() || !preds.IsArray() {
		return vertexBody // pass through unexpected/error shape for debugging
	}
	out := []byte(`{"object":"list","data":[],"model":"","usage":{"prompt_tokens":0,"total_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "model", modelName)
	i := 0
	var totalTokens int64
	preds.ForEach(func(_, p gjson.Result) bool {
		vals := p.Get("embeddings.values")
		if vals.Exists() && vals.IsArray() {
			out, _ = sjson.SetRawBytes(out, fmt.Sprintf("data.%d.embedding", i), []byte(vals.Raw))
			out, _ = sjson.SetBytes(out, fmt.Sprintf("data.%d.index", i), i)
			out, _ = sjson.SetBytes(out, fmt.Sprintf("data.%d.object", i), "embedding")
			i++
		}
		if tc := p.Get("embeddings.statistics.token_count"); tc.Exists() {
			totalTokens += tc.Int()
		}
		return true
	})
	if i == 0 {
		return vertexBody
	}
	out, _ = sjson.SetBytes(out, "usage.prompt_tokens", totalTokens)
	out, _ = sjson.SetBytes(out, "usage.total_tokens", totalTokens)
	return out
}
