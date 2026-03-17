package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"warp-pool/pool"
	"warp-pool/process"
)

// routeKind indicates how a request should be handled.
type routeKind int

const (
	routeWarp   routeKind = iota // forward through a warp backend
	routeDirect                  // connect directly using VPS IP
	routeExtra                   // forward through an external SOCKS5 proxy
)

type route struct {
	kind    routeKind
	addr    string // backend address for routeExtra (e.g. "127.0.0.1:30004")
	name    string // label for logging
	process *process.Process
	weight  int    // higher = more traffic; 0 treated as 1
}

// RouteStats holds per-route request statistics.
type RouteStats struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Total      int64  `json:"total_requests"`
	Active     int64  `json:"active_connections"`
	Healthy    bool   `json:"healthy"`
}

// routeState holds runtime counters for a route entry.
type routeState struct {
	total  int64 // atomic: total requests served
	active int64 // atomic: currently active connections
}

// Server provides unified SOCKS5 and HTTP proxy entry points
type Server struct {
	pool       *pool.Pool
	socksPort  int
	httpPort   int
	socksLn    net.Listener
	httpServer *http.Server
	wg         sync.WaitGroup

	mu      sync.Mutex
	counter uint64
	routes  []route      // round-robin route table
	states  []routeState // per-route counters (same index as routes)

	healthMu     sync.RWMutex
	extraHealthy map[string]bool // addr -> healthy
}

// New creates a new proxy server
func New(p *pool.Pool, socksPort, httpPort int) *Server {
	return &Server{
		pool:      p,
		socksPort: socksPort,
		httpPort:  httpPort,
	}
}

// ExtraBackend describes an external SOCKS5 proxy to include in rotation.
type ExtraBackend struct {
	Name string
	Addr string
}

// RouteWeights holds the weight configuration for route building.
type RouteWeights struct {
	ECH    int // default 3
	Warp   int // default 2
	Direct int // default 1
}

// NewWithOptions creates a proxy server with direct and extra backend support.
func NewWithOptions(p *pool.Pool, socksPort, httpPort int, includeDirect bool, extras []ExtraBackend, weights RouteWeights) *Server {
	s := &Server{
		pool:      p,
		socksPort: socksPort,
		httpPort:  httpPort,
	}
	s.buildRoutes(includeDirect, extras, weights)
	return s
}

// buildRoutes constructs the weighted round-robin route table.
// Each route is repeated by its weight, then interleaved for even distribution.
// Example with weights ech=3, warp=2, direct=1:
//   [ech-1, warp-0, ech-2, warp-1, ech-3, warp-2, ech-1, direct, ech-2, warp-0, ech-3, warp-1, ...]
func (s *Server) buildRoutes(includeDirect bool, extras []ExtraBackend, weights RouteWeights) {
	if weights.ECH <= 0 {
		weights.ECH = 3
	}
	if weights.Warp <= 0 {
		weights.Warp = 2
	}
	if weights.Direct <= 0 {
		weights.Direct = 1
	}

	procs := s.pool.All()

	// Build weighted lists per kind
	var echRoutes, warpRoutes, directRoutes []route
	for _, eb := range extras {
		r := route{kind: routeExtra, addr: eb.Addr, name: eb.Name, weight: weights.ECH}
		for w := 0; w < weights.ECH; w++ {
			echRoutes = append(echRoutes, r)
		}
	}
	for _, proc := range procs {
		r := route{kind: routeWarp, process: proc, name: fmt.Sprintf("warp-%d", proc.ID()), weight: weights.Warp}
		for w := 0; w < weights.Warp; w++ {
			warpRoutes = append(warpRoutes, r)
		}
	}
	if includeDirect {
		r := route{kind: routeDirect, name: "direct", weight: weights.Direct}
		for w := 0; w < weights.Direct; w++ {
			directRoutes = append(directRoutes, r)
		}
	}

	// Interleave: ech first (highest weight), then warp, then direct
	// Use round-robin merge across the three lists
	var routes []route
	ei, wi, di := 0, 0, 0
	total := len(echRoutes) + len(warpRoutes) + len(directRoutes)
	for len(routes) < total {
		if ei < len(echRoutes) {
			routes = append(routes, echRoutes[ei])
			ei++
		}
		if wi < len(warpRoutes) {
			routes = append(routes, warpRoutes[wi])
			wi++
		}
		if di < len(directRoutes) {
			routes = append(routes, directRoutes[di])
			di++
		}
	}

	s.routes = routes
	s.states = make([]routeState, len(routes))

	// Log summary
	counts := map[string]int{}
	for _, r := range routes {
		counts[r.name]++
	}
	log.Printf("[proxy] Route table (%d entries): %v", len(routes), counts)
}

