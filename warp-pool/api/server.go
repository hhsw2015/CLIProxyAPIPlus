package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"warp-pool/config"
	"warp-pool/health"
	"warp-pool/license"
	"warp-pool/pool"
)

// Server provides HTTP API for pool management
type Server struct {
	cfg     *config.Config
	pool    *pool.Pool
	checker *health.Checker
	licMgr  *license.Manager
	server  *http.Server
}

// New creates a new API server
func New(cfg *config.Config, p *pool.Pool, checker *health.Checker, licMgr *license.Manager) *Server {
	return &Server{
		cfg:     cfg,
		pool:    p,
		checker: checker,
		licMgr:  licMgr,
	}
}

// Start starts the API server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// Routes
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/processes", s.withAuth(s.handleProcesses))
	mux.HandleFunc("/api/process/", s.withAuth(s.handleProcess))
	mux.HandleFunc("/api/restart", s.withAuth(s.handleRestart))
	mux.HandleFunc("/api/restart/", s.withAuth(s.handleRestartOne))
	mux.HandleFunc("/api/health", s.withAuth(s.handleHealth))
	mux.HandleFunc("/api/rotate", s.withAuth(s.handleRotate))
	mux.HandleFunc("/api/direct", s.withAuth(s.handleDirect))
	mux.HandleFunc("/api/license/fetch", s.withAuth(s.handleLicenseFetch))
	mux.HandleFunc("/api/license/apply", s.withAuth(s.handleLicenseApply))

	s.server = &http.Server{
		Addr:    ":" + strconv.Itoa(s.cfg.API.Port),
		Handler: mux,
	}

	log.Printf("[api] Management API listening on :%d", s.cfg.API.Port)

	go func() {
		if err := s.server.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("[api] Server error: %v", err)
		}
	}()

	return nil
}

// Stop stops the API server
func (s *Server) Stop() error {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.server.Shutdown(ctx)
	}
	return nil
}

// withAuth wraps a handler with authentication check
func (s *Server) withAuth(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.API.Token != "" {
			token := r.Header.Get("Authorization")
			if token == "" {
				token = r.URL.Query().Get("token")
			}
			if token != "Bearer "+s.cfg.API.Token && token != s.cfg.API.Token {
				s.jsonError(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		handler(w, r)
	}
}

// handleIndex shows API info
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.jsonResponse(w, map[string]interface{}{
		"name":    "warp-pool",
		"version": "2.0.0",
		"endpoints": []string{
			"GET  /api/status        - Pool status and statistics",
			"GET  /api/processes     - List all processes",
			"GET  /api/process/:id   - Get process details",
			"GET  /api/direct        - Get direct connection endpoints",
			"POST /api/restart       - Restart all processes (rotates IPs)",
			"POST /api/restart/:id   - Restart specific process",
			"POST /api/rotate        - Rotate IP for a random process",
			"GET  /api/health        - Health check endpoint",
			"POST /api/license/fetch - Fetch WARP+ license keys",
			"POST /api/license/apply - Apply licenses to all processes",
		},
	})
}

// handleStatus returns pool status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.pool.Stats()
	s.jsonResponse(w, map[string]interface{}{
		"status":         "ok",
		"pool_size":      stats.Total,
		"running":        stats.Running,
		"starting":       stats.Starting,
		"error":          stats.Error,
		"stopped":        stats.Stopped,
		"total_requests": stats.TotalRequests,
		"socks_port":     s.cfg.Proxy.SocksPort,
		"http_port":      s.cfg.Proxy.HTTPPort,
	})
}

// handleProcesses returns all process info
func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.pool.Stats()
	s.jsonResponse(w, stats.Processes)
}

