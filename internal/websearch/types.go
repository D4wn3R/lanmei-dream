// Package websearch 为 Agent 提供有界、会话隔离的公开互联网摘要检索。
package websearch

import "context"

// AdapterVersion 对应上游固定提交 112593b8e17c084f3e9943b8fd9861c6570feca0。
const AdapterVersion = "openserp-2.1"

type SearchRequest struct {
	Query string
	Limit int
}

type SearchResult struct {
	ID        string `json:"id,omitempty"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
	Engine    string `json:"engine,omitempty"`
	FetchedAt string `json:"fetched_at"`
}

type SearchResponse struct {
	Status  string         `json:"status"`
	Query   string         `json:"query,omitempty"`
	Results []SearchResult `json:"results"`
	Cached  bool           `json:"cached"`
	Message string         `json:"message,omitempty"`
}

type Searcher interface {
	Search(context.Context, SearchRequest) (SearchResponse, error)
}

type searchError string

func (e searchError) Error() string { return string(e) }

const (
	errInvalidResponse searchError = "invalid_response"
	errUnavailable     searchError = "unavailable"
	errRateLimited     searchError = "rate_limited"
)

func failure(status string) SearchResponse {
	messages := map[string]string{
		"timeout":           "本轮联网检索超时，尚未核实网上信息。",
		"unavailable":       "联网检索暂不可用，尚未核实网上信息。",
		"rate_limited":      "联网检索繁忙或会话调用过快，请稍后再试。",
		"budget_exceeded":   "本条消息的搜索次数或等待预算已用尽，请根据已有资料作答并说明局限。",
		"invalid_arguments": "参数无效或包含疑似私人信息；仅允许 1–200 字的公开关键词与 1–5 的 limit。",
		"invalid_response":  "搜索服务返回的数据无法作为可靠资料使用。",
	}
	return SearchResponse{Status: status, Results: []SearchResult{}, Message: messages[status]}
}
