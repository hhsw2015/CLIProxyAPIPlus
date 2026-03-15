package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// GetAuthByIndex returns auth metadata for a single auth_index, including
// config-backed entries that are not listed by /auth-files.
func (h *Handler) GetAuthByIndex(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	authIndex := strings.TrimSpace(c.Param("id"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("id"))
	}
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing auth index"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	entry := h.buildAuthLookupEntry(auth)
	if entry == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"auth": entry})
}

func (h *Handler) buildAuthLookupEntry(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return nil
	}

	entry := h.buildAuthFileEntry(auth)
	path := strings.TrimSpace(authAttribute(auth, "path"))
	if entry == nil {
		// buildAuthFileEntry intentionally hides removed file-backed auths. Preserve
		// that behavior and only synthesize a response for config-backed entries.
		if path != "" && !isRuntimeOnlyAuth(auth) {
			return nil
		}

		auth.EnsureIndex()
		name := strings.TrimSpace(auth.FileName)
		if name == "" {
			name = auth.ID
		}
		entry = gin.H{
			"id":             auth.ID,
			"auth_index":     auth.Index,
			"name":           name,
			"type":           strings.TrimSpace(auth.Provider),
			"provider":       strings.TrimSpace(auth.Provider),
			"label":          auth.Label,
			"status":         auth.Status,
			"status_message": auth.StatusMessage,
			"disabled":       auth.Disabled,
			"unavailable":    auth.Unavailable,
			"runtime_only":   isRuntimeOnlyAuth(auth),
			"size":           int64(0),
			"source":         classifyLookupSource(auth),
		}
		if email := authEmail(auth); email != "" {
			entry["email"] = email
		}
		if accountType, account := auth.AccountInfo(); accountType != "" || account != "" {
			if accountType != "" {
				entry["account_type"] = accountType
			}
			if account != "" {
				entry["account"] = account
			}
		}
		if !auth.CreatedAt.IsZero() {
			entry["created_at"] = auth.CreatedAt
		}
		if !auth.UpdatedAt.IsZero() {
			entry["modtime"] = auth.UpdatedAt
			entry["updated_at"] = auth.UpdatedAt
		}
		if !auth.LastRefreshedAt.IsZero() {
			entry["last_refresh"] = auth.LastRefreshedAt
		}
		if !auth.NextRetryAfter.IsZero() {
			entry["next_retry_after"] = auth.NextRetryAfter
		}
	}

	if baseURL := strings.TrimSpace(authAttribute(auth, "base_url")); baseURL != "" {
		entry["base_url"] = baseURL
	}
	if compatName := strings.TrimSpace(authAttribute(auth, "compat_name")); compatName != "" {
		entry["compat_name"] = compatName
	}
	if providerKey := strings.TrimSpace(authAttribute(auth, "provider_key")); providerKey != "" {
		entry["provider_key"] = providerKey
	}
	if priority := strings.TrimSpace(authAttribute(auth, "priority")); priority != "" {
		entry["priority"] = priority
	}
	if auth.Prefix != "" {
		entry["prefix"] = auth.Prefix
	}
	if auth.ProxyURL != "" {
		entry["proxy_url"] = auth.ProxyURL
	}
	if rawSource := strings.TrimSpace(authAttribute(auth, "source")); rawSource != "" {
		entry["source_detail"] = rawSource
	}
	return entry
}

func classifyLookupSource(auth *coreauth.Auth) string {
	if auth == nil {
		return "memory"
	}
	if path := strings.TrimSpace(authAttribute(auth, "path")); path != "" {
		return "file"
	}
	raw := strings.TrimSpace(authAttribute(auth, "source"))
	switch {
	case raw == "":
		return "memory"
	case strings.HasPrefix(strings.ToLower(raw), "config:"):
		return "config"
	default:
		return raw
	}
}
