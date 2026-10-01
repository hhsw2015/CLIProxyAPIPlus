package configaccess

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// mintEph builds a cpa-eph-style HS256 JWT for tests (the dispatcher's minter does this).
func mintEph(secret []byte, alg, iss string, exp int64, sub string) string {
	hdr, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
	pl, _ := json.Marshal(map[string]any{"iss": iss, "exp": exp, "iat": time.Now().Unix(), "sub": sub})
	h := base64.RawURLEncoding.EncodeToString(hdr)
	p := base64.RawURLEncoding.EncodeToString(pl)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(h + "." + p))
	s := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return h + "." + p + "." + s
}

func TestVerifyEphToken(t *testing.T) {
	secret := []byte("test-master-secret")
	future := time.Now().Add(30 * time.Minute).Unix()
	past := time.Now().Add(-time.Minute).Unix()

	// valid token authenticates and returns its subject
	if sub, ok := verifyEphToken(mintEph(secret, "HS256", "cpa-eph", future, "agentA"), secret); !ok || sub != "agentA" {
		t.Fatalf("valid token: got sub=%q ok=%v, want agentA true", sub, ok)
	}

	// each of these must be rejected
	cases := []struct {
		name   string
		tok    string
		secret []byte
	}{
		{"wrong-secret (forged sig)", mintEph(secret, "HS256", "cpa-eph", future, "x"), []byte("other-secret")},
		{"expired", mintEph(secret, "HS256", "cpa-eph", past, "x"), secret},
		{"wrong-issuer", mintEph(secret, "HS256", "nope", future, "x"), secret},
		{"alg-none (confusion guard)", mintEph(secret, "none", "cpa-eph", future, "x"), secret},
		{"empty-secret", mintEph(secret, "HS256", "cpa-eph", future, "x"), nil},
		{"not-a-jwt (plain relay key)", "sk-plain-relay-key", secret},
	}
	for _, c := range cases {
		if _, ok := verifyEphToken(c.tok, c.secret); ok {
			t.Errorf("%s: should have been rejected", c.name)
		}
	}

	// tampered signature must fail
	tok := mintEph(secret, "HS256", "cpa-eph", future, "agentA")
	last := byte('A')
	if tok[len(tok)-1] == 'A' {
		last = 'B'
	}
	if _, ok := verifyEphToken(tok[:len(tok)-1]+string(last), secret); ok {
		t.Error("tampered signature: should have been rejected")
	}
}
