package api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Azure Speech runs on the multi-service AIServices.S0 resources whose keys are
// already in config for Azure OpenAI (the *.cognitiveservices.azure.com hosts, not
// the huo* dead ones). The same api-key mints a Speech STS token; the token's
// region drives the regional TTS/STT data-plane endpoints.

type azureSpeechTok struct {
	token  string
	region string
	expiry time.Time
}

var (
	azureSpeechMu    sync.Mutex
	azureSpeechCache azureSpeechTok
	azureRegionRe    = regexp.MustCompile(`urn:ms\.speechservices\.([a-z0-9]+)`)
	azureHostRegRe   = regexp.MustCompile(`-([a-z]+[a-z0-9]*)\.cognitiveservices\.azure\.com`)
)

// azureSpeechCredHost returns a live (non-huo) cognitiveservices host + key from
// config. These are the AIServices resources that also serve Speech.
func (s *Server) azureSpeechCredHost() (host, key string, ok bool) {
	if s.cfg == nil {
		return "", "", false
	}
	hostRe := regexp.MustCompile(`https://([a-z0-9-]+\.cognitiveservices\.azure\.com)`)
	keyRe := regexp.MustCompile(`[A-Za-z0-9]{40,}`)
	for i := range s.cfg.OpenAICompatibility {
		e := &s.cfg.OpenAICompatibility[i]
		m := hostRe.FindStringSubmatch(e.BaseURL)
		if m == nil || strings.HasPrefix(m[1], "huo") {
			continue
		}
		for _, ak := range e.APIKeyEntries {
			if keyRe.MatchString(ak.APIKey) {
				return m[1], ak.APIKey, true
			}
		}
	}
	return "", "", false
}

// azureSpeechToken mints (and caches ~9min) a Speech STS token + its region.
func (s *Server) azureSpeechToken() (token, region string, err error) {
	azureSpeechMu.Lock()
	defer azureSpeechMu.Unlock()
	if azureSpeechCache.token != "" && time.Now().Before(azureSpeechCache.expiry) {
		return azureSpeechCache.token, azureSpeechCache.region, nil
	}
	host, key, ok := s.azureSpeechCredHost()
	if !ok {
		return "", "", fmt.Errorf("no live Azure cognitiveservices key in config")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/sts/v1.0/issueToken", nil)
	req.Header.Set("Ocp-Apim-Subscription-Key", key)
	req.Header.Set("Content-Length", "0")
	resp, doErr := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if doErr != nil {
		return "", "", doErr
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("issueToken %d", resp.StatusCode)
	}
	tok := string(b)
	reg := ""
	// JWT payload is base64url (no padding); extract region from urn:ms.speechservices.<region>.
	if parts := strings.Split(tok, "."); len(parts) >= 2 {
		if dec, e := base64.RawURLEncoding.DecodeString(parts[1]); e == nil {
			if m := azureRegionRe.FindSubmatch(dec); m != nil {
				reg = string(m[1])
			}
		}
	}
	if reg == "" { // fallback: parse region from hostname suffix
		if m := azureHostRegRe.FindStringSubmatch(host); m != nil {
			reg = m[1]
		}
	}
	if reg == "" {
		return "", "", fmt.Errorf("could not determine Azure Speech region")
	}
	azureSpeechCache = azureSpeechTok{token: tok, region: reg, expiry: time.Now().Add(9 * time.Minute)}
	return tok, reg, nil
}

// azureVoiceName maps OpenAI voice ids to Azure neural voices (multilingual voices
// handle Chinese well too); a full Azure voice name passes through.
func azureVoiceName(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "alloy":
		return "en-US-AvaMultilingualNeural"
	case "echo":
		return "en-US-AndrewMultilingualNeural"
	case "fable":
		return "en-GB-RyanNeural"
	case "onyx":
		return "en-US-BrianMultilingualNeural"
	case "nova":
		return "en-US-EmmaMultilingualNeural"
	case "shimmer":
		return "en-US-JennyNeural"
	default:
		return v
	}
}

// handleAzureSpeechTTS serves /v1/audio/speech for Azure Speech (model "azure-tts").
func (s *Server) handleAzureSpeechTTS(c *gin.Context, modelName string, body []byte) bool {
	input := gjson.GetBytes(body, "input").String()
	if input == "" {
		input = gjson.GetBytes(body, "text").String()
	}
	if strings.TrimSpace(input) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "input is required", "type": "invalid_request_error"}})
		return true
	}
	token, region, err := s.azureSpeechToken()
	if err != nil {
		return false // no Azure Speech creds → fall through to other providers
	}
	voice := azureVoiceName(gjson.GetBytes(body, "voice").String())
	lang := voice
	if i := strings.Index(voice, "-"); i >= 0 {
		if j := strings.Index(voice[i+1:], "-"); j >= 0 {
			lang = voice[:i+1+j]
		}
	}
	ssml := fmt.Sprintf(`<speak version='1.0' xml:lang='%s'><voice name='%s'>%s</voice></speak>`,
		lang, voice, ssmlEscape(input))
	url := fmt.Sprintf("https://%s.tts.speech.microsoft.com/cognitiveservices/v1", region)
	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, strings.NewReader(ssml))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/ssml+xml")
	req.Header.Set("X-Microsoft-OutputFormat", "audio-24khz-48kbitrate-mono-mp3")
	req.Header.Set("User-Agent", "cliproxyapi")
	resp, doErr := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("azure-tts: %v", doErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	audio, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		c.Data(resp.StatusCode, "application/json", audio)
		return true
	}
	log.Debugf("[azure-tts] region=%s voice=%s bytes=%d", region, voice, len(audio))
	c.Data(http.StatusOK, "audio/mpeg", audio)
	return true
}

// handleAzureSpeechSTT serves /v1/audio/transcriptions for Azure Speech
// (model "azure-transcribe") via the short-audio REST API. Accepts a WAV upload.
func (s *Server) handleAzureSpeechSTT(c *gin.Context, modelName string, body []byte) bool {
	audio, filename := extractAudioFromMultipart(body, c.GetHeader("Content-Type"))
	if len(audio) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "audio file is required (multipart 'file')", "type": "invalid_request_error"}})
		return true
	}
	if _, rate, ok := wavToPCM(audio); !ok || rate <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": fmt.Sprintf("azure-transcribe requires a PCM WAV file (got %s)", filename),
			"type":    "invalid_request_error",
		}})
		return true
	}
	_, rate, _ := wavToPCM(audio)
	token, region, err := s.azureSpeechToken()
	if err != nil {
		return false
	}
	url := fmt.Sprintf("https://%s.stt.speech.microsoft.com/speech/recognition/conversation/cognitiveservices/v1?language=en-US&format=detailed", region)
	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, bytes.NewReader(audio))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", fmt.Sprintf("audio/wav; codecs=audio/pcm; samplerate=%d", rate))
	req.Header.Set("Accept", "application/json")
	resp, doErr := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if doErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("azure-transcribe: %v", doErr), "type": "server_error"}})
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		c.Data(resp.StatusCode, "application/json", rb)
		return true
	}
	text := gjson.GetBytes(rb, "DisplayText").String()
	if text == "" {
		text = gjson.GetBytes(rb, "NBest.0.Display").String()
	}
	log.Debugf("[azure-transcribe] region=%s rate=%d text_len=%d", region, rate, len(text))
	out, _ := sjson.SetBytes([]byte(`{"text":""}`), "text", text)
	c.Data(http.StatusOK, "application/json", out)
	return true
}

// ssmlEscape escapes XML-special characters for SSML text content.
func ssmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;", "\"", "&quot;")
	return r.Replace(s)
}
