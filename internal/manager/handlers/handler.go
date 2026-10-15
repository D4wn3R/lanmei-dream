// Package handlers 提供管理面板 HTTP API 处理器（Fiber v3）。
//
// 安全约定（零信任）：敏感操作（管理员管理 / LLM Provider / Conduit 编辑）
// 必须 Auth + RequireRole(super) + RequireStepUp 三重校验；所有写操作经
// audit.Record 全量留痕；认证相关错误统一泛化，避免账号枚举。
package handlers

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/ai/prompt"
	"github.com/DaWesen/lanmei-dream/internal/ai/skill"
	"github.com/DaWesen/lanmei-dream/internal/bizplugin/antifraud"
	"github.com/DaWesen/lanmei-dream/internal/bot"
	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/kb"
	"github.com/DaWesen/lanmei-dream/internal/manager/audit"
	"github.com/DaWesen/lanmei-dream/internal/manager/auth"
	"github.com/DaWesen/lanmei-dream/internal/manager/billing"
	"github.com/DaWesen/lanmei-dream/internal/manager/control"
	"github.com/DaWesen/lanmei-dream/internal/manager/middleware"
	"github.com/DaWesen/lanmei-dream/internal/manager/store"
	"github.com/DaWesen/lanmei-dream/internal/manager/trace"
	"github.com/DaWesen/lanmei-dream/internal/model"
	"github.com/DaWesen/lanmei-dream/internal/plugin"
)

// Handler 聚合管理面板各 API 处理器依赖。
type Handler struct {
	store     *store.Store
	authSvc   *auth.Service
	auditLog  *audit.Log
	control   *control.Controller
	traceCol  *trace.Collector
	billing   *billing.TokenAccounting
	llmMgr    *llm.ProviderManager
	bot       *bot.Bot
	logger    *zap.Logger
	skills    *skill.Manager   // Skill 系统（内容管理；nil = 未启用）
	prompts   *prompt.Manager  // Prompt 系统（内容管理；nil = 未启用）
	knowledge *kb.Service      // 知识库系统（内容管理；nil = 未启用）
	wasm      *plugin.WasmManager // Wasm 插件管理器（nil = 未启用）
	commands  *command.System  // 命令系统（只读展示）
	antifraud *antifraud.Plugin // 反诈插件（违规记录与审批；nil = 未启用）
}

// Options Handler 依赖注入参数。
type Options struct {
	Store     *store.Store
	Auth      *auth.Service
	Audit     *audit.Log
	Control   *control.Controller
	Trace     *trace.Collector
	Billing   *billing.TokenAccounting
	LLMMgr    *llm.ProviderManager
	Bot       *bot.Bot
	Logger    *zap.Logger
	Skills    *skill.Manager
	Prompts   *prompt.Manager
	Knowledge *kb.Service
	Wasm      *plugin.WasmManager
	Commands  *command.System
	AntiFraud *antifraud.Plugin
}

// New 创建 Handler。
// Logger 为空时回退为 Nop Logger；Skills/Prompts/Knowledge/Wasm 为 nil 时，
// 对应内容管理接口返回不可用错误而非 panic。
func New(opts Options) *Handler {
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	return &Handler{
		store:     opts.Store,
		authSvc:   opts.Auth,
		auditLog:  opts.Audit,
		control:   opts.Control,
		traceCol:  opts.Trace,
		billing:   opts.Billing,
		llmMgr:    opts.LLMMgr,
		bot:       opts.Bot,
		logger:    opts.Logger,
		skills:    opts.Skills,
		prompts:   opts.Prompts,
		knowledge: opts.Knowledge,
		wasm:      opts.Wasm,
		commands:  opts.Commands,
		antifraud: opts.AntiFraud,
	}
}

// bind 解析 JSON 请求体并统一报错。
func (h *Handler) bind(c fiber.Ctx, out any) error {
	if err := c.Bind().Body(out); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "请求参数非法"})
	}
	return nil
}

// audit 写入审计日志（result: ok/deny/error）。
func (h *Handler) audit(c fiber.Ctx, admin *model.ManagerAdmin, action, targetType, targetID, detail, result string) {
	if admin == nil {
		return
	}
	h.auditLog.Record(c.Context(), &admin.ID, admin.Username, action, targetType, targetID, detail, c.IP(), result)
}

// auditOK 记录成功审计。
func (h *Handler) auditOK(c fiber.Ctx, admin *model.ManagerAdmin, action, targetType, targetID, detail string) {
	h.audit(c, admin, action, targetType, targetID, detail, "ok")
}

// auditDeny 记录拒绝审计。
func (h *Handler) auditDeny(c fiber.Ctx, admin *model.ManagerAdmin, action, targetType, targetID, detail string) {
	h.audit(c, admin, action, targetType, targetID, detail, "deny")
}

// pageQuery 解析分页参数（默认 page=1, page_size=20，上限 100）。
func pageQuery(c fiber.Ctx) (offset, limit int) {
	page, _ := strconv.Atoi(c.Query("page", "1"))
	size, _ := strconv.Atoi(c.Query("page_size", "20"))
	if page < 1 {
		page = 1
	}
	// page 上限防深分页与 offset 溢出（(page-1)*size 为负会生成非法 SQL）
	if page > 10000 {
		page = 10000
	}
	if size < 1 || size > 100 {
		size = 20
	}
	return (page - 1) * size, size
}

// timeRange 解析 since/until 查询参数（秒级时间戳或 RFC3339；缺省返回 0，由调用方按最近 24h 兜底）。
func timeRange(c fiber.Ctx) (since, until int64, err error) {
	if s := c.Query("since"); s != "" {
		since, err = parseUnixOrRFC3339(s)
		if err != nil {
			return 0, 0, err
		}
	}
	if u := c.Query("until"); u != "" {
		until, err = parseUnixOrRFC3339(u)
		if err != nil {
			return 0, 0, err
		}
	}
	return since, until, nil
}

// parseUnixOrRFC3339 兼容秒级时间戳与 RFC3339 格式。
func parseUnixOrRFC3339(s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	t, err := timeParse(s)
	if err != nil {
		return 0, errors.New("时间格式非法（支持秒级时间戳或 RFC3339）")
	}
	return t.Unix(), nil
}

// timeParse RFC3339 时间解析。
func timeParse(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// zapErr 便捷包装 zap.Error 字段（日志调用处避免重复 import）。
func zapErr(err error) zap.Field {
	return zap.Error(err)
}

// jsonDetail 将任意结构序列化为审计详情文本（失败返回空串）。
func jsonDetail(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// clientUA 获取客户端 User-Agent。
func clientUA(c fiber.Ctx) string {
	return strings.TrimSpace(c.Get("User-Agent"))
}

// currentAdmin 获取当前登录管理员（middleware.Auth 已注入）。
func currentAdmin(c fiber.Ctx) *model.ManagerAdmin {
	return middleware.GetAdmin(c)
}
