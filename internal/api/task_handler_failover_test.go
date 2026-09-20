package api

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestResolveTaskCandidatesPriorityOrderAndPerEntryPlatform(t *testing.T) {
	s := &Server{cfg: &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
		{Name: "skywork-sora", Priority: 1, BaseURL: "http://127.0.0.1:7000/gpt-proxy/azure/sora",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k-proxy"}},
			Models:        []config.OpenAICompatibilityModel{{Name: "sora-2"}, {Name: "sora-2-pro"}}},
		{Name: "azure-us-sora-2-1", Priority: 10, BaseURL: "https://az.openai.azure.com",
			Headers: map[string]string{"api-key": "k-azure"},
			Models:  []config.OpenAICompatibilityModel{{Name: "sora-2"}}},
		{Name: "shubiaobiao-sora", Priority: 5, BaseURL: "https://api.shubiaobiao.cn",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k-sbb"}},
			Models:        []config.OpenAICompatibilityModel{{Name: "sora-2"}, {Name: "sora-2-pro"}}},
		{Name: "unrelated-chat", Priority: 10, BaseURL: "https://x",
			Models: []config.OpenAICompatibilityModel{{Name: "gpt-5"}}},
		{Name: "fal-wan", Priority: 8, BaseURL: "https://queue.fal.run/alibaba/wan-3.0/text-to-video",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k-fal"}},
			Models:        []config.OpenAICompatibilityModel{{Name: "alibaba-wan-3.0-text-to-video", Alias: "wan3.0-video"}}},
		{Name: "dashscope-wan", Priority: 9, BaseURL: "https://dashscope.aliyuncs.com",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k-ds"}},
			Models:        []config.OpenAICompatibilityModel{{Name: "wan3.0-t2v", Alias: "wan3.0-video"}}},
	}}}

	got := s.resolveTaskCandidates("sora-2", "auto")
	wantNames := []string{"azure-us-sora-2-1", "shubiaobiao-sora", "skywork-sora"}
	wantPlatforms := []string{"sora", "sora", "gpt-proxy"}
	wantKeys := []string{"k-azure", "k-sbb", "k-proxy"}
	if len(got) != len(wantNames) {
		t.Fatalf("candidates = %d, want %d", len(got), len(wantNames))
	}
	for i, c := range got {
		if c.name != wantNames[i] || c.platform != wantPlatforms[i] || c.provider.apiKey != wantKeys[i] {
			t.Errorf("candidate[%d] = %s/%s/%s, want %s/%s/%s", i, c.name, c.platform, c.provider.apiKey,
				wantNames[i], wantPlatforms[i], wantKeys[i])
		}
	}

	// sora-2-pro: the dead gpt-proxy entry is first in config order but lowest
	// priority, so the reseller must come first.
	pro := s.resolveTaskCandidates("sora-2-pro", "auto")
	if len(pro) != 2 || pro[0].name != "shubiaobiao-sora" {
		t.Fatalf("sora-2-pro candidates = %+v, want shubiaobiao-sora first", pro)
	}

	// An explicit route platform overrides per-entry detection.
	if k := s.resolveTaskCandidates("sora-2", "kling"); len(k) == 0 || k[0].platform != "kling" {
		t.Fatalf("explicit platform not honored: %+v", k)
	}
	if s.resolveTaskCandidates("nope", "auto") != nil {
		t.Fatal("unknown model must yield no candidates")
	}

	// A client alias shared by two providers: each candidate carries ITS OWN
	// upstream id and platform, dashscope (P9) before fal (P8).
	wan := s.resolveTaskCandidates("wan3.0-video", "auto")
	if len(wan) != 2 || wan[0].name != "dashscope-wan" || wan[0].upstream != "wan3.0-t2v" || wan[0].platform != "dashscope" ||
		wan[1].name != "fal-wan" || wan[1].upstream != "alibaba-wan-3.0-text-to-video" || wan[1].platform != "fal" {
		t.Fatalf("shared-alias candidates = %+v", wan)
	}
}

func TestTaskSubmitFailsOver(t *testing.T) {
	for _, st := range []int{401, 402, 403, 404, 408, 429, 500, 502, 503} {
		if !taskSubmitFailsOver(st) {
			t.Errorf("%d must fail over", st)
		}
	}
	for _, st := range []int{http.StatusBadRequest, 409, 413, 422} {
		if taskSubmitFailsOver(st) {
			t.Errorf("%d is a client error and must not fail over", st)
		}
	}
}
