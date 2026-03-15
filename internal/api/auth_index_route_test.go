package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestManagementAuthIndexRoute(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")

	server := newTestServer(t)
	first := &coreauth.Auth{
		ID:        "openai-compatibility:skywork:14a7c4e134b9",
		Provider:  "skywork",
		Label:     "skywork",
		Status:    coreauth.StatusActive,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Attributes: map[string]string{
			"source":   "config:skywork[14a7c4e134b9]",
			"base_url": "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy",
		},
	}
	second := &coreauth.Auth{
		ID:        "openai-compatibility:skyclaw1:cbebf8850bad",
		Provider:  "skyclaw1",
		Label:     "skyclaw1",
		Status:    coreauth.StatusActive,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Attributes: map[string]string{
			"source":   "config:skyclaw1[cbebf8850bad]",
			"base_url": "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy",
		},
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), first); err != nil {
		t.Fatalf("register first auth: %v", err)
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), second); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-index/"+second.EnsureIndex(), nil)
	req.Header.Set("Authorization", "Bearer test-management-password")

	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "\"provider\":\"skyclaw1\"") {
		t.Fatalf("expected provider in body, got %s", body)
	}
	if body := rec.Body.String(); strings.Contains(body, "\"provider\":\"skywork\"") {
		t.Fatalf("expected lookup to match requested auth index, got %s", body)
	}
}

func TestManagementAuthIndexQueryRoute(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")

	server := newTestServer(t)
	record := &coreauth.Auth{
		ID:        "openai-compatibility:skyclaw1:cbebf8850bad",
		Provider:  "skyclaw1",
		Label:     "skyclaw1",
		Status:    coreauth.StatusActive,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Attributes: map[string]string{
			"source":   "config:skyclaw1[cbebf8850bad]",
			"base_url": "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy",
		},
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), record); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-index?id="+record.EnsureIndex(), nil)
	req.Header.Set("Authorization", "Bearer test-management-password")

	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "\"auth_index\":\""+record.EnsureIndex()+"\"") {
		t.Fatalf("expected auth index in body, got %s", body)
	}
}