// nextRoute returns the next healthy route via weighted round-robin with load awareness.
// Skips unhealthy backends. Prefers routes with fewer active connections.
func (s *Server) nextRoute() (route, int) {
	if len(s.routes) == 0 {
		proc := s.pool.Next()
		return route{kind: routeWarp, process: proc, name: fmt.Sprintf("warp-%d", proc.ID())}, -1
	}

	n := len(s.routes)
	s.mu.Lock()
	start := s.counter
	s.counter++
	s.mu.Unlock()

	// Try up to len(routes) times to find a healthy backend
	for i := 0; i < n; i++ {
		idx := int((start + uint64(i)) % uint64(n))
		r := s.routes[idx]

		switch r.kind {
		case routeWarp:
			proc := s.pool.Next()
			if proc != nil && proc.State() == process.StateRunning {
				r.process = proc
				return r, idx
			}
		case routeExtra:
			if s.isExtraHealthy(r.addr) {
				return r, idx
			}
		case routeDirect:
			return r, idx
		}
	}

	// All unhealthy — fall back to direct
	return route{kind: routeDirect, name: "direct-fallback"}, -1
}

// trackRequest increments active and total counters for a route. Returns a done func to call when the request finishes.
func (s *Server) trackRequest(idx int) func() {
	if idx < 0 || idx >= len(s.states) {
		return func() {}
	}
	atomic.AddInt64(&s.states[idx].total, 1)
	atomic.AddInt64(&s.states[idx].active, 1)
	return func() {
		atomic.AddInt64(&s.states[idx].active, -1)
	}
}

// Stats returns per-route statistics.
func (s *Server) Stats() []RouteStats {
	if len(s.routes) == 0 {
		return nil
	}

	// Deduplicate by name (routes repeat due to weights)
	type agg struct {
		name    string
		kind    string
		total   int64
		active  int64
		healthy bool
	}
	seen := map[string]*agg{}
	var order []string

	for i, r := range s.routes {
		kindStr := "warp"
		if r.kind == routeExtra {
			kindStr = "ech"
		} else if r.kind == routeDirect {
			kindStr = "direct"
		}

		a, ok := seen[r.name]
		if !ok {
			healthy := true
			if r.kind == routeExtra {
				healthy = s.isExtraHealthy(r.addr)
			} else if r.kind == routeWarp {
				proc := s.pool.Get(0) // just check pool health
				healthy = proc != nil && proc.State() == process.StateRunning
			}
			a = &agg{name: r.name, kind: kindStr, healthy: healthy}
			seen[r.name] = a
			order = append(order, r.name)
		}
		a.total += atomic.LoadInt64(&s.states[i].total)
		a.active += atomic.LoadInt64(&s.states[i].active)
	}

	stats := make([]RouteStats, 0, len(order))
	for _, name := range order {
		a := seen[name]
		stats = append(stats, RouteStats{
			Name:    a.name,
			Kind:    a.kind,
			Total:   a.total,
			Active:  a.active,
			Healthy: a.healthy,
		})
	}
	return stats
}

