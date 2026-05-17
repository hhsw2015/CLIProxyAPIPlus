package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	kiroclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/kiro/claude"
)

var wsPool *wsKeyPool

func InitWebSearchPool(cfg *config.WebSearchConfig) {
	if cfg == nil || !cfg.Enabled || len(cfg.APIKeys) == 0 {
		wsPool = nil
		return
	}
	wsPool = newWSKeyPool(cfg.APIKeys)
}

func dispatchWebSearch(ctx context.Context, cfg *config.Config, query string) (*kiroclaude.WebSearchResults, error) {
	if cfg == nil || !cfg.WebSearch.Enabled {
		return nil, fmt.Errorf("web search disabled")
	}

	provider := strings.ToLower(strings.TrimSpace(cfg.WebSearch.Provider))
	if provider == "" {
		provider = "tinyfish"
	}

	// Use a dedicated context with generous timeout for the search API call.
	// The parent ctx may be close to expiry after conductor retries; we don't
	// want the search to be cancelled mid-flight.
	searchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := newProxyAwareHTTPClient(searchCtx, cfg, nil, 15*time.Second)

	switch provider {
	case "tinyfish":
		if wsPool == nil || len(cfg.WebSearch.APIKeys) == 0 {
			return nil, fmt.Errorf("tinyfish: no api keys configured")
		}
		key := wsPool.Next()
		results, err := fetchTinyFishSearch(searchCtx, client, key, query)
		if err != nil {
			if strings.Contains(err.Error(), "429") {
				wsPool.MarkRateLimited(key)
				key2 := wsPool.Next()
				if key2 != key {
					return fetchTinyFishSearch(searchCtx, client, key2, query)
				}
			}
			return nil, err
		}
		return results, nil
	case "bing-rss":
		return fetchBingRSSWebSearch(searchCtx, client, query)
	default:
		return nil, fmt.Errorf("unknown web search provider: %s", provider)
	}
}
