package gateway

import (
	"context"
	"fmt"
	"time"
)

// defaultActionTimeout 是等待平台动作响应（SendToWait）的默认上限。
const defaultActionTimeout = 5 * time.Second

// Actions 把撤回、禁言、主动发送等 OneBot 动作封装成协议无关、可确认成败的调用。
//
// 与 Hub 的 Send* 的区别：Send* 面向「回复触发消息」，目标由消息上下文决定；
// Actions 面向「动作与结果」，目标由调用方指定（群 / 私聊用户），并返回平台动作响应，
// 供调用方如实记录成败（如非群管理员导致的权限不足）。
//
// 注意：所有方法并发安全；connID 已失效（如网关重启、连接漂移）时按平台回退到同平台的
// 任一活跃连接，因此调用方应尽量提供 platform。
type Actions struct {
	hub     *Hub
	timeout time.Duration
}

// NewActions 创建出站动作适配器。
func NewActions(hub *Hub) *Actions {
	return &Actions{hub: hub, timeout: defaultActionTimeout}
}

// resolve 定位目标连接：优先 connID，失效时回退到同平台的任一活跃连接。
func (a *Actions) resolve(platform, connID string) (*Connection, error) {
	if connID != "" {
		if conn, ok := a.hub.Get(connID); ok {
			return conn, nil
		}
	}
	for _, conn := range a.hub.All() {
		if platform == "" || string(conn.Platform) == platform {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("gateway: 无可用连接（platform=%s conn=%s）", platform, connID)
}

// waitTimeout 计算本次等待上限：不晚于 ctx 截止时间；ctx 已取消时直接返回错误。
func (a *Actions) waitTimeout(ctx context.Context) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	timeout := a.timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remain := time.Until(deadline); remain < timeout {
			timeout = remain
		}
	}
	return timeout, nil
}

// RecallMessage 撤回消息。
//
// 参数：
//   - platform：平台标识，connID 失效时按平台回退（可为空）
//   - connID：目标连接 ID
//   - messageID：平台消息 ID
//
// 返回：平台动作响应；连接不可用、ctx 取消或等待超时返回错误。
//
// 注意：撤回他人消息需机器人具备群管理员/群主权限，权限不足等失败原因由响应的
// RetCode / Message 表达，调用方应原样记录而非自行推断。
func (a *Actions) RecallMessage(ctx context.Context, platform, connID, messageID string) (*ActionResponse, error) {
	timeout, err := a.waitTimeout(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := a.resolve(platform, connID)
	if err != nil {
		return nil, err
	}
	req := BuildDeleteMsgV12(messageID)
	if conn.Protocol == ProtocolV11 {
		req = BuildDeleteMsgV11(messageID)
	}
	return a.hub.SendToWait(conn.ID, req, timeout)
}

// BanGroupMember 禁言群成员，seconds 为 0 表示解除禁言。
//
// 参数：
//   - platform：平台标识，connID 失效时按平台回退（可为空）
//   - connID：目标连接 ID
//   - groupID：群 ID
//   - userID：目标成员 ID
//   - seconds：禁言时长（秒）
//
// 返回：平台动作响应；连接不可用、ctx 取消或等待超时返回错误。
//
// 注意：需机器人具备群管理员/群主权限，且不能禁言群主；失败原因同样由响应表达。
func (a *Actions) BanGroupMember(ctx context.Context, platform, connID, groupID, userID string, seconds int) (*ActionResponse, error) {
	timeout, err := a.waitTimeout(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := a.resolve(platform, connID)
	if err != nil {
		return nil, err
	}
	req := BuildSetGroupBanV12(groupID, userID, seconds)
	if conn.Protocol == ProtocolV11 {
		req = BuildSetGroupBanV11(groupID, userID, seconds)
	}
	return a.hub.SendToWait(conn.ID, req, timeout)
}

// SendTextTo 主动发送纯文本消息到指定群或私聊用户（不受触发消息限制）。
//
// 参数：
//   - platform：平台标识，connID 失效时按平台回退（可为空）
//   - connID：目标连接 ID
//   - isGroup：true 发送到群（targetID 为群 ID），false 发送私聊（targetID 为用户 ID）
//   - targetID：群 ID 或用户 ID
//   - text：正文文本
//
// 返回：连接不可用或写帧失败时返回错误（不等待平台响应）。
func (a *Actions) SendTextTo(ctx context.Context, platform, connID string, isGroup bool, targetID, text string) error {
	conn, err := a.resolve(platform, connID)
	if err != nil {
		return err
	}
	msg := &NormalizedMessage{IsGroup: isGroup}
	if isGroup {
		msg.GroupID = targetID
	} else {
		msg.UserID = targetID
	}
	return a.hub.SendSegments(conn.ID, msg, []NormalizedSegment{{Type: "text", Text: text}})
}

// SendAtTextTo 在群里发送「@成员 + 文本」的公开消息。
//
// 参数：
//   - platform：平台标识，connID 失效时按平台回退（可为空）
//   - connID：目标连接 ID
//   - groupID：目标群 ID
//   - atUserID：被 at 的成员 ID；为空时退化为纯文本
//   - text：正文文本
//
// 返回：连接不可用或写帧失败时返回错误（不等待平台响应）。
//
// 注意：at 段与正文之间的空格由 Hub.SendSegments 统一补齐，调用方无需自行添加。
func (a *Actions) SendAtTextTo(ctx context.Context, platform, connID, groupID, atUserID, text string) error {
	conn, err := a.resolve(platform, connID)
	if err != nil {
		return err
	}
	segs := make([]NormalizedSegment, 0, 2)
	if atUserID != "" {
		segs = append(segs, NormalizedSegment{Type: "at", Data: map[string]string{"user_id": atUserID}})
	}
	segs = append(segs, NormalizedSegment{Type: "text", Text: text})
	return a.hub.SendSegments(conn.ID, &NormalizedMessage{IsGroup: true, GroupID: groupID}, segs)
}
