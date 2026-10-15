package gateway

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/lxzan/gws"
)

// Connection 表示一个反向 WebSocket 连接（来自 Onebots 或 NapCat）。
//
// 注意：SelfID 与 Impl 由读循环在收到事件时并发写入，写入经 SetSelfID / SetImpl 加锁；
// 其余字段在注册前设置后不再变更。直接访问 SelfID / Impl 字段时应自行保证同步。
type Connection struct {
	ID       string    // 连接唯一标识
	Platform Platform  // 来源平台
	Protocol Protocol  // OneBot 协议版本
	SelfID   string    // 机器人自身 ID（并发写入，通过 mu 保护）
	Impl     string    // OneBot 实现名（并发写入，通过 mu 保护）
	Socket   *gws.Conn // 底层 gws 连接

	mu sync.Mutex // 保护 SelfID、Impl 的并发写入
}

// SetSelfID 安全设置 SelfID（仅首次设置生效）。
//
// 参数：
//   - id：机器人自身 ID；空字符串不产生写入
//
// 注意：并发调用安全；已有非空值时忽略后续值，避免事件乱序覆盖首次解析结果。
func (c *Connection) SetSelfID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.SelfID == "" {
		c.SelfID = id
	}
}

// SetImpl 安全设置 Impl（仅首次设置生效）。
//
// 参数：
//   - impl：OneBot 实现名（如 go_onebot）；空字符串不产生写入
//
// 注意：并发调用安全；已有非空值时忽略后续值。
func (c *Connection) SetImpl(impl string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Impl == "" {
		c.Impl = impl
	}
}

// Hub 管理所有活跃的反向 WS 连接，是出站消息的统一入口。
//
// 注意：所有方法并发安全；Send* 方法按连接协议自动选择动作，见 SendSegments。
type Hub struct {
	mu    sync.RWMutex
	conns map[string]*Connection

	// pending 是 echo → 等待通道的登记表，仅 SendToWait 的调用方在此等待，
	// 由 ResolveActionResponse 投递；为空表示无人等待响应。
	pendingMu sync.Mutex
	pending   map[string]chan ActionResponse
	echoSeq   atomic.Uint64
}

// NewHub 创建连接管理中心。
//
// 返回：空的 Hub，可立即注册连接。
func NewHub() *Hub {
	return &Hub{conns: make(map[string]*Connection)}
}

// Register 注册新连接。
//
// 参数：
//   - conn：连接对象；ID 相同的连接会覆盖旧值，调用方应保证 ID 唯一
//
// 注意：并发安全，通常在 WS 升级完成后由网关调用。
func (h *Hub) Register(conn *Connection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[conn.ID] = conn
}

// Unregister 移除连接。
//
// 参数：
//   - connID：连接 ID；不存在时静默返回（不报错）。
func (h *Hub) Unregister(connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, connID)
}

// Get 获取指定连接。
//
// 参数：
//   - connID：连接 ID
//
// 返回：连接指针与是否存在；不存在时返回 (nil, false)。
func (h *Hub) Get(connID string) (*Connection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.conns[connID]
	return c, ok
}

// SendTo 向指定连接发送动作请求。
//
// 参数：
//   - connID：目标连接 ID
//   - req：动作请求，经 JSON 编码为文本帧发送
//
// 返回：连接不存在或写帧失败时返回错误。
func (h *Hub) SendTo(connID string, req *ActionRequest) error {
	h.mu.RLock()
	conn, ok := h.conns[connID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("gateway: 连接 %s 不存在", connID)
	}
	return writeJSON(conn.Socket, req)
}

// SendToWait 发送动作请求并等待同 Echo 的响应，用于需要确认真实成败的动作（撤回 / 禁言）。
//
// 参数：
//   - connID：目标连接 ID
//   - req：动作请求；Echo 为空时自动生成
//   - timeout：等待响应的最长时间
//
// 返回：平台动作响应；连接不存在、写帧失败或超时返回错误。
//
// 注意：平台不回报该动作时不区分「未支持」与「未处理」，一律走超时错误；
// 超时只代表本端未收到响应，动作可能已生效，调用方不应据此重试补偿。
func (h *Hub) SendToWait(connID string, req *ActionRequest, timeout time.Duration) (*ActionResponse, error) {
	if req.Echo == "" {
		req.Echo = fmt.Sprintf("lanmei-%d-%d", time.Now().UnixNano(), h.echoSeq.Add(1))
	}
	ch := make(chan ActionResponse, 1)
	h.pendingMu.Lock()
	if h.pending == nil {
		h.pending = make(map[string]chan ActionResponse)
	}
	h.pending[req.Echo] = ch
	h.pendingMu.Unlock()
	defer func() {
		h.pendingMu.Lock()
		delete(h.pending, req.Echo)
		h.pendingMu.Unlock()
	}()

	if err := h.SendTo(connID, req); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		return &resp, nil
	case <-timer.C:
		return nil, fmt.Errorf("gateway: 动作 %s 等待响应超时（%s）", req.Action, timeout)
	}
}

// ResolveActionResponse 把平台动作响应投递给等待中的 SendToWait 调用方。
//
// 参数：
//   - echo：响应携带的请求标识
//   - resp：动作响应
//
// 返回：存在等待者并投递成功时为 true；无等待者（普通发消息响应、重复响应）返回 false。
func (h *Hub) ResolveActionResponse(echo string, resp *ActionResponse) bool {
	if echo == "" || resp == nil {
		return false
	}
	h.pendingMu.Lock()
	ch, ok := h.pending[echo]
	h.pendingMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- *resp:
	default: // 等待者已收到响应（重复回包），丢弃
	}
	return true
}

