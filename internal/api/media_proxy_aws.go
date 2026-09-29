package api

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/polly"
	pollytypes "github.com/aws/aws-sdk-go-v2/service/polly/types"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	tstypes "github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// awsConverseFamily reports whether a model name belongs to a non-Claude Bedrock
// family. Used only to LOCATE the AWS-cred config entry that has the broad AI-service
// access (the ClaudeAws / AKIAU6GDVU account also grants Polly + Transcribe), so the
// media handlers pick the right key rather than a Claude-only reseller key.
func awsConverseFamily(name string) bool {
	l := strings.ToLower(strings.TrimSpace(name))
	for _, p := range []string{"amazon.", "deepseek", "meta.", "mistral.", "nvidia.", "minimax", "zai.", "nova-"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// awsMediaCreds returns AWS creds (ak, sk, region) from the claude-api-key entry
// that also serves Converse models — that account (AKIAU6GDVU) has Polly/Transcribe
// access, unlike the Claude-only reseller keys. Scans fresh each call (cheap; media
// requests are infrequent) so it stays correct across config hot-reload.
func (s *Server) awsMediaCreds() (ak, sk, region string, ok bool) {
	if s.cfg == nil {
		return "", "", "", false
	}
	for i := range s.cfg.ClaudeKey {
		k := &s.cfg.ClaudeKey[i]
		if strings.TrimSpace(k.AWSAccessKeyID) == "" {
			continue
		}
		for _, m := range k.Models {
			if awsConverseFamily(m.Name) {
				region = strings.TrimSpace(k.AWSRegion)
				if region == "" {
					region = "us-east-1"
				}
				return k.AWSAccessKeyID, k.AWSSecretAccessKey, region, true
			}
		}
	}
	return "", "", "", false
}

// pollyVoiceID maps OpenAI /v1/audio/speech voice names to Polly voice ids; a Polly
// voice name passes through unchanged.
func pollyVoiceID(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "alloy":
		return "Joanna"
	case "echo":
		return "Matthew"
	case "fable":
		return "Amy"
	case "onyx":
		return "Stephen"
	case "nova":
		return "Ruth"
	case "shimmer":
		return "Kimberly"
	default:
		return v
	}
}

// handleAWSPollyTTS serves /v1/audio/speech for AWS Polly (model contains "polly").
// Returns mp3. Reuses the Bedrock account's AWS creds.
func (s *Server) handleAWSPollyTTS(c *gin.Context, modelName string, body []byte) bool {
	ak, sk, region, ok := s.awsMediaCreds()
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
	engine := pollytypes.EngineNeural
	switch strings.ToLower(gjson.GetBytes(body, "engine").String()) {
	case "generative":
		engine = pollytypes.EngineGenerative
	case "long-form", "longform":
		engine = pollytypes.EngineLongForm
	case "standard":
		engine = pollytypes.EngineStandard
	}

	client := polly.New(polly.Options{
		Region:      region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(ak, sk, "")),
	})
	out, err := client.SynthesizeSpeech(c.Request.Context(), &polly.SynthesizeSpeechInput{
		Text:         aws.String(input),
		VoiceId:      pollytypes.VoiceId(pollyVoiceID(gjson.GetBytes(body, "voice").String())),
		OutputFormat: pollytypes.OutputFormatMp3,
		Engine:       engine,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("polly: %v", err), "type": "server_error"}})
		return true
	}
	defer func() { _ = out.AudioStream.Close() }()
	audio, _ := io.ReadAll(out.AudioStream)
	log.Debugf("[polly] region=%s voice=%s bytes=%d", region, pollyVoiceID(gjson.GetBytes(body, "voice").String()), len(audio))
	c.Data(http.StatusOK, "audio/mpeg", audio)
	return true
}

// handleBedrockStabilityImage serves /v1/images/generations for Stability models
// on Bedrock (model contains "stability"/"sd3"/"stable-image"). Stability only serves
// in us-west-2; sync InvokeModel returns base64 → OpenAI images {b64_json}.
func (s *Server) handleBedrockStabilityImage(c *gin.Context, modelName string, body []byte) bool {
	ak, sk, _, ok := s.awsMediaCreds()
	if !ok {
		return false
	}
	prompt := gjson.GetBytes(body, "prompt").String()
	if strings.TrimSpace(prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "prompt is required", "type": "invalid_request_error"}})
		return true
	}
	reqBody := []byte(`{"prompt":"","output_format":"jpeg"}`)
	reqBody, _ = sjson.SetBytes(reqBody, "prompt", prompt)
	if strings.Contains(strings.ToLower(modelName), "sd3") {
		reqBody, _ = sjson.SetBytes(reqBody, "mode", "text-to-image")
	}
	if ar := gjson.GetBytes(body, "aspect_ratio").String(); ar != "" {
		reqBody, _ = sjson.SetBytes(reqBody, "aspect_ratio", ar)
	}
	client := bedrockruntime.New(bedrockruntime.Options{
		Region:      "us-west-2", // Stability image only serves here
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(ak, sk, "")),
	})
	out, err := client.InvokeModel(c.Request.Context(), &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(modelName),
		Body:        reqBody,
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("bedrock-stability: %v", err), "type": "server_error"}})
		return true
	}
	var b64s []string
	gjson.GetBytes(out.Body, "images").ForEach(func(_, v gjson.Result) bool {
		if v.String() != "" {
			b64s = append(b64s, v.String())
		}
		return true
	})
	if len(b64s) == 0 { // sd3.5 uses artifacts[].base64
		gjson.GetBytes(out.Body, "artifacts").ForEach(func(_, a gjson.Result) bool {
			if b := a.Get("base64").String(); b != "" {
				b64s = append(b64s, b)
			}
			return true
		})
	}
	if len(b64s) == 0 {
		c.Data(http.StatusOK, "application/json", out.Body) // no image — surface raw
		return true
	}
	resp := []byte(`{"created":0,"data":[]}`)
	resp, _ = sjson.SetBytes(resp, "created", time.Now().Unix())
	for i, b := range b64s {
		resp, _ = sjson.SetBytes(resp, fmt.Sprintf("data.%d.b64_json", i), b)
	}
	log.Debugf("[bedrock-stability] model=%s images=%d", modelName, len(b64s))
	c.Data(http.StatusOK, "application/json", resp)
	return true
}