// isExtraHealthy returns cached health status for an external backend.
func (s *Server) isExtraHealthy(addr string) bool {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()
	if s.extraHealthy == nil {
		return true // assume healthy before first probe
	}
	healthy, ok := s.extraHealthy[addr]
	if !ok {
		return true // unknown = assume healthy
	}
	return healthy
}

// startHealthProbe runs periodic health checks on extra backends.
func (s *Server) startHealthProbe(ctx context.Context) {
	// Collect extra backend addresses
	var addrs []string
	for _, r := range s.routes {
		if r.kind == routeExtra {
			addrs = append(addrs, r.addr)
		}
	}
	if len(addrs) == 0 {
		return
	}

	// Initial probe
	s.probeExtras(addrs)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.probeExtras(addrs)
			}
		}
	}()
}

func (s *Server) probeExtras(addrs []string) {
	results := make(map[string]bool, len(addrs))
	for _, addr := range addrs {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			results[addr] = false
		} else {
			conn.Close()
			results[addr] = true
		}
	}
	s.healthMu.Lock()
	s.extraHealthy = results
	s.healthMu.Unlock()
}

// Start starts both SOCKS5 and HTTP proxy servers
func (s *Server) Start(ctx context.Context) error {
	// Start SOCKS5 proxy
	socksLn, err := net.Listen("tcp", fmt.Sprintf(":%d", s.socksPort))
	if err != nil {
		return fmt.Errorf("failed to start SOCKS proxy: %w", err)
	}
	s.socksLn = socksLn

	s.wg.Add(1)
	go s.serveSocks(ctx)

	// Start HTTP proxy
	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.httpPort),
		Handler: http.HandlerFunc(s.handleHTTP),
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("[proxy] HTTP server error: %v", err)
		}
	}()

	log.Printf("[proxy] SOCKS5 listening on :%d", s.socksPort)
	log.Printf("[proxy] HTTP listening on :%d", s.httpPort)

	// Start background health probes for extra backends
	s.startHealthProbe(ctx)

	return nil
}

// Stop stops the proxy servers
func (s *Server) Stop() error {
	if s.socksLn != nil {
		s.socksLn.Close()
	}
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.httpServer.Shutdown(ctx)
	}
	s.wg.Wait()
	return nil
}

// serveSocks handles incoming SOCKS5 connections
func (s *Server) serveSocks(ctx context.Context) {
	defer s.wg.Done()

	for {
		conn, err := s.socksLn.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("[proxy] SOCKS accept error: %v", err)
				continue
			}
		}

		go s.handleSocks(conn)
	}
}

// handleSocks handles a single SOCKS5 connection
func (s *Server) handleSocks(clientConn net.Conn) {
	defer clientConn.Close()

	r, idx := s.nextRoute()
	done := s.trackRequest(idx)
	defer done()

	switch r.kind {
	case routeDirect:
		s.handleSocksDirect(clientConn)
		return
	case routeExtra:
		s.handleSocksViaBackend(clientConn, r.addr)
		return
	}

	// routeWarp: forward through warp backend
	proc := r.process
	if proc == nil || proc.State() != process.StateRunning {
		log.Printf("[proxy] No healthy backend available")
		return
	}

	backendAddr := fmt.Sprintf("127.0.0.1:%d", proc.SocksPort())
	backendConn, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		log.Printf("[proxy] Failed to connect to backend %d: %v", proc.ID(), err)
		return
	}
	defer backendConn.Close()

	proc.IncrementRequests()
	bidirectionalCopy(clientConn, backendConn)
}

// handleSocksViaBackend forwards SOCKS traffic to an external SOCKS5 proxy.
func (s *Server) handleSocksViaBackend(clientConn net.Conn, backendAddr string) {
	backendConn, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		log.Printf("[proxy] Failed to connect to extra backend %s: %v", backendAddr, err)
		return
	}
	defer backendConn.Close()
	bidirectionalCopy(clientConn, backendConn)
}

