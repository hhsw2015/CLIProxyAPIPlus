package executor

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	kiroclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/claude"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

var openAICompatWebSearchFunc = fetchBingRSSWebSearch

const (
	openAICompatSkipWebSearchInterceptMetadataKey = "openai_compat_skip_web_search_intercept"
	maxOpenAICompatWebSearchIterations            = 5
)

func (e *OpenAICompatExecutor) shouldInterceptWebSearch(auth *cliproxyauth.Auth, opts cliproxyexecutor.Options, originalPayload []byte) bool {
	if len(opts.Metadata) > 0 {
		if raw, ok := opts.Metadata[openAICompatSkipWebSearchInterceptMetadataKey]; ok {
			if skip, ok := raw.(bool); ok && skip {
				return false
			}
		}
	}
	if !e.requiresAnthropicImageContent(auth) {
		return false
	}
	if opts.SourceFormat != "claude" {
		return false
	}
	return kiroclaude.HasWebSearchTool(originalPayload)
}

func (e *OpenAICompatExecutor) handleInterceptedWebSearchStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	originalPayload []byte,
) (*cliproxyexecutor.StreamResult, error) {
	query := kiroclaude.ExtractSearchQuery(originalPayload)
	if strings.TrimSpace(query) == "" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "web_search query is empty"}
	}

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 30*time.Second)
	requestedModel := payloadRequestedModel(opts, req.Model)
	out := make(chan cliproxyexecutor.StreamChunk, 16)
	go func() {
		defer close(out)

		msgStart := kiroclaude.BuildClaudeMessageStartEvent(requestedModel, int64(max(1, len(originalPayload)/4)))
		out <- cliproxyexecutor.StreamChunk{Payload: ensureSSETerminated(msgStart)}

		currentPayload, err := kiroclaude.ReplaceWebSearchToolDescription(bytes.Clone(originalPayload))
		if err != nil {
			currentPayload = bytes.Clone(originalPayload)
		}
		currentQuery := query
		contentBlockIndex := 0

		for iteration := 0; iteration < maxOpenAICompatWebSearchIterations; iteration++ {
			results, err := openAICompatWebSearchFunc(ctx, httpClient, currentQuery)
			if err != nil {
				out <- cliproxyexecutor.StreamChunk{Err: statusErr{code: http.StatusBadGateway, msg: fmt.Sprintf("web_search failed: %v", err)}}
				return
			}

			toolUseID := "srvtoolu_websearch"
			if id, req := kiroclaude.CreateMcpRequest(currentQuery); req != nil && strings.TrimSpace(id) != "" {
				toolUseID = id
			}
			for _, event := range kiroclaude.GenerateSearchIndicatorEvents(currentQuery, toolUseID, results, contentBlockIndex) {
				out <- cliproxyexecutor.StreamChunk{Payload: ensureSSETerminated(event)}
			}
			contentBlockIndex += 2

			modifiedPayload, err := kiroclaude.InjectToolResultsClaude(currentPayload, toolUseID, currentQuery, results)
			if err != nil {
				for _, event := range kiroclaude.BuildFallbackTextEvents(contentBlockIndex, currentQuery, results) {
					out <- cliproxyexecutor.StreamChunk{Payload: ensureSSETerminated(event)}
				}
				return
			}
			currentPayload = modifiedPayload

			modifiedReq := req
			modifiedReq.Payload = modifiedPayload
			modifiedOpts := opts
			modifiedOpts.OriginalRequest = modifiedPayload
			modifiedOpts.Metadata = cloneExecutorMetadata(opts.Metadata)
			modifiedOpts.Metadata[openAICompatSkipWebSearchInterceptMetadataKey] = true

			streamResult, err := e.ExecuteStream(ctx, auth, modifiedReq, modifiedOpts)
			if err != nil {
				out <- cliproxyexecutor.StreamChunk{Err: err}
				return
			}

			chunks, err := collectStreamPayloads(ctx, streamResult.Chunks)
			if err != nil {
				out <- cliproxyexecutor.StreamChunk{Err: err}
				return
			}

			analysis := kiroclaude.AnalyzeBufferedStream(chunks)
			if analysis.HasWebSearchToolUse && analysis.WebSearchQuery != "" && iteration+1 < maxOpenAICompatWebSearchIterations {
				filtered := kiroclaude.FilterChunksForClient(chunks, analysis.WebSearchToolUseIndex, contentBlockIndex)
				for _, chunk := range filtered {
					out <- cliproxyexecutor.StreamChunk{Payload: ensureSSETerminated(chunk)}
				}
				currentQuery = analysis.WebSearchQuery
				continue
			}

			for _, chunk := range chunks {
				adjusted, shouldForward := kiroclaude.AdjustSSEChunk(chunk, contentBlockIndex)
				if !shouldForward {
					continue
				}
				out <- cliproxyexecutor.StreamChunk{Payload: ensureSSETerminated(adjusted)}
			}
			return
		}

		out <- cliproxyexecutor.StreamChunk{Err: statusErr{code: http.StatusBadGateway, msg: "web_search exceeded maximum iterations"}}
	}()
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"Content-Type": {"text/event-stream"}}, Chunks: out}, nil
}

func cloneExecutorMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return map[string]any{}
	}
	dst := make(map[string]any, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func collectStreamPayloads(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([][]byte, error) {
	var out [][]byte
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return out, nil
		}
		if chunk.Err != nil {
			return nil, chunk.Err
		}
		if len(chunk.Payload) > 0 {
			out = append(out, bytes.Clone(chunk.Payload))
		}
	}
}

func ensureSSETerminated(payload []byte) []byte {
	trimmed := bytes.TrimRight(payload, "\n")
	out := append([]byte(nil), trimmed...)
	out = append(out, '\n', '\n')
	return out
}

type bingRSS struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
		} `xml:"item"`
	} `xml:"channel"`
}

func fetchBingRSSWebSearch(ctx context.Context, client *http.Client, query string) (*kiroclaude.WebSearchResults, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	endpoint := "https://www.bing.com/search?format=rss&q=" + url.QueryEscape(strings.TrimSpace(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, */*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bing rss returned status %d", resp.StatusCode)
	}

	var feed bingRSS
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, err
	}

	results := &kiroclaude.WebSearchResults{
		Results: make([]kiroclaude.WebSearchResult, 0, len(feed.Channel.Items)),
	}
	for _, item := range feed.Channel.Items {
		title := strings.TrimSpace(item.Title)
		link := strings.TrimSpace(item.Link)
		desc := strings.TrimSpace(item.Description)
		if title == "" || link == "" {
			continue
		}
		res := kiroclaude.WebSearchResult{Title: title, URL: link}
		if desc != "" {
			descCopy := desc
			res.Snippet = &descCopy
		}
		results.Results = append(results.Results, res)
		if len(results.Results) >= 5 {
			break
		}
	}
	if len(results.Results) == 0 {
		msg := "No search results found."
		results.Error = &msg
	}
	return results, nil
}
