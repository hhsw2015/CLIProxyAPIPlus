package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestIsBedrockAuth(t *testing.T) {
	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want bool
	}{
		{
			name: "nil auth",
			auth: nil,
			want: false,
		},
		{
			name: "empty attributes",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{}},
			want: false,
		},
		{
			name: "standard api-key auth",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"api_key":  "sk-ant-xxx",
					"base_url": "https://api.anthropic.com",
				},
			},
			want: false,
		},
		{
			name: "bedrock auth with AK/SK",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"aws_access_key_id":     "AKIAIOSFODNN7EXAMPLE",
					"aws_secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
					"aws_region":            "us-east-1",
				},
			},
			want: true,
		},
		{
			name: "bedrock auth with whitespace-only AK",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"aws_access_key_id": "  ",
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBedrockAuth(tt.auth); got != tt.want {
				t.Errorf("isBedrockAuth() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBedrockCreds(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"aws_access_key_id":     "AKIAIOSFODNN7EXAMPLE",
			"aws_secret_access_key": "wJalrXUtnFEMI/K7MDENG",
			"aws_region":            "us-west-2",
		},
	}
	ak, sk, region := bedrockCreds(auth)
	if ak != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("ak = %q, want AKIAIOSFODNN7EXAMPLE", ak)
	}
	if sk != "wJalrXUtnFEMI/K7MDENG" {
		t.Errorf("sk = %q, want wJalrXUtnFEMI/K7MDENG", sk)
	}
	if region != "us-west-2" {
		t.Errorf("region = %q, want us-west-2", region)
	}
}

func TestBedrockCredsDefaultRegion(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"aws_access_key_id":     "AKIAIOSFODNN7EXAMPLE",
			"aws_secret_access_key": "secret",
		},
	}
	_, _, region := bedrockCreds(auth)
	if region != "us-east-1" {
		t.Errorf("region = %q, want us-east-1 (default)", region)
	}
}

func TestPrepareBedrockBody(t *testing.T) {
	input := []byte(`{"model":"claude-opus-4.6","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)
	got := prepareBedrockBody(input)
	gotStr := string(got)

	// model field should be removed
	if contains(gotStr, `"model"`) {
		t.Error("expected model field to be removed")
	}
	// stream field should be removed
	if contains(gotStr, `"stream"`) {
		t.Error("expected stream field to be removed")
	}
	// anthropic_version should be set
	if !contains(gotStr, `"anthropic_version":"bedrock-2023-05-31"`) {
		t.Errorf("expected anthropic_version to be set, got: %s", gotStr)
	}
	// messages should be preserved
	if !contains(gotStr, `"messages"`) {
		t.Error("expected messages to be preserved")
	}
}

func TestPrepareBedrockBodyPreservesExistingVersion(t *testing.T) {
	input := []byte(`{"model":"x","anthropic_version":"custom-version","messages":[]}`)
	got := prepareBedrockBody(input)
	if !contains(string(got), `"anthropic_version":"custom-version"`) {
		t.Errorf("expected existing anthropic_version to be preserved, got: %s", string(got))
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
