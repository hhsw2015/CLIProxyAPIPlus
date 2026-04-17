package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"warp-pool/api"
	"warp-pool/config"
	"warp-pool/ech"
	"warp-pool/health"
	"warp-pool/license"
	"warp-pool/pool"
	"warp-pool/proxy"
)

var ensureUniqueIPv4Func = ensureUniqueIPv4

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	applyLicense := flag.Bool("license", false, "Apply WARP+ license keys on startup")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("Starting warp-pool with %d instances", cfg.PoolSize)

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize pool
	p := pool.New(cfg)

	// Initialize license manager
	licMgr := license.New(cfg.LicenseKeyURL)

	// Optionally fetch and apply licenses before starting
	if *applyLicense && cfg.LicenseKeyURL != "" {
		log.Println("Fetching WARP+ license keys...")
		if err := licMgr.FetchKeys(ctx); err != nil {
			log.Printf("Warning: Failed to fetch license keys: %v", err)
		} else {
			log.Println("Applying license keys to pool (before start)...")
			// Need to initialize data dirs first
			for _, proc := range p.All() {
				if err := os.MkdirAll(proc.DataDir(), 0755); err != nil {
					log.Printf("Warning: Failed to create data dir for process %d: %v", proc.ID(), err)
				}
			}
			if err := licMgr.ApplyToPool(ctx, p); err != nil {
				log.Printf("Warning: Some licenses failed to apply: %v", err)
			}
		}
	}

	// Initialize health checker
	checker := health.New(p, time.Duration(cfg.HealthCheckInterval)*time.Second)

	// Start managed ech-workers instances
	echMgr := ech.New(cfg.ECHWorkers)
	echBackends := echMgr.Start(ctx)

	// Initialize proxy server (only if ports are configured)
	var proxyServer *proxy.Server
	if cfg.Proxy.SocksPort > 0 || cfg.Proxy.HTTPPort > 0 {
		var extras []proxy.ExtraBackend
		for _, eb := range cfg.Proxy.ExtraBackends {
			extras = append(extras, proxy.ExtraBackend{Name: eb.Name, Addr: eb.Addr})
		}
		// Append ech-workers managed backends
		extras = append(extras, echBackends...)
		weights := proxy.RouteWeights{
			ECH:    cfg.Proxy.WeightECH,
			Warp:   cfg.Proxy.WeightWarp,
			Direct: cfg.Proxy.WeightDirect,
		}
		proxyServer = proxy.NewWithOptions(p, cfg.Proxy.SocksPort, cfg.Proxy.HTTPPort, cfg.Proxy.IncludeDirect, extras, weights)
	}

	// Initialize API server
	apiServer := api.New(cfg, p, checker, licMgr)
	if proxyServer != nil {
		apiServer.SetProxyStats(func() interface{} {
			return proxyServer.Stats()
		})
	}

	// Start pool
	if err := p.Start(ctx); err != nil {
		log.Printf("Warning: Some processes failed to start: %v", err)
	}

	// Wait for at least one healthy WARP process (skip if pool_size=0, ECH-only mode)
	if cfg.PoolSize > 0 {
		log.Println("Waiting for processes to become healthy...")
		if err := p.WaitForHealthy(60 * time.Second); err != nil {
			log.Printf("Warning: %v", err)
		}

		// Start health checker (needed for IP detection)
		checker.Start(ctx)

		// Wait for initial health check
		log.Println("Waiting for IP detection...")
		time.Sleep(5 * time.Second)
	} else {
		log.Println("ECH-only mode (pool_size=0), skipping WARP health check")
		checker.Start(ctx)
	}

	// Start auto rotation (if enabled, disabled by default for stable pool)
	if cfg.Rotation.Enabled {
		p.StartAutoRotation()
		log.Printf("Auto rotation enabled: threshold=%d requests, min_interval=%ds",
			cfg.Rotation.RequestsThreshold, cfg.Rotation.MinInterval)
	}

	// Start proxy server
	if proxyServer != nil {
		if err := proxyServer.Start(ctx); err != nil {
			log.Fatalf("Failed to start proxy server: %v", err)
		}
	}

	// Start API server
	if err := apiServer.Start(ctx); err != nil {
		log.Fatalf("Failed to start API server: %v", err)
	}

	// Print final status
	printStatus(p, cfg, proxyServer)

	// Ensure unique IPv4 addresses in the background so proxy/API startup
	// is not blocked when upstream rotation takes a long time.
	startUniqueIPv4Async(ctx, p, checker, cfg)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")

	// Stop all components
	apiServer.Stop()
	if proxyServer != nil {
		proxyServer.Stop()
	}
	checker.Stop()
	echMgr.Stop()
	p.Stop()

	log.Println("Goodbye!")
}

