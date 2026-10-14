package antifraud

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/DaWesen/lanmei-dream/internal/model"
)

// Store 是违规记录的数据库访问。
type Store struct {
	db *gorm.DB
}

// NewStore 创建违规记录存储。
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// Migrate 建表（幂等）；表由插件自管，不加入主库迁移列表。
func (s *Store) Migrate(ctx context.Context) error {
	if err := s.db.WithContext(ctx).AutoMigrate(&Record{}); err != nil {
		return fmt.Errorf("antifraud: 违规记录表迁移失败: %w", err)
	}
	return nil
}

// Create 写入一条违规记录。
func (s *Store) Create(ctx context.Context, rec *Record) error {
	if err := s.db.WithContext(ctx).Create(rec).Error; err != nil {
		return fmt.Errorf("antifraud: 违规记录写入失败: %w", err)
	}
	return nil
}

// Save 保存记录（处置结果、审批结论等更新）。
func (s *Store) Save(ctx context.Context, rec *Record) error {
	if err := s.db.WithContext(ctx).Save(rec).Error; err != nil {
		return fmt.Errorf("antifraud: 违规记录更新失败: %w", err)
	}
	return nil
}

// Get 按主键读取记录。
//
// 返回：记录不存在时返回 gorm.ErrRecordNotFound。
func (s *Store) Get(ctx context.Context, id uint) (*Record, error) {
	var rec Record
	if err := s.db.WithContext(ctx).First(&rec, id).Error; err != nil {
		return nil, err
	}
	return &rec, nil
}

// FindRecentDuplicate 查找合并窗口内同群同用户同文本的既有记录。
//
// 返回：无匹配时返回 (nil, nil)。
func (s *Store) FindRecentDuplicate(ctx context.Context, groupID, userID, contentHash string, since time.Time) (*Record, error) {
	var rec Record
	err := s.db.WithContext(ctx).
		Where("group_id = ? AND user_id = ? AND content_hash = ? AND last_seen_at >= ?", groupID, userID, contentHash, since).
		Order("id DESC").
		First(&rec).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("antifraud: 查询重复记录失败: %w", err)
	}
	return &rec, nil
}

// TouchRepeat 累加重复次数并刷新最近出现时间（只更新计数列，不覆盖处置结果）。
func (s *Store) TouchRepeat(ctx context.Context, id uint, seenAt time.Time) error {
	err := s.db.WithContext(ctx).Model(&Record{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"repeat_count": gorm.Expr("repeat_count + 1"),
			"last_seen_at": seenAt,
		}).Error
	if err != nil {
		return fmt.Errorf("antifraud: 重复计数更新失败: %w", err)
	}
	return nil
}

// MarkReviewed 写入审批结论。
//
// 参数：
//   - status：StatusApproved 或 StatusRejected
//   - reviewer：面板管理员用户名
//   - note：审批备注（可为空）
func (s *Store) MarkReviewed(ctx context.Context, id uint, status, reviewer, note string, at time.Time) error {
	err := s.db.WithContext(ctx).Model(&Record{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":      status,
			"reviewed_by": reviewer,
			"reviewed_at": at,
			"review_note": note,
		}).Error
	if err != nil {
		return fmt.Errorf("antifraud: 审批结论写入失败: %w", err)
	}
	return nil
}

// MarkNotified 记录上报成功时间。
func (s *Store) MarkNotified(ctx context.Context, id uint, at time.Time) error {
	err := s.db.WithContext(ctx).Model(&Record{}).
		Where("id = ?", id).
		Update("notified_at", at).Error
	if err != nil {
		return fmt.Errorf("antifraud: 上报时间写入失败: %w", err)
	}
	return nil
}

// Delete 删除单条记录。
func (s *Store) Delete(ctx context.Context, id uint) error {
	if err := s.db.WithContext(ctx).Delete(&Record{}, id).Error; err != nil {
		return fmt.Errorf("antifraud: 违规记录删除失败: %w", err)
	}
	return nil
}

