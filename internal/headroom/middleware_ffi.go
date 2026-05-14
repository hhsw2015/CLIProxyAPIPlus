//go:build headroom_ffi

// Package headroom: FFI-backed compression. Built only when -tags headroom_ffi.
// Without the tag, middleware_stub.go provides no-op fallbacks and the binary
// does not link libheadroom_ffi.so.
package headroom

// #cgo CFLAGS: -I${SRCDIR}
// #cgo LDFLAGS: -lheadroom_ffi
// #include <stdlib.h>
// #include <headroom.h>
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"
)

func computeCompressionBudget() time.Duration {
	cpu := runtime.NumCPU()
	switch {
	case cpu <= 2:
		return 60 * time.Second
	case cpu <= 4:
		return 15 * time.Second
	case cpu <= 8:
		return 8 * time.Second
	default:
		return 5 * time.Second
	}
}

func computeCompressionWorkers() int {
	cpu := runtime.NumCPU()
	switch {
	case cpu <= 2:
		return 1
	case cpu <= 4:
		return 2
	default:
		return min(32, cpu*4)
	}
}

var (
	compressionWorkers = make(chan struct{}, computeCompressionWorkers())
	compressionBudget  = computeCompressionBudget()
)

func runWithBudget(fn func() *Result) (*Result, bool) {
	select {
	case compressionWorkers <- struct{}{}:
	default:
		return nil, false
	}

	resultCh := make(chan *Result, 1)
	go func() {
		defer func() { <-compressionWorkers }()
		defer func() {
			if r := recover(); r != nil {
				select {
				case resultCh <- &Result{Error: fmt.Errorf("compress panic: %v", r)}:
				default:
				}
			}
		}()
		select {
		case resultCh <- fn():
		default:
		}
	}()

	timer := time.NewTimer(compressionBudget)
	defer timer.Stop()
	select {
	case r := <-resultCh:
		return r, true
	case <-timer.C:
		return nil, false
	}
}

func CcrGet(hash string) (string, bool, error) {
	cHash := C.CString(hash)
	defer C.free(unsafe.Pointer(cHash))
	cResult := C.headroom_ccr_get(cHash)
	defer C.headroom_result_free(cResult)
	if cResult == nil {
		return "", false, errors.New("nil ccr result")
	}
	var got struct {
		Found   bool    `json:"found"`
		Content string  `json:"content"`
		Error   *string `json:"error"`
	}
	if err := json.Unmarshal([]byte(C.GoString(cResult)), &got); err != nil {
		return "", false, fmt.Errorf("ccr parse: %w", err)
	}
	if got.Error != nil && *got.Error != "" {
		return "", false, errors.New(*got.Error)
	}
	return got.Content, got.Found, nil
}

func RegisterSqliteCCRStore(dbPath string, ttlSeconds uint64) error {
	cPath := C.CString(dbPath)
	defer C.free(unsafe.Pointer(cPath))
	cErr := C.headroom_ccr_init_sqlite(cPath, C.ulonglong(ttlSeconds))
	if cErr == nil {
		return nil
	}
	defer C.headroom_result_free(cErr)
	return errors.New(C.GoString(cErr))
}

func CompressBytes(body []byte, model string, auth AuthMode) *Result {
	if !enabled.Load() || len(body) == 0 || !shouldRun(body, model) {
		return &Result{Modified: false, CompressedBody: body}
	}
	if sidecarClient() != nil {
		return compressViaSidecarOrFallback(sidecarFmtOpenAIFull, body, model, 0, auth)
	}
	r, ok := runWithBudget(func() *Result {
		cBody := C.CBytes(body)
		defer C.free(unsafe.Pointer(cBody))
		cModel := C.CString(model)
		defer C.free(unsafe.Pointer(cModel))
		cResult := C.headroom_compress_openai_full(
			(*C.uchar)(cBody), C.size_t(len(body)), cModel, C.uint8_t(auth))
		defer C.headroom_result_free(cResult)
		return parseResult(cResult, body)
	})
	if !ok {
		return &Result{Modified: false, CompressedBody: body, Error: errors.New("compression budget exceeded")}
	}
	return r
}

