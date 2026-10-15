package antifraud

import (
	"time"

	"gorm.io/datatypes"
)

const (
	// StatusPending 待审批：违规分达到上报阈值但未达自动处置阈值，等管理员定夺。
	StatusPending = "pending"
	// StatusAutoHandled 已自动处置：违规分达到自动阈值，撤回与禁言已执行。
	StatusAutoHandled = "auto_handled"
	// StatusApproved 审批通过：管理员通过后执行撤回与禁言。
	StatusApproved = "approved"
	// StatusRejected 审批驳回：管理员判定为误报，不做处置。
	StatusRejected = "rejected"
)

const (
	// ActionSkipped 该动作未执行（未达自动阈值，或配置关闭）。
	ActionSkipped = "skipped"
	// ActionOK 动作成功。
	ActionOK = "ok"
	// ActionFailed 动作失败（权限不足、消息已失效、等待响应超时等）。
	ActionFailed = "failed"
)

// Record 是一条反诈违规记录（表 antifraud_records）。
//
// 由插件自行建表（AutoMigrate），不加入主库迁移列表：表是插件私有数据，
// 插件未启用时表可以不存在。
//
// 处置结果分两列记录：recall_* 与 mute_* 保存平台动作的真实结果与原始错误，
// 面板与上报文案直接展示，不对失败原因做推断。
type Record struct {
	ID        uint      `json:"id"         gorm:"primaryKey;autoIncrement;comment:主键"`
	CreatedAt time.Time `json:"created_at" gorm:"index:idx_antifraud_status_created,priority:2;index:idx_antifraud_group_created,priority:2;index:idx_antifraud_user_created,priority:2;comment:判定时间"`
	UpdatedAt time.Time `json:"updated_at" gorm:"comment:更新时间"`

	Platform  string `json:"platform"   gorm:"size:32;index;comment:平台标识（qq/napcat/wechat/telegram）"`
	ConnID    string `json:"conn_id"    gorm:"size:64;comment:来源连接ID（处置时优先用该连接）"`
	GroupID   string `json:"group_id"   gorm:"size:64;index:idx_antifraud_group_created,priority:1;comment:群ID（私聊为空）"`
	UserID    string `json:"user_id"    gorm:"size:64;index:idx_antifraud_user_created,priority:1;comment:发送者平台用户ID"`
	Nickname  string `json:"nickname"   gorm:"size:255;comment:发送者昵称"`
	MessageID string `json:"message_id" gorm:"size:64;comment:平台消息ID（撤回目标）"`

	Content     string `json:"content"      gorm:"type:text;comment:送判文本（截断后，即模型实际看到的内容）"`
	ContentHash string `json:"content_hash" gorm:"size:64;index;comment:送判文本的SHA-256（去重合并与缓存键）"`

	LabelID        int            `json:"label_id"        gorm:"comment:top1 类别ID（0=正常）"`
	LabelKey       string         `json:"label_key"       gorm:"size:64;comment:top1 类别英文标识"`
	LabelName      string         `json:"label_name"      gorm:"size:64;comment:top1 类别中文名"`
	Confidence     float64        `json:"confidence"      gorm:"comment:top1 类别置信度"`
	ViolationScore float64        `json:"violation_score" gorm:"comment:违规分（1 - P(正常)）"`
	Probabilities  datatypes.JSON `json:"probabilities"   gorm:"type:jsonb;comment:10 类概率分布"`
	ExpertWeights  datatypes.JSON `json:"expert_weights"  gorm:"type:jsonb;comment:专家门控权重 [{key,weight}]"`

	Status      string    `json:"status"       gorm:"size:16;index:idx_antifraud_status_created,priority:1;comment:pending/auto_handled/approved/rejected"`
	RepeatCount int       `json:"repeat_count" gorm:"default:1;comment:同一用户同文本的重复次数（合并窗口内）"`
	LastSeenAt  time.Time `json:"last_seen_at" gorm:"comment:最近一次重复出现时间"`

	RecallStatus string `json:"recall_status" gorm:"size:16;comment:撤回动作结果 skipped/ok/failed"`
	RecallError  string `json:"recall_error"  gorm:"size:255;comment:撤回失败原因（平台原始错误）"`
	MuteStatus   string `json:"mute_status"   gorm:"size:16;comment:禁言动作结果 skipped/ok/failed"`
	MuteError    string `json:"mute_error"    gorm:"size:255;comment:禁言失败原因（平台原始错误）"`
	MuteSeconds  int    `json:"mute_seconds"  gorm:"comment:实际下发的禁言时长（秒）"`

	ReviewedBy string     `json:"reviewed_by"  gorm:"size:64;comment:审批人（面板管理员用户名）"`
	ReviewedAt *time.Time `json:"reviewed_at"  gorm:"comment:审批时间"`
	ReviewNote string     `json:"review_note"  gorm:"size:255;comment:审批备注"`
	NotifiedAt *time.Time `json:"notified_at"  gorm:"comment:上报成功时间（至少一个目标送达）"`
}

// TableName 指定 GORM 表名为 antifraud_records。
func (Record) TableName() string { return "antifraud_records" }

// Filter 是管理面板的违规记录查询条件，零值表示不过滤。
type Filter struct {
	Status   string // pending / auto_handled / approved / rejected
	Platform string
	GroupID  string
	UserID   string
	LabelKey string
	Keyword  string // 送判文本或昵称模糊匹配
	Since    *time.Time
	Until    *time.Time
	Offset   int
	Limit    int
}

// ScoreBucket 是违规分区间计数（用于面板调阈值）。
type ScoreBucket struct {
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Count int64   `json:"count"`
}

// LabelCount 是按类别统计的违规数。
type LabelCount struct {
	LabelKey  string `json:"label_key"`
	LabelName string `json:"label_name"`
	Count     int64  `json:"count"`
}

// Stats 是反诈面板的概览统计。
type Stats struct {
	Pending      int64         `json:"pending"`       // 待审批总数
	TodayTotal   int64         `json:"today_total"`   // 今日新增违规记录
	TodayAuto    int64         `json:"today_auto"`    // 今日自动处置数
	ActionFailed int64         `json:"action_failed"` // 处置动作失败的记录数
	ScoreBuckets []ScoreBucket `json:"score_buckets"` // 近 7 天违规分分布
	LabelCounts  []LabelCount  `json:"label_counts"`  // 近 7 天按类别计数
}
