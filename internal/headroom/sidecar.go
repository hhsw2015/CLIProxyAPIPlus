//go:build headroom_ffi

package headroom

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"time"
)

// Wire protocol — must match `crates/headroom-sidecar/src/main.rs`.
//
// Request:
//
//	u8        format tag (1=anthropic_full, 2=openai_full, 3=responses_full)
//	u8        auth mode (0=Payg, 1=OAuth, 2=Subscription, 3=Unknown)
//	u64 le    frozen_count
//	u32 le    model name byte length
//	bytes     model name (utf-8)
//	u32 le    body byte length
//	bytes     body (utf-8 JSON)
//
// Reply:
//
//	u32 le    bincode payload length
//	bytes     bincode-encoded { modified, body, tokens_before, tokens_after,
//	                            tokens_saved, ratio, error }
const (
	sidecarFmtAnthropicFull byte = 1
	sidecarFmtOpenAIFull    byte = 2
	sidecarFmtResponsesFull byte = 3
)

// sidecarPath returns the configured UDS path or "" when sidecar mode is
// off. Set HEADROOM_SIDECAR_UDS to opt into the out-of-process pipeline;
// the in-process cgo fallback is otherwise used unchanged.
func sidecarPath() string {
	return os.Getenv("HEADROOM_SIDECAR_UDS")
}

// sidecarPool keeps a small pool of UnixConns so high-rate compress
// calls don't pay the connect-syscall + accept overhead per request.
// Connection lifetime is bounded by sidecarConnIdleTimeout to recover
// from sidecar restarts.
type sidecarPool struct {
	path    string
	mu      sync.Mutex
	idle    []*sidecarConn
	maxIdle int
}

type sidecarConn struct {
	c       net.Conn
	lastUse time.Time
}

const sidecarConnIdleTimeout = 60 * time.Second

var (
	sidecarOnce sync.Once
	sidecar     *sidecarPool
)

func sidecarClient() *sidecarPool {
	sidecarOnce.Do(func() {
		path := sidecarPath()
		if path == "" {
			return
		}
		sidecar = &sidecarPool{path: path, maxIdle: 8}
	})
	return sidecar
}

func (p *sidecarPool) acquire() (*sidecarConn, error) {
	if p == nil {
		return nil, errors.New("sidecar disabled")
	}
	p.mu.Lock()
	for len(p.idle) > 0 {
		n := len(p.idle) - 1
		conn := p.idle[n]
		p.idle = p.idle[:n]
		if time.Since(conn.lastUse) < sidecarConnIdleTimeout {
			p.mu.Unlock()
			return conn, nil
		}
		_ = conn.c.Close()
	}
	p.mu.Unlock()

	c, err := net.DialTimeout("unix", p.path, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial sidecar: %w", err)
	}
	return &sidecarConn{c: c}, nil
}

func (p *sidecarPool) release(conn *sidecarConn, broken bool) {
	if p == nil || conn == nil {
		return
	}
	if broken {
		_ = conn.c.Close()
		return
	}
	conn.lastUse = time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle) >= p.maxIdle {
		_ = conn.c.Close()
		return
	}
	p.idle = append(p.idle, conn)
}

// compressViaSidecar issues one request/reply round-trip on a pooled
// UnixConn and returns the parsed CompressResult. `body` is the raw
// JSON bytes; we never copy or reparse it on this side.
func compressViaSidecar(
	formatTag byte,
	body []byte,
	model string,
	frozenCount int,
	auth AuthMode,
) (*Result, error) {
	pool := sidecarClient()
	if pool == nil {
		return nil, errors.New("sidecar disabled")
	}

	conn, err := pool.acquire()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(compressionBudget)
	if err := conn.c.SetDeadline(deadline); err != nil {
		pool.release(conn, true)
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	// ── Encode + send request ────────────────────────────────────
	modelBytes := []byte(model)
	header := make([]byte, 0, 2+8+4+len(modelBytes)+4)
	header = append(header, formatTag, byte(auth))
	var u64 [8]byte
	binary.LittleEndian.PutUint64(u64[:], uint64(frozenCount))
	header = append(header, u64[:]...)
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], uint32(len(modelBytes)))
	header = append(header, u32[:]...)
	header = append(header, modelBytes...)
	binary.LittleEndian.PutUint32(u32[:], uint32(len(body)))
	header = append(header, u32[:]...)

	if _, err := conn.c.Write(header); err != nil {
		pool.release(conn, true)
		return nil, fmt.Errorf("write header: %w", err)
	}
	if _, err := conn.c.Write(body); err != nil {
		pool.release(conn, true)
		return nil, fmt.Errorf("write body: %w", err)
	}

	// ── Read length-prefixed bincode reply ───────────────────────
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn.c, lenBuf[:]); err != nil {
		pool.release(conn, true)
		return nil, fmt.Errorf("read len: %w", err)
	}
	replyLen := binary.LittleEndian.Uint32(lenBuf[:])
	// Cap the reply size at 64 MiB so a corrupt frame doesn't cause an
	// unbounded allocation. Production payloads are <2 MiB.
	if replyLen > 64<<20 {
		pool.release(conn, true)
		return nil, fmt.Errorf("reply too large: %d", replyLen)
	}
	payload := make([]byte, replyLen)
	if _, err := io.ReadFull(conn.c, payload); err != nil {
		pool.release(conn, true)
		return nil, fmt.Errorf("read payload: %w", err)
	}

	pool.release(conn, false)

	parsed, err := decodeBincodeReply(payload)
	if err != nil {
		return nil, fmt.Errorf("decode reply: %w", err)
	}

	out := &Result{
		Modified:         parsed.Modified && parsed.Body != nil,
		CompressedBody:   body,
		TokensBefore:     int(parsed.TokensBefore),
		TokensAfter:      int(parsed.TokensAfter),
		TokensSaved:      int(parsed.TokensSaved),
		CompressionRatio: parsed.Ratio,
	}
	if parsed.Error != nil && *parsed.Error != "" {
		out.Modified = false
		out.Error = errors.New(*parsed.Error)
		return out, nil
	}
	if out.Modified {
		out.CompressedBody = []byte(*parsed.Body)
	}
	return out, nil
}

