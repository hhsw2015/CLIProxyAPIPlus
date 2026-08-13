package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// handleVertexImagen serves /v1/images/generations for Vertex Imagen models.
//
// Flow:
//  1. Find a gemini-api-key entry with credentials-b64 that lists modelName.
//  2. Mint an OAuth token from the SA private key (JWT bearer → curl exchange).
//  3. Translate OpenAI images/generations body → Vertex Imagen :predict body.
//  4. POST to https://<region>-aiplatform.googleapis.com/.../models/<model>:predict
//  5. Translate Vertex response → OpenAI images/generations shape.
//
// Returns true if the request was handled (success OR failure with response written).
// Returns false if no matching provider found — caller falls through to next resolver.
func (s *Server) handleVertexImagen(c *gin.Context, modelName string, body []byte) bool {
	if s.cfg == nil {
		return false
	}
	entry, project, region := s.findImagenEntry(modelName)
	if entry == nil {
		return false
	}

	// Mint OAuth token from SA credentials.
	saJSON, decErr := base64.StdEncoding.DecodeString(entry.CredentialsB64)
	if decErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
			"message": fmt.Sprintf("imagen: decode SA creds: %v", decErr),
			"type":    "server_error",
		}})
		return true
	}
	token, tokErr := mintGCPToken(saJSON)
	if tokErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": fmt.Sprintf("imagen: token mint: %v", tokErr),
			"type":    "server_error",
		}})
		return true
	}

	// Translate OpenAI request → Vertex Imagen :predict shape.
	vBody, tErr := convertOpenAIImagesToImagen(body)
	if tErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": fmt.Sprintf("imagen: request translate: %v", tErr),
			"type":    "invalid_request_error",
		}})
		return true
	}

	upstreamURL := fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
		region, project, region, modelName,
	)
	req, reqErr := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(vBody))
	if reqErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
			"message": fmt.Sprintf("imagen: build upstream request: %v", reqErr),
			"type":    "server_error",
		}})
		return true
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	// Bounded non-streaming image call: Imagen :predict returns a single JSON
	// blob (no SSE), so a hard ceiling here can't truncate a live stream. 90s
	// covers the slowest ultra-model generation with headroom.
	client := &http.Client{Timeout: 90 * time.Second}
	resp, doErr := client.Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": fmt.Sprintf("imagen: upstream POST: %v", doErr),
			"type":    "server_error",
		}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rspBody, _ := io.ReadAll(resp.Body)
	log.Debugf("[imagen] project=%s region=%s model=%s status=%d body_len=%d", project, region, modelName, resp.StatusCode, len(rspBody))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.Data(resp.StatusCode, "application/json", rspBody)
		return true
	}

	openaiResp := convertImagenToOpenAIImages(rspBody, modelName)
	c.Data(http.StatusOK, "application/json", openaiResp)
	return true
}

// findImagenEntry scans gemini-api-key entries for one that lists modelName in
// its model-project-pool AND carries credentials-b64. Returns (entry, project, region).
func (s *Server) findImagenEntry(modelName string) (*geminiImagenEntry, string, string) {
	if s.cfg == nil {
		return nil, "", ""
	}
	for i := range s.cfg.GeminiKey {
		e := &s.cfg.GeminiKey[i]
		if e.Disabled || strings.TrimSpace(e.CredentialsB64) == "" {
			continue
		}
		region := strings.TrimSpace(e.VertexLocation)
		if region == "" {
			continue
		}
		// Match model in ModelProjectPool. No pool → this entry can't serve
		// the model; skip to the next.
		projects := e.ModelProjectPool[modelName]
		if len(projects) == 0 {
			continue
		}
		// Round-robin naive: pick first. CPA session-affinity happens at the
		// per-region layer (one entry per region), and we already have N
		// region entries so single-project-per-entry is fine.
		return &geminiImagenEntry{
			APIKey:         e.APIKey,
			CredentialsB64: e.CredentialsB64,
			VertexLocation: region,
		}, projects[0], region
	}
	return nil, "", ""
}

type geminiImagenEntry struct {
	APIKey         string
	CredentialsB64 string
	VertexLocation string
}

// imagenTokenCache caches minted OAuth tokens per SA client_email so we don't
// sign a JWT + round-trip Google's token endpoint on every image request
// (adds 100-400ms and can itself get throttled under bursts). Tokens are
// valid ~1h; we reuse until 5 min before expiry.
type cachedImagenToken struct {
	token  string
	expiry time.Time
}

var imagenTokenCache sync.Map // client_email → cachedImagenToken