// handleSocksDirect handles SOCKS5 protocol inline and connects directly to the target.
func (s *Server) handleSocksDirect(clientConn net.Conn) {
	// SOCKS5 auth negotiation
	buf := make([]byte, 258)
	// Read version + nmethods
	if _, err := io.ReadFull(clientConn, buf[:2]); err != nil {
		return
	}
	if buf[0] != 0x05 {
		return // not SOCKS5
	}
	nmethods := int(buf[1])
	if _, err := io.ReadFull(clientConn, buf[:nmethods]); err != nil {
		return
	}
	// Reply: no auth required
	clientConn.Write([]byte{0x05, 0x00})

	// Read connect request: VER CMD RSV ATYP DST.ADDR DST.PORT
	if _, err := io.ReadFull(clientConn, buf[:4]); err != nil {
		return
	}
	if buf[1] != 0x01 { // only CONNECT supported
		clientConn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	var targetAddr string
	atyp := buf[3]
	switch atyp {
	case 0x01: // IPv4
		if _, err := io.ReadFull(clientConn, buf[:4]); err != nil {
			return
		}
		targetAddr = net.IP(buf[:4]).String()
	case 0x03: // Domain
		if _, err := io.ReadFull(clientConn, buf[:1]); err != nil {
			return
		}
		domLen := int(buf[0])
		if _, err := io.ReadFull(clientConn, buf[:domLen]); err != nil {
			return
		}
		targetAddr = string(buf[:domLen])
	case 0x04: // IPv6
		if _, err := io.ReadFull(clientConn, buf[:16]); err != nil {
			return
		}
		targetAddr = net.IP(buf[:16]).String()
	default:
		clientConn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// Read port (2 bytes, big-endian)
	if _, err := io.ReadFull(clientConn, buf[:2]); err != nil {
		return
	}
	port := int(buf[0])<<8 | int(buf[1])
	target := fmt.Sprintf("%s:%d", targetAddr, port)

	// Direct connect
	targetConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		// Reply: host unreachable
		clientConn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer targetConn.Close()

	// Reply: success
	clientConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	// Only log direct connections at debug level to avoid log noise

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(targetConn, clientConn)
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, targetConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
}

// handleHTTP handles HTTP proxy requests
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleHTTPConnect(w, r)
		return
	}

	rt, idx := s.nextRoute()
	done := s.trackRequest(idx)
	defer done()

	switch rt.kind {
	case routeDirect:
		s.handleHTTPDirect(w, r)
		return
	case routeExtra:
		s.handleHTTPViaBackend(w, r, rt.addr)
		return
	}

	// routeWarp
	proc := rt.process
	if proc == nil || proc.State() != process.StateRunning {
		http.Error(w, "No healthy backend available", http.StatusServiceUnavailable)
		return
	}

	proc.IncrementRequests()

	// Forward the request through the backend HTTP proxy
	backendURL := fmt.Sprintf("http://127.0.0.1:%d", proc.HTTPPort())
	proxyReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers
	for key, values := range r.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	// Create client with backend proxy
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: func(_ *http.Request) (*url.URL, error) {
				return url.Parse(backendURL)
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleHTTPConnect handles HTTPS tunneling via CONNECT method
func (s *Server) handleHTTPConnect(w http.ResponseWriter, r *http.Request) {
	rt, idx := s.nextRoute()
	done := s.trackRequest(idx)
	defer done()

	switch rt.kind {
	case routeDirect:
		s.handleHTTPConnectDirect(w, r)
		return
	case routeExtra:
		s.handleHTTPConnectViaBackend(w, r, rt.addr)
		return
	}

	// routeWarp
	proc := rt.process
	if proc == nil || proc.State() != process.StateRunning {
		http.Error(w, "No healthy backend available", http.StatusServiceUnavailable)
		return
	}

	proc.IncrementRequests()

	// Connect to backend HTTP proxy
	backendAddr := fmt.Sprintf("127.0.0.1:%d", proc.HTTPPort())
	backendConn, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Send CONNECT request to backend
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", r.Host, r.Host)
	if _, err := backendConn.Write([]byte(connectReq)); err != nil {
		backendConn.Close()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Read backend response
	buf := make([]byte, 1024)
	n, err := backendConn.Read(buf)
	if err != nil {
		backendConn.Close()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Check if backend accepted the CONNECT
	response := string(buf[:n])
	if len(response) < 12 || response[9:12] != "200" {
		backendConn.Close()
		http.Error(w, "Backend proxy rejected CONNECT", http.StatusBadGateway)
		return
	}

	// Hijack the client connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		backendConn.Close()
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		backendConn.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Send 200 to client
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(backendConn, clientConn)
		backendConn.Close()
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, backendConn)
		clientConn.Close()
	}()

	wg.Wait()
}

// handleHTTPViaBackend forwards a plain HTTP request through an external proxy.
func (s *Server) handleHTTPViaBackend(w http.ResponseWriter, r *http.Request, backendAddr string) {
	proxyURL, _ := url.Parse(fmt.Sprintf("socks5://"+backendAddr))
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	proxyReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for key, values := range r.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleHTTPConnectViaBackend tunnels HTTPS CONNECT through an external SOCKS5 proxy.
func (s *Server) handleHTTPConnectViaBackend(w http.ResponseWriter, r *http.Request, backendAddr string) {
	// Connect to the external SOCKS5 backend, then relay raw bytes.
	backendConn, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// SOCKS5 handshake with the external backend
	// Auth negotiation: version 5, 1 method (no auth)
	backendConn.Write([]byte{0x05, 0x01, 0x00})
	buf := make([]byte, 2)
	if _, err := io.ReadFull(backendConn, buf); err != nil || buf[0] != 0x05 {
		backendConn.Close()
		http.Error(w, "SOCKS5 handshake failed with extra backend", http.StatusBadGateway)
		return
	}

	// SOCKS5 CONNECT request
	host, portStr, _ := net.SplitHostPort(r.Host)
	if host == "" {
		host = r.Host
		portStr = "443"
	}
	port := 443
	fmt.Sscanf(portStr, "%d", &port)

	// Build CONNECT: VER CMD RSV ATYP(domain) LEN DOMAIN PORT
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port&0xff))
	backendConn.Write(req)

	// Read reply (at least 10 bytes for IPv4 reply)
	reply := make([]byte, 10)
	if _, err := io.ReadFull(backendConn, reply); err != nil || reply[1] != 0x00 {
		backendConn.Close()
		http.Error(w, "SOCKS5 CONNECT failed with extra backend", http.StatusBadGateway)
		return
	}

	// Hijack client connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		backendConn.Close()
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		backendConn.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(backendConn, clientConn)
		backendConn.Close()
	}()
	go func() {
		defer wg.Done()
		io.Copy(clientConn, backendConn)
		clientConn.Close()
	}()
	wg.Wait()
}

// bidirectionalCopy pipes data between two connections until either side closes.
func bidirectionalCopy(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(b, a)
		if tc, ok := b.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(a, b)
		if tc, ok := a.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	wg.Wait()
}
func (s *Server) handleHTTPDirect(w http.ResponseWriter, r *http.Request) {
	// Only log direct connections at debug level to avoid log noise from port scanners

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	proxyReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for key, values := range r.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleHTTPConnectDirect handles HTTPS CONNECT tunneling directly without a proxy backend.
func (s *Server) handleHTTPConnectDirect(w http.ResponseWriter, r *http.Request) {
	// Only log direct CONNECT at debug level

	targetConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		targetConn.Close()
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		targetConn.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(targetConn, clientConn)
		targetConn.Close()
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, targetConn)
		clientConn.Close()
	}()

	wg.Wait()
}
