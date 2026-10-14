// Package gateway 是反向 WebSocket 的 OneBot 协议适配层：Onebots / NapCat 等外部
// 适配器作为 WS 客户端主动连接本服务，网关负责鉴权、协议与平台识别、事件标准化，
// 以及出站动作的构造与发送。
//
// 对接要点：
//   - 端点：/onebot（自动识别协议）、/onebot/v12、/onebot/v11（NapCat 兼容）。
//   - 鉴权：ListenConfig.AccessToken 非空时校验 Authorization: Bearer <token>
//     或 access_token 查询参数；为空则不鉴权。
//   - 协议与平台识别：Sec-WebSocket-Protocol 头（如 "12.xxx" / "11.xxx"）与
//     protocol / platform 查询参数，查询参数优先；默认 OneBot 12 + QQ。
//   - 入站：Server 按连接协议调用 NormalizeV12 / NormalizeV11 标准化为
//     NormalizedMessage 后交给 EventHandler.OnMessage。
//   - 出站：经 Hub 的 Send* 方法，按连接协议自动选择 send_message 或
//     send_group_msg / send_private_msg。
//   - 协议差异：at 段 user_id（OneBot 12）/ qq（OneBot 11），引用段 message_id / id，
//     OneBot 11 数字 ID 与 CQ 码转义见 EventV11、MessageSegmentV11、ParseCQSegmentsV11。
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lxzan/gws"
	"go.uber.org/zap"
)

const (
	sessionKeyConnID = "gw_conn_id"
	sessionKeyProto  = "gw_protocol"
	sessionKeyPlat   = "gw_platform"
)

// EventHandler 网关事件处理接口（由 bot 层实现）。
type EventHandler interface {
	// OnMessage 收到标准化消息。
	//
	// 参数：
	//   - msg：已完成协议标准化的消息；可能来自多个连接的不同读循环，实现方需自行保证并发安全
	OnMessage(msg *NormalizedMessage)
}

// Server 是反向 WebSocket 网关服务端，接受 Onebots/NapCat 的连接。
//
// 注意：通过 NewServer 创建；Run 挂载 /onebot、/onebot/v12、/onebot/v11 三个端点；
// 事件处理器可通过 SetHandler 在创建后补设。
type Server struct {
	cfg      *ListenConfig
	hub      *Hub
	handler  EventHandler
	upgrader *gws.Upgrader
	connSeq  atomic.Int64 // 连接 ID 递增序列
	httpSrv  *http.Server // HTTP 服务（用于优雅关闭）
	logger   *zap.Logger
}

// ListenConfig 网关监听配置。
type ListenConfig struct {
	ListenAddr  string // 监听地址，如 "0.0.0.0:8080"
	AccessToken string // 鉴权 token（空则不鉴权）
}

// NewServer 创建网关服务端。
//
// 参数：
//   - cfg：监听配置，不可为 nil（Run 时使用）
//   - logger：日志器，不可为 nil（内部直接调用）
//   - handler：事件处理器，可为 nil；为 nil 时收到的消息会被丢弃并记录告警，可用 SetHandler 补设
//
// 返回：已初始化但未启动的 Server，调用 Run 才开始监听。
func NewServer(cfg *ListenConfig, logger *zap.Logger, handler EventHandler) *Server {
	s := &Server{
		cfg:     cfg,
		hub:     NewHub(),
		handler: handler,
		logger:  logger,
	}
	s.upgrader = gws.NewUpgrader(s, &gws.ServerOption{
		ParallelEnabled: true,
		Recovery:        gws.Recovery,
		// 兼容非 13 的 WS 版本：gws 只接受 Sec-WebSocket-Version: 13（RFC6455），
		// 部分 OneBot 客户端（如新版 llonebot）握手时发送其他版本；两者帧格式相同
		//（RFC6455 基于 hybi-10），改写版本头放行即可。Authorize 在 gws 的版本检查之前执行。
		Authorize: func(r *http.Request, _ gws.SessionStorage) bool {
			ver := r.Header.Get("Sec-WebSocket-Version")
			if ver != "" && ver != "13" {
				logger.Info("gateway: WS 版本兼容",
					zap.String("version", ver),
					zap.String("remote", r.RemoteAddr),
				)
			}
			r.Header.Set("Sec-WebSocket-Version", "13")
			return true
		},
	})
	return s
}

// Hub 返回连接管理中心（供 bot 层发送消息用）。
//
// 返回：Server 内部持有的 Hub，与 Server 同生命周期。
func (s *Server) Hub() *Hub {
	return s.hub
}

