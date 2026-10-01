package configaccess

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// ephSecretEnv holds the dispatcher's HMAC secret for ephemeral capability tokens
// (cpa-eph). When set, the config-access provider also accepts a valid cpa-eph HS256
// JWT (short TTL) in addition to the configured api-keys. See docs/cpa-ephemeral-token.md.
const ephSecretEnv = "CPA_EPH_SECRET"

// Register ensures the config-access provider is available to the access manager.
func Register(cfg *sdkconfig.SDKConfig) {
	if cfg == nil {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	keys := normalizeKeys(cfg.APIKeys)
	ephSecret := []byte(strings.TrimSpace(os.Getenv(ephSecretEnv)))
	if len(keys) == 0 && len(ephSecret) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	sdkaccess.RegisterProvider(
		sdkaccess.AccessProviderTypeConfigAPIKey,
		newProvider(sdkaccess.DefaultAccessProviderName, keys, ephSecret),
	)
}

type provider struct {
	name      string
	keys      map[string]struct{}
	ephSecret []byte
}

func newProvider(name string, keys []string, ephSecret []byte) *provider {
	providerName := strings.TrimSpace(name)
	if providerName == "" {
		providerName = sdkaccess.DefaultAccessProviderName
	}
	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	return &provider{name: providerName, keys: keySet, ephSecret: ephSecret}
}

func (p *provider) Identifier() string {
	if p == nil || p.name == "" {
		return sdkaccess.DefaultAccessProviderName
	}
	return p.name
}

func (p *provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	if len(p.keys) == 0 && len(p.ephSecret) == 0 {
		return nil, sdkaccess.NewNotHandledError()
	}
	authHeader := r.Header.Get("Authorization")
	authHeaderGoogle := r.Header.Get("X-Goog-Api-Key")
	authHeaderAnthropic := r.Header.Get("X-Api-Key")
	queryKey := ""
	queryAuthToken := ""
	if r.URL != nil {
		queryKey = r.URL.Query().Get("key")
		queryAuthToken = r.URL.Query().Get("auth_token")
	}
	if authHeader == "" && authHeaderGoogle == "" && authHeaderAnthropic == "" && queryKey == "" && queryAuthToken == "" {
		return nil, sdkaccess.NewNoCredentialsError()
	}

	apiKey := extractBearerToken(authHeader)

	candidates := []struct {
		value  string
		source string
	}{
		{apiKey, "authorization"},
		{authHeaderGoogle, "x-goog-api-key"},
		{authHeaderAnthropic, "x-api-key"},
		{queryKey, "query-key"},
		{queryAuthToken, "query-auth-token"},
	}

	for _, candidate := range candidates {
		if candidate.value == "" {
			continue
		}
		if _, ok := p.keys[candidate.value]; ok {
			return &sdkaccess.Result{
				Provider:  p.Identifier(),
				Principal: candidate.value,
				Metadata: map[string]string{
					"source": candidate.source,
				},
			}, nil
		}
		// Ephemeral capability token (cpa-eph HS256 JWT): short-TTL, signed by the
		// dispatcher's secret (env CPA_EPH_SECRET), verified statelessly. Grants the
		// same access as a relay key until exp; no revocation (TTL is the control).
		// The "eph" metadata marks the request so model-listing endpoints can hide the
		// full catalog. See docs/cpa-ephemeral-token.md.
		if sub, ok := verifyEphToken(candidate.value, p.ephSecret); ok {
			return &sdkaccess.Result{
				Provider:  p.Identifier(),
				Principal: "eph:" + sub,
				Metadata: map[string]string{
					"source": candidate.source,
					"eph":    "1",
				},
			}, nil
		}
	}

	return nil, sdkaccess.NewInvalidCredentialError()
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if strings.ToLower(parts[0]) != "bearer" {
		return header
	}
	return strings.TrimSpace(parts[1])
}

func normalizeKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		if _, exists := seen[trimmedKey]; exists {
			continue
		}
		seen[trimmedKey] = struct{}{}
		normalized = append(normalized, trimmedKey)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// verifyEphToken validates a cpa-eph ephemeral capability token (HS256 JWT) signed with
// secret, returning its subject when the alg, signature, issuer and expiry all check out.
// Stateless; no revocation (short TTL is the control). Rejecting any alg other than HS256
// closes the alg:none / alg-confusion hole. See docs/cpa-ephemeral-token.md.
func verifyEphToken(tok string, secret []byte) (string, bool) {
	if len(secret) == 0 || tok == "" {
		return "", false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", false
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err = json.Unmarshal(header, &h); err != nil || h.Alg != "HS256" {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var c struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err = json.Unmarshal(payload, &c); err != nil {
		return "", false
	}
	if c.Iss != "cpa-eph" || c.Exp <= time.Now().Unix() {
		return "", false
	}
	return c.Sub, true
}
