package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	kiroclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/kiro/claude"
	log "github.com/sirupsen/logrus"
)

// providerPools holds one rotating key pool per provider id (tinyfish,
// anysearch, ...). Switching the primary in config is a one-line change:
// rename `provider:`, the pool catalog stays put.
var providerPools map[string]*wsKeyPool

func InitWebSearchPool(cfg *config.WebSearchConfig) {
	providerPools = nil
	if cfg == nil || !cfg.Enabled {
		return
	}
	providerPools = make(map[string]*wsKeyPool)

	// Catalog form: providers: { tinyfish: {api-keys:[...]}, ... }
	for name, p := range cfg.Providers {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || len(p.APIKeys) == 0 {
			continue
		}
		providerPools[name] = newWSKeyPool(p.APIKeys)
	}

	// Legacy form: top-level api-keys feeds the primary provider when the
	// catalog has no entry for it.
	primary := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if primary != "" && len(cfg.APIKeys) > 0 {
		if _, ok := providerPools[primary]; !ok {
			providerPools[primary] = newWSKeyPool(cfg.APIKeys)
		}
	}
}

// sanitizeWebSearchQuery clips a search query to a length the upstream
// providers will accept and strips Claude Code's noisy "Proactive Context
// Expansion" appendage.
//
// TinyFish rejects queries > 2000 chars. Claude Code sometimes glues
// compressed conversation history onto the query as a context-expansion
// block, which inflates the field by 5-20 KB and makes the search useless
// even when accepted. Truncate at the first marker, then bound length.
func sanitizeWebSearchQuery(query string) string {
	const maxQueryLen = 1900
	if i := strings.Index(query, "[Proactive Context Expansion"); i >= 0 {
		query = query[:i]
	}
	if i := strings.Index(query, "\n\n"); i >= 0 && len(query) > maxQueryLen {
		// Prefer cutting at a paragraph break before falling back to hard cut.
		if i < maxQueryLen {
			query = query[:i]
		}
	}
	query = strings.TrimSpace(query)
	if len(query) > maxQueryLen {
		query = query[:maxQueryLen]
	}
	return query
}

func dispatchWebSearch(ctx context.Context, cfg *config.Config, query string) (*kiroclaude.WebSearchResults, error) {
	if cfg == nil || !cfg.WebSearch.Enabled {
		return nil, fmt.Errorf("web search disabled")
	}
	query = sanitizeWebSearchQuery(query)
	if query == "" {
		return nil, fmt.Errorf("empty query after sanitize")
	}

	primary := strings.ToLower(strings.TrimSpace(cfg.WebSearch.Provider))
	if primary == "" {
		primary = "tinyfish"
	}

	results, err := callWebSearchProvider(ctx, cfg, primary, providerPools[primary], query)
	if err == nil {
		return results, nil
	}
	primaryErr := err

	for _, fbName := range cfg.WebSearch.Fallbacks {
		fbProvider := strings.ToLower(strings.TrimSpace(fbName))
		if fbProvider == "" || fbProvider == primary {
			continue
		}
		log.Warnf("web-search: %s failed (%v); trying fallback %s", primary, primaryErr, fbProvider)
		if results, err := callWebSearchProvider(ctx, cfg, fbProvider, providerPools[fbProvider], query); err == nil {
			return results, nil
		} else {
			log.Warnf("web-search: fallback %s also failed: %v", fbProvider, err)
		}
	}

	return nil, fmt.Errorf("all web search providers failed; primary=%s err=%w", primary, primaryErr)
}

// callWebSearchProvider runs a single provider call with its own timeout +
// HTTP client + key rotation. Each call gets a fresh 15s context so a
// fallback isn't penalised by time the primary already burnt.
func callWebSearchProvider(parent context.Context, cfg *config.Config, provider string, pool *wsKeyPool, query string) (*kiroclaude.WebSearchResults, error) {
	searchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = parent
	client := newProxyAwareHTTPClient(searchCtx, cfg, nil, 15*time.Second)

	switch provider {
	case "tinyfish":
		if pool == nil {
			return nil, fmt.Errorf("tinyfish: no api keys configured")
		}
		return runWithKeyRotation(searchCtx, client, pool, query, fetchTinyFishSearch)
	case "anysearch":
		// AnySearch allows anonymous access; a key only raises rate limits.
		return runWithKeyRotation(searchCtx, client, pool, query, func(c context.Context, hc *http.Client, key, q string) (*kiroclaude.WebSearchResults, error) {
			return fetchAnySearch(c, hc, key, q, 10)
		})
	case "bing-rss":
		return fetchBingRSSWebSearch(searchCtx, client, query)
	default:
		return nil, fmt.Errorf("unknown web search provider: %s", provider)
	}
}

// runWithKeyRotation tries one key, rotates on 429, allows nil pool for
// providers with anonymous fallback (anysearch).
func runWithKeyRotation(ctx context.Context, client *http.Client, pool *wsKeyPool, query string,
	fn func(context.Context, *http.Client, string, string) (*kiroclaude.WebSearchResults, error)) (*kiroclaude.WebSearchResults, error) {
	var key string
	if pool != nil {
		key = pool.Next()
	}
	results, err := fn(ctx, client, key, query)
	if err == nil {
		return results, nil
	}
	if pool != nil && key != "" && strings.Contains(err.Error(), "429") {
		pool.MarkRateLimited(key)
		if key2 := pool.Next(); key2 != "" && key2 != key {
			return fn(ctx, client, key2, query)
		}
	}
	return nil, err
}