// SetHandler 设置事件处理器（可在 NewServer 后设置，解决循环依赖）。
//
// 参数：
//   - handler：事件处理器；传 nil 会恢复「消息被丢弃并告警」的行为。
func (s *Server) SetHandler(handler EventHandler) {
	s.handler = handler
}

// Run 启动网关 HTTP 服务，阻塞运行。
//
// 返回：监听失败返回错误；Shutdown 触发的正常关闭返回 nil。
func (s *Server) Run() error {
	mux := http.NewServeMux()
	// 通用端点：通过 Sec-WebSocket-Protocol 头或查询参数自动检测协议。
	mux.HandleFunc("/onebot", s.handleUpgrade)
	// OneBot 12 专用端点。
	mux.HandleFunc("/onebot/v12", s.handleUpgradeV12)
	// OneBot 11 专用端点（NapCat 兼容）。
	mux.HandleFunc("/onebot/v11", s.handleUpgradeV11)

	s.httpSrv = &http.Server{Addr: s.cfg.ListenAddr, Handler: mux}

	s.logger.Info("gateway: 监听", zap.String("addr", s.cfg.ListenAddr))
	err := s.httpSrv.ListenAndServe()
	// http.ErrServerClosed 是 Shutdown()/Close() 引起的正常关闭，不视为错误。
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 优雅关闭：关闭所有 WS 连接，然后关闭 HTTP 监听器。
//
// 注意：向每个活跃连接发送 1000（正常关闭）帧后关闭监听器，Run 随之返回 nil。
func (s *Server) Shutdown() {
	for _, conn := range s.hub.All() {
		conn.Socket.WriteClose(1000, []byte("server shutdown"))
	}
	// 关闭 HTTP 监听器（使 Run() 返回）。
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	protocol, platform := detectProtocolAndPlatform(r)
	s.doUpgrade(w, r, protocol, platform)
}

func (s *Server) handleUpgradeV12(w http.ResponseWriter, r *http.Request) {
	_, platform := detectProtocolAndPlatform(r)
	s.doUpgrade(w, r, ProtocolV12, platform)
}

func (s *Server) handleUpgradeV11(w http.ResponseWriter, r *http.Request) {
	_, platform := detectProtocolAndPlatform(r)
	s.doUpgrade(w, r, ProtocolV11, platform)
}

func (s *Server) doUpgrade(w http.ResponseWriter, r *http.Request, protocol Protocol, platform Platform) {
	// 鉴权：支持 Authorization 头和 access_token 查询参数（OneBot 惯例）。
	if s.cfg.AccessToken != "" {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("access_token")
		}
		if token != s.cfg.AccessToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	socket, err := s.upgrader.Upgrade(w, r)
	if err != nil {
		s.logger.Error("gateway: WS 升级失败", zap.Error(err))
		return
	}

	connID := strconv.FormatInt(s.connSeq.Add(1), 10)
	conn := &Connection{
		ID:       connID,
		Platform: platform,
		Protocol: protocol,
		Socket:   socket,
	}
	// 将连接信息存入 socket 的 SessionStorage，供 OnClose/OnMessage 回调使用。
	session := socket.Session()
	session.Store(sessionKeyConnID, connID)
	session.Store(sessionKeyProto, string(protocol))
	session.Store(sessionKeyPlat, string(platform))

	s.hub.Register(conn)
	s.logger.Info("gateway: 新连接",
		zap.String("id", connID),
		zap.String("protocol", string(protocol)),
		zap.String("platform", string(platform)),
		zap.String("remote", socket.RemoteAddr().String()),
	)

	// 启动 ReadLoop 读取帧，否则 OnMessage/OnPing/OnClose 等回调不会被调用。
	go socket.ReadLoop()
}

// detectProtocolAndPlatform 从请求头和查询参数检测协议版本和平台。
func detectProtocolAndPlatform(r *http.Request) (Protocol, Platform) {
	protocol := ProtocolV12 // 默认 OneBot 12
	platform := PlatformQQ  // 默认 QQ

	// Sec-WebSocket-Protocol 头：OneBot 12 为 "12.xxx"，OneBot 11 为 "11.xxx"。
	wsProto := r.Header.Get("Sec-WebSocket-Protocol")
	if wsProto != "" {
		parts := strings.Split(wsProto, ".")
		if len(parts) > 0 {
			if parts[0] == "11" {
				protocol = ProtocolV11
			}
			// 如果子协议包含平台信息，如 "12.go_onebot_wechat"
			if len(parts) > 1 {
				if strings.Contains(parts[1], "wechat") {
					platform = PlatformWechat
				} else if strings.Contains(parts[1], "telegram") {
					platform = PlatformTelegram
				} else if strings.Contains(parts[1], "napcat") {
					platform = PlatformNapCat
					protocol = ProtocolV11 // NapCat 必定是 OB11
				}
			}
		}
	}

	// 查询参数覆盖（优先级最高）。
	if q := r.URL.Query().Get("protocol"); q != "" {
		switch strings.ToLower(q) {
		case "v11", "11", "onebot11":
			protocol = ProtocolV11
		case "v12", "12", "onebot12":
			protocol = ProtocolV12
		}
	}
	if q := r.URL.Query().Get("platform"); q != "" {
		platform = Platform(q)
	}

	return protocol, platform
}

// OnOpen 连接建立后刷新读超时（90 秒）。
func (s *Server) OnOpen(socket *gws.Conn) {
	_ = socket.SetDeadline(time.Now().Add(60*time.Second + 30*time.Second))
}

// OnClose 连接关闭，从 Hub 注销该连接。
//
// 参数：
//   - socket：关闭的连接；SessionStorage 中无连接 ID 时直接返回
//   - err：关闭原因，非 nil 时随日志记录
func (s *Server) OnClose(socket *gws.Conn, err error) {
	connID, ok := loadSessionString(socket.Session(), sessionKeyConnID)
	if !ok {
		return
	}
	s.hub.Unregister(connID)
	if err != nil {
		s.logger.Info("gateway: 连接关闭", zap.String("id", connID), zap.Error(err))
	} else {
		s.logger.Info("gateway: 连接关闭", zap.String("id", connID))
	}
}

// OnMessage 收到消息帧，按连接协议分发到 OneBot 12 / 11 处理器。
//
// 参数：
//   - socket：来源连接，用于读取 SessionStorage 与刷新读超时
//   - message：收到的帧；仅 OpcodeText 被解析，其余帧只刷新读超时
func (s *Server) OnMessage(socket *gws.Conn, message *gws.Message) {
	// 收到任何帧都刷新 deadline，防止只发消息不发 ping 的客户端超时。
	_ = socket.SetDeadline(time.Now().Add(60*time.Second + 30*time.Second))

	connID, ok := loadSessionString(socket.Session(), sessionKeyConnID)
	if !ok {
		return
	}
	conn, ok := s.hub.Get(connID)
	if !ok {
		return
	}

	// 只处理文本帧（OneBot 协议使用 JSON 文本）。
	if message.Opcode != gws.OpcodeText {
		return
	}

	data := message.Data.Bytes()
	s.logger.Debug("gateway: 收到帧", zap.String("conn", connID), zap.String("protocol", string(conn.Protocol)), zap.Int("len", len(data)))

	switch conn.Protocol {
	case ProtocolV12:
		s.handleV12Message(conn, data)
	case ProtocolV11:
		s.handleV11Message(conn, data)
	default:
		s.logger.Warn("gateway: 未知协议", zap.String("conn", connID), zap.String("protocol", string(conn.Protocol)))
	}
}

// OnPing 收到心跳帧，刷新读超时（90 秒）。
func (s *Server) OnPing(socket *gws.Conn, payload []byte) {
	_ = socket.SetDeadline(time.Now().Add(60*time.Second + 30*time.Second))
}

// OnPong 收到心跳回复帧，刷新读超时（90 秒）。
func (s *Server) OnPong(socket *gws.Conn, payload []byte) {
	_ = socket.SetDeadline(time.Now().Add(60*time.Second + 30*time.Second))
}

func (s *Server) handleV12Message(conn *Connection, data []byte) {
	tag := peekJSONTag(data)
	if tag == "response" {
		var resp ActionResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			s.logger.Error("gateway: OB12 响应解析失败", zap.String("conn", conn.ID), zap.Error(err))
			return
		}
		s.hub.ResolveActionResponse(resp.Echo, &resp)
		if resp.RetCode != 0 || resp.Status == "failed" {
			s.logger.Warn("gateway: OB12 API 失败",
				zap.String("conn", conn.ID),
				zap.String("echo", resp.Echo),
				zap.String("status", resp.Status),
				zap.Int64("retcode", resp.RetCode),
				zap.String("msg", resp.Message),
			)
		}
		return
	}

	var evt EventV12
	if err := json.Unmarshal(data, &evt); err != nil {
		s.logger.Error("gateway: OB12 事件解析失败", zap.String("conn", conn.ID), zap.Error(err), zap.String("data", string(data)))
		return
	}

	// 首次收到事件时更新 SelfID（并发安全）。
	if sid := evt.ResolveSelfID(); sid != "" {
		conn.SetSelfID(sid)
	}

	if evt.Type == "meta" {
		s.logger.Info("gateway: 元事件",
			zap.String("conn", conn.ID),
			zap.String("impl", evt.Impl),
			zap.String("platform", evt.Platform),
			zap.String("self_id", evt.ResolveSelfID()),
		)
		if evt.Impl != "" {
			conn.SetImpl(evt.Impl)
		}
		return
	}

	msg := NormalizeV12(conn.ID, &evt, conn.Platform)
	if msg == nil {
		return // 非消息事件，暂不处理
	}

	s.logger.Info("gateway: OB12 消息",
		zap.String("conn", conn.ID),
		zap.String("user", msg.UserID),
		zap.String("group", msg.GroupID),
		zap.Bool("is_group", msg.IsGroup),
		zap.Int("content_len", len(msg.Content)),
	)

	if s.handler != nil {
		s.handler.OnMessage(msg)
	} else {
		s.logger.Warn("gateway: handler 未设置，消息被丢弃", zap.String("conn", conn.ID))
	}
}