// SendMessageTo 向指定连接发送文本消息（自动选择协议动作）。
//
// 参数：
//   - connID：目标连接 ID
//   - msg：消息上下文，IsGroup / UserID / GroupID 决定会话类型与发送目标
//   - text：纯文本内容
//
// 返回：连接不存在或发送失败时返回错误。
//
// 注意：等价于用单个 text 段调用 SendSegments。
func (h *Hub) SendMessageTo(connID string, msg *NormalizedMessage, text string) error {
	return h.SendSegments(connID, msg, []NormalizedSegment{{Type: "text", Text: text}})
}

// SendSegments 向指定连接发送结构化消息段（text/image/at 组合，自动选择协议动作）。
//
// 参数：
//   - connID：目标连接 ID
//   - msg：消息上下文，IsGroup 决定群聊 / 私聊，UserID / GroupID 为发送目标
//   - segs：标准化段列表；为空时不报错，会构造并发送空 message 数组
//
// 返回：连接不存在或写帧失败时返回错误。
//
// 注意：
//   - 协议动作：OneBot 12 走 send_message（detail_type 按 IsGroup 取 group / private）；
//     OneBot 11 走 send_group_msg / send_private_msg。at 段的 user_id / qq 字段差异
//     由 ToMessageSegmentV11 / V12 收敛。
//   - 发送前统一调用 spaceAfterAt 补齐 at 段与正文间的空格，这是出站段的唯一补齐点；
//     入站文本不受影响。
func (h *Hub) SendSegments(connID string, msg *NormalizedMessage, segs []NormalizedSegment) error {
	h.mu.RLock()
	conn, ok := h.conns[connID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("gateway: 连接 %s 不存在", connID)
	}

	segs = spaceAfterAt(segs)
	var req *ActionRequest
	if conn.Protocol == ProtocolV11 {
		req = BuildSendMessageSegmentsV11(msg.IsGroup, msg.UserID, msg.GroupID, segs)
	} else {
		detailType := "private"
		if msg.IsGroup {
			detailType = "group"
		}
		req = BuildSendMessageSegmentsV12(detailType, msg.UserID, msg.GroupID, segs)
	}
	return writeJSON(conn.Socket, req)
}

// spaceAfterAt 为紧跟 at 段的文本段补一个前导空格，避免客户端把 at 段与正文粘连成 "@某某内容"。
// 它是出站段的统一补齐点：SendSegments 在构造协议动作前调用，插件与回复路径无需自行处理空格。
//
// 补齐规则：仅当文本段非空、紧邻前一段为 at、且文本未以空白字符开头时，在文本前插入一个半角空格；
// 连续多个 at 段只补其后的文本段，at 段后是 image / reply 等其他段时不处理。
// 无需补齐时原样返回入参切片，需要时返回修改后的副本，不修改调用方传入的数据，可重复调用。
// 入站解析不经此函数，原始文本不受影响。
func spaceAfterAt(segs []NormalizedSegment) []NormalizedSegment {
	var patched []NormalizedSegment
	for i, seg := range segs {
		if i == 0 || seg.Type != "text" || segs[i-1].Type != "at" || seg.Text == "" {
			continue
		}
		if strings.TrimLeftFunc(seg.Text, unicode.IsSpace) != seg.Text {
			continue
		}
		if patched == nil {
			patched = make([]NormalizedSegment, len(segs))
			copy(patched, segs)
		}
		patched[i].Text = " " + seg.Text
	}
	if patched == nil {
		return segs
	}
	return patched
}

// SendImage 向指定连接发送图片（file 支持 url / base64(data:image/...) / RustFS presign URL / 本地路径）。
//
// 参数：
//   - connID：目标连接 ID
//   - msg：消息上下文，决定会话类型与发送目标
//   - file：图片文件标识（url / base64 data URI / 本地路径 / 对象存储预签名 URL）
//
// 返回：连接不存在或发送失败时返回错误。
func (h *Hub) SendImage(connID string, msg *NormalizedMessage, file string) error {
	return h.SendSegments(connID, msg, []NormalizedSegment{{
		Type: "image",
		Data: map[string]string{"file": file},
	}})
}

// SendAtText 向指定连接发送 "@用户 + 文本" 组合消息。
// atUserID 为空时退化为纯文本发送。
//
// 参数：
//   - connID：目标连接 ID
//   - msg：消息上下文，决定会话类型与发送目标
//   - atUserID：被 at 的用户 ID（OneBot 12 user_id 语义）；为空时不追加 at 段
//   - text：正文文本
//
// 返回：连接不存在或发送失败时返回错误。
//
// 注意：at 段与正文之间的空格由 SendSegments 统一补齐，调用方无需在 text 前加空格。
func (h *Hub) SendAtText(connID string, msg *NormalizedMessage, atUserID, text string) error {
	segs := make([]NormalizedSegment, 0, 2)
	if atUserID != "" {
		segs = append(segs, NormalizedSegment{Type: "at", Data: map[string]string{"user_id": atUserID}})
	}
	segs = append(segs, NormalizedSegment{Type: "text", Text: text})
	return h.SendSegments(connID, msg, segs)
}

// All 返回所有活跃连接的快照。
//
// 返回：连接指针切片；无连接时返回空切片（非 nil），顺序不保证稳定。
func (h *Hub) All() []*Connection {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]*Connection, 0, len(h.conns))
	for _, c := range h.conns {
		result = append(result, c)
	}
	return result
}

// Count 返回活跃连接数。
//
// 返回：当前 Hub 中的连接数量。
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}