func CompressOpenAILiveZoneBytes(body []byte, model string, auth AuthMode) *Result {
	if !enabled.Load() || len(body) == 0 || !shouldRun(body, model) {
		return &Result{Modified: false, CompressedBody: body}
	}
	cBody := C.CBytes(body)
	defer C.free(unsafe.Pointer(cBody))
	bodyLen := C.size_t(len(body))
	cModel := C.CString(model)
	defer C.free(unsafe.Pointer(cModel))

	cResult := C.headroom_compress_openai(
		(*C.uchar)(cBody), bodyLen, cModel, C.uint8_t(auth))
	defer C.headroom_result_free(cResult)
	return parseResult(cResult, body)
}

func CompressAnthropicBytes(body []byte, model string, frozenCount int, auth AuthMode) *Result {
	if !enabled.Load() || len(body) == 0 || !shouldRun(body, model) {
		return &Result{Modified: false, CompressedBody: body}
	}
	if sidecarClient() != nil {
		return compressViaSidecarOrFallback(sidecarFmtAnthropicFull, body, model, frozenCount, auth)
	}
	r, ok := runWithBudget(func() *Result {
		cBody := C.CBytes(body)
		defer C.free(unsafe.Pointer(cBody))
		cModel := C.CString(model)
		defer C.free(unsafe.Pointer(cModel))
		cResult := C.headroom_compress_anthropic_full(
			(*C.uchar)(cBody), C.size_t(len(body)), C.size_t(frozenCount), cModel, C.uint8_t(auth))
		defer C.headroom_result_free(cResult)
		return parseResult(cResult, body)
	})
	if !ok {
		return &Result{Modified: false, CompressedBody: body, Error: errors.New("compression budget exceeded")}
	}
	return r
}

func CompressAnthropicLiveZoneBytes(body []byte, model string, frozenCount int, auth AuthMode) *Result {
	if !enabled.Load() || len(body) == 0 || !shouldRun(body, model) {
		return &Result{Modified: false, CompressedBody: body}
	}
	cBody := C.CBytes(body)
	defer C.free(unsafe.Pointer(cBody))
	bodyLen := C.size_t(len(body))
	cModel := C.CString(model)
	defer C.free(unsafe.Pointer(cModel))

	cResult := C.headroom_compress_anthropic(
		(*C.uchar)(cBody), bodyLen, C.size_t(frozenCount), cModel, C.uint8_t(auth))
	defer C.headroom_result_free(cResult)
	return parseResult(cResult, body)
}

func CompressResponsesBytes(body []byte, model string, auth AuthMode) *Result {
	if !enabled.Load() || len(body) == 0 || !shouldRun(body, model) {
		return &Result{Modified: false, CompressedBody: body}
	}
	if sidecarClient() != nil {
		return compressViaSidecarOrFallback(sidecarFmtResponsesFull, body, model, 0, auth)
	}
	r, ok := runWithBudget(func() *Result {
		cBody := C.CBytes(body)
		defer C.free(unsafe.Pointer(cBody))
		cModel := C.CString(model)
		defer C.free(unsafe.Pointer(cModel))
		cResult := C.headroom_compress_openai_responses_full(
			(*C.uchar)(cBody), C.size_t(len(body)), cModel, C.uint8_t(auth))
		defer C.headroom_result_free(cResult)
		return parseResult(cResult, body)
	})
	if !ok {
		return &Result{Modified: false, CompressedBody: body, Error: errors.New("compression budget exceeded")}
	}
	return r
}

func compressViaSidecarOrFallback(
	formatTag byte,
	body []byte,
	model string,
	frozenCount int,
	auth AuthMode,
) *Result {
	r, err := compressViaSidecar(formatTag, body, model, frozenCount, auth)
	if err != nil {
		return &Result{Modified: false, CompressedBody: body, Error: err}
	}
	return r
}

