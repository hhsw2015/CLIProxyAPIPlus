package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	kiroclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/kiro/claude"
)

type tinyfishSearchResponse struct {
	Query        string           `json:"query"`
	Results      []tinyfishResult `json:"results"`
	TotalResults int              `json:"total_results"`
}

type tinyfishResult struct {
	Position int    `json:"position"`
	SiteName string `json:"site_name"`
	Title    string `json:"title"`
	Snippet  string `json:"snippet"`
	URL      string `json:"url"`
}

func fetchTinyFishSearch(ctx context.Context, client *http.Client, apiKey, query string) (*kiroclaude.WebSearchResults, error) {
	u := "https://api.search.tinyfish.ai?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("tinyfish rate limited (429)")
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("tinyfish search %d: %s", resp.StatusCode, body)
	}

	var sr tinyfishSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("tinyfish decode: %w", err)
	}

	results := &kiroclaude.WebSearchResults{}
	for i, r := range sr.Results {
		if i >= 5 {
			break
		}
		entry := kiroclaude.WebSearchResult{Title: r.Title, URL: r.URL}
		if r.Snippet != "" {
			s := r.Snippet
			entry.Snippet = &s
		}
		results.Results = append(results.Results, entry)
	}
	if len(results.Results) == 0 {
		e := "No search results found."
		results.Error = &e
	}
	return results, nil
}

type tinyfishFetchRequest struct {
	URLs   []string `json:"urls"`
	Format string   `json:"format"`
}

type tinyfishFetchResponse struct {
	Results []tinyfishFetchResult `json:"results"`
	Errors  []tinyfishFetchError  `json:"errors"`
}

type tinyfishFetchResult struct {
	URL         string `json:"url"`
	FinalURL    string `json:"final_url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Language    string `json:"language"`
	Text        string `json:"text"`
}

type tinyfishFetchError struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

type FetchResult struct {
	URL         string
	FinalURL    string
	Title       string
	Description string
	Text        string
	Error       string
}

func fetchTinyFishContent(ctx context.Context, client *http.Client, apiKey string, urls []string) ([]FetchResult, error) {
	payload, _ := json.Marshal(tinyfishFetchRequest{URLs: urls, Format: "markdown"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.fetch.tinyfish.ai", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("tinyfish fetch rate limited (429)")
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("tinyfish fetch %d: %s", resp.StatusCode, body)
	}

	var fr tinyfishFetchResponse
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		return nil, fmt.Errorf("tinyfish fetch decode: %w", err)
	}

	out := make([]FetchResult, 0, len(fr.Results)+len(fr.Errors))
	for _, r := range fr.Results {
		out = append(out, FetchResult{
			URL:         r.URL,
			FinalURL:    r.FinalURL,
			Title:       r.Title,
			Description: r.Description,
			Text:        r.Text,
		})
	}
	for _, e := range fr.Errors {
		out = append(out, FetchResult{URL: e.URL, Error: e.Error})
	}
	return out, nil
}
