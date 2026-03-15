package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetAuthByIndex_ReturnsConfigBackedAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:        "openai-compatibility:skyclaw1:cbebf8850bad",
		Provider:  "skyclaw1",
		Label:     "skyclaw1",
		Prefix:    "skyclaw1",
		Status:    coreauth.StatusActive,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Attributes: map[string]string{
			"source":       "config:skyclaw1[cbebf8850bad]",
			"base_url":     "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy",
			"compat_name":  "skyclaw1",
			"provider_key": "skyclaw1",
		},
	}
	if _, err := manager.Register(context.Background(), record); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	authIndex := record.EnsureIndex()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	router := gin.New()
	router.GET("/v0/management/auth-index/:id", h.GetAuthByIndex)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-index/"+authIndex, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Auth map[string]any `json:"auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Auth["auth_index"] != authIndex {
		t.Fatalf("expected auth_index %q, got %#v", authIndex, payload.Auth["auth_index"])
	}
	if payload.Auth["provider"] != "skyclaw1" {
		t.Fatalf("expected provider skyclaw1, got %#v", payload.Auth["provider"])
	}
	if payload.Auth["label"] != "skyclaw1" {
		t.Fatalf("expected label skyclaw1, got %#v", payload.Auth["label"])
	}
	if payload.Auth["source"] != "config" {
		t.Fatalf("expected source config, got %#v", payload.Auth["source"])
	}
	if payload.Auth["base_url"] != "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy" {
		t.Fatalf("expected base_url in response, got %#v", payload.Auth["base_url"])
	}
}

func TestGetAuthByIndex_ReturnsNotFound(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, coreauth.NewManager(nil, nil, nil))
	router := gin.New()
	router.GET("/v0/management/auth-index/:id", h.GetAuthByIndex)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-index/missing", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d body=%s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["error"] != "auth not found" {
		t.Fatalf("expected not found error, got %#v", payload["error"])
	}
}