// handleProcess returns specific process info
func (s *Server) handleProcess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract ID from path
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		s.jsonError(w, "Invalid process ID", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(parts[3])
	if err != nil {
		s.jsonError(w, "Invalid process ID", http.StatusBadRequest)
		return
	}

	proc := s.pool.Get(id)
	if proc == nil {
		s.jsonError(w, "Process not found", http.StatusNotFound)
		return
	}

	info := proc.Info()

	// Optionally get IP info
	if r.URL.Query().Get("ipinfo") == "true" {
		ipInfo, err := s.checker.GetIPInfo(proc)
		if err == nil {
			s.jsonResponse(w, map[string]interface{}{
				"process": info,
				"ipinfo":  ipInfo,
			})
			return
		}
	}

	s.jsonResponse(w, info)
}

// handleRestart restarts all processes
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := s.pool.RestartAll(); err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"status":  "ok",
		"message": "All processes restarted",
	})
}

// handleRestartOne restarts a specific process
func (s *Server) handleRestartOne(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract ID from path
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		s.jsonError(w, "Invalid process ID", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(parts[3])
	if err != nil {
		s.jsonError(w, "Invalid process ID", http.StatusBadRequest)
		return
	}

	if err := s.pool.Restart(id); err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"status":  "ok",
		"message": "Process " + strconv.Itoa(id) + " restarted",
	})
}

// handleRotate rotates IP by restarting a random healthy process
func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get a healthy process and restart it
	proc := s.pool.Next()
	if proc == nil {
		s.jsonError(w, "No processes available", http.StatusServiceUnavailable)
		return
	}

	oldIP := proc.Info().IP
	if err := s.pool.Restart(proc.ID()); err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Wait for the process to become healthy again
	time.Sleep(3 * time.Second)
	s.checker.CheckOne(proc)

	newIP := proc.Info().IP

	s.jsonResponse(w, map[string]interface{}{
		"status":     "ok",
		"process_id": proc.ID(),
		"old_ip":     oldIP,
		"new_ip":     newIP,
	})
}

// handleHealth returns health status
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.pool.Stats()
	status := "healthy"
	httpStatus := http.StatusOK

	if stats.Running == 0 {
		status = "unhealthy"
		httpStatus = http.StatusServiceUnavailable
	} else if stats.Running < stats.Total/2 {
		status = "degraded"
	}

	w.WriteHeader(httpStatus)
	s.jsonResponse(w, map[string]interface{}{
		"status":  status,
		"running": stats.Running,
		"total":   stats.Total,
	})
}

// handleDirect returns direct connection endpoints
func (s *Server) handleDirect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.pool.Config()
	endpoints := s.pool.DirectEndpoints()

	s.jsonResponse(w, map[string]interface{}{
		"enabled":   cfg.Direct.Enabled,
		"external":  cfg.Direct.ExposeExternal,
		"endpoints": endpoints,
		"usage": map[string]string{
			"socks5": "socks5://HOST:SOCKS_PORT",
			"http":   "http://HOST:HTTP_PORT",
		},
	})
}

// handleLicenseFetch fetches WARP+ license keys
func (s *Server) handleLicenseFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.licMgr == nil {
		s.jsonError(w, "License manager not configured", http.StatusNotImplemented)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.licMgr.FetchKeys(ctx); err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"status":    "ok",
		"key_count": s.licMgr.KeyCount(),
	})
}

// handleLicenseApply applies licenses to all processes
func (s *Server) handleLicenseApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.licMgr == nil {
		s.jsonError(w, "License manager not configured", http.StatusNotImplemented)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	// Stop pool first
	s.pool.Stop()

	// Apply licenses
	if err := s.licMgr.ApplyToPool(ctx, s.pool); err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Restart pool
	if err := s.pool.Start(ctx); err != nil {
		s.jsonError(w, "Licenses applied but failed to restart pool: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"status":  "ok",
		"message": "Licenses applied and pool restarted",
	})
}

// jsonResponse sends a JSON response
func (s *Server) jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// jsonError sends a JSON error response
func (s *Server) jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": message,
		"code":  code,
	})
}