// DeleteBefore 删除 cutoff 之前的已终结记录（pending 永不自动删除，避免丢失待审批项）。
//
// 返回：受影响行数。
func (s *Store) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res := s.db.WithContext(ctx).
		Where("created_at < ? AND status <> ?", cutoff, StatusPending).
		Delete(&Record{})
	if res.Error != nil {
		return 0, fmt.Errorf("antifraud: 过期记录清理失败: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// List 分页查询违规记录，附带过滤条件总数。
//
// 返回：记录列表（按判定时间倒序）与满足条件的总数；Limit <= 0 时按默认 20 条返回，
// 避免调用方漏设分页导致 GORM 生成 LIMIT 0 而拿到空列表。
func (s *Store) List(ctx context.Context, f Filter) ([]Record, int64, error) {
	if f.Limit <= 0 {
		f.Limit = 20
	}
	q := s.db.WithContext(ctx).Model(&Record{})
	countQ := s.db.WithContext(ctx).Model(&Record{})
	for _, cond := range filterConditions(f) {
		q = q.Where(cond.query, cond.args...)
		countQ = countQ.Where(cond.query, cond.args...)
	}

	var total int64
	if err := countQ.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("antifraud: 违规记录计数失败: %w", err)
	}
	var list []Record
	if err := q.Order("id DESC").Offset(f.Offset).Limit(f.Limit).Find(&list).Error; err != nil {
		return nil, 0, fmt.Errorf("antifraud: 违规记录查询失败: %w", err)
	}
	return list, total, nil
}

// sqlCond 是一条过滤条件（查询片段与参数）。
type sqlCond struct {
	query string
	args  []any
}

// filterConditions 把查询条件转成 SQL 片段；计数与列表共用同一组条件，避免两处漂移。
func filterConditions(f Filter) []sqlCond {
	conds := make([]sqlCond, 0, 9)
	if f.Status != "" {
		conds = append(conds, sqlCond{"status = ?", []any{f.Status}})
	}
	if f.Platform != "" {
		conds = append(conds, sqlCond{"platform = ?", []any{f.Platform}})
	}
	if f.GroupID != "" {
		conds = append(conds, sqlCond{"group_id = ?", []any{f.GroupID}})
	}
	if f.UserID != "" {
		conds = append(conds, sqlCond{"user_id = ?", []any{f.UserID}})
	}
	if f.LabelKey != "" {
		conds = append(conds, sqlCond{"label_key = ?", []any{f.LabelKey}})
	}
	if f.Keyword != "" {
		like := "%" + f.Keyword + "%"
		conds = append(conds, sqlCond{"(LOWER(content) LIKE LOWER(?) OR LOWER(nickname) LIKE LOWER(?))", []any{like, like}})
	}
	if f.Since != nil {
		conds = append(conds, sqlCond{"created_at >= ?", []any{*f.Since}})
	}
	if f.Until != nil {
		conds = append(conds, sqlCond{"created_at <= ?", []any{*f.Until}})
	}
	return conds
}

// ListAdminTargets 返回动态管理员（bot_admin 表）的平台与用户 ID，用于违规上报。
func (s *Store) ListAdminTargets(ctx context.Context) ([]AdminTarget, error) {
	var rows []AdminTarget
	err := s.db.WithContext(ctx).Model(&model.BotAdmin{}).
		Select("platform, user_id").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("antifraud: 动态管理员查询失败: %w", err)
	}
	return rows, nil
}

// Stats 汇总面板概览：待审批数、今日计数、动作失败数与近 7 天的违规分分布/类别计数。
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekAgo := dayStart.AddDate(0, 0, -6)

	// 每个计数都用独立会话：GORM 的 Where 会就地累积条件，复用同一 *gorm.DB 会把
	// 前一个统计的条件带给下一个，得到互相与运算的错误结果。
	query := func() *gorm.DB { return s.db.WithContext(ctx).Model(&Record{}) }
	count := func(q *gorm.DB, dest *int64) error {
		if err := q.Count(dest).Error; err != nil {
			return fmt.Errorf("antifraud: 违规记录计数失败: %w", err)
		}
		return nil
	}

	if err := count(query().Where("status = ?", StatusPending), &stats.Pending); err != nil {
		return stats, err
	}
	if err := count(query().Where("created_at >= ?", dayStart), &stats.TodayTotal); err != nil {
		return stats, err
	}
	// 今日处置数包含自动处置与审批通过两种（都实际执行了动作）
	if err := count(query().Where("created_at >= ? AND status IN ?", dayStart, []string{StatusAutoHandled, StatusApproved}), &stats.TodayAuto); err != nil {
		return stats, err
	}
	if err := count(query().Where("recall_status = ? OR mute_status = ?", ActionFailed, ActionFailed), &stats.ActionFailed); err != nil {
		return stats, err
	}

	buckets, err := s.scoreBuckets(ctx, weekAgo)
	if err != nil {
		return stats, err
	}
	stats.ScoreBuckets = buckets

	labels, err := s.labelCounts(ctx, weekAgo)
	if err != nil {
		return stats, err
	}
	stats.LabelCounts = labels
	return stats, nil
}

// scoreBuckets 统计近 7 天违规分分布（固定区间，供管理员校准阈值）。
func (s *Store) scoreBuckets(ctx context.Context, since time.Time) ([]ScoreBucket, error) {
	table := Record{}.TableName()
	query := fmt.Sprintf(`SELECT
		COALESCE(SUM(CASE WHEN violation_score < 0.7 THEN 1 ELSE 0 END), 0) AS b1,
		COALESCE(SUM(CASE WHEN violation_score >= 0.7 AND violation_score < 0.8 THEN 1 ELSE 0 END), 0) AS b2,
		COALESCE(SUM(CASE WHEN violation_score >= 0.8 AND violation_score < 0.9 THEN 1 ELSE 0 END), 0) AS b3,
		COALESCE(SUM(CASE WHEN violation_score >= 0.9 THEN 1 ELSE 0 END), 0) AS b4
		FROM %s WHERE created_at >= ?`, table)
	var row struct {
		B1 int64 `gorm:"column:b1"`
		B2 int64 `gorm:"column:b2"`
		B3 int64 `gorm:"column:b3"`
		B4 int64 `gorm:"column:b4"`
	}
	if err := s.db.WithContext(ctx).Raw(query, since).Scan(&row).Error; err != nil {
		return nil, fmt.Errorf("antifraud: 违规分分布统计失败: %w", err)
	}
	return []ScoreBucket{
		{Min: 0.6, Max: 0.7, Count: row.B1},
		{Min: 0.7, Max: 0.8, Count: row.B2},
		{Min: 0.8, Max: 0.9, Count: row.B3},
		{Min: 0.9, Max: 1.0, Count: row.B4},
	}, nil
}

// labelCounts 统计近 7 天各类别的违规数（最多 10 类）。
func (s *Store) labelCounts(ctx context.Context, since time.Time) ([]LabelCount, error) {
	var rows []LabelCount
	err := s.db.WithContext(ctx).Model(&Record{}).
		Select("label_key, label_name, COUNT(*) AS count").
		Where("created_at >= ?", since).
		Group("label_key, label_name").
		Order("count DESC").
		Limit(10).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("antifraud: 类别统计失败: %w", err)
	}
	return rows, nil
}
