package antifraud

// normalLabelID 是「正常」类别在概率分布中的下标，取自模型约定
// （LanmeiAntiFraud config.toml 的 experts[0]，index 即类别 id）。
const normalLabelID = 0

// Thresholds 是处置阈值（违规分 = 1 - P(正常)）。
type Thresholds struct {
	Auto   float64 // 达到该值：自动撤回 + 禁言 + 上报
	Report float64 // 达到该值：入库并上报，等管理员审批
}

// Verdict 是一次判定的精简结果（缓存、处置与入库只依赖这些字段）。
type Verdict struct {
	LabelID        int            `json:"label_id"`
	LabelKey       string         `json:"label_key"`
	LabelName      string         `json:"label_name"`
	Confidence     float64        `json:"confidence"`
	ViolationScore float64        `json:"violation_score"`
	Probabilities  []float64      `json:"probabilities"`
	ExpertWeights  []ExpertWeight `json:"expert_weights,omitempty"`
}

// ExpertWeight 是单个专家的门控权重（缓存里不保留专家的整段概率分布，控制 Redis 体积）。
type ExpertWeight struct {
	Key    string  `json:"key"`
	Weight float64 `json:"weight"`
}

// Level 是判定等级。
type Level int

const (
	// LevelNone 未达上报阈值（或 top1 为正常类）：不记录、不通知。
	LevelNone Level = iota
	// LevelReport 达上报阈值：入库并上报，等管理员审批。
	LevelReport
	// LevelAuto 达自动阈值：自动撤回 + 禁言并上报。
	LevelAuto
)

// toVerdict 把服务返回的单条结果转成精简判定（丢弃专家概率分布，保留门控权重）。
func toVerdict(item InferItem) Verdict {
	weights := make([]ExpertWeight, 0, len(item.ExpertsOpinion))
	for _, e := range item.ExpertsOpinion {
		weights = append(weights, ExpertWeight{Key: e.Key, Weight: e.Weight})
	}
	return Verdict{
		LabelID:        item.LabelID,
		LabelKey:       item.LabelKey,
		LabelName:      item.LabelName,
		Confidence:     item.Confidence,
		ViolationScore: violationScore(item.Probabilities),
		Probabilities:  item.Probabilities,
		ExpertWeights:  weights,
	}
}

// violationScore 计算违规分：1 - P(正常)；概率缺失或无「正常」类别时返回 0。
func violationScore(probabilities []float64) float64 {
	if len(probabilities) <= normalLabelID {
		return 0
	}
	score := 1 - probabilities[normalLabelID]
	if score < 0 {
		return 0
	}
	return score
}

// classify 按阈值给出处置等级。
//
// 判定同时要求 top1 是违规类别：若模型认为最可能的类别是「正常」（P(正常) 为最大但不够大），
// 其余类别的概率之和仍可能越过阈值，此时只记录分数不做违规处置，避免误伤。
func classify(v Verdict, th Thresholds) Level {
	if len(v.Probabilities) == 0 || v.LabelID == normalLabelID {
		return LevelNone
	}
	switch {
	case v.ViolationScore >= th.Auto:
		return LevelAuto
	case v.ViolationScore >= th.Report:
		return LevelReport
	default:
		return LevelNone
	}
}
