package helps

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// BenchmarkRTK_Disabled measures the cost on the fast path when RTK is off.
// Should be a few hundred ns — early-out before any unmarshal.
func BenchmarkRTK_Disabled(b *testing.B) {
	payload := buildBenchPayload(10)
	cfg := &config.Config{}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = applyRTKToolResultCompression(cfg, payload)
	}
}

// BenchmarkRTK_NoToolResult measures the cost when RTK is enabled but the
// payload has no tool_result / no input. Should hit the early-out after
// unmarshal.
func BenchmarkRTK_NoToolResult(b *testing.B) {
	payload := []byte(`{"model":"x","temperature":0.5}`)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true}}}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = applyRTKToolResultCompression(cfg, payload)
	}
}

// BenchmarkRTK_ManyToolResults exercises the hot path with N tool_result
// blocks. With the previous sjson-based implementation this was O(N×payload
// size); the map-based implementation should be O(payload size).
func BenchmarkRTK_ManyToolResults(b *testing.B) {
	payload := buildBenchPayload(20)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RTK: config.RTKConfig{Enabled: true, MinSavingsPct: 5}}}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = applyRTKToolResultCompression(cfg, payload)
	}
}

func buildBenchPayload(numToolResults int) []byte {
	bigDiff := strings.Repeat("diff --git a/x b/x\nindex abc1234..def5678 100644\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-old\n+new\n", 200)
	var sb strings.Builder
	sb.WriteString(`{"messages":[{"role":"assistant","content":[`)
	for i := 0; i < numToolResults; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"type":"tool_use","id":"t`)
		sb.WriteString(itoa(i))
		sb.WriteString(`","name":"Bash","input":{"command":"git diff "}}`)
	}
	sb.WriteString(`]},{"role":"user","content":[`)
	for i := 0; i < numToolResults; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"type":"tool_result","tool_use_id":"t`)
		sb.WriteString(itoa(i))
		sb.WriteString(`","content":"`)
		sb.WriteString(jsonEscape(bigDiff))
		sb.WriteString(`"}`)
	}
	sb.WriteString(`]}]}`)
	return []byte(sb.String())
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