// mintGCPToken exchanges a service-account JWT bearer for an OAuth access token,
// caching the result per SA until shortly before expiry. Uses curl subprocess
// (Python-side google-auth is slow on macOS; curl is fine on Linux too), with
// an inline http.Client fallback if curl is absent.
func mintGCPToken(saJSON []byte) (string, error) {
	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(saJSON, &sa); err != nil {
		return "", fmt.Errorf("parse SA JSON: %w", err)
	}
	if sa.ClientEmail != "" {
		if v, ok := imagenTokenCache.Load(sa.ClientEmail); ok {
			if ct, ok := v.(cachedImagenToken); ok && time.Now().Before(ct.expiry) {
				return ct.token, nil
			}
		}
	}
	// Sign JWT with RS256.
	claims := jwt.MapClaims{
		"iss":   sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   sa.TokenURI,
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	block, _ := jwt.ParseRSAPrivateKeyFromPEM([]byte(sa.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("parse SA private key: not PEM")
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	assertion, err := tok.SignedString(block)
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	cacheToken := func(tok string) {
		if sa.ClientEmail != "" && tok != "" {
			imagenTokenCache.Store(sa.ClientEmail, cachedImagenToken{
				token:  tok,
				expiry: time.Now().Add(55 * time.Minute),
			})
		}
	}

	// Prefer curl (bypasses Python-style HTTPS slowness — safe on Linux/macOS).
	if _, lookErr := exec.LookPath("curl"); lookErr == nil {
		out, curlErr := runCurl(sa.TokenURI, form.Encode(), 10*time.Second)
		if curlErr == nil {
			var r struct {
				AccessToken string `json:"access_token"`
			}
			if jerr := json.Unmarshal(out, &r); jerr == nil && r.AccessToken != "" {
				cacheToken(r.AccessToken)
				return r.AccessToken, nil
			}
		}
	}

	// Fallback: net/http POST.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, sa.TokenURI, strings.NewReader(form.Encode()))
	if reqErr != nil {
		return "", fmt.Errorf("build token request: %w", reqErr)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	var r struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("parse token response: %w (body=%s)", err, string(respBody[:minInt(len(respBody), 200)]))
	}
	if r.AccessToken == "" {
		return "", fmt.Errorf("token response missing access_token: %s", string(respBody[:minInt(len(respBody), 200)]))
	}
	cacheToken(r.AccessToken)
	return r.AccessToken, nil
}

func runCurl(u, body string, timeout time.Duration) ([]byte, error) {
	cmd := exec.Command(
		"curl", "-sS", "--max-time", fmt.Sprintf("%.0f", timeout.Seconds()),
		"-X", "POST",
		"-H", "Content-Type: application/x-www-form-urlencoded",
		"--data", body,
		u,
	)
	return cmd.Output()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// convertOpenAIImagesToImagen translates an OpenAI /v1/images/generations body
// into Vertex Imagen's :predict body shape:
//
//	{ "instances": [{"prompt": "..."}], "parameters": {"sampleCount": N} }
func convertOpenAIImagesToImagen(openaiBody []byte) ([]byte, error) {
	prompt := gjson.GetBytes(openaiBody, "prompt").String()
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	n := int(gjson.GetBytes(openaiBody, "n").Int())
	if n <= 0 {
		n = 1
	}

	out := []byte(`{"instances":[{"prompt":""}],"parameters":{"sampleCount":1}}`)
	out, _ = sjson.SetBytes(out, "instances.0.prompt", prompt)
	out, _ = sjson.SetBytes(out, "parameters.sampleCount", n)

	// Optional pass-throughs.
	if v := gjson.GetBytes(openaiBody, "aspect_ratio"); v.Exists() {
		out, _ = sjson.SetBytes(out, "parameters.aspectRatio", v.String())
	}
	if v := gjson.GetBytes(openaiBody, "negative_prompt"); v.Exists() {
		out, _ = sjson.SetBytes(out, "instances.0.negativePrompt", v.String())
	}
	// OpenAI "size" (e.g. "1024x1024") — Vertex uses aspectRatio semantics.
	// Skip; users can pass "aspect_ratio" explicitly.

	return out, nil
}

// convertImagenToOpenAIImages translates a Vertex Imagen :predict response
// into OpenAI /v1/images/generations shape:
//
//	{ "created": ts, "data": [{"b64_json": "..."}, ...] }
func convertImagenToOpenAIImages(vertexBody []byte, modelName string) []byte {
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", time.Now().Unix())

	// Vertex response shape: {"predictions": [{"bytesBase64Encoded": "...", "mimeType": "..."}, ...]}
	preds := gjson.GetBytes(vertexBody, "predictions")
	if !preds.Exists() || !preds.IsArray() {
		// Pass through error / unexpected shape as-is so the caller can debug.
		return vertexBody
	}
	i := 0
	preds.ForEach(func(_, p gjson.Result) bool {
		b64 := p.Get("bytesBase64Encoded").String()
		if b64 == "" {
			b64 = p.Get("bytes_base64_encoded").String()
		}
		if b64 != "" {
			out, _ = sjson.SetBytes(out, fmt.Sprintf("data.%d.b64_json", i), b64)
			i++
		}
		return true
	})
	if i == 0 {
		// No images extracted — return raw so debug is possible.
		return vertexBody
	}
	_ = modelName
	return out
}