// wavToPCM extracts raw 16-bit little-endian PCM samples + sample rate from a WAV
// container (walks RIFF chunks). Transcribe streaming wants bare PCM.
func wavToPCM(b []byte) (pcm []byte, sampleRate int, ok bool) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, false
	}
	i := 12
	for i+8 <= len(b) {
		cid := string(b[i : i+4])
		csz := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		bodyStart := i + 8
		if cid == "fmt " && bodyStart+8 <= len(b) {
			sampleRate = int(binary.LittleEndian.Uint32(b[bodyStart+4 : bodyStart+8]))
		} else if cid == "data" {
			end := bodyStart + csz
			if end > len(b) || csz <= 0 {
				end = len(b)
			}
			if sampleRate <= 0 {
				return nil, 0, false
			}
			return b[bodyStart:end], sampleRate, true
		}
		i = bodyStart + csz
		if csz%2 == 1 {
			i++ // chunks are word-aligned
		}
	}
	return nil, 0, false
}

// handleAWSTranscribeSTT serves /v1/audio/transcriptions for AWS Transcribe
// (model contains "aws-transcribe") via the streaming API (no S3): the uploaded
// WAV is decoded to PCM and streamed; final transcript segments are concatenated.
func (s *Server) handleAWSTranscribeSTT(c *gin.Context, modelName string, body []byte) bool {
	ak, sk, region, ok := s.awsMediaCreds()
	if !ok {
		return false
	}
	audio, filename := extractAudioFromMultipart(body, c.GetHeader("Content-Type"))
	if len(audio) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "audio file is required (multipart 'file')", "type": "invalid_request_error"}})
		return true
	}
	pcm, rate, wavOK := wavToPCM(audio)
	if !wavOK {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": fmt.Sprintf("aws-transcribe streaming requires a PCM WAV file (got %s); convert to 16-bit WAV", filename),
			"type":    "invalid_request_error",
		}})
		return true
	}

	client := transcribestreaming.New(transcribestreaming.Options{
		Region:      region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(ak, sk, "")),
	})
	out, err := client.StartStreamTranscription(c.Request.Context(), &transcribestreaming.StartStreamTranscriptionInput{
		LanguageCode:         tstypes.LanguageCodeEnUs,
		MediaEncoding:        tstypes.MediaEncodingPcm,
		MediaSampleRateHertz: aws.Int32(int32(rate)),
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("aws-transcribe: %v", err), "type": "server_error"}})
		return true
	}
	stream := out.GetStream()

	// Send audio in the background, paced ~real-time (Transcribe streaming drops
	// audio blasted faster than playback), then close the stream to flush finals.
	go func() {
		defer func() { _ = stream.Close() }()
		const chunk = 4096 // ~85ms at 24kHz/16-bit mono
		bytesPerSec := rate * 2
		for i := 0; i < len(pcm); i += chunk {
			end := i + chunk
			if end > len(pcm) {
				end = len(pcm)
			}
			cp := make([]byte, end-i)
			copy(cp, pcm[i:end])
			if sendErr := stream.Send(c.Request.Context(), &tstypes.AudioStreamMemberAudioEvent{
				Value: tstypes.AudioEvent{AudioChunk: cp},
			}); sendErr != nil {
				log.Warnf("[aws-transcribe] send: %v", sendErr)
				return
			}
			if bytesPerSec > 0 {
				time.Sleep(time.Duration(float64(end-i) / float64(bytesPerSec) * float64(time.Second)))
			}
		}
		// Let Transcribe finalize the trailing segment before closing (otherwise the
		// last word is dropped while still partial).
		time.Sleep(800 * time.Millisecond)
	}()

	// Concatenate final (non-partial) segments; fall back to the last partial if
	// no finals arrived (very short clips).
	var final strings.Builder
	var lastPartial string
	for event := range stream.Events() {
		te, isTE := event.(*tstypes.TranscriptResultStreamMemberTranscriptEvent)
		if !isTE || te.Value.Transcript == nil {
			continue
		}
		for _, r := range te.Value.Transcript.Results {
			if len(r.Alternatives) == 0 || r.Alternatives[0].Transcript == nil {
				continue
			}
			seg := *r.Alternatives[0].Transcript
			if r.IsPartial {
				lastPartial = seg
			} else {
				final.WriteString(seg)
			}
		}
	}
	if streamErr := stream.Err(); streamErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": fmt.Sprintf("aws-transcribe stream: %v", streamErr), "type": "server_error"}})
		return true
	}
	text := final.String()
	if strings.TrimSpace(text) == "" {
		text = lastPartial
	}
	log.Infof("[aws-transcribe] region=%s rate=%d pcm=%d text_len=%d", region, rate, len(pcm), len(text))
	outBody, _ := sjson.SetBytes([]byte(`{"text":""}`), "text", text)
	c.Data(http.StatusOK, "application/json", outBody)
	return true
}
