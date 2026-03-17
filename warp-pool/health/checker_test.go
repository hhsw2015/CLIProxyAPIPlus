package health

import "testing"

func TestShouldAutoRecoverHealthFailure(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{name: "connection refused", msg: `Get "https://cloudflare.com/cdn-cgi/trace": proxyconnect tcp: dial tcp 127.0.0.1:10001: connect: connection refused`, want: true},
		{name: "connection reset", msg: `Get "https://cloudflare.com/cdn-cgi/trace": read tcp 127.0.0.1:46500->127.0.0.1:1080: read: connection reset by peer`, want: true},
		{name: "eof", msg: `Get "https://cloudflare.com/cdn-cgi/trace": EOF`, want: true},
		{name: "timeout", msg: `Get "https://cloudflare.com/cdn-cgi/trace": context deadline exceeded`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAutoRecoverHealthFailure(tt.msg); got != tt.want {
				t.Fatalf("shouldAutoRecoverHealthFailure(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}
