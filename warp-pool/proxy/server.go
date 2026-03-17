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
	"time"

	"warp-pool/pool"
	"warp-pool/process"
)

// Server provides unified SOCKS5 and HTTP proxy entry points
type Server struct {
	pool       *pool.Pool
	socksPort  int
	httpPort   int
	socksLn    net.Listener
	httpServer *http.Server
	wg         sync.WaitGroup
}

// New creates a new proxy server
func New(p *pool.Pool, socksPort, httpPort int) *Server {
	return &Server{
		pool:      p,
		socksPort: socksPort,
		httpPort:  httpPort,
	}
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

	// Get next healthy process
	proc := s.pool.Next()
	if proc == nil || proc.State() != process.StateRunning {
		log.Printf("[proxy] No healthy backend available")
		return
	}

	// Connect to backend SOCKS proxy
	backendAddr := fmt.Sprintf("127.0.0.1:%d", proc.SocksPort())
	backendConn, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		log.Printf("[proxy] Failed to connect to backend %d: %v", proc.ID(), err)
		return
	}
	defer backendConn.Close()

	proc.IncrementRequests()

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(backendConn, clientConn)
		backendConn.(*net.TCPConn).CloseWrite()
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, backendConn)
		clientConn.(*net.TCPConn).CloseWrite()
	}()

	wg.Wait()
}

// handleHTTP handles HTTP proxy requests
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleHTTPConnect(w, r)
		return
	}

	// Get next healthy process
	proc := s.pool.Next()
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
	// Get next healthy process
	proc := s.pool.Next()
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