// bincodeReply mirrors `CompressReply` in
// crates/headroom-sidecar/src/main.rs.
//
// Field order MUST match the Rust struct declaration order — bincode 2's
// `serde::encode_to_vec` with the `standard` config writes fields in
// declaration order with no field names.
type bincodeReply struct {
	Modified     bool
	Body         *string
	TokensBefore uint64
	TokensAfter  uint64
	TokensSaved  uint64
	Ratio        float64
	Error        *string
}

// decodeBincodeReply parses bincode 2 `Configuration::standard()` output
// for our `CompressReply` struct.
//
// bincode 2 wire format (standard config):
//   - bool         → 1 byte (0/1)
//   - Option<T>    → 1 byte tag (0=None, 1=Some) followed by T when Some
//   - String       → varint(u64) length + UTF-8 bytes
//   - u64/i64      → varint
//   - f64          → 8 bytes little-endian (no varint)
//
// Varint encoding (bincode-specific, NOT protobuf):
//   - byte < 251         → that byte
//   - byte == 251        → next 2 bytes le → u16
//   - byte == 252        → next 4 bytes le → u32
//   - byte == 253        → next 8 bytes le → u64
//   - byte == 254        → next 16 bytes le → u128 (we never see this)
//   - byte == 255        → reserved
func decodeBincodeReply(buf []byte) (*bincodeReply, error) {
	r := &bincodeReader{buf: buf}
	out := &bincodeReply{}

	mod, err := r.readBool()
	if err != nil {
		return nil, fmt.Errorf("modified: %w", err)
	}
	out.Modified = mod

	body, err := r.readOptionString()
	if err != nil {
		return nil, fmt.Errorf("body: %w", err)
	}
	out.Body = body

	tb, err := r.readVarintU64()
	if err != nil {
		return nil, fmt.Errorf("tokens_before: %w", err)
	}
	out.TokensBefore = tb

	ta, err := r.readVarintU64()
	if err != nil {
		return nil, fmt.Errorf("tokens_after: %w", err)
	}
	out.TokensAfter = ta

	ts, err := r.readVarintU64()
	if err != nil {
		return nil, fmt.Errorf("tokens_saved: %w", err)
	}
	out.TokensSaved = ts

	ratio, err := r.readF64()
	if err != nil {
		return nil, fmt.Errorf("ratio: %w", err)
	}
	out.Ratio = ratio

	errStr, err := r.readOptionString()
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	out.Error = errStr

	return out, nil
}

type bincodeReader struct {
	buf []byte
	pos int
}

func (r *bincodeReader) need(n int) ([]byte, error) {
	if r.pos+n > len(r.buf) {
		return nil, fmt.Errorf("short read: need %d, have %d", n, len(r.buf)-r.pos)
	}
	out := r.buf[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

func (r *bincodeReader) readBool() (bool, error) {
	b, err := r.need(1)
	if err != nil {
		return false, err
	}
	return b[0] != 0, nil
}

func (r *bincodeReader) readVarintU64() (uint64, error) {
	b, err := r.need(1)
	if err != nil {
		return 0, err
	}
	tag := b[0]
	switch {
	case tag < 251:
		return uint64(tag), nil
	case tag == 251:
		x, err := r.need(2)
		if err != nil {
			return 0, err
		}
		return uint64(binary.LittleEndian.Uint16(x)), nil
	case tag == 252:
		x, err := r.need(4)
		if err != nil {
			return 0, err
		}
		return uint64(binary.LittleEndian.Uint32(x)), nil
	case tag == 253:
		x, err := r.need(8)
		if err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint64(x), nil
	default:
		return 0, fmt.Errorf("varint tag %d unsupported", tag)
	}
}

func (r *bincodeReader) readF64() (float64, error) {
	b, err := r.need(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
}

func (r *bincodeReader) readOptionString() (*string, error) {
	tag, err := r.need(1)
	if err != nil {
		return nil, err
	}
	switch tag[0] {
	case 0:
		return nil, nil
	case 1:
		length, err := r.readVarintU64()
		if err != nil {
			return nil, fmt.Errorf("string len: %w", err)
		}
		bytes, err := r.need(int(length))
		if err != nil {
			return nil, err
		}
		s := string(bytes)
		return &s, nil
	default:
		return nil, fmt.Errorf("bad Option tag %d", tag[0])
	}
}

// keep context import quiet — used by future cancel-aware variants
var _ = context.TODO
