package executor

import (
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestFillAuthLogInfo_IncludesLookupFieldsForConfigAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "openai-compatibility:skyclaw1:cbebf8850bad",
		Provider: "skyclaw1",
		Label:    "skyclaw1",
		Attributes: map[string]string{
			"source":   "config:skyclaw1[cbebf8850bad]",
			"base_url": "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy",
		},
	}

	info := upstreamRequestLog{Provider: "skyclaw1"}
	fillAuthLogInfo(&info, auth)

	if info.AuthIndex != auth.EnsureIndex() {
		t.Fatalf("expected auth index %q, got %q", auth.EnsureIndex(), info.AuthIndex)
	}
	if info.AuthBaseURL != "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy" {
		t.Fatalf("expected base url in log info, got %q", info.AuthBaseURL)
	}
	if info.AuthSource != "config:skyclaw1[cbebf8850bad]" {
		t.Fatalf("expected source detail in log info, got %q", info.AuthSource)
	}

	formatted := formatAuthInfo(info)
	for _, fragment := range []string{
		"provider=skyclaw1",
		"auth_id=openai-compatibility:skyclaw1:cbebf8850bad",
		"label=skyclaw1",
		"auth_index=" + auth.EnsureIndex(),
		"base_url=https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy",
		"source=config:skyclaw1[cbebf8850bad]",
	} {
		if !strings.Contains(formatted, fragment) {
			t.Fatalf("expected formatted auth info to contain %q, got %q", fragment, formatted)
		}
	}
}
