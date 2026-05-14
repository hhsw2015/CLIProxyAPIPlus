//go:build !headroom_ffi

// Package headroom: no-op stub. Compiled by default when the headroom_ffi
// build tag is absent. Lets the binary ship without libheadroom_ffi.so —
// suitable when an external compression proxy (Python headroom) sits in
// front of CPA. All Compress* / NormalizeAnthropicTools / Register*CCRStore
// calls return the body unchanged.
package headroom

// CcrGet always reports "not found" in stub mode. ExpandMarkers will leave
// any <<ccr:HASH>> markers in place.
func CcrGet(hash string) (string, bool, error) {
	return "", false, nil
}

func RegisterSqliteCCRStore(dbPath string, ttlSeconds uint64) error {
	return nil
}

func RegisterRedisCCRStore(url string, keyPrefix string, ttlSeconds uint64) error {
	return nil
}

func CompressBytes(body []byte, model string, auth AuthMode) *Result {
	return &Result{Modified: false, CompressedBody: body}
}

func CompressOpenAILiveZoneBytes(body []byte, model string, auth AuthMode) *Result {
	return &Result{Modified: false, CompressedBody: body}
}

func CompressAnthropicBytes(body []byte, model string, frozenCount int, auth AuthMode) *Result {
	return &Result{Modified: false, CompressedBody: body}
}

func CompressAnthropicLiveZoneBytes(body []byte, model string, frozenCount int, auth AuthMode) *Result {
	return &Result{Modified: false, CompressedBody: body}
}

func CompressResponsesBytes(body []byte, model string, auth AuthMode) *Result {
	return &Result{Modified: false, CompressedBody: body}
}

func CompressResponsesLiveZoneBytes(body []byte, model string, auth AuthMode) *Result {
	return &Result{Modified: false, CompressedBody: body}
}

func NormalizeAnthropicTools(body []byte, auth AuthMode) *NormalizeResult {
	return &NormalizeResult{Body: body}
}
