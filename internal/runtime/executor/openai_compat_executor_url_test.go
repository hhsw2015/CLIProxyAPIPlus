package executor

import "testing"

func Test_openAICompatRequestURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		want     string
	}{
		{
			name:     "empty base returns endpoint",
			baseURL:  "",
			endpoint: "/chat/completions",
			want:     "/chat/completions",
		},
		{
			name:     "empty endpoint returns trimmed base",
			baseURL:  "https://api.example.com/v1/",
			endpoint: "",
			want:     "https://api.example.com/v1",
		},
		{
			name:     "base without endpoint appends it",
			baseURL:  "https://openrouter.ai/api/v1",
			endpoint: "/chat/completions",
			want:     "https://openrouter.ai/api/v1/chat/completions",
		},
		{
			name:     "base already ending with endpoint is kept as-is",
			baseURL:  "https://api.example.com/v1/chat/completions",
			endpoint: "/chat/completions",
			want:     "https://api.example.com/v1/chat/completions",
		},
		{
			name:     "base with trailing slash and endpoint suffix",
			baseURL:  "https://api.example.com/v1/chat/completions/",
			endpoint: "/chat/completions",
			want:     "https://api.example.com/v1/chat/completions",
		},
		{
			name:     "Azure URL with query params path ends with endpoint",
			baseURL:  "https://huo-02.openai.azure.com/openai/deployments/gpt-5.4/chat/completions?api-version=2024-08-01-preview",
			endpoint: "/chat/completions",
			want:     "https://huo-02.openai.azure.com/openai/deployments/gpt-5.4/chat/completions?api-version=2024-08-01-preview",
		},
		{
			name:     "Azure URL with query params path without endpoint appends correctly",
			baseURL:  "https://huo-02.openai.azure.com/openai/deployments/gpt-5.4?api-version=2024-08-01-preview",
			endpoint: "/chat/completions",
			want:     "https://huo-02.openai.azure.com/openai/deployments/gpt-5.4/chat/completions?api-version=2024-08-01-preview",
		},
		{
			name:     "Deepseek standard base URL",
			baseURL:  "https://api.deepseek.com/v1",
			endpoint: "/chat/completions",
			want:     "https://api.deepseek.com/v1/chat/completions",
		},
		{
			name:     "Skywork URL without endpoint suffix",
			baseURL:  "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy",
			endpoint: "/chat/completions",
			want:     "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy/chat/completions",
		},
		{
			name:     "Skywork URL already ending with chat/completions",
			baseURL:  "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy/chat/completions",
			endpoint: "/chat/completions",
			want:     "https://desktop-llm.skywork.ai/skycowork_llm/v1/proxy/chat/completions",
		},
		{
			name:     "responses compact endpoint",
			baseURL:  "https://api.example.com/v1",
			endpoint: "/responses/compact",
			want:     "https://api.example.com/v1/responses/compact",
		},
		{
			name:     "case insensitive path matching",
			baseURL:  "https://api.example.com/v1/Chat/Completions",
			endpoint: "/chat/completions",
			want:     "https://api.example.com/v1/Chat/Completions",
		},
		{
			name:     "Azure Responses API path matches responses/compact endpoint",
			baseURL:  "https://huo-6608-resource.openai.azure.com/openai/v1/responses?api-version=preview",
			endpoint: "/responses/compact",
			want:     "https://huo-6608-resource.openai.azure.com/openai/v1/responses?api-version=preview",
		},
		{
			name:     "Azure Responses API without query params",
			baseURL:  "https://host.openai.azure.com/openai/responses",
			endpoint: "/responses/compact",
			want:     "https://host.openai.azure.com/openai/responses",
		},
		{
			name:     "non-responses base URL still appends responses/compact",
			baseURL:  "https://api.example.com/v1",
			endpoint: "/responses/compact",
			want:     "https://api.example.com/v1/responses/compact",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := openAICompatRequestURL(tt.baseURL, tt.endpoint)
			if got != tt.want {
				t.Errorf("openAICompatRequestURL(%q, %q)\n  got  = %q\n  want = %q", tt.baseURL, tt.endpoint, got, tt.want)
			}
		})
	}
}
