package websearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/config"
)

type openSERP struct {
	client        *http.Client
	endpoint      string
	engines, mode string
	maxBytes      int
}

func newOpenSERP(cfg config.WebSearchConfig) *openSERP {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = cfg.MaxConcurrency
	transport.MaxIdleConnsPerHost = cfg.MaxConcurrency
	return &openSERP{
		client: &http.Client{Transport: transport, Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + "/mega/search",
		engines:  strings.Join(cfg.Engines, ","), mode: cfg.Mode, maxBytes: cfg.MaxResponseBytes,
	}
}

func (c *openSERP) Search(ctx context.Context, request SearchRequest) (SearchResponse, error) {
	values := url.Values{"text": {request.Query}, "engines": {c.engines}, "mode": {c.mode},
		"limit": {strconv.Itoa(request.Limit)}, "extract": {"0"}, "format": {"json"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"?"+values.Encode(), nil)
	if err != nil {
		return SearchResponse{}, errUnavailable
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return SearchResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return SearchResponse{}, errRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return SearchResponse{}, errUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxBytes)+1))
	if err != nil {
		return SearchResponse{}, err
	}
	if len(body) > c.maxBytes {
		return SearchResponse{}, errInvalidResponse
	}
	return decodeEnvelope(body, request.Query)
}

// decodeEnvelope 只接收固定的 v2.1 协议，不猜测数组或任意嵌套字段。
func decodeEnvelope(body []byte, query string) (SearchResponse, error) {
	var envelope struct {
		Meta *struct {
			Version   string   `json:"version"`
			Responded []string `json:"engines_responded"`
			Failed    []string `json:"engines_failed"`
			Errors    []struct {
				Engine string `json:"engine"`
				Error  string `json:"error"`
			} `json:"engine_errors"`
		} `json:"meta"`
		Results json.RawMessage `json:"results"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Meta == nil || envelope.Meta.Version != "2.1" ||
		len(envelope.Results) == 0 || string(envelope.Results) == "null" || len(envelope.Error) > 0 {
		return SearchResponse{}, errInvalidResponse
	}
	var results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
		Engine  string `json:"engine"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(envelope.Results, &results) != nil {
		return SearchResponse{}, errInvalidResponse
	}
	failed := len(envelope.Meta.Failed) > 0 || len(envelope.Meta.Errors) > 0
	if len(envelope.Meta.Responded) == 0 {
		if failed {
			return SearchResponse{}, errUnavailable
		}
		return SearchResponse{}, errInvalidResponse
	}
	response := SearchResponse{Status: "ok", Query: query, Results: []SearchResult{}}
	if failed {
		response.Status = "partial"
	}
	if len(results) == 0 {
		if failed {
			return SearchResponse{}, errUnavailable
		}
		response.Status = "no_results"
	}
	fetchedAt := time.Now().UTC().Format(time.RFC3339)
	for _, r := range results {
		if r.Type == "ad" {
			continue
		}
		response.Results = append(response.Results, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet, Engine: r.Engine, FetchedAt: fetchedAt})
	}
	if len(results) > 0 && len(response.Results) == 0 {
		return SearchResponse{}, errInvalidResponse
	}
	return response, nil
}
