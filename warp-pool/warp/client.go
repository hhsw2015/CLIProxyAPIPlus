package warp

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Status represents WARP connection status
type Status int

const (
	StatusDisconnected Status = iota
	StatusConnecting
	StatusConnected
)

func (s Status) String() string {
	switch s {
	case StatusDisconnected:
		return "disconnected"
	case StatusConnecting:
		return "connecting"
	case StatusConnected:
		return "connected"
	default:
		return "unknown"
	}
}

// Info contains WARP runtime information
type Info struct {
	Status    Status    `json:"status"`
	StatusStr string    `json:"status_str"`
	IP        string    `json:"ip,omitempty"`
	WarpMode  string    `json:"warp_mode,omitempty"`
	ProxyPort int       `json:"proxy_port"`
	LastCheck time.Time `json:"last_check,omitempty"`
	Rotations int64     `json:"rotations"`
}

// Client manages the WARP CLI
type Client struct {
	mu sync.RWMutex

	proxyPort int
	status    Status
	ip        string
	warpMode  string
	lastCheck time.Time
	rotations int64
}

// New creates a new WARP client
func New(proxyPort int) *Client {
	return &Client{
		proxyPort: proxyPort,
		status:    StatusDisconnected,
	}
}

// ProxyPort returns the WARP proxy port
func (c *Client) ProxyPort() int {
	return c.proxyPort
}

// Info returns current WARP information
func (c *Client) Info() Info {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return Info{
		Status:    c.status,
		StatusStr: c.status.String(),
		IP:        c.ip,
		WarpMode:  c.warpMode,
		ProxyPort: c.proxyPort,
		LastCheck: c.lastCheck,
		Rotations: c.rotations,
	}
}

// Connect connects to WARP
func (c *Client) Connect(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "warp-cli", "--accept-tos", "connect")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("connect failed: %w: %s", err, string(output))
	}

	c.mu.Lock()
	c.status = StatusConnected
	c.mu.Unlock()

	// Update status after connection
	return c.RefreshStatus(ctx)
}

// Disconnect disconnects from WARP
func (c *Client) Disconnect(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "warp-cli", "--accept-tos", "disconnect")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("disconnect failed: %w: %s", err, string(output))
	}

	c.mu.Lock()
	c.status = StatusDisconnected
	c.ip = ""
	c.mu.Unlock()

	return nil
}

// RotateIP rotates the IP by disconnecting and reconnecting
func (c *Client) RotateIP(ctx context.Context) error {
	// Disconnect
	if err := c.Disconnect(ctx); err != nil {
		return fmt.Errorf("disconnect for rotation failed: %w", err)
	}

	// Wait a moment
	time.Sleep(500 * time.Millisecond)

	// Reconnect
	if err := c.Connect(ctx); err != nil {
		return fmt.Errorf("reconnect for rotation failed: %w", err)
	}

	c.mu.Lock()
	c.rotations++
	c.mu.Unlock()

	return nil
}

// RefreshStatus updates the current status from warp-cli
func (c *Client) RefreshStatus(ctx context.Context) error {
	// Get status
	cmd := exec.CommandContext(ctx, "warp-cli", "--accept-tos", "status")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("status check failed: %w", err)
	}

	statusStr := strings.ToLower(string(output))

	c.mu.Lock()
	defer c.mu.Unlock()

	if strings.Contains(statusStr, "connected") {
		c.status = StatusConnected
	} else if strings.Contains(statusStr, "connecting") {
		c.status = StatusConnecting
	} else {
		c.status = StatusDisconnected
	}

	c.lastCheck = time.Now()
	return nil
}

// CheckIP checks the current exit IP through the WARP proxy
func (c *Client) CheckIP(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "curl", "-s", "--max-time", "10",
		"-x", fmt.Sprintf("socks5://127.0.0.1:%d", c.proxyPort),
		"https://cloudflare.com/cdn-cgi/trace")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("IP check failed: %w", err)
	}

	// Parse response
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	var ip, warpMode string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "ip=") {
			ip = strings.TrimPrefix(line, "ip=")
		}
		if strings.HasPrefix(line, "warp=") {
			warpMode = strings.TrimPrefix(line, "warp=")
		}
	}

	if ip == "" {
		return "", fmt.Errorf("failed to parse IP from response")
	}

	c.mu.Lock()
	c.ip = ip
	c.warpMode = warpMode
	c.lastCheck = time.Now()
	c.mu.Unlock()

	return ip, nil
}

// IsConnected returns whether WARP is connected
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status == StatusConnected
}

// SetProxyPort sets the WARP proxy port
func (c *Client) SetProxyPort(ctx context.Context, port int) error {
	cmd := exec.CommandContext(ctx, "warp-cli", "--accept-tos", "proxy", "port", fmt.Sprintf("%d", port))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("set proxy port failed: %w: %s", err, string(output))
	}

	c.mu.Lock()
	c.proxyPort = port
	c.mu.Unlock()

	return nil
}

// EnsureConnected ensures WARP is connected
func (c *Client) EnsureConnected(ctx context.Context) error {
	if err := c.RefreshStatus(ctx); err != nil {
		return err
	}

	if !c.IsConnected() {
		return c.Connect(ctx)
	}

	return nil
}
