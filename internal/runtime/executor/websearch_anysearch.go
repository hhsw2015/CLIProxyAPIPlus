package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	kiroclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/kiro/claude"
)

// AnySearch is a unified search service that exposes a JSON-RPC 2.0 endpoint
// at https://api.anysearch.com/mcp. It returns search hits as a single
// markdown-formatted text block; we parse that block into the canonical
// kiroclaude.WebSearchResults shape so downstream tool-call translation is
// identical regardless of provider.

const anysearchEndpoint = "https://api.anysearch.com/mcp"

type anysearchRPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  anysearchCall `json:"params"`
}

type anysearchCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type anysearchRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result,omitempty"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func fetchAnySearch(ctx context.Context, client *http.Client, apiKey, query string, maxResults int) (*kiroclaude.WebSearchResults, error) {
	if maxResults <= 0 {
		maxResults = 10
	}
	args := map[string]any{"query": query, "max_results": maxResults}
	body, _ := json.Marshal(anysearchRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  anysearchCall{Name: "search", Arguments: args},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anysearchEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("anysearch rate limited (429)")
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anysearch %d: %s", resp.StatusCode, b)
	}

	var rpc anysearchRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return nil, fmt.Errorf("anysearch decode: %w", err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("anysearch error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	if rpc.Result == nil || len(rpc.Result.Content) == 0 {
		return &kiroclaude.WebSearchResults{Results: []kiroclaude.WebSearchResult{}, Query: &query}, nil
	}

	var md strings.Builder
	for _, c := range rpc.Result.Content {
		if c.Type == "text" {
			md.WriteString(c.Text)
			md.WriteString("\n")
		}
	}

	results := parseAnySearchMarkdown(md.String())
	q := query
	total := len(results)
	return &kiroclaude.WebSearchResults{
		Results:      results,
		TotalResults: &total,
		Query:        &q,
	}, nil
}

// parseAnySearchMarkdown parses the AnySearch markdown output. Format example:
//
//	## Search Results (10 results, 2483ms)
//
//	### 1. Title goes here
//	- **URL**: https://example.com
//	- snippet text on a single trailing line.
var (
	anysearchHeaderRE = regexp.MustCompile(`(?m)^### \s*(\d+)\.\s*(.+?)\s*$`)
	anysearchURLRE    = regexp.MustCompile(`(?m)^\s*[-*]\s*\*\*URL\*\*:\s*(\S+)`)
)

func parseAnySearchMarkdown(md string) []kiroclaude.WebSearchResult {
	out := []kiroclaude.WebSearchResult{}
	if md == "" {
		return out
	}

	idxs := anysearchHeaderRE.FindAllStringSubmatchIndex(md, -1)
	for i, m := range idxs {
		titleStart := m[4]
		titleEnd := m[5]
		title := strings.TrimSpace(md[titleStart:titleEnd])

		// block extends to next header or EOF
		blockStart := m[1]
		blockEnd := len(md)
		if i+1 < len(idxs) {
			blockEnd = idxs[i+1][0]
		}
		block := md[blockStart:blockEnd]

		urlMatch := anysearchURLRE.FindStringSubmatch(block)
		var url string
		if len(urlMatch) >= 2 {
			url = urlMatch[1]
		}
		if url == "" {
			continue
		}

		snippet := extractAnySearchSnippet(block, url)
		var snippetPtr *string
		if snippet != "" {
			snippetPtr = &snippet
		}

		idStr := strconv.Itoa(len(out) + 1)
		_ = title // keep title even if empty fallback below
		if title == "" {
			title = url
		}
		out = append(out, kiroclaude.WebSearchResult{
			Title:   title,
			URL:     url,
			Snippet: snippetPtr,
			ID:      &idStr,
		})
	}
	return out
}

// extractAnySearchSnippet returns the trailing bullet text in the block that
// is not the URL line, joining if multiple lines.
func extractAnySearchSnippet(block, url string) string {
	lines := strings.Split(block, "\n")
	parts := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "###") {
			continue
		}
		ln = strings.TrimPrefix(ln, "-")
		ln = strings.TrimPrefix(ln, "*")
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "**URL**") || strings.Contains(ln, url) {
			continue
		}
		if ln != "" {
			parts = append(parts, ln)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}