func CompressResponsesLiveZoneBytes(body []byte, model string, auth AuthMode) *Result {
	if !enabled.Load() || len(body) == 0 || !shouldRun(body, model) {
		return &Result{Modified: false, CompressedBody: body}
	}
	cBody := C.CBytes(body)
	defer C.free(unsafe.Pointer(cBody))
	bodyLen := C.size_t(len(body))
	cModel := C.CString(model)
	defer C.free(unsafe.Pointer(cModel))

	cResult := C.headroom_compress_openai_responses(
		(*C.uchar)(cBody), bodyLen, cModel, C.uint8_t(auth))
	defer C.headroom_result_free(cResult)
	return parseResult(cResult, body)
}

func RegisterRedisCCRStore(url string, keyPrefix string, ttlSeconds uint64) error {
	cURL := C.CString(url)
	defer C.free(unsafe.Pointer(cURL))
	var cPrefix *C.char
	if keyPrefix != "" {
		cPrefix = C.CString(keyPrefix)
		defer C.free(unsafe.Pointer(cPrefix))
	}
	cErr := C.headroom_ccr_init_redis(cURL, cPrefix, C.ulonglong(ttlSeconds))
	if cErr == nil {
		return nil
	}
	defer C.headroom_result_free(cErr)
	return errors.New(C.GoString(cErr))
}

func NormalizeAnthropicTools(body []byte, auth AuthMode) *NormalizeResult {
	if !enabled.Load() || len(body) == 0 {
		return &NormalizeResult{Body: body}
	}
	cBody := C.CBytes(body)
	defer C.free(unsafe.Pointer(cBody))
	cResult := C.headroom_normalize_anthropic_tools(
		(*C.uchar)(cBody), C.size_t(len(body)), C.uint8_t(auth))
	defer C.headroom_result_free(cResult)
	return parseNormalizeResult(cResult, body)
}

type nrJSON struct {
	Modified  bool    `json:"modified"`
	Body      *string `json:"body"`
	E1Applied bool    `json:"e1_applied"`
	E2Applied bool    `json:"e2_applied"`
	Error     *string `json:"error"`
}

func parseNormalizeResult(cResult *C.char, originalBody []byte) *NormalizeResult {
	if cResult == nil {
		return &NormalizeResult{Body: originalBody, Error: errors.New("nil ffi result")}
	}
	resultStr := C.GoString(cResult)
	var nr nrJSON
	if err := json.Unmarshal([]byte(resultStr), &nr); err != nil {
		return &NormalizeResult{Body: originalBody, Error: fmt.Errorf("parse: %w", err)}
	}
	out := &NormalizeResult{
		Modified:  nr.Modified && nr.Body != nil,
		Body:      originalBody,
		E1Applied: nr.E1Applied,
		E2Applied: nr.E2Applied,
	}
	if nr.Error != nil && *nr.Error != "" {
		out.Modified = false
		out.Error = errors.New(*nr.Error)
		return out
	}
	if out.Modified {
		out.Body = []byte(*nr.Body)
	}
	return out
}

type crJSON struct {
	Modified         bool    `json:"modified"`
	Body             *string `json:"body"`
	TokensBefore     int     `json:"tokens_before"`
	TokensAfter      int     `json:"tokens_after"`
	TokensSaved      int     `json:"tokens_saved"`
	CompressionRatio float64 `json:"compression_ratio"`
	Error            *string `json:"error"`
}

func parseResult(cResult *C.char, originalBody []byte) *Result {
	if cResult == nil {
		return &Result{CompressedBody: originalBody, Error: errors.New("nil ffi result")}
	}
	resultStr := C.GoString(cResult)
	var cr crJSON
	if err := json.Unmarshal([]byte(resultStr), &cr); err != nil {
		return &Result{CompressedBody: originalBody, Error: fmt.Errorf("parse: %w", err)}
	}
	out := &Result{
		Modified:         cr.Modified && cr.Body != nil,
		CompressedBody:   originalBody,
		TokensBefore:     cr.TokensBefore,
		TokensAfter:      cr.TokensAfter,
		TokensSaved:      cr.TokensSaved,
		CompressionRatio: cr.CompressionRatio,
	}
	if cr.Error != nil && *cr.Error != "" {
		out.Modified = false
		out.Error = errors.New(*cr.Error)
		return out
	}
	if out.Modified {
		out.CompressedBody = []byte(*cr.Body)
	}
	return out
}
