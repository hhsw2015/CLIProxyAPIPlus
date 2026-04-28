package tlsfingerprint

import "strings"

// Built-in TLS fingerprint profiles

func ChromeProfile() Profile {
	return Profile{
		// Chrome 136 / macOS
		CipherSuites: []string{
			"TLS_AES_128_GCM_SHA256",
			"TLS_AES_256_GCM_SHA384",
			"TLS_CHACHA20_POLY1305_SHA256",
			"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
			"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
			"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
			"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
		},
		Curves:       []string{"X25519", "P-256", "P-384"},
		PointFormats: []string{"uncompressed"},
		SignatureAlgorithms: []string{
			"ecdsa_secp256r1_sha256", "rsa_pss_rsae_sha256", "rsa_pkcs1_sha256",
			"ecdsa_secp384r1_sha384", "rsa_pss_rsae_sha384", "rsa_pkcs1_sha384",
			"rsa_pss_rsae_sha512", "rsa_pkcs1_sha512",
		},
		ALPNProtocols:     []string{"h2", "http/1.1"},
		SupportedVersions: []string{"TLS 1.3", "TLS 1.2"},
		KeyShareGroups:    []string{"X25519"},
		PSKModes:          []string{"psk_dhe_ke"},
	}
}

func NodeJSProfile() Profile {
	return Profile{
		// Node.js 24.x default
		CipherSuites: []string{
			"TLS_AES_256_GCM_SHA384",
			"TLS_CHACHA20_POLY1305_SHA256",
			"TLS_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
			"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
			"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
			"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
			"TLS_RSA_WITH_AES_128_GCM_SHA256",
			"TLS_RSA_WITH_AES_256_GCM_SHA384",
		},
		Curves:       []string{"X25519", "P-256", "P-384", "P-521"},
		PointFormats: []string{"uncompressed"},
		SignatureAlgorithms: []string{
			"ecdsa_secp256r1_sha256", "ecdsa_secp384r1_sha384", "ecdsa_secp521r1_sha512",
			"rsa_pss_rsae_sha256", "rsa_pss_rsae_sha384", "rsa_pss_rsae_sha512",
			"rsa_pkcs1_sha256", "rsa_pkcs1_sha384", "rsa_pkcs1_sha512",
		},
		ALPNProtocols:     []string{"h2", "http/1.1"},
		SupportedVersions: []string{"TLS 1.3", "TLS 1.2"},
		KeyShareGroups:    []string{"X25519"},
		PSKModes:          []string{"psk_dhe_ke"},
	}
}

func FirefoxProfile() Profile {
	return Profile{
		// Firefox 137
		CipherSuites: []string{
			"TLS_AES_128_GCM_SHA256",
			"TLS_CHACHA20_POLY1305_SHA256",
			"TLS_AES_256_GCM_SHA384",
			"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
			"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
			"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
			"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
		},
		Curves:       []string{"X25519", "P-256", "P-384"},
		PointFormats: []string{"uncompressed"},
		SignatureAlgorithms: []string{
			"ecdsa_secp256r1_sha256", "ecdsa_secp384r1_sha384", "ecdsa_secp521r1_sha512",
			"rsa_pss_rsae_sha256", "rsa_pss_rsae_sha384", "rsa_pss_rsae_sha512",
			"rsa_pkcs1_sha256", "rsa_pkcs1_sha384", "rsa_pkcs1_sha512",
		},
		ALPNProtocols:     []string{"h2", "http/1.1"},
		SupportedVersions: []string{"TLS 1.3", "TLS 1.2"},
		KeyShareGroups:    []string{"X25519", "P-256"},
		PSKModes:          []string{"psk_dhe_ke"},
	}
}

// ProfileByName returns a built-in profile by name. Returns nil if unknown.
func ProfileByName(name string) *Profile {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "chrome":
		p := ChromeProfile()
		return &p
	case "nodejs", "node":
		p := NodeJSProfile()
		return &p
	case "firefox":
		p := FirefoxProfile()
		return &p
	default:
		return nil
	}
}