func (s *Server) handleV11Message(conn *Connection, data []byte) {
	tag := peekJSONTag(data)
	if tag == "response" {
		var resp ActionResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			s.logger.Error("gateway: OB11 响应解析失败", zap.String("conn", conn.ID), zap.Error(err))
			return
		}
		s.hub.ResolveActionResponse(resp.Echo, &resp)
		// 仅失败时告警。
		if resp.RetCode != 0 || resp.Status == "failed" {
			s.logger.Warn("gateway: OB11 API 失败",
				zap.String("conn", conn.ID),
				zap.String("echo", resp.Echo),
				zap.String("status", resp.Status),
				zap.Int64("retcode", resp.RetCode),
				zap.String("msg", resp.Message),
			)
		}
		return
	}

	var evt EventV11
	if err := json.Unmarshal(data, &evt); err != nil {
		s.logger.Error("gateway: OB11 事件解析失败", zap.String("conn", conn.ID), zap.Error(err), zap.String("data", string(data)))
		return
	}

	// 更新 SelfID（并发安全）。
	if evt.SelfID != 0 {
		conn.SetSelfID(strconv.FormatInt(evt.SelfID, 10))
	}

	if evt.PostType == "meta_event" {
		s.logger.Info("gateway: 元事件", zap.String("conn", conn.ID), zap.Int64("self_id", evt.SelfID))
		return
	}

	msg := NormalizeV11(conn.ID, &evt, conn.Platform)
	if msg == nil {
		s.logger.Debug("gateway: OB11 非消息事件", zap.String("conn", conn.ID), zap.String("post_type", evt.PostType))
		return
	}

	s.logger.Info("gateway: OB11 消息",
		zap.String("conn", conn.ID),
		zap.String("user", msg.UserID),
		zap.String("group", msg.GroupID),
		zap.Bool("is_group", msg.IsGroup),
		zap.Int("content_len", len(msg.Content)),
	)

	if s.handler != nil {
		s.handler.OnMessage(msg)
	} else {
		s.logger.Warn("gateway: handler 未设置，消息被丢弃", zap.String("conn", conn.ID))
	}
}

// peekJSONTag 轻量检测 JSON 帧类型：有 post_type（OB11）或 type（OB12）返回 "event"，
// 有 echo 或 status 返回 "response"，无法识别返回 ""。
// 事件优先检测：OB11 meta_event.heartbeat 也带 status 字段（对象），
// 而真正的 OneBot API 响应中 status 是字符串 "ok"/"failed"。
func peekJSONTag(data []byte) string {
	peek := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &peek); err != nil {
		return ""
	}
	if _, ok := peek["post_type"]; ok {
		return "event"
	}
	if _, ok := peek["type"]; ok {
		return "event"
	}
	if _, ok := peek["echo"]; ok {
		return "response"
	}
	if _, ok := peek["status"]; ok {
		return "response"
	}
	return ""
}

// writeJSON 通过 WS 连接发送 JSON 数据。
func writeJSON(socket *gws.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("gateway: JSON 编码失败: %w", err)
	}
	return socket.WriteMessage(gws.OpcodeText, data)
}

// loadSessionString 从 SessionStorage 读取字符串值。
func loadSessionString(ss gws.SessionStorage, key string) (string, bool) {
	val, ok := ss.Load(key)
	if !ok {
		return "", false
	}
	s, ok := val.(string)
	return s, ok
}
