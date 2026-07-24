package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// DefaultCodexLiveMediaMaxSessions is the default in-process media session limit.
const DefaultCodexLiveMediaMaxSessions = 32

// EffectiveMaxSessions returns the configured media session limit.
func (c CodexLiveMediaRelayConfig) EffectiveMaxSessions() int {
	if c.MaxSessions > 0 {
		return c.MaxSessions
	}
	return DefaultCodexLiveMediaMaxSessions
}

// Validate verifies the Codex Live media relay configuration.
func (c CodexLiveMediaRelayConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.MaxSessions < 0 {
		return errors.New("codex.live-media-relay.max-sessions must not be negative")
	}
	if publicIP := strings.TrimSpace(c.PublicIP); publicIP != "" && net.ParseIP(publicIP) == nil {
		return fmt.Errorf("codex.live-media-relay.public-ip is invalid: %q", publicIP)
	}
	if (c.UDPPortMin == 0) != (c.UDPPortMax == 0) {
		return errors.New("codex.live-media-relay UDP port minimum and maximum must both be set")
	}
	if c.UDPPortMin > c.UDPPortMax {
		return errors.New("codex.live-media-relay.udp-port-min must not exceed udp-port-max")
	}
	if c.UDPPortMin != 0 {
		availablePorts := int(c.UDPPortMax) - int(c.UDPPortMin) + 1
		requiredPorts := c.EffectiveMaxSessions() * 2
		if availablePorts < requiredPorts {
			return fmt.Errorf("codex.live-media-relay UDP range requires at least %d ports for %d sessions", requiredPorts, c.EffectiveMaxSessions())
		}
	}
	for serverIndex, server := range c.ICEServers {
		if len(server.URLs) == 0 {
			return fmt.Errorf("codex.live-media-relay.ice-servers[%d].urls is required", serverIndex)
		}
		for _, rawURL := range server.URLs {
			parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
			if errParse != nil || parsed.Scheme == "" {
				return fmt.Errorf("codex.live-media-relay.ice-servers[%d] contains an invalid URL", serverIndex)
			}
			switch strings.ToLower(parsed.Scheme) {
			case "stun", "stuns", "turn", "turns":
			default:
				return fmt.Errorf("codex.live-media-relay.ice-servers[%d] uses unsupported scheme %q", serverIndex, parsed.Scheme)
			}
		}
	}
	return nil
}
