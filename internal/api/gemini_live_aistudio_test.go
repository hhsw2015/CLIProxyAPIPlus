package api

import (
	"encoding/json"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestGeminiLiveIsAIStudioModel(t *testing.T) {
	aistudio := []string{"gemini-3.8-live", "gemini-3.8-live-extended-thinking", "GEMINI-3.8-LIVE"}
	vertex := []string{"gemini-live-2.5-flash", "gemini-2.0-flash-live-001", ""}
	for _, m := range aistudio {
		if !geminiLiveIsAIStudioModel(m) {
			t.Errorf("%q should route to AI Studio", m)
		}
	}
	for _, m := range vertex {
		if geminiLiveIsAIStudioModel(m) {
			t.Errorf("%q should NOT route to AI Studio (stays Vertex)", m)
		}
	}
}

func TestRewriteAIStudioSetup(t *testing.T) {
	// a valid setup frame -> model rewritten to models/<id>, type Text
	in := []byte(`{"setup":{"model":"whatever","generationConfig":{"responseModalities":["AUDIO"]}}}`)
	out, mt := rewriteAIStudioSetup(in, websocket.BinaryMessage, "gemini-3.8-live")
	if out == nil || mt != websocket.TextMessage {
		t.Fatalf("valid setup rejected: out=%v mt=%d", out, mt)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("bad json out: %v", err)
	}
	if got := m["setup"].(map[string]any)["model"]; got != "models/gemini-3.8-live" {
		t.Fatalf("model = %v, want models/gemini-3.8-live", got)
	}
	// generationConfig preserved
	if _, ok := m["setup"].(map[string]any)["generationConfig"]; !ok {
		t.Fatal("generationConfig dropped")
	}
	// a non-setup first frame -> rejected
	if out, _ := rewriteAIStudioSetup([]byte(`{"clientContent":{}}`), websocket.TextMessage, "x"); out != nil {
		t.Fatal("non-setup frame must be rejected")
	}
	if out, _ := rewriteAIStudioSetup([]byte(`not json`), websocket.TextMessage, "x"); out != nil {
		t.Fatal("non-json frame must be rejected")
	}
}

func TestGeminiLiveFrameIsError(t *testing.T) {
	if !geminiLiveFrameIsError([]byte(`{"error":{"code":403,"message":"bad key"}}`)) {
		t.Error("error frame not detected")
	}
	if geminiLiveFrameIsError([]byte(`{"setupComplete":{}}`)) {
		t.Error("setupComplete misflagged as error")
	}
	if geminiLiveFrameIsError([]byte("\x00\x01binary-audio")) {
		t.Error("binary frame must not be an error signal")
	}
}

func TestGeminiAIStudioKeysFiltersVertexAndDisabled(t *testing.T) {
	dis := true
	s := &Server{cfg: &config.Config{GeminiKey: []config.GeminiKey{
		{APIKey: "AIzaLIVE1"},
		{APIKey: "AIzaVERTEX", CredentialsB64: "eyJ...", VertexLocation: "global"}, // Vertex SA -> excluded
		{APIKey: "AIzaDISABLED", Disabled: dis},                                    // disabled -> excluded
		{APIKey: "sk-not-aistudio"},                                                // not an AIza key -> excluded
		{APIKey: "AIzaLIVE2"},
	}}}
	got := s.geminiAIStudioKeys()
	if len(got) != 2 || got[0] != "AIzaLIVE1" || got[1] != "AIzaLIVE2" {
		t.Fatalf("geminiAIStudioKeys = %v, want [AIzaLIVE1 AIzaLIVE2]", got)
	}
}
