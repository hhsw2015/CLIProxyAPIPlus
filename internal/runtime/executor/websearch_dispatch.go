package executor

import (
	"context"
	"fmt"
	"net/http"
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

	client := &http.Client{Timeout: 15 * time.Second}

	switch provider {
	case "tinyfish":
		if wsPool == nil || len(cfg.WebSearch.APIKeys) == 0 {
			return nil, fmt.Errorf("tinyfish: no api keys configured")
		}
		key := wsPool.Next()
		results, err := fetchTinyFishSearch(ctx, client, key, query)
		if err != nil {
			if strings.Contains(err.Error(), "429") {
				wsPool.MarkRateLimited(key)
				key2 := wsPool.Next()
				if key2 != key {
					return fetchTinyFishSearch(ctx, client, key2, query)
				}
			}
			return nil, err
		}
		return results, nil
	case "bing-rss":
		return fetchBingRSSWebSearch(ctx, client, query)
	default:
		return nil, fmt.Errorf("unknown web search provider: %s", provider)
	}
}
