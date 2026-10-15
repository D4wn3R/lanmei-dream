// 反诈面板接口：违规记录查询、审批处置与概览统计。
//
// 数据来自反诈插件自管的表 antifraud_records；插件未启用时统一返回 503，
// 前端据此提示「插件未启用」。审批与驳回顾名思义属于流程内操作，
// 只要求登录态并全程审计留痕；删除记录为破坏性操作，仅超管 + step-up 可执行。

package handlers

import (
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/DaWesen/lanmei-dream/internal/bizplugin/antifraud"
)

// errAntiFraudDisabled 反诈插件未启用（或未配置推理服务地址）时的统一错误。
var errAntiFraudDisabled = fiber.NewError(fiber.StatusServiceUnavailable, "反诈插件未启用")

// ListAntiFraudRecords 违规记录列表（分页 + 过滤）。权限：登录即可。
func (h *Handler) ListAntiFraudRecords(c fiber.Ctx) error {
	if h.antifraud == nil {
		return errAntiFraudDisabled
	}
	offset, limit := pageQuery(c)
	since, until, err := timeRange(c)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "时间范围参数非法"})
	}

	filter := antifraud.Filter{
		Status:   c.Query("status"),
		Platform: c.Query("platform"),
		GroupID:  c.Query("group_id"),
		UserID:   c.Query("user_id"),
		LabelKey: c.Query("label_key"),
		Keyword:  c.Query("keyword"),
		Offset:   offset,
		Limit:    limit,
	}
	if since > 0 {
		t := time.Unix(since, 0)
		filter.Since = &t
	}
	if until > 0 {
		t := time.Unix(until, 0)
		filter.Until = &t
	}

	list, total, err := h.antifraud.ListRecords(c.Context(), filter)
	if err != nil {
		h.logger.Error("反诈记录查询失败", zapErr(err))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "违规记录加载失败"})
	}
	return c.JSON(fiber.Map{"items": list, "total": total})
}

// GetAntiFraudRecord 违规记录详情。权限：登录即可。
func (h *Handler) GetAntiFraudRecord(c fiber.Ctx) error {
	if h.antifraud == nil {
		return errAntiFraudDisabled
	}
	id, err := parseIDParam(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "记录 ID 非法"})
	}
	rec, err := h.antifraud.GetRecord(c.Context(), id)
	if errors.Is(err, antifraud.ErrRecordNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "违规记录不存在"})
	}
	if err != nil {
		h.logger.Error("反诈记录详情查询失败", zapErr(err))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "违规记录加载失败"})
	}
	return c.JSON(fiber.Map{"record": rec})
}

// AntiFraudStats 概览统计（待审批数、今日计数、动作失败数、近 7 天分布）。权限：登录即可。
func (h *Handler) AntiFraudStats(c fiber.Ctx) error {
	if h.antifraud == nil {
		return errAntiFraudDisabled
	}
	stats, err := h.antifraud.Stats(c.Context())
	if err != nil {
		h.logger.Error("反诈统计查询失败", zapErr(err))
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "违规统计加载失败"})
	}
	return c.JSON(stats)
}

// ApproveAntiFraudRecord 审批通过：执行撤回与禁言，返回含动作结果的最新记录。权限：登录即可。
func (h *Handler) ApproveAntiFraudRecord(c fiber.Ctx) error {
	if h.antifraud == nil {
		return errAntiFraudDisabled
	}
	admin := currentAdmin(c)
	id, err := parseIDParam(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "记录 ID 非法"})
	}
	var req struct {
		MuteSeconds int    `json:"mute_seconds"`
		Note        string `json:"note"`
	}
	if err := h.bind(c, &req); err != nil {
		return err
	}

	rec, err := h.antifraud.Approve(c.Context(), id, admin.Username, antifraud.ApprovalOptions{
		MuteSeconds: req.MuteSeconds,
		Note:        req.Note,
	})
	if errors.Is(err, antifraud.ErrRecordNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "违规记录不存在"})
	}
	if err != nil {
		h.auditDeny(c, admin, "antifraud.approve", "antifraud_record", c.Params("id"), err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "处置执行失败"})
	}
	h.auditOK(c, admin, "antifraud.approve", "antifraud_record", c.Params("id"),
		jsonDetail(fiber.Map{"recall": rec.RecallStatus, "mute": rec.MuteStatus, "note": req.Note}))
	return c.JSON(fiber.Map{"ok": true, "record": rec})
}

// RejectAntiFraudRecord 审批驳回（误报）：只标记结论，不执行处置。权限：登录即可。
func (h *Handler) RejectAntiFraudRecord(c fiber.Ctx) error {
	if h.antifraud == nil {
		return errAntiFraudDisabled
	}
	admin := currentAdmin(c)
	id, err := parseIDParam(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "记录 ID 非法"})
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := h.bind(c, &req); err != nil {
		return err
	}

	rec, err := h.antifraud.Reject(c.Context(), id, admin.Username, req.Note)
	if errors.Is(err, antifraud.ErrRecordNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "违规记录不存在"})
	}
	if err != nil {
		h.auditDeny(c, admin, "antifraud.reject", "antifraud_record", c.Params("id"), err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "驳回操作失败"})
	}
	h.auditOK(c, admin, "antifraud.reject", "antifraud_record", c.Params("id"), req.Note)
	return c.JSON(fiber.Map{"ok": true, "record": rec})
}

// DeleteAntiFraudRecord 删除违规记录。权限：仅 super + step-up。
func (h *Handler) DeleteAntiFraudRecord(c fiber.Ctx) error {
	if h.antifraud == nil {
		return errAntiFraudDisabled
	}
	admin := currentAdmin(c)
	id, err := parseIDParam(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "记录 ID 非法"})
	}
	if err := h.antifraud.DeleteRecord(c.Context(), id); err != nil {
		h.auditDeny(c, admin, "antifraud.delete", "antifraud_record", c.Params("id"), err.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "违规记录删除失败"})
	}
	h.auditOK(c, admin, "antifraud.delete", "antifraud_record", c.Params("id"), "")
	return c.JSON(fiber.Map{"ok": true})
}

// parseIDParam 解析路径中的无符号整型 ID（上限 32 位，避免溢出）。
func parseIDParam(raw string) (uint, error) {
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint(id), nil
}
