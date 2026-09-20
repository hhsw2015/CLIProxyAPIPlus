package api

import "testing"

func TestAtlasAdaptorActionURLAndParsing(t *testing.T) {
	a := &atlasAdaptor{}
	cases := map[string]string{
		"alibaba/wan-3.0/text-to-video":         "video",
		"kwaivgi/kling-v3.0-pro/image-to-video": "video",
		"bytedance/avatar-omni-human-v1.5":      "video",
		"sync/lipsync-v3":                       "video",
		"minimax/music-3.0":                     "audio",
		"suno/chirp-v5":                         "audio",
		"google/gemini-3.1-flash-tts":           "audio",
		"bytedance/seed-asr-2.0":                "audio",
		"reve-ai/reve-2.1/text-to-image":        "image",
		"qwen-image-3.0-pro/edit":               "image",
	}
	for model, want := range cases {
		if got := atlasActionForModel(model); got != want {
			t.Errorf("%s -> %s, want %s", model, got, want)
		}
	}
	if u := a.BuildRequestURL("https://api.atlascloud.ai/", "video"); u != "https://api.atlascloud.ai/api/v1/model/generateVideo" {
		t.Fatalf("video url = %s", u)
	}
	if u := a.BuildRequestURL("https://api.atlascloud.ai", "audio"); u != "https://api.atlascloud.ai/api/v1/model/generateAudio" {
		t.Fatalf("audio url = %s", u)
	}
	info, err := a.ParseTaskResult([]byte(`{"data":{"id":"p1","status":"completed","outputs":["https://cdn/x.mp4"],"error":null}}`))
	if err != nil || info.Status != TaskStatusSuccess || info.URL != "https://cdn/x.mp4" {
		t.Fatalf("completed parse = %+v err=%v", info, err)
	}
	info, _ = a.ParseTaskResult([]byte(`{"data":{"id":"p1","status":"failed","error":"insufficient balance","outputs":[]}}`))
	if info.Status != TaskStatusFailure || info.Reason != "insufficient balance" {
		t.Fatalf("failed parse = %+v", info)
	}
	if platformForEntry("atlas-media-wan-3.0-t2v", "https://api.atlascloud.ai") != "atlas" {
		t.Fatal("atlas-media-* entries must route to the atlas adaptor")
	}
	if platformForEntry("atlascloud-extra", "https://origin-api.atlascloud.ai/v1") == "atlas" {
		t.Fatal("the LLM entry must not route to the media adaptor")
	}
}