func startUniqueIPv4Async(ctx context.Context, p *pool.Pool, checker *health.Checker, cfg *config.Config) {
	if cfg == nil || !cfg.UniqueIPv4.Enabled {
		return
	}
	go func() {
		for {
			log.Println("Ensuring unique IPv4 addresses...")
			if err := ensureUniqueIPv4Func(ctx, p, checker, cfg); err != nil {
				log.Printf("Warning: %v", err)
			}
			// Monitor: re-check every 60s in case IPs drift
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Second):
			}
			// Check if IPs are still unique
			ipv4Map := make(map[string]bool)
			for _, proc := range p.All() {
				ip := proc.Info().IP
				if ip != "" && !isIPv6(ip) {
					ipv4Map[ip] = true
				}
			}
			if len(ipv4Map) < cfg.PoolSize {
				log.Printf("[unique-ipv4] IP drift detected: only %d unique IPs, re-running dedup", len(ipv4Map))
			} else {
				// Still good, wait longer before next check
				select {
				case <-ctx.Done():
					return
				case <-time.After(4 * time.Minute):
				}
			}
		}
	}()
}

// ensureUniqueIPv4 ensures all processes have unique IPv4 addresses
func ensureUniqueIPv4(ctx context.Context, p *pool.Pool, checker *health.Checker, cfg *config.Config) error {
	maxRetries := cfg.UniqueIPv4.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 10
	}

	poolSize := cfg.PoolSize
	log.Printf("[unique-ipv4] Target: %d unique IPv4 addresses", poolSize)

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Collect current IPv4s
		ipv4Map := make(map[string][]int) // IP -> process IDs
		for _, proc := range p.All() {
			ip := proc.Info().IP
			if ip != "" && !isIPv6(ip) {
				ipv4Map[ip] = append(ipv4Map[ip], proc.ID())
			}
		}

		uniqueCount := len(ipv4Map)
		log.Printf("[unique-ipv4] Attempt %d: Found %d unique IPv4s", attempt, uniqueCount)

		if uniqueCount >= poolSize {
			log.Printf("[unique-ipv4] Success! Got %d unique IPv4 addresses", uniqueCount)
			for ip, ids := range ipv4Map {
				log.Printf("[unique-ipv4]   %s -> instance %v", ip, ids)
			}
			return nil
		}

		// Find processes with duplicate IPs to restart
		var toRestart []int
		for ip, ids := range ipv4Map {
			if len(ids) > 1 {
				// Keep the first, restart the rest
				log.Printf("[unique-ipv4] IP %s is used by instances %v, will restart %v", ip, ids, ids[1:])
				toRestart = append(toRestart, ids[1:]...)
			}
		}

		if len(toRestart) == 0 {
			// No duplicates but not enough unique IPs
			// This shouldn't happen, but just in case
			log.Printf("[unique-ipv4] No duplicates but only %d unique IPs", uniqueCount)
			break
		}

		// Restart one process at a time
		for _, id := range toRestart {
			proc := p.Get(id)
			if proc == nil {
				continue
			}

			log.Printf("[unique-ipv4] Restarting instance %d...", id)
			if err := proc.Restart(ctx); err != nil {
				log.Printf("[unique-ipv4] Failed to restart instance %d: %v", id, err)
				continue
			}

			// Wait for process to come up and get IP
			time.Sleep(8 * time.Second)

			// Force health check to update IP
			checker.CheckOne(proc)

			newIP := proc.Info().IP
			log.Printf("[unique-ipv4] Instance %d now has IP: %s", id, newIP)

			// Check if we now have enough unique IPs
			ipv4Map := make(map[string]bool)
			for _, proc := range p.All() {
				ip := proc.Info().IP
				if ip != "" && !isIPv6(ip) {
					ipv4Map[ip] = true
				}
			}
			if len(ipv4Map) >= poolSize {
				log.Printf("[unique-ipv4] Success! Got %d unique IPv4 addresses", len(ipv4Map))
				return nil
			}
		}
	}

	// Final count
	ipv4Map := make(map[string]bool)
	for _, proc := range p.All() {
		ip := proc.Info().IP
		if ip != "" && !isIPv6(ip) {
			ipv4Map[ip] = true
		}
	}
	return fmt.Errorf("could only get %d unique IPv4s (target: %d)", len(ipv4Map), poolSize)
}

func isIPv6(ip string) bool {
	for i := 0; i < len(ip); i++ {
		if ip[i] == ':' {
			return true
		}
	}
	return false
}

func printStatus(p *pool.Pool, cfg *config.Config, proxyServer *proxy.Server) {
	stats := p.Stats()
	log.Printf("Pool ready: %d/%d processes running", stats.Running, stats.Total)

	// Print unique IPs
	ipSet := make(map[string]bool)
	for _, proc := range stats.Processes {
		if proc.IP != "" {
			ipSet[proc.IP] = true
		}
	}
	log.Printf("Unique IPs: %d", len(ipSet))
	for ip := range ipSet {
		log.Printf("  - %s", ip)
	}

	if proxyServer != nil {
		log.Printf("Unified SOCKS5 proxy: :%d", cfg.Proxy.SocksPort)
		log.Printf("Unified HTTP proxy: :%d", cfg.Proxy.HTTPPort)
	}

	if cfg.Direct.Enabled {
		log.Printf("Direct mode enabled:")
		for _, proc := range p.All() {
			info := proc.Info()
			addr := "127.0.0.1"
			if cfg.Direct.ExposeExternal {
				addr = "0.0.0.0"
			}
			fmt.Printf("  Instance %d: socks5://%s:%d  http://%s:%d  IP=%s\n",
				proc.ID(), addr, proc.SocksPort(), addr, proc.HTTPPort(), info.IP)
		}
	}

	log.Printf("Management API: :%d", cfg.API.Port)
}
