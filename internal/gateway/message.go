package gateway

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// MessageSegmentV12 表示 OneBot 12 消息段。
//
// 注意：at 段用 user_id 标识目标（OneBot 11 用 qq），引用段用 message_id
// （OneBot 11 用 id）；Data 内的值经 ParseSegmentsV12 统一转为字符串。
type MessageSegmentV12 struct {
	Type string         `json:"type"` // text / image / at / reply / ...
	Data map[string]any `json:"data"`
}

// TextContent 提取消息段的纯文本内容。
//
// 返回：text 段返回 text 字段原文；at 段返回 "@" + user_id（字段非字符串时返回空）；
// 其余类型返回空字符串。
func (m MessageSegmentV12) TextContent() string {
	switch m.Type {
	case "text":
		if s, ok := m.Data["text"].(string); ok {
			return s
		}
	case "at":
		if uid, ok := m.Data["user_id"].(string); ok {
			return "@" + uid
		}
	}
	return ""
}

// ExtractPlainTextV12 从 OneBot 12 消息段列表提取纯文本。
//
// 参数：
//   - segments：消息段列表，可为 nil
//
// 返回：各段文本表示（见 TextContent）按原顺序无分隔拼接；无文本时返回空字符串。
func ExtractPlainTextV12(segments []MessageSegmentV12) string {
	var parts []string
	for _, seg := range segments {
		if t := seg.TextContent(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "")
}

// MessageSegmentV11 表示 OneBot 11 消息段（NapCat 方言）。
//
// 注意：at 段目标在 qq 字段（OneBot 12 为 user_id），值可能是 JSON number 或 string；
// 引用段用 id 字段（OneBot 12 为 message_id）。Data 内的值经 ParseSegmentsV11 统一转字符串。
type MessageSegmentV11 struct {
	Type string         `json:"type"` // text / image / at / face / reply / ...
	Data map[string]any `json:"data"`
}

// TextContent 提取消息段的纯文本内容。
//
// 返回：text 段返回 text 字段原文；at 段优先取 name（昵称）、其次 qq，
// 两者均缺失时返回空字符串；其余类型返回空字符串。
func (m MessageSegmentV11) TextContent() string {
	switch m.Type {
	case "text":
		if s, ok := m.Data["text"].(string); ok {
			return s
		}
	case "at":
		// NapCat 的 at 消息段，qq 字段可能是 number 或 string；昵称优先（name 字段）
		if name := m.Data["name"]; name != nil {
			return "@" + formatIntOrString(name)
		}
		if qq := m.Data["qq"]; qq != nil {
			return "@" + formatIntOrString(qq)
		}
	}
	return ""
}

// ExtractPlainTextV11 从 OneBot 11 消息段列表提取纯文本。
//
// 参数：
//   - segments：消息段列表，可为 nil
//
// 返回：各段文本表示（见 TextContent）按原顺序无分隔拼接；无文本时返回空字符串。
func ExtractPlainTextV11(segments []MessageSegmentV11) string {
	var parts []string
	for _, seg := range segments {
		if t := seg.TextContent(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "")
}

// MessageType 事件类型。
type MessageType string

const (
	// MessageTypeMessage 普通消息事件：含完整消息段，是机器人回复的主要触发源。
	MessageTypeMessage MessageType = "message" // 普通消息
	// MessageTypeNotice 系统通知事件（进群/退群/撤回/禁言等）；白名单见 notice.go。
	MessageTypeNotice MessageType = "notice" // 系统通知
	// MessageTypeRequest 请求事件（好友申请 / 加群邀请）；仅标准化透传给上层，网关不自动处理。
	MessageTypeRequest MessageType = "request" // 请求（好友/加群）
)

// NormalizedSegment 跨协议标准化消息段。
// 保留 type / mime type / data，供需要结构化信息的消费方（MediaPass、Topic 提及检测等）使用。
type NormalizedSegment struct {
	Type     string            // text / image / at / face / reply / file / audio / video / record / ...
	MimeType string            // 多媒体 MIME 类型（image/png、audio/ogg...），text/at 段为空
	Data     map[string]string // 平台原始 data（file_id / file / url / path / text / qq / name / ...）
	Text     string            // 该段的纯文本表示（text 内容 / @昵称 / 空）
}

// NormalizedMessage 是跨平台标准化后的消息，供 bot 层消费。
type NormalizedMessage struct {
	Platform   Platform // 来源平台
	Protocol   Protocol // 协议版本
	SelfID     string   // 机器人自身 ID
	UserID     string   // 平台用户 ID（字符串，跨平台）；notice 事件为被操作者
	GroupID    string   // 平台群 ID（空字符串 = 私聊）
	IsGroup    bool     // 是否群消息
	Content    string   // 纯文本内容（所有 text 段拼接，at 段转 "@昵称"）
	SenderName string   // 发送者昵称
	MessageID  string   // 消息 ID
	ConnID     string   // 来源连接 ID（用于回复路由）
	// ReceivedAt 消息到达时间（由事件时间戳填充；用于"回复前会话是否已有新消息"判定）
	ReceivedAt time.Time

	// 多模态段由 message 事件填充；事件字段由 notice/request 事件填充（普通消息为空）
	Segments     []NormalizedSegment // 完整段列表
	AtTargets    []string            // at 目标 user_id 列表
	MimeTypes    []string            // 去重后的 MIME 类型列表
	ImageURLs    []string            // 去重后的图片段 url 列表（无 url 的图片段不收录）
	MessageType  MessageType         // message / notice / request
	EventType    string              // 规范化事件类型（见 notice.go；普通消息为空）
	EventSubType string              // 事件子类型（透传原始 sub_type，可为空）
	EventData    map[string]any      // 事件全字段（普通消息为 nil）
}

// NormalizeV12 将 OneBot 12 事件标准化为 NormalizedMessage。
// 支持 message / notice / request 三类事件；meta 事件返回 nil。
//
// 参数：
//   - connID：来源连接 ID，写入 NormalizedMessage.ConnID 用于回复路由
//   - evt：OneBot 12 事件；Type 为空视同 meta 事件
//   - platform：连接配置的平台兜底值；事件自带 platform 字段非空时以事件为准
//
// 返回：标准化消息；Type 为空 / meta / 未知，以及 notice 白名单（见 notice.go）
// 之外的事件返回 nil。
//
// 注意：消息事件的纯文本优先取 alt_message，缺失时才回退为消息段提取（at 段转
// "@user_id" 而非昵称）；notice / request 事件不解析消息段。
func NormalizeV12(connID string, evt *EventV12, platform Platform) *NormalizedMessage {
	if evt.Type == "" || evt.Type == "meta" {
		return nil
	}

	// 事件平台字段有效时优先使用事件中的平台标识
	p := platform
	if evt.Platform != "" {
		p = Platform(evt.Platform)
	}

	msg := &NormalizedMessage{
		Platform:  p,
		Protocol:  ProtocolV12,
		SelfID:    evt.ResolveSelfID(),
		UserID:    evt.UserID,
		GroupID:   evt.GroupID,
		IsGroup:   evt.GroupID != "",
		MessageID: evt.ID,
		ConnID:    connID,
	}
	// 事件时间戳（Unix 秒，浮点保留亚秒精度）填充到达时间。
	if evt.Time > 0 {
		sec := int64(evt.Time)
		nsec := int64((evt.Time - float64(sec)) * 1e9)
		msg.ReceivedAt = time.Unix(sec, nsec)
	}

	switch evt.Type {
	case "message":
		msg.MessageType = MessageTypeMessage
		msg.Segments = ParseSegmentsV12(evt.Message)
		msg.Content = evt.AltMessage
		if msg.Content == "" {
			msg.Content = ExtractPlainTextV12(evt.Message)
		}
		collectSegmentMeta(msg, msg.Segments)
		msg.SenderName = "" // OB12 消息事件不含 sender 昵称，需从 sender 子对象获取（如有）

	case "notice":
		// 通知事件：仅接收白名单内的事件类型（见 notice.go），白名单外返回 nil。
		eventType, subType, data, ok := normalizeNoticeV12(evt)
		if !ok {
			return nil
		}
		msg.MessageType = MessageTypeNotice
		msg.EventType = eventType
		msg.EventSubType = subType
		msg.EventData = data

	case "request":
		msg.MessageType = MessageTypeRequest
		msg.EventType = evt.DetailType
		msg.EventSubType = evt.SubType
		msg.EventData = map[string]any{
			"user_id":     evt.UserID,
			"group_id":    evt.GroupID,
			"sub_type":    evt.SubType,
			"detail_type": evt.DetailType,
		}

	default:
		return nil
	}

	return msg
}

// NormalizeV11 将 OneBot 11 事件标准化为 NormalizedMessage。
// 支持 message / notice / request 三类事件；meta_event 返回 nil。
//
// 参数：
//   - connID：来源连接 ID，写入 NormalizedMessage.ConnID 用于回复路由
//   - evt：OneBot 11 事件；PostType 为空视同 meta_event
//   - platform：连接配置的平台标识（OneBot 11 事件不含平台字段）
//
// 返回：标准化消息；PostType 为空 / meta_event / 未知，以及 notice 白名单
// （见 notice.go）之外的事件返回 nil。
//
// 注意：
//   - 群聊判定以 message_type == "group" 为主，message_type 缺失而 group_id 非 0
//     时兜底判为群聊。
//   - message 字段为空时回退 raw_message（CQ 码）解析；Content 一律取纯文本
//     （at 段转 "@昵称"），段解析彻底失败时才兜底保留 raw_message 原文。
func NormalizeV11(connID string, evt *EventV11, platform Platform) *NormalizedMessage {
	if evt.PostType == "" || evt.PostType == "meta_event" {
		return nil
	}

	msg := &NormalizedMessage{
		Platform: platform,
		Protocol: ProtocolV11,
		SelfID:   strconv.FormatInt(evt.SelfID, 10),
		ConnID:   connID,
	}
	// 事件时间戳（Unix 秒）填充到达时间。
	if evt.Time > 0 {
		msg.ReceivedAt = time.Unix(evt.Time, 0)
	}

	switch evt.PostType {
	case "message":
		msg.MessageType = MessageTypeMessage
		msg.UserID = strconv.FormatInt(evt.UserID, 10)
		if evt.MessageType == "group" {
			msg.GroupID = strconv.FormatInt(evt.GroupID, 10)
			msg.IsGroup = true
		}
		// 群聊兜底：message_type 缺失时 group_id 非 0 也判为群聊（notice 事件不经过此分支）
		if !msg.IsGroup && evt.GroupID != 0 {
			msg.GroupID = strconv.FormatInt(evt.GroupID, 10)
			msg.IsGroup = true
		}
		segs := ParseSegmentsV11(evt.ParseMessageSegments())
		// NapCat 部分配置下 message 字段为空字符串：回退用 raw_message（CQ 码）解析，
		// 确保 at 目标（平台 ID）与纯文本内容可被下游消费。
		if len(segs) == 0 && evt.RawMessage != "" {
			segs = ParseSegmentsV11(ParseCQSegmentsV11(evt.RawMessage))
		}
		msg.Segments = segs
		// Content 一律使用纯文本（at 段转为 "@昵称"）：CQ 码内的 name= 会污染意图分析、
		// 话题提及检测与记忆层，导致昵称误命中。纯媒体消息不 fallback 为 CQ 码，交媒体管线处理。
		msg.Content = ExtractNormalizedTextV11(segs)
		if msg.Content == "" && len(segs) == 0 && evt.RawMessage != "" {
			msg.Content = evt.RawMessage // 段解析彻底失败时兜底保留原文，避免消息被丢弃
		}
		collectSegmentMeta(msg, segs)

		senderName := evt.Sender.Nickname
		if msg.IsGroup && evt.Sender.Card != "" {
			senderName = evt.Sender.Card
		}
		msg.SenderName = senderName
		// message_id 缺失/为 0 时置空串：Deduper 对空串放行，避免所有消息共享键 "0" 被误判重复丢弃
		if evt.MessageID != 0 {
			msg.MessageID = strconv.FormatInt(evt.MessageID, 10)
		}

	case "notice":
		// 通知事件：仅接收白名单内的事件类型（见 notice.go），白名单外返回 nil。
		eventType, subType, data, ok := normalizeNoticeV11(evt)
		if !ok {
			return nil
		}
		msg.MessageType = MessageTypeNotice
		msg.UserID = strconv.FormatInt(evt.UserID, 10)
		msg.GroupID = strconv.FormatInt(evt.GroupID, 10)
		msg.IsGroup = evt.GroupID != 0
		msg.EventType = eventType
		msg.EventSubType = subType
		msg.EventData = data

	case "request":
		msg.MessageType = MessageTypeRequest
		msg.UserID = strconv.FormatInt(evt.UserID, 10)
		msg.GroupID = strconv.FormatInt(evt.GroupID, 10)
		msg.IsGroup = evt.GroupID != 0
		msg.EventType = evt.RequestType
		msg.EventSubType = evt.SubType
		msg.EventData = map[string]any{
			"user_id":      strconv.FormatInt(evt.UserID, 10),
			"group_id":     strconv.FormatInt(evt.GroupID, 10),
			"sub_type":     evt.SubType,
			"request_type": evt.RequestType,
		}

	default:
		return nil
	}

	return msg
}

// ParseSegmentsV12 将 OneBot 12 消息段列表解析为标准化段。
// 多媒体段的 MIME 类型按文件扩展名推断；at 段的 Text 为 "@昵称"（无昵称时用 user_id）。
//
// 参数：
//   - segs：OneBot 12 消息段列表，可为 nil
//
// 返回：与输入等长的标准化段列表；Data 内数值 / 布尔值统一转为字符串，
// 输入为空时返回空切片（非 nil）。
//
// 注意：at 段以 user_id 为目标标识；face 段的 Text 固定为 "[表情]"；其余类型仅保留
// 原始 Data，不做文本化。
func ParseSegmentsV12(segs []MessageSegmentV12) []NormalizedSegment {
	result := make([]NormalizedSegment, 0, len(segs))
	for _, seg := range segs {
		data := toStringMap(seg.Data)
		ns := NormalizedSegment{Type: seg.Type, Data: data}
		switch seg.Type {
		case "text":
			ns.Text = data["text"]
		case "image", "audio", "video", "file", "record", "voice":
			ns.MimeType = DetectMimeByExt(seg.Type, data["file"], data["url"])
		case "at":
			ns.Text = "@" + atDisplayName(data["user_id"], data["name"])
		case "face":
			ns.Text = "[表情]"
		}
		result = append(result, ns)
	}
	return result
}

// ParseSegmentsV11 将 OneBot 11 消息段列表解析为标准化段。
// at 段的 user_id 同时兼容 JSON number 与 string 两种编码。
//
// 参数：
//   - segs：OneBot 11 消息段列表，可为 nil
//
// 返回：与输入等长的标准化段列表；Data 内数值 / 布尔值统一转为字符串，
// 输入为空时返回空切片（非 nil）。
//
// 注意：at 段的目标 ID 优先取 qq 字段、缺失时回退 user_id，并回写 Data["user_id"]，
// Text 为 "@昵称"（无昵称时用目标 ID）；CQ 码字符串需先经 ParseCQSegmentsV11 解析。
func ParseSegmentsV11(segs []MessageSegmentV11) []NormalizedSegment {
	result := make([]NormalizedSegment, 0, len(segs))
	for _, seg := range segs {
		data := toStringMap(seg.Data)
		ns := NormalizedSegment{Type: seg.Type, Data: data}
		switch seg.Type {
		case "text":
			ns.Text = data["text"]
		case "image", "audio", "video", "file", "record", "voice":
			ns.MimeType = DetectMimeByExt(seg.Type, data["file"], data["url"])
		case "at":
			uid := data["qq"]
			if uid == "" {
				uid = data["user_id"]
			}
			data["user_id"] = uid
			ns.Text = "@" + atDisplayName(uid, data["name"])
		case "face":
			ns.Text = "[表情]"
		}
		result = append(result, ns)
	}
	return result
}

// ParseCQSegmentsV11 将 OneBot 11 的 CQ 码字符串（raw_message）解析为消息段列表。
//
// 支持文本与 CQ 码混排（如 "你好[CQ:at,qq=123,name=张三]在吗"）。
// 参数值中的转义（&#44; 逗号、&#91; [、&#93; ]、&amp; &）在解析时还原。
//
// 参数：
//   - raw：CQ 码原始文本，可为空
//
// 返回：消息段列表；纯文本片段生成 text 段，空文本片段跳过，raw 为空时返回 nil。
//
// 注意：未闭合的 "[CQ:" 按普通文本处理，避免吞掉后续内容；转义还原为单遍替换，
// "&amp;#91;" 还原为 "&#91;" 而非 "["。
func ParseCQSegmentsV11(raw string) []MessageSegmentV11 {
	var segs []MessageSegmentV11
	rest := raw
	for len(rest) > 0 {
		start := strings.Index(rest, "[CQ:")
		if start < 0 {
			// 剩余为纯文本：文本中的 [ / ] / & 均以转义形式出现，不会误命中 CQ 码前缀。
			if t := unescapeCQText(rest); t != "" {
				segs = append(segs, MessageSegmentV11{Type: "text", Data: map[string]any{"text": t}})
			}
			break
		}
		if t := unescapeCQText(rest[:start]); t != "" {
			segs = append(segs, MessageSegmentV11{Type: "text", Data: map[string]any{"text": t}})
		}
		end := strings.IndexByte(rest[start:], ']')
		if end < 0 {
			// 未闭合的 CQ 码按文本处理，避免吞掉后续内容
			if t := unescapeCQText(rest[start:]); t != "" {
				segs = append(segs, MessageSegmentV11{Type: "text", Data: map[string]any{"text": t}})
			}
			break
		}
		end += start
		if seg := parseCQSegment(rest[start : end+1]); seg.Type != "" {
			segs = append(segs, seg)
		}
		rest = rest[end+1:]
	}
	return segs
}

// parseCQSegment 解析单个 CQ 码为消息段。
// 格式：[CQ:type,key1=value1,key2=value2]
func parseCQSegment(code string) MessageSegmentV11 {
	inner := strings.TrimPrefix(code, "[CQ:")
	inner = strings.TrimSuffix(inner, "]")
	segType, params, hasParams := strings.Cut(inner, ",")
	seg := MessageSegmentV11{Type: segType, Data: map[string]any{}}
	if !hasParams {
		return seg
	}
	// CQ 码参数以字面逗号分隔（值内的逗号被转义为 &#44;，切分后由 unescapeCQText 还原）
	for _, pair := range strings.Split(params, ",") {
		key, val, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			continue
		}
		seg.Data[key] = unescapeCQText(val)
	}
	return seg
}

// cqTextReplacer 还原 CQ 码转义序列。
// 单遍替换避免级联（&amp;#91; 应还原为 &#91; 而非 "["）。
var cqTextReplacer = strings.NewReplacer(
	"&#91;", "[",
	"&#93;", "]",
	"&#44;", ",",
	"&amp;", "&",
)

// unescapeCQText 还原 CQ 码转义序列。
func unescapeCQText(s string) string {
	return cqTextReplacer.Replace(s)
}

// ExtractNormalizedTextV11 从标准化段列表提取纯文本（at 段使用 "@昵称" 表示）。
func ExtractNormalizedTextV11(segs []NormalizedSegment) string {
	var parts []string
	for _, s := range segs {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, "")
}

// collectSegmentMeta 收集 at 目标列表、去重 MIME 类型列表与图片 url 列表写入 msg。
func collectSegmentMeta(msg *NormalizedMessage, segs []NormalizedSegment) {
	seen := make(map[string]struct{})
	for _, s := range segs {
		if s.Type == "at" {
			if uid := s.Data["user_id"]; uid != "" {
				if _, ok := seen["at:"+uid]; !ok {
					seen["at:"+uid] = struct{}{}
					msg.AtTargets = append(msg.AtTargets, uid)
				}
			}
		}
		if s.MimeType != "" {
			if _, ok := seen["mime:"+s.MimeType]; !ok {
				seen["mime:"+s.MimeType] = struct{}{}
				msg.MimeTypes = append(msg.MimeTypes, s.MimeType)
			}
		}
		if s.Type == "image" {
			if u := s.Data["url"]; u != "" {
				if _, ok := seen["img:"+u]; !ok {
					seen["img:"+u] = struct{}{}
					msg.ImageURLs = append(msg.ImageURLs, u)
				}
			}
		}
	}
}

// toStringMap 将任意值 map 转为字符串 map，兼容 JSON number/string/bool。
func toStringMap(data map[string]any) map[string]string {
	if len(data) == 0 {
		return map[string]string{}
	}
	result := make(map[string]string, len(data))
	for k, v := range data {
		result[k] = formatAny(v)
	}
	return result
}

// formatAny 将 JSON 值格式化为字符串（处理 number/string/bool）。
func formatAny(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case bool:
		return strconv.FormatBool(n)
	case json.Number:
		return n.String()
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(n, 10)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// atDisplayName 返回 at 段的显示名（昵称优先，其次 user_id）。
func atDisplayName(userID, name string) string {
	if name != "" {
		return name
	}
	return userID
}

// DetectMimeByExt 根据段类型与 file/url 扩展名推断 MIME 类型。
// 无法推断时返回 ""。
//
// 参数：
//   - segType：消息段类型，仅 image / audio / record / voice / video 参与推断
//   - file：文件名或路径，优先取其扩展名
//   - url：文件 URL，file 无扩展名时回退使用
//
// 返回：MIME 类型字符串（如 image/png、audio/ogg、video/mp4）；无法识别时返回 ""。
func DetectMimeByExt(segType, file, url string) string {
	ext := strings.ToLower(path.Ext(file))
	if ext == "" {
		ext = strings.ToLower(path.Ext(url))
	}
	switch segType {
	case "image":
		switch ext {
		case ".png":
			return "image/png"
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".gif":
			return "image/gif"
		case ".webp":
			return "image/webp"
		case ".bmp":
			return "image/bmp"
		}
	case "audio", "record", "voice":
		switch ext {
		case ".mp3":
			return "audio/mpeg"
		case ".ogg":
			return "audio/ogg"
		case ".wav":
			return "audio/wav"
		case ".flac":
			return "audio/flac"
		case ".m4a":
			return "audio/mp4"
		}
	case "video":
		switch ext {
		case ".mp4":
			return "video/mp4"
		case ".webm":
			return "video/webm"
		}
	}
	return ""
}

// ActionRequest 表示要发送给 OneBot 实现的动作请求。
//
// 注意：OneBot 12 用 send_message 动作（detail_type 区分私聊 / 群聊），OneBot 11 用
// send_private_msg / send_group_msg；由 BuildSendMessage* 系列函数按协议构造。
type ActionRequest struct {
	Action string         `json:"action"`         // 动作名，如 send_message / send_group_msg
	Params map[string]any `json:"params"`         // 动作参数
	Echo   string         `json:"echo,omitempty"` // 请求标识（用于匹配响应）
}

// ActionResponse 表示 OneBot 实现返回的动作响应。
//
// 注意：网关对失败响应（RetCode != 0 或 Status == "failed"）记录告警日志；
// 带 Echo 的响应还会交给 Hub.ResolveActionResponse，配对给 SendToWait 的等待者。
type ActionResponse struct {
	Status  string          `json:"status"`            // ok / failed
	RetCode int64           `json:"retcode"`           // 返回码
	Data    json.RawMessage `json:"data"`              // 响应数据
	Message string          `json:"message,omitempty"` // 错误信息
	Echo    string          `json:"echo,omitempty"`    // 请求标识
}

// Failed 报告动作响应是否为失败（Status 非 "ok" 或 RetCode 非 0）。
func (r *ActionResponse) Failed() bool {
	return r == nil || r.Status != "ok" || r.RetCode != 0
}

// FailureText 返回失败的简短描述（错误信息 + retcode），成功时返回空串。
//
// 返回：形如 "retcode=100 msg=权限不足" 的文本；平台未给出 message 时只有 retcode。
func (r *ActionResponse) FailureText() string {
	if r == nil {
		return "无响应"
	}
	if r.Message != "" {
		return fmt.Sprintf("retcode=%d msg=%s", r.RetCode, r.Message)
	}
	return fmt.Sprintf("retcode=%d", r.RetCode)
}

// BuildSendMessageV12 构建 OneBot 12 send_message 动作（纯文本）。
//
// 参数：
//   - detailType：会话类型，"private" 私聊，其余值按群聊处理
//   - userID：私聊目标用户 ID，仅私聊时写入参数
//   - groupID：群聊目标群 ID，仅群聊时写入参数
//   - text：纯文本内容
//
// 返回：send_message 动作请求（message 为单个 text 段）。
func BuildSendMessageV12(detailType, userID, groupID, text string) *ActionRequest {
	params := map[string]any{
		"detail_type": detailType, // private / group
		"message": []MessageSegmentV12{
			{Type: "text", Data: map[string]any{"text": text}},
		},
	}
	if detailType == "private" {
		params["user_id"] = userID
	} else {
		params["group_id"] = groupID
	}
	return &ActionRequest{
		Action: "send_message",
		Params: params,
	}
}

// BuildSendMessageV11 构建 OneBot 11 send_private_msg / send_group_msg 动作（纯文本）。
//
// 参数：
//   - isGroup：true 发送 send_group_msg 并取 groupID，false 发送 send_private_msg 并取 userID
//   - userID：私聊目标用户 ID（十进制字符串）
//   - groupID：群聊目标群 ID（十进制字符串）
//   - text：纯文本内容
//
// 返回：对应动作请求（message 为单个 text 段）。
//
// 注意：ID 经 strconv.ParseInt 转 int64，非法或为空时按 0 发送，由平台侧报错。
func BuildSendMessageV11(isGroup bool, userID, groupID, text string) *ActionRequest {
	if isGroup {
		gid, _ := strconv.ParseInt(groupID, 10, 64)
		return &ActionRequest{
			Action: "send_group_msg",
			Params: map[string]any{
				"group_id": gid,
				"message": []MessageSegmentV11{
					{Type: "text", Data: map[string]any{"text": text}},
				},
			},
		}
	}
	uid, _ := strconv.ParseInt(userID, 10, 64)
	return &ActionRequest{
		Action: "send_private_msg",
		Params: map[string]any{
			"user_id": uid,
			"message": []MessageSegmentV11{
				{Type: "text", Data: map[string]any{"text": text}},
			},
		},
	}
}

// BuildSendMessageSegmentsV12 构建 OneBot 12 send_message 动作（支持富媒体段）。
//
// 参数：
//   - detailType：会话类型，"private" 私聊，其余值按群聊处理
//   - userID：私聊目标用户 ID，仅私聊时写入参数
//   - groupID：群聊目标群 ID，仅群聊时写入参数
//   - segs：标准化消息段，经 ToMessageSegmentV12 转换；为空时发送空 message 数组
//
// 返回：send_message 动作请求。
func BuildSendMessageSegmentsV12(detailType, userID, groupID string, segs []NormalizedSegment) *ActionRequest {
	params := map[string]any{
		"detail_type": detailType,
		"message":     ToMessageSegmentV12(segs),
	}
	if detailType == "private" {
		params["user_id"] = userID
	} else {
		params["group_id"] = groupID
	}
	return &ActionRequest{Action: "send_message", Params: params}
}

// BuildSendMessageSegmentsV11 构建 OneBot 11 富媒体动作（send_group_msg / send_private_msg）。
//
// 参数：
//   - isGroup：true 发送 send_group_msg，false 发送 send_private_msg
//   - userID：私聊目标用户 ID（十进制字符串），非法或为空按 0 处理
//   - groupID：群聊目标群 ID（十进制字符串），非法或为空按 0 处理
//   - segs：标准化消息段，经 ToMessageSegmentV11 转换（at 段转为 qq 字段）
//
// 返回：对应动作请求。
func BuildSendMessageSegmentsV11(isGroup bool, userID, groupID string, segs []NormalizedSegment) *ActionRequest {
	if isGroup {
		gid, _ := strconv.ParseInt(groupID, 10, 64)
		return &ActionRequest{
			Action: "send_group_msg",
			Params: map[string]any{
				"group_id": gid,
				"message":  ToMessageSegmentV11(segs),
			},
		}
	}
	uid, _ := strconv.ParseInt(userID, 10, 64)
	return &ActionRequest{
		Action: "send_private_msg",
		Params: map[string]any{
			"user_id": uid,
			"message": ToMessageSegmentV11(segs),
		},
	}
}

// BuildDeleteMsgV12 构建 OneBot 12 delete_message 动作（撤回消息）。
//
// 参数：
//   - messageID：平台消息 ID（OneBot 12 为字符串）
//
// 返回：delete_message 动作请求。
func BuildDeleteMsgV12(messageID string) *ActionRequest {
	return &ActionRequest{
		Action: "delete_message",
		Params: map[string]any{"message_id": messageID},
	}
}

// BuildDeleteMsgV11 构建 OneBot 11 delete_msg 动作（撤回消息）。
//
// 参数：
//   - messageID：平台消息 ID（十进制字符串）
//
// 返回：delete_msg 动作请求。
//
// 注意：ID 经 strconv.ParseInt 转 int64，非法或为空时按 0 发送，由平台侧报错。
func BuildDeleteMsgV11(messageID string) *ActionRequest {
	mid, _ := strconv.ParseInt(messageID, 10, 64)
	return &ActionRequest{
		Action: "delete_msg",
		Params: map[string]any{"message_id": mid},
	}
}

// BuildSetGroupBanV12 构建 OneBot 12 set_group_ban 动作（禁言群成员）。
//
// 参数：
//   - groupID：群 ID（字符串）
//   - userID：目标成员 ID（字符串）
//   - seconds：禁言时长（秒），0 表示解除禁言
//
// 返回：set_group_ban 动作请求。
func BuildSetGroupBanV12(groupID, userID string, seconds int) *ActionRequest {
	return &ActionRequest{
		Action: "set_group_ban",
		Params: map[string]any{
			"group_id": groupID,
			"user_id":  userID,
			"duration": seconds,
		},
	}
}

// BuildSetGroupBanV11 构建 OneBot 11 set_group_ban 动作（禁言群成员）。
//
// 参数：
//   - groupID：群 ID（十进制字符串）
//   - userID：目标成员 ID（十进制字符串）
//   - seconds：禁言时长（秒），0 表示解除禁言
//
// 返回：set_group_ban 动作请求。
//
// 注意：ID 经 strconv.ParseInt 转 int64，非法或为空时按 0 发送，由平台侧报错。
func BuildSetGroupBanV11(groupID, userID string, seconds int) *ActionRequest {
	gid, _ := strconv.ParseInt(groupID, 10, 64)
	uid, _ := strconv.ParseInt(userID, 10, 64)
	return &ActionRequest{
		Action: "set_group_ban",
		Params: map[string]any{
			"group_id": gid,
			"user_id":  uid,
			"duration": seconds,
		},
	}
}

// ToMessageSegmentV12 将标准化段转为 OneBot 12 消息段列表。
//
// 参数：
//   - segs：标准化段列表，可为 nil
//
// 返回：与输入等长的 OneBot 12 消息段列表；输入为空时返回空切片（非 nil）。
//
// 注意：at 段输出 user_id（优先 user_id，缺失时回退 qq）；image 段按
// file_id → file → url 顺序取首个非空字段；其余类型（reply / face 等）原样透传 Data，
// 引用段的 message_id（V12）/ id（V11）命名差异由调用方保证。
func ToMessageSegmentV12(segs []NormalizedSegment) []MessageSegmentV12 {
	result := make([]MessageSegmentV12, 0, len(segs))
	for _, s := range segs {
		switch s.Type {
		case "text":
			result = append(result, MessageSegmentV12{Type: "text", Data: map[string]any{"text": s.Text}})
		case "image":
			data := map[string]any{}
			if v := s.Data["file_id"]; v != "" {
				data["file_id"] = v
			} else if v := s.Data["file"]; v != "" {
				data["file"] = v
			} else if v := s.Data["url"]; v != "" {
				data["url"] = v
			}
			result = append(result, MessageSegmentV12{Type: "image", Data: data})
		case "at":
			uid := s.Data["user_id"]
			if uid == "" {
				uid = s.Data["qq"]
			}
			result = append(result, MessageSegmentV12{Type: "at", Data: map[string]any{"user_id": uid}})
		default:
			// 其余类型透传原始 data（值已在 toStringMap 中转为字符串，兼容 number 场景）
			data := make(map[string]any, len(s.Data))
			for k, v := range s.Data {
				data[k] = v
			}
			result = append(result, MessageSegmentV12{Type: s.Type, Data: data})
		}
	}
	return result
}

// ToMessageSegmentV11 将标准化段转为 OneBot 11 消息段列表。
//
// 参数：
//   - segs：标准化段列表，可为 nil
//
// 返回：与输入等长的 OneBot 11 消息段列表；输入为空时返回空切片（非 nil）。
//
// 注意：at 段目标写入 qq 字段（OneBot 12 为 user_id）；image 段 file 优先取
// file、缺失时回退 url（支持 url / base64 data URI / 本地路径）；其余类型原样透传 Data。
func ToMessageSegmentV11(segs []NormalizedSegment) []MessageSegmentV11 {
	result := make([]MessageSegmentV11, 0, len(segs))
	for _, s := range segs {
		switch s.Type {
		case "text":
			result = append(result, MessageSegmentV11{Type: "text", Data: map[string]any{"text": s.Text}})
		case "image":
			// OneBot 11 file 支持 url / base64(data:image/...) / 本地路径
			file := s.Data["file"]
			if file == "" {
				file = s.Data["url"]
			}
			result = append(result, MessageSegmentV11{Type: "image", Data: map[string]any{"file": file}})
		case "at":
			uid := s.Data["user_id"]
			if uid == "" {
				uid = s.Data["qq"]
			}
			result = append(result, MessageSegmentV11{Type: "at", Data: map[string]any{"qq": uid}})
		default:
			data := make(map[string]any, len(s.Data))
			for k, v := range s.Data {
				data[k] = v
			}
			result = append(result, MessageSegmentV11{Type: s.Type, Data: data})
		}
	}
	return result
}

// formatIntOrString 将值格式化为字符串（处理 JSON number 或 string）。
func formatIntOrString(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case string:
		return n
	default:
		return fmt.Sprintf("%v", v)
	}
}
