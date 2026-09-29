package api

import (
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

// handleAzureModeration serves OpenAI-style /v1/moderations via Azure Content Safety
// (POST {host}/contentsafety/text:analyze) on the same AIServices key already used for
// Azure OpenAI/Speech. Maps Azure per-category severity (0-6) to the OpenAI moderation
// shape (flagged + categories + category_scores).
func (s *Server) handleAzureModeration(c *gin.Context) {
	raw, _ := io.ReadAll(c.Request.Body)
	// input may be a string or an array of strings.
	var inputs []string
	in := gjson.GetBytes(raw, "input")
	if in.IsArray() {
		in.ForEach(func(_, v gjson.Result) bool { inputs = append(inputs, v.String()); return true })
	} else if in.Exists() {
		inputs = []string{in.String()}
	}
	if len(inputs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "input is required", "type": "invalid_request_error"}})
		return
	}
	host, key, ok := s.azureSpeechCredHost() // AIServices resource also serves Content Safety
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "no Azure Content Safety key configured", "type": "server_error"}})
		return
	}
	model := gjson.GetBytes(raw, "model").String()
	if model == "" {
		model = "azure-content-safety"
	}

	// Azure category → OpenAI moderation category key.
	catMap := map[string]string{"Hate": "hate", "SelfHarm": "self-harm", "Sexual": "sexual", "Violence": "violence"}
	client := &http.Client{Timeout: 30 * time.Second}
	out := []byte(`{"id":"modr-azure","model":"","results":[]}`)
	out, _ = sjson.SetBytes(out, "model", model)

	for idx, text := range inputs {
		reqBody := fmt.Sprintf(`{"text":%s}`, jsonQuote(text))
		url := "https://" + host + "/contentsafety/text:analyze?api-version=2024-09-01"
		req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, strings.NewReader(reqBody))
		req.Header.Set("Ocp-Apim-Subscription-Key", key)
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := client.Do(req)
		if doErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("azure-moderation: %v", doErr), "type": "server_error"}})
			return
		}
		rb, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			c.Data(resp.StatusCode, "application/json", rb)
			return
		}
		flagged := false
		base := fmt.Sprintf("results.%d", idx)
		out, _ = sjson.SetBytes(out, base+".flagged", false)
		gjson.GetBytes(rb, "categoriesAnalysis").ForEach(func(_, ca gjson.Result) bool {
			azCat := ca.Get("category").String()
			sev := ca.Get("severity").Float()
			key := catMap[azCat]
			if key == "" {
				key = strings.ToLower(azCat)
			}
			if sev >= 2 { // Azure severity 0/2/4/6; >=2 = flag
				flagged = true
				out, _ = sjson.SetBytes(out, base+".categories."+key, true)
			} else {
				out, _ = sjson.SetBytes(out, base+".categories."+key, false)
			}
			out, _ = sjson.SetBytes(out, base+".category_scores."+key, sev/6.0)
			return true
		})
		out, _ = sjson.SetBytes(out, base+".flagged", flagged)
	}
	log.Debugf("[azure-moderation] host=%s inputs=%d", host, len(inputs))
	c.Data(http.StatusOK, "application/json", out)
}

// jsonQuote returns a JSON-safe quoted string.
func jsonQuote(s string) string {
	b, _ := sjson.SetBytes([]byte(`{"v":""}`), "v", s)
	return gjson.GetBytes(b, "v").Raw
}
