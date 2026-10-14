package antifraud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 提交与轮询参数：推理服务是 ticket 式异步协议，提交后轮询取结果。
const (
	inferPath  = "/api/v1/infer"
	maxBatch   = 256 // 服务端上限（MAX_TEXTS）
	pollFirst  = 50 * time.Millisecond
	pollMax    = 400 * time.Millisecond
	submitCode = http.StatusAccepted
)

// InferItem 是推理服务对单条文本的判定结果（result.items 元素）。
type InferItem struct {
	Text           string          `json:"text"`
	LabelID        int             `json:"label_id"`
	LabelKey       string          `json:"label_key"`
	LabelName      string          `json:"label_name"`
	Confidence     float64         `json:"confidence"`
	Probabilities  []float64       `json:"probabilities"`
	ExpertsOpinion []ExpertOpinion `json:"experts_opinion"`
}

// ExpertOpinion 是单个专家的概率分布与门控权重。
type ExpertOpinion struct {
	ID            int       `json:"id"`
	Key           string    `json:"key"`
	Probabilities []float64 `json:"probabilities"`
	Weight        float64   `json:"weight"`
}

// inferRequest 是 POST /api/v1/infer 的请求体。
type inferRequest struct {
	Texts   []string `json:"texts"`
	Explain bool     `json:"explain"`
}

// inferTicket 是提交任务与查询任务共用的响应体。
type inferTicket struct {
	Ticket string `json:"ticket"`
	Status string `json:"status"`
	Error  string `json:"error"`
	Result *struct {
		Items []InferItem `json:"items"`
	} `json:"result"`
}

// Client 是反诈推理服务的 HTTP 客户端（提交 + 轮询）。
type Client struct {
	baseURL   string
	http      *http.Client
	pollDelay time.Duration
}

// NewClient 创建推理客户端。
//
// 参数：
//   - endpoint：服务基址，如 http://antifraud:8000（仅允许 http/https）
//   - timeout：单批判定总预算（含轮询），<=0 取默认 8s
//
// 返回：endpoint 为空、非法或非 http(s) 时返回错误。
func NewClient(endpoint string, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("antifraud: 推理服务地址非法: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("antifraud: 推理服务地址必须以 http:// 或 https:// 开头，实际为 %q", endpoint)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("antifraud: 推理服务地址缺少主机名: %q", endpoint)
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return &Client{
		baseURL:   strings.TrimRight(parsed.String(), "/"),
		pollDelay: pollFirst,
		// 单次 HTTP 请求超时略小于总预算，避免轮询请求本身吃掉全部时间
		http: &http.Client{Timeout: timeout},
	}, nil
}

// Infer 提交一批文本并等待判定结果（提交 → 轮询至终态）。
//
// 参数：
//   - texts：待判定文本，1~maxBatch 条，顺序与返回切片一一对应
//
// 返回：与 texts 等长的判定结果；网络错误、服务失败（status=failed）或超时返回错误。
//
// 注意：ctx 决定本次判定的总预算，由调用方按配置截止；任一步失败即整体失败，
// 不做部分结果返回——批内条目要么全部拿到结果，要么全部重试。
func (c *Client) Infer(ctx context.Context, texts []string) ([]InferItem, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("antifraud: 待判定文本为空")
	}
	if len(texts) > maxBatch {
		return nil, fmt.Errorf("antifraud: 单批文本数 %d 超过服务上限 %d", len(texts), maxBatch)
	}

	ticket, err := c.submit(ctx, texts)
	if err != nil {
		return nil, err
	}
	return c.await(ctx, ticket, len(texts))
}

// submit 提交判定任务，返回 ticket。
func (c *Client) submit(ctx context.Context, texts []string) (string, error) {
	body, err := json.Marshal(inferRequest{Texts: texts})
	if err != nil {
		return "", fmt.Errorf("antifraud: 请求体编码失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+inferPath, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("antifraud: 构造提交请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("antifraud: 提交判定请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("antifraud: 读取提交响应失败: %w", err)
	}
	if resp.StatusCode != submitCode {
		return "", fmt.Errorf("antifraud: 提交判定返回 %d: %s", resp.StatusCode, truncateText(string(payload), 200))
	}
	var parsed inferTicket
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("antifraud: 提交响应解析失败: %w", err)
	}
	if parsed.Ticket == "" {
		return "", fmt.Errorf("antifraud: 提交响应缺少 ticket")
	}
	return parsed.Ticket, nil
}

// await 轮询任务直至 done / failed / ctx 结束。
func (c *Client) await(ctx context.Context, ticket string, want int) ([]InferItem, error) {
	delay := c.pollDelay
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("antifraud: 判定任务 %s 等待超时: %w", ticket, ctx.Err())
		case <-time.After(delay):
		}
		if delay < pollMax {
			delay *= 2
			if delay > pollMax {
				delay = pollMax
			}
		}

		task, err := c.query(ctx, ticket)
		if err != nil {
			return nil, err
		}
		switch task.Status {
		case "done":
			if task.Result == nil {
				return nil, fmt.Errorf("antifraud: 判定任务 %s 完成但无结果", ticket)
			}
			items := task.Result.Items
			if len(items) != want {
				return nil, fmt.Errorf("antifraud: 判定任务 %s 返回 %d 条结果，期望 %d 条", ticket, len(items), want)
			}
			return items, nil
		case "failed":
			return nil, fmt.Errorf("antifraud: 判定任务 %s 失败: %s", ticket, task.Error)
		default: // queued / running：继续轮询
		}
	}
}

// query 查询任务快照。
func (c *Client) query(ctx context.Context, ticket string) (*inferTicket, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+inferPath+"/"+url.PathEscape(ticket), nil)
	if err != nil {
		return nil, fmt.Errorf("antifraud: 构造查询请求失败: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("antifraud: 查询判定任务失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("antifraud: 读取查询响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("antifraud: 查询判定任务返回 %d: %s", resp.StatusCode, truncateText(string(payload), 200))
	}
	var parsed inferTicket
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("antifraud: 查询响应解析失败: %w", err)
	}
	return &parsed, nil
}
