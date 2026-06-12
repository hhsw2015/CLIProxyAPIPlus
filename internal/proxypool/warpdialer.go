package proxypool

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	usqueapi "github.com/hhsw2015/usque/v3/api"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// WARPDialer dials TCP connections through an in-process Cloudflare WARP
// MASQUE tunnel (one per registered usque account). It exposes the same
// Dial(addr) interface as ECHDialer so the proxy pool can mix them freely.
//
// Construction: NewWARPDialer parses the embedded config, brings up a
// netstack TUN device, and spawns api.MaintainTunnel in a goroutine. After
// Connect() returns, the dialer is ready and the QUIC tunnel reconnects
// automatically on drop.
type WARPDialer struct {
	name    string
	tunNet  *netstack.Net
	healthy atomic.Bool
	cancel  context.CancelFunc
}

// WARPInstance is the YAML-friendly subset of usque's config.json used by CPA.
// Only the four required fields (private-key, endpoint-pub-key, endpoint-v4,
// ipv4) must be set; ipv6 enables dual-stack inside the tunnel.
type WARPInstance = config.WARPInstance

// NewWARPDialer constructs a dialer from a single instance config and starts
// its MASQUE tunnel maintenance loop. Returns an error only on misconfigured
// fields (bad PEM / address) — transient connection failures are handled
// inside MaintainTunnel.
func NewWARPDialer(parent context.Context, inst WARPInstance) (*WARPDialer, error) {
	if inst.Name == "" {
		inst.Name = "warp"
	}

	privKey, err := decodeECDSAPriv(inst.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("warp %s: private-key: %w", inst.Name, err)
	}
	peerPubKey, err := decodeECDSAPub(inst.EndpointPubKey)
	if err != nil {
		return nil, fmt.Errorf("warp %s: endpoint-pub-key: %w", inst.Name, err)
	}

	endpointIP := net.ParseIP(inst.EndpointV4)
	if endpointIP == nil {
		return nil, fmt.Errorf("warp %s: invalid endpoint-v4 %q", inst.Name, inst.EndpointV4)
	}
	endpoint := &net.UDPAddr{IP: endpointIP, Port: 443}

	ipv4Addr, err := netip.ParseAddr(inst.IPv4)
	if err != nil {
		return nil, fmt.Errorf("warp %s: invalid ipv4 %q: %w", inst.Name, inst.IPv4, err)
	}
	localAddrs := []netip.Addr{ipv4Addr}
	if inst.IPv6 != "" {
		if v6, err := netip.ParseAddr(inst.IPv6); err == nil {
			localAddrs = append(localAddrs, v6)
		}
	}

	cert, err := usqueapi.GenerateCert(privKey, &privKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("warp %s: generate cert: %w", inst.Name, err)
	}
	tlsCfg, err := usqueapi.PrepareTlsConfig(privKey, peerPubKey, cert, usqueapi.ConnectSNI, false)
	if err != nil {
		return nil, fmt.Errorf("warp %s: tls config: %w", inst.Name, err)
	}

	const mtu = 1280
	dnsAddrs := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("1.0.0.1")}
	tunDev, tunNet, err := netstack.CreateNetTUN(localAddrs, dnsAddrs, mtu)
	if err != nil {
		return nil, fmt.Errorf("warp %s: create netstack tun: %w", inst.Name, err)
	}

	ctx, cancel := context.WithCancel(parent)
	d := &WARPDialer{
		name:   inst.Name,
		tunNet: tunNet,
		cancel: cancel,
	}
	d.healthy.Store(true)

	go usqueapi.MaintainTunnel(ctx, usqueapi.MaintainTunnelConfig{
		TLSConfig:         tlsCfg,
		KeepalivePeriod:   30 * time.Second,
		InitialPacketSize: 1242,
		Endpoint:          endpoint,
		Device:            usqueapi.NewNetstackAdapter(tunDev),
		MTU:               mtu,
		ReconnectDelay:    1 * time.Second,
		AlwaysReconnect:   true,
		UseHTTP2:          false,
	})

	return d, nil
}

// Dial implements the same contract as ECHDialer.Dial — return a net.Conn
// for the upstream address routed through this WARP tunnel.
func (d *WARPDialer) Dial(addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), "tcp", addr)
}

// DialContext is the http.Transport-friendly form. It feeds straight into
// the netstack net which handles SYN/ACK over the MASQUE tunnel.
func (d *WARPDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.tunNet.DialContext(ctx, network, addr)
}

// Close cancels the MaintainTunnel goroutine. The netstack TUN closes when
// MaintainTunnel returns.
func (d *WARPDialer) Close() {
	if d.cancel != nil {
		d.cancel()
	}
}

func decodeECDSAPriv(b64 string) (*ecdsa.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	key, err := x509.ParseECPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse ec private: %w", err)
	}
	return key, nil
}

func decodeECDSAPub(pemStr string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("pem decode failed")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse pkix: %w", err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not ecdsa public key")
	}
	return ec, nil
}
