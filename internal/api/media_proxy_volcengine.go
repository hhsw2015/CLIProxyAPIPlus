package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// Volcengine Visual (CV) image-editing on visual.volcengineapi.com — SigV4-style
// signing (access_key/secret_key, region cn-north-1, service cv). CPA otherwise has
// no Volcengine CV route. Sync ops via Action=CVProcess. The client sends an input
// image (b64/data-URL/url) + prompt on /v1/images/generations.

const (
	volcCVHost    = "visual.volcengineapi.com"
	volcCVRegion  = "cn-north-1"
	volcCVService = "cv"
)

// volcCVReqKey maps the client model name to the Volcengine req_key.
var volcCVReqKey = map[string]string{
	"volcengine-cv-inpaint":          "image2image_dream_inpaint_jimeng",
	"volcengine-cv-outpaint":         "i2i_outpainting",
	"volcengine-cv-super-resolution": "lens_nnsr2_pic_common",
	"volcengine-cv-saliency-seg":     "saliency_seg",
}

// volcCVCreds returns (accessKey, secretKey) from the volcengine-cv config entry
// (api-key stored as "AK:SK").
func (s *Server) volcCVCreds() (ak, sk string, ok bool) {
	if s.cfg == nil {
		return "", "", false
	}
	for i := range s.cfg.OpenAICompatibility {
		e := &s.cfg.OpenAICompatibility[i]
		if !strings.Contains(e.BaseURL, volcCVHost) {
			continue
		}
		for _, k := range e.APIKeyEntries {
			if parts := strings.SplitN(k.APIKey, ":", 2); len(parts) == 2 {
				return parts[0], parts[1], true
			}
		}
	}
	return "", "", false
}

func volcHMAC(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func volcSHA256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// volcSignedHeaders builds the SigV4 headers for a Volcengine Visual POST.
func volcSignedHeaders(ak, sk, query string, body []byte) map[string]string {
	now := time.Now().UTC()
	xdate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	payloadHash := volcSHA256Hex(body)
	canonicalHeaders := fmt.Sprintf("content-type:application/json\nhost:%s\nx-content-sha256:%s\nx-date:%s\n",
		volcCVHost, payloadHash, xdate)
	signedHeaders := "content-type;host;x-content-sha256;x-date"
	canonicalReq := strings.Join([]string{"POST", "/", query, canonicalHeaders, signedHeaders, payloadHash}, "\n")
	scope := fmt.Sprintf("%s/%s/%s/request", date, volcCVRegion, volcCVService)
	stringToSign := strings.Join([]string{"HMAC-SHA256", xdate, scope, volcSHA256Hex([]byte(canonicalReq))}, "\n")
	kDate := volcHMAC([]byte(sk), date)
	kRegion := volcHMAC(kDate, volcCVRegion)
	kService := volcHMAC(kRegion, volcCVService)
	kSigning := volcHMAC(kService, "request")
	sig := hex.EncodeToString(volcHMAC(kSigning, stringToSign))
	auth := fmt.Sprintf("HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", ak, scope, signedHeaders, sig)
	return map[string]string{
		"Content-Type":     "application/json",
		"X-Date":           xdate,
		"X-Content-Sha256": payloadHash,
		"Authorization":    auth,
	}
}

// handleVolcengineCV serves /v1/images/generations for Volcengine CV image-editing
// (model contains "volcengine-cv"). Sync CVProcess; returns OpenAI images b64_json.
func (s *Server) handleVolcengineCV(c *gin.Context, modelName string, body []byte) bool {
	ak, sk, ok := s.volcCVCreds()
	if !ok {
		return false
	}
	reqKey := volcCVReqKey[strings.ToLower(strings.TrimSpace(modelName))]
	if reqKey == "" {
		return false // not a known volcengine-cv op → fall through
	}
	// Input image: accept image / image_url (data-URL, raw b64, or http URL).
	img := gjson.GetBytes(body, "image").String()
	if img == "" {
		img = gjson.GetBytes(body, "image_url").String()
	}
	vBody := []byte(`{"req_key":""}`)
	vBody, _ = sjson.SetBytes(vBody, "req_key", reqKey)
	if img != "" {
		if strings.HasPrefix(img, "http") {
			vBody, _ = sjson.SetBytes(vBody, "image_urls.0", img)
		} else {
			if i := strings.Index(img, ","); strings.HasPrefix(img, "data:") && i >= 0 {
				img = img[i+1:] // strip data:...;base64,
			}
			vBody, _ = sjson.SetBytes(vBody, "binary_data_base64.0", img)
		}
	}
	if p := gjson.GetBytes(body, "prompt").String(); p != "" {
		vBody, _ = sjson.SetBytes(vBody, "prompt", p)
	}

	query := "Action=CVProcess&Version=2022-08-31"
	hdrs := volcSignedHeaders(ak, sk, query, vBody)
	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://"+volcCVHost+"/?"+query, strings.NewReader(string(vBody)))
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, doErr := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("volcengine-cv: %v", doErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	if gjson.GetBytes(rb, "code").Int() != 10000 {
		c.Data(http.StatusBadGateway, "application/json", rb)
		return true
	}
	var b64s []string
	gjson.GetBytes(rb, "data.binary_data_base64").ForEach(func(_, v gjson.Result) bool {
		if v.String() != "" {
			b64s = append(b64s, v.String())
		}
		return true
	})
	var urls []string
	gjson.GetBytes(rb, "data.image_urls").ForEach(func(_, v gjson.Result) bool {
		if v.String() != "" {
			urls = append(urls, v.String())
		}
		return true
	})
	if len(b64s) == 0 && len(urls) == 0 {
		c.Data(http.StatusOK, "application/json", rb) // no image — surface raw
		return true
	}
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", time.Now().Unix())
	i := 0
	for _, b := range b64s {
		out, _ = sjson.SetBytes(out, fmt.Sprintf("data.%d.b64_json", i), b)
		i++
	}
	for _, u := range urls {
		out, _ = sjson.SetBytes(out, fmt.Sprintf("data.%d.url", i), u)
		i++
	}
	log.Debugf("[volcengine-cv] model=%s req_key=%s images=%d", modelName, reqKey, i)
	c.Data(http.StatusOK, "application/json", out)
	return true
}
